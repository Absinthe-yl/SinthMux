package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

// Session notifications live in tmux itself: `sinthmux-connector notify`, run
// inside a pane, stores a session user option and signals a wait-for channel.
// The running connector wakes up, reads every session's option and sends the
// Hub a snapshot. No extra port, socket or file is involved, and the
// notification survives connector restarts because tmux keeps it.
const (
	notifyOption  = "@sinthmux_notify"
	notifyChannel = "sinthmux-notify"
	notifyPoll    = 15 * time.Second
)

const notifySupported = extendedTmux

// encodeNotification stores "v1:<unix ms>:<color>:<base64url message>". The
// colon separator and base64 keep tmux format characters (#, |) out of it.
func encodeNotification(color, message string, at time.Time) string {
	return "v1:" + strconv.FormatInt(at.UnixMilli(), 10) + ":" + color + ":" + base64.RawURLEncoding.EncodeToString([]byte(message))
}

func decodeNotification(session, value string) (protocol.SessionNotification, bool) {
	parts := strings.SplitN(value, ":", 4)
	if len(parts) != 4 || parts[0] != "v1" {
		return protocol.SessionNotification{}, false
	}
	at, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return protocol.SessionNotification{}, false
	}
	message, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || !utf8.Valid(message) {
		return protocol.SessionNotification{}, false
	}
	color := parts[2]
	if !protocol.NotificationColors[color] {
		color = "blue"
	}
	return protocol.SessionNotification{Session: session, Color: color, Message: truncateUTF8(string(message), protocol.MaxNotificationMessage), At: at}, true
}

// truncateUTF8 cuts s to at most limit bytes without splitting a character.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// parseNotifications reads `list-sessions -F '#{session_name}|#{@sinthmux_notify}'`.
// The value never contains "|", so the last "|" separates even names that do.
func parseNotifications(output string) []protocol.SessionNotification {
	items := []protocol.SessionNotification{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		separator := strings.LastIndex(line, "|")
		if separator < 0 {
			continue
		}
		session, value := line[:separator], line[separator+1:]
		if value == "" || !protocol.ValidSessionName(session) {
			continue
		}
		if item, ok := decodeNotification(session, value); ok {
			items = append(items, item)
		}
		if len(items) == protocol.MaxNotifications {
			break
		}
	}
	slices.SortFunc(items, func(a, b protocol.SessionNotification) int { return strings.Compare(a.Session, b.Session) })
	return items
}

func readNotifications(ctx context.Context) ([]protocol.SessionNotification, error) {
	output, err := runTmux(ctx, "list-sessions", "-F", "#{session_name}|#{"+notifyOption+"}")
	if err != nil {
		var commandErr *tmuxError
		if errors.As(err, &commandErr) && commandErr.code == "not_found" {
			return []protocol.SessionNotification{}, nil
		}
		return nil, err
	}
	return parseNotifications(output), nil
}

// signalNotify wakes the connector; tmux keeps the signal if nobody waits yet.
func signalNotify(ctx context.Context) {
	_, _ = runTmux(ctx, "wait-for", "-S", notifyChannel)
}

func clearNotification(ctx context.Context, session string) error {
	if !notifySupported {
		return &tmuxError{code: "unsupported_method", message: "session notifications are not supported on this device"}
	}
	if _, err := runTmux(ctx, "set-option", "-u", "-t", paneTarget(session), notifyOption); err != nil {
		return err
	}
	signalNotify(ctx)
	return nil
}

type notifier struct {
	logger  *slog.Logger
	mu      sync.Mutex
	current []protocol.SessionNotification
	changed chan struct{}
}

func newNotifier(logger *slog.Logger) *notifier {
	return &notifier{logger: logger, current: []protocol.SessionNotification{}, changed: make(chan struct{}, 1)}
}

