package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ---------- reconnect strategies ----------

// link is one live terminal connection, SinthMux or SSH.
type link interface {
	frames() <-chan frame
	done() <-chan struct{}
	ping() error // application-level probe; nil error means sent
	close()
}

type wsLink struct {
	t   *terminal
	seq int
}

func (l *wsLink) frames() <-chan frame  { return l.t.frames }
func (l *wsLink) done() <-chan struct{} { return l.t.done }
func (l *wsLink) close()                { l.t.close() }
func (l *wsLink) ping() error {
	l.seq++
	return l.t.control(map[string]string{"type": "ping", "id": fmt.Sprint(l.seq)})
}

type sshLink struct{ t *sshTerminal }

func (l *sshLink) frames() <-chan frame  { return l.t.frames }
func (l *sshLink) done() <-chan struct{} { return l.t.done }
func (l *sshLink) close()                { l.t.close() }
func (l *sshLink) ping() error           { return nil }

// strategy mirrors apps/web/src/terminalConnection.ts when configured as
// "sinthmux": heartbeat every 15 s with a 6 s answer window, a dead live
// connection retries at once, then 1/2/4/8/10 s backoff; an "online" event
// (network back) probes a live connection within 3 s or ends a backoff wait.
type strategy struct {
	name        string
	open        func() (link, error)
	heartbeat   time.Duration // 0: no application heartbeat
	pongWithin  time.Duration
	backoff     func(failures int) time.Duration // <0: never reconnect
	immediate   bool                             // first retry after a live connection is immediate
	onlineEvent bool
}

func sinthmuxBackoff(f int) time.Duration {
	d := time.Second << f
	if f > 4 || d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

func fixedBackoff(d time.Duration) func(int) time.Duration {
	return func(int) time.Duration { return d }
}

func never(int) time.Duration { return -1 }

type event struct {
	kind string // live, dead, attempt-fail
	at   time.Time
}

// run drives one strategy until ctx ends and reports events.
func (s strategy) run(ctx context.Context, online <-chan struct{}, report func(event)) {
	failures := 0
	connectedOnce := false
	for ctx.Err() == nil {
		// Connect, ignoring online events while an attempt runs (as the page does).
		result := make(chan struct {
			l   link
			err error
		}, 1)
		go func() {
			l, err := s.open()
			result <- struct {
				l   link
				err error
			}{l, err}
		}()
		var current link
	waitOpen:
		for {
			select {
			case <-ctx.Done():
				go func() {
					if r := <-result; r.l != nil {
						r.l.close()
					}
				}()
				return
			case <-online:
			case r := <-result:
				if r.err != nil {
					report(event{"attempt-fail", time.Now()})
				}
				current = r.l
				break waitOpen
			}
		}
		immediate := false
		if current != nil {
			failures = 0
			connectedOnce = true
			report(event{"live", time.Now()})
			s.live(ctx, current, online)
			current.close()
			if ctx.Err() != nil {
				return
			}
			report(event{"dead", time.Now()})
			immediate = s.immediate
		}
		_ = connectedOnce
		var delay time.Duration
		if immediate && failures == 0 {
			delay = 0
		} else {
			delay = s.backoff(failures)
		}
		failures++
		if delay < 0 {
			<-ctx.Done()
			return
		}
		timer := time.NewTimer(delay)
	waitRetry:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				break waitRetry
			case <-online:
				if s.onlineEvent {
					timer.Stop()
					failures = 0
					break waitRetry
				}
			}
		}
	}
}

// live returns when the connection is considered dead.
func (s strategy) live(ctx context.Context, l link, online <-chan struct{}) {
	var lastReceived time.Time = time.Now()
	var ticker <-chan time.Time
	if s.heartbeat > 0 {
		t := time.NewTicker(s.heartbeat)
		defer t.Stop()
		ticker = t.C
	}
	type probe struct {
		sentAt time.Time
		timer  *time.Timer
	}
	var probes []probe
	expired := make(chan time.Time, 16)
	startProbe := func(within time.Duration) {
		sentAt := time.Now()
		if l.ping() != nil {
			return
		}
		timer := time.AfterFunc(within, func() { expired <- sentAt })
		probes = append(probes, probe{sentAt, timer})
	}
	defer func() {
		for _, p := range probes {
			p.timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-l.frames():
			lastReceived = f.at
		case <-l.done():
			return
		case <-ticker:
			startProbe(s.pongWithin)
		case sentAt := <-expired:
			if lastReceived.Before(sentAt) {
				return
			}
		case <-online:
			if s.onlineEvent && s.heartbeat > 0 {
				startProbe(3 * time.Second)
			}
		}
	}
}