// refresh rereads tmux and reports whether the snapshot changed.
func (n *notifier) refresh(ctx context.Context) {
	items, err := readNotifications(ctx)
	if err != nil {
		return
	}
	n.mu.Lock()
	same := slices.Equal(items, n.current)
	n.current = items
	n.mu.Unlock()
	if !same {
		select {
		case n.changed <- struct{}{}:
		default:
		}
	}
}

func (n *notifier) snapshot() protocol.NotificationSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	return protocol.NotificationSnapshot{Items: slices.Clone(n.current)}
}

func (n *notifier) clear(ctx context.Context, session string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !protocol.ValidSessionName(session) {
		return
	}
	n.mu.Lock()
	present := slices.ContainsFunc(n.current, func(item protocol.SessionNotification) bool { return item.Session == session })
	n.mu.Unlock()
	if present && clearNotification(ctx, session) == nil {
		n.refresh(ctx)
	}
}

// watch blocks in `tmux wait-for` so a notify reaches the Hub immediately; a
// periodic reread covers missed signals and options changed by hand. Without a
// tmux server, wait-for fails at once, so errors back off.
func (n *notifier) watch(ctx context.Context) {
	n.refresh(ctx)
	wake := make(chan struct{}, 1)
	go func() {
		for ctx.Err() == nil {
			command := exec.CommandContext(ctx, tmuxExecutable(), tmuxArgs("wait-for", notifyChannel)...)
			hideConsole(command)
			if command.Run() != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				continue
			}
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}()
	ticker := time.NewTicker(notifyPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		n.refresh(refreshCtx)
		cancel()
	}
}

// publish sends the full snapshot when a connection starts and on every change.
func (n *notifier) publish(ctx context.Context, send func(protocol.NotificationSnapshot) error) {
	if send(n.snapshot()) != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.changed:
			if send(n.snapshot()) != nil {
				return
			}
		}
	}
}

// notifyCommand implements `sinthmux-connector notify [--color C] [--clear] [--session S] [message]`.
func notifyCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("notify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	color := flags.String("color", "blue", "blue, green, yellow or red")
	clear := flags.Bool("clear", false, "remove the session's notification")
	session := flags.String("session", "", "session name (default: the session this command runs in)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "用法：sinthmux-connector notify [--color blue|green|yellow|red] [--session 名称] 消息")
		fmt.Fprintln(stderr, "      sinthmux-connector notify --clear [--session 名称]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if !notifySupported {
		fmt.Fprintln(stderr, "此平台的设备代理暂不支持会话提醒")
		return 2
	}
	if !protocol.NotificationColors[*color] {
		fmt.Fprintln(stderr, "颜色只能是 blue、green、yellow 或 red")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target := ""
	if *session != "" {
		if !protocol.ValidSessionName(*session) {
			fmt.Fprintln(stderr, "会话名只能包含字母、数字、下划线和连字符")
			return 2
		}
		target = paneTarget(*session)
	} else {
		pane := os.Getenv("TMUX_PANE")
		socket, _, _ := strings.Cut(os.Getenv("TMUX"), ",")
		if pane == "" || socket == "" {
			fmt.Fprintln(stderr, "不在 tmux 会话中运行；请在 SinthMux 会话里执行，或用 --session 指定会话名")
			return 2
		}
		// Talk to the tmux server this pane belongs to, even if it uses a
		// private socket (tmux -L sinthmux).
		os.Setenv("SINTHMUX_TMUX_SOCKET", "")
		tmuxSocketPath = socket
		target = pane
	}
	var err error
	if *clear {
		_, err = runTmux(ctx, "set-option", "-u", "-t", target, notifyOption)
	} else {
		message := truncateUTF8(strings.Join(flags.Args(), " "), protocol.MaxNotificationMessage)
		_, err = runTmux(ctx, "set-option", "-t", target, notifyOption, encodeNotification(*color, message, time.Now()))
	}
	if err != nil {
		fmt.Fprintln(stderr, "设置提醒失败："+err.Error())
		return 1
	}
	signalNotify(ctx)
	return 0
}