// ---------- the experiment ----------

type scenario struct {
	name   string
	class  string // which side loses the network: client or device
	mode   string // blackhole or refuse
	outage time.Duration
	online bool // the browser gets an "online" event when the network returns
	reset  bool // frozen links get an RST when the network returns
}

func scenarios(names string) []scenario {
	all := map[string]scenario{
		"switch":        {"switch", "client", "blackhole", 0, false, false},
		"down-5s":       {"down-5s", "client", "refuse", 5 * time.Second, true, false},
		"down-30s":      {"down-30s", "client", "refuse", 30 * time.Second, true, false},
		"down-120s":     {"down-120s", "client", "refuse", 120 * time.Second, true, false},
		"silent-30s":    {"silent-30s", "client", "blackhole", 30 * time.Second, false, false},
		"device-switch": {"device-switch", "device", "blackhole", 10 * time.Second, false, true},
		"device-silent": {"device-silent", "device", "blackhole", 10 * time.Second, false, false},
	}
	var result []scenario
	for _, name := range strings.Split(names, ",") {
		if sc, ok := all[strings.TrimSpace(name)]; ok {
			result = append(result, sc)
		}
	}
	return result
}

func runRecovery(s Stack, flags *flag.FlagSet, args []string) error {
	repeats := flags.Int("n", 6, "trials per scenario")
	names := flags.String("scenarios", "switch,down-5s,down-30s,down-120s,silent-30s,device-switch,device-silent", "scenarios")
	strategyNames := flags.String("strategies", "sinthmux,sinthmux-no-event,fixed-5s,ssh-keepalive,ssh-default", "strategies")
	limit := flags.Duration("limit", 120*time.Second, "give up this long after the network returns")
	profileName := flags.String("profile", "wan", "network profile")
	seed := flags.Int64("seed", 20261012, "random seed for the outage phase")
	_ = flags.Parse(args)
	random := rand.New(rand.NewSource(*seed))

	if err := applyProfile(s, profiles[*profileName]); err != nil {
		return err
	}
	defer applyProfile(s, profiles["lan"])
	owner, err := login(s.Proxy, s.Proxy, s.Owner)
	if err != nil {
		return err
	}
	device, err := owner.deviceByName(s.Device, 20*time.Second)
	if err != nil {
		return err
	}
	sshCfg, err := sshConfig(s.SSHKey)
	if err != nil {
		return err
	}
	// Browsers close pooled sockets on a network change; the "online" event
	// does the same for the test clients.
	var webClients []*client
	build := func(name string) strategy {
		session := "exp-rec-" + strings.ReplaceAll(name, "-", "")
		_ = owner.ensureSession(device.ID, session)
		web := owner.clone(s.Proxy, 8*time.Second)
		webClients = append(webClients, web)
		openWeb := func() (link, error) {
			t, _, err := openTerminal(web, device.ID, session, 10*time.Second)
			if err != nil {
				return nil, err
			}
			return &wsLink{t: t}, nil
		}
		command := fmt.Sprintf("tmux -L %s -u attach-session -t =%s", s.Sock, session)
		openSSHLink := func(keepalive bool) func() (link, error) {
			return func() (link, error) {
				t, _, err := openSSH(s.SSH, sshCfg, command, 10*time.Second)
				if err != nil {
					return nil, err
				}
				if keepalive {
					t.keepalive(15*time.Second, 3)
				}
				return &sshLink{t: t}, nil
			}
		}
		switch name {
		case "sinthmux":
			return strategy{name: name, open: openWeb, heartbeat: 15 * time.Second, pongWithin: 6 * time.Second, backoff: sinthmuxBackoff, immediate: true, onlineEvent: true}
		case "sinthmux-no-event":
			return strategy{name: name, open: openWeb, heartbeat: 15 * time.Second, pongWithin: 6 * time.Second, backoff: sinthmuxBackoff, immediate: true}
		case "fixed-5s":
			return strategy{name: name, open: openWeb, heartbeat: 15 * time.Second, pongWithin: 6 * time.Second, backoff: fixedBackoff(5 * time.Second)}
		case "ssh-keepalive":
			return strategy{name: name, open: openSSHLink(true), backoff: fixedBackoff(5 * time.Second)}
		case "ssh-default":
			return strategy{name: name, open: openSSHLink(false), backoff: never}
		}
		panic("unknown strategy " + name)
	}
	var chosen []strategy
	for _, name := range strings.Split(*strategyNames, ",") {
		chosen = append(chosen, build(strings.TrimSpace(name)))
	}

	out, err := newTable(s, "recovery.csv", "scenario", "trial", "strategy", "recovered", "detect_ms", "recover_after_restore_ms", "total_ms", "attempts_failed")
	if err != nil {
		return err
	}
	defer out.close()

	for _, sc := range scenarios(*names) {
		if sc.class == "device" {
			if err := deviceOutage(s, owner, device.ID, out, sc, min(*repeats, 3), *limit); err != nil {
				return err
			}
			continue
		}
		for trial := 0; trial < *repeats; trial++ {
			type state struct {
				mu       sync.Mutex
				live     bool
				events   []event
				onlineCh chan struct{}
			}
			states := make([]*state, len(chosen))
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			for i, st := range chosen {
				states[i] = &state{onlineCh: make(chan struct{}, 1)}
				wg.Add(1)
				go func(st strategy, state *state) {
					defer wg.Done()
					st.run(ctx, state.onlineCh, func(e event) {
						state.mu.Lock()
						state.events = append(state.events, e)
						state.live = e.kind == "live"
						state.mu.Unlock()
					})
				}(st, states[i])
			}
			// All live, then a random heartbeat phase.
			deadline := time.Now().Add(30 * time.Second)
			for {
				all := true
				for _, st := range states {
					st.mu.Lock()
					all = all && st.live
					st.mu.Unlock()
				}
				if all {
					break
				}
				if time.Now().After(deadline) {
					cancel()
					wg.Wait()
					return fmt.Errorf("%s trial %d: strategies did not come up", sc.name, trial)
				}
				time.Sleep(100 * time.Millisecond)
			}
			time.Sleep(time.Duration(random.Int63n(int64(15 * time.Second))))

			start := time.Now()
			if sc.class == "device" {
				_ = control(s.HubCtl, "/block?class=device&mode="+sc.mode)
			} else {
				_ = control(s.HubCtl, "/block?class=client&mode="+sc.mode)
				_ = control(s.SSHCtl, "/block?mode="+sc.mode)
			}
			time.Sleep(sc.outage)
			restored := time.Now()
			if sc.class == "device" {
				_ = control(s.HubCtl, "/unblock?class=device")
			} else {
				_ = control(s.HubCtl, "/unblock?class=client")
				_ = control(s.SSHCtl, "/unblock")
			}
			if sc.online {
				for _, web := range webClients {
					web.http.CloseIdleConnections()
				}
				for _, st := range states {
					select {
					case st.onlineCh <- struct{}{}:
					default:
					}
				}
			}
			// Wait until every strategy has a live connection opened after the outage began.
			recoveredAt := func(st *state) (time.Time, time.Time, int) {
				st.mu.Lock()
				defer st.mu.Unlock()
				var dead, live time.Time
				failed := 0
				for _, e := range st.events {
					if e.at.Before(start) {
						continue
					}
					switch e.kind {
					case "dead":
						if dead.IsZero() {
							dead = e.at
						}
					case "live":
						if live.IsZero() {
							live = e.at
						}
					case "attempt-fail":
						failed++
					}
				}
				return dead, live, failed
			}
			end := restored.Add(*limit)
			for time.Now().Before(end) {
				all := true
				for i, st := range states {
					_, live, _ := recoveredAt(st)
					// For a device-side outage only SinthMux strategies can tell.
					// ssh-default never reconnects by design; do not wait for it.
					if live.IsZero() && chosen[i].name != "ssh-default" {
						all = false
					}
				}
				if all {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			// For the device scenario "live" in the terminal sense needs output from
			// the device, which the strategies only see on a new connection.
			for i, st := range states {
				dead, live, failed := recoveredAt(st)
				if sc.class == "device" && strings.HasPrefix(chosen[i].name, "ssh") {
					continue // SSH does not cross the Hub; not affected
				}
				detect := any("")
				if !dead.IsZero() {
					detect = dead.Sub(start)
				}
				if live.IsZero() {
					out.row(sc.name, trial, chosen[i].name, 0, detect, "", "", failed)
				} else {
					after := live.Sub(restored)
					if after < 0 {
						after = 0
					}
					out.row(sc.name, trial, chosen[i].name, 1, detect, after, live.Sub(start), failed)
				}
			}
			cancel()
			wg.Wait()
			if sc.class == "device" {
				// The frozen connector link never ends by itself: restart the
				// connector so the next trial starts from a working device.
				if err := restartConnector(s, owner, device.ID); err != nil {
					return err
				}
			}
			// Pooled connections that crossed the outage may be frozen forever.
			owner.http.CloseIdleConnections()
			for _, web := range webClients {
				web.http.CloseIdleConnections()
			}
			logf("recovery %s trial %d done", sc.name, trial)
			time.Sleep(2 * time.Second)
		}
	}
	return nil
}

// deviceOutage freezes the connector ↔ Hub link (the device switched networks)
// and then, once a second, checks whether a terminal can be used again: open a
// fresh terminal and see an echoed key. The browser-side heartbeat cannot see
// this failure because the Hub answers it, so this probe is what a user sees.
func deviceOutage(s Stack, owner *client, deviceID string, out *table, sc scenario, trials int, limit time.Duration) error {
	direct := owner.clone(s.Hub, 5*time.Second)
	var lastErr error
	usable := func() bool {
		t, _, err := openTerminal(direct, deviceID, "exp-rec-dev", 3*time.Second)
		if err != nil {
			lastErr = err
			return false
		}
		defer t.close()
		drainFrames(t.frames, 100*time.Millisecond, time.Second)
		_ = t.send([]byte{'q'})
		_, ok := waitByte(t.frames, t.done, 'q', 2*time.Second)
		_ = t.send([]byte{0x15})
		if !ok {
			lastErr = fmt.Errorf("no echo")
			if out, err := exec.Command("tmux", "-L", s.Sock, "list-clients", "-F", "#{client_tty} #{session_name} #{client_width}x#{client_height}").Output(); err == nil {
				lastErr = fmt.Errorf("no echo; tmux clients:\n%s", out)
			}
		}
		return ok
	}
	// Earlier client-side outages froze pooled proxy connections of owner;
	// talk to the Hub directly and fail loudly.
	owner.http.CloseIdleConnections()
	if err := direct.ensureSession(deviceID, "exp-rec-dev"); err != nil {
		return err
	}
	for trial := 0; trial < trials; trial++ {
		if !usable() {
			return fmt.Errorf("device not usable before trial %d: %v", trial, lastErr)
		}
		start := time.Now()
		_ = control(s.HubCtl, "/block?class=device&mode="+sc.mode)
		time.Sleep(sc.outage)
		restored := time.Now()
		if sc.reset {
			_ = control(s.HubCtl, "/unblock?class=device&reset=1")
		} else {
			_ = control(s.HubCtl, "/unblock?class=device")
		}
		var firstDown, back time.Time
		for time.Since(restored) < limit {
			if usable() {
				if !firstDown.IsZero() {
					back = time.Now()
					break
				}
			} else if firstDown.IsZero() {
				firstDown = time.Now()
			}
			time.Sleep(time.Second)
		}
		detect := any("")
		if !firstDown.IsZero() {
			detect = firstDown.Sub(start)
		}
		if back.IsZero() {
			out.row(sc.name, trial, "sinthmux-device", 0, detect, "", "", "")
		} else {
			out.row(sc.name, trial, "sinthmux-device", 1, detect, back.Sub(restored), back.Sub(start), "")
		}
		logf("recovery %s trial %d: recovered=%v", sc.name, trial, !back.IsZero())
		if err := restartConnector(s, owner, deviceID); err != nil {
			return err
		}
	}
	return nil
}
