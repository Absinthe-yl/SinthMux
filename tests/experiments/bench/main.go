// Command bench runs the SinthMux experiments against the isolated stack that
// tests/experiments/run.sh starts. Each subcommand writes CSV files to -out.
//
//	bench access    terminal access success rate and first-screen time (SinthMux vs SSH)
//	bench latency   keystroke-to-echo latency (SinthMux vs SSH)
//	bench recovery  recovery after network outages for five reconnect strategies
//	bench authz     unauthorized request interception
//	bench revoke    device revocation and role downgrade effect time
//	bench load      Hub CPU / memory under concurrent connections
//
// Common flags come from the environment written by run.sh (see env()).
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Stack describes the running experiment environment.
type Stack struct {
	Hub     string // direct Hub base URL
	Proxy   string // Hub through netproxy (public origin)
	HubCtl  string // netproxy control in front of the Hub
	SSH     string // host:port of the SSH baseline through its netproxy
	SSHCtl  string
	SSHKey  string
	Sock    string // tmux socket of the real device
	Device  string // name of the real paired device
	Owner   string // owner login token
	Out     string
	HubPID  int
	ConnPID int
	ConnBin string
	WorkDir string
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func loadStack() Stack {
	pid := func(name string) int { n, _ := strconv.Atoi(os.Getenv(name)); return n }
	return Stack{
		Hub: env("EXP_HUB", "http://127.0.0.1:18093"), Proxy: env("EXP_PROXY", "http://127.0.0.1:18094"),
		HubCtl: env("EXP_HUB_CTL", "http://127.0.0.1:18095"), SSH: env("EXP_SSH", "127.0.0.1:18098"),
		SSHCtl: env("EXP_SSH_CTL", "http://127.0.0.1:18097"), SSHKey: os.Getenv("EXP_SSH_KEY"),
		Sock: env("EXP_SOCK", "smexp"), Device: env("EXP_DEVICE", "Exp Mac"), Owner: os.Getenv("EXP_OWNER_TOKEN"),
		Out: env("EXP_OUT", "tests/experiments/results"), HubPID: pid("EXP_HUB_PID"), ConnPID: pid("EXP_CONN_PID"),
		ConnBin: os.Getenv("EXP_CONN_BIN"), WorkDir: os.Getenv("EXP_DIR"),
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: bench access|latency|recovery|authz|revoke|load [flags]")
		os.Exit(2)
	}
	stack := loadStack()
	_ = os.MkdirAll(stack.Out, 0o755)
	flags := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	var err error
	switch os.Args[1] {
	case "access":
		err = runAccess(stack, flags, os.Args[2:])
	case "latency":
		err = runLatency(stack, flags, os.Args[2:])
	case "recovery":
		err = runRecovery(stack, flags, os.Args[2:])
	case "authz":
		err = runAuthz(stack, flags, os.Args[2:])
	case "revoke":
		err = runRevoke(stack, flags, os.Args[2:])
	case "load":
		err = runLoad(stack, flags, os.Args[2:])
	default:
		err = fmt.Errorf("unknown experiment %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

// ---------- Hub client ----------

type client struct {
	base   string // where requests go (direct Hub or proxy)
	origin string // Origin header; always the public origin
	http   *http.Client
	csrf   string
	userID string
	spaces []struct{ ID, Name, Role string }
}

func newClient(base, origin string) *client {
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 64}
	return &client{base: strings.TrimRight(base, "/"), origin: origin, http: &http.Client{Jar: jar, Transport: transport, Timeout: 20 * time.Second}}
}

// clone shares the login cookie but routes requests to another base with its own connections.
func (c *client) clone(base string, timeout time.Duration) *client {
	copy := *c
	copy.base = strings.TrimRight(base, "/")
	copy.http = &http.Client{Jar: c.http.Jar, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4}, Timeout: timeout}
	return &copy
}

func (c *client) request(ctx context.Context, method, path, contentType string, body io.Reader) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Origin", c.origin)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if c.csrf != "" {
		request.Header.Set("X-Sinthmux-CSRF", c.csrf)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, data, nil
}

func (c *client) call(method, path string, in, out any) (int, error) {
	return c.callCtx(context.Background(), method, path, in, out)
}

func (c *client) callCtx(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	contentType := ""
	if in != nil {
		data, _ := json.Marshal(in)
		body, contentType = bytes.NewReader(data), "application/json"
	}
	status, data, err := c.request(ctx, method, path, contentType, body)
	if err == nil && out != nil && len(data) > 0 {
		_ = json.Unmarshal(data, out)
	}
	return status, err
}

// login exchanges a login token for a session; it waits out the per-address
// login limit (10 per minute) instead of failing.
func login(base, origin, token string) (*client, error) {
	c := newClient(base, origin)
	for attempt := 0; ; attempt++ {
		status, err := c.call(http.MethodPost, "/api/v1/auth/token", map[string]string{"token": token}, nil)
		if err != nil {
			return nil, err
		}
		if status == http.StatusTooManyRequests && attempt < 8 {
			time.Sleep(10 * time.Second)
			continue
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("login: HTTP %d", status)
		}
		break
	}
	var me struct {
		CSRF   string
		User   struct{ ID string }
		Spaces []struct{ ID, Name, Role string }
	}
	if _, err := c.call(http.MethodGet, "/api/v1/auth/me", nil, &me); err != nil {
		return nil, err
	}
	c.csrf, c.userID, c.spaces = me.CSRF, me.User.ID, me.Spaces
	return c, nil
}

type deviceInfo struct {
	ID, Name, Status, SpaceID string
	ConnectedAt               time.Time
}

func (c *client) devices() ([]deviceInfo, error) {
	var list struct{ Devices []deviceInfo }
	status, err := c.call(http.MethodGet, "/api/v1/devices", nil, &list)
	if err == nil && status != 200 {
		err = fmt.Errorf("devices: HTTP %d", status)
	}
	return list.Devices, err
}

func (c *client) deviceByName(name string, wait time.Duration) (deviceInfo, error) {
	deadline := time.Now().Add(wait)
	for {
		list, err := c.devices()
		if err == nil {
			for _, d := range list {
				if d.Name == name && d.Status == "online" {
					return d, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return deviceInfo{}, fmt.Errorf("device %q not online", name)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (c *client) ensureSession(deviceID, name string) error {
	status, err := c.call(http.MethodPost, "/api/v1/devices/"+url.PathEscape(deviceID)+"/sessions", map[string]string{"name": name}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusConflict {
		return fmt.Errorf("create session %s: HTTP %d", name, status)
	}
	return nil
}

func (c *client) ticket(ctx context.Context, deviceID, session string) (string, int, error) {
	var out struct{ Ticket string }
	status, err := c.callCtx(ctx, http.MethodPost, "/api/v1/devices/"+url.PathEscape(deviceID)+"/sessions/"+url.PathEscape(session)+"/ticket", nil, &out)
	if err == nil && (status != http.StatusOK || out.Ticket == "") {
		err = fmt.Errorf("ticket: HTTP %d", status)
	}
	return out.Ticket, status, err
}

// ---------- terminal WebSocket ----------

type frame struct {
	at   time.Time
	data []byte
	text bool
}

type terminal struct {
	conn      *websocket.Conn
	frames    chan frame
	done      chan struct{}
	err       error
	closeCode websocket.StatusCode
	closedAt  time.Time
}

func dialTerminal(ctx context.Context, c *client, ticket string) (*terminal, *http.Response, error) {
	endpoint := strings.Replace(c.base, "http", "ws", 1) + "/ws/v1/terminal"
	conn, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: http.Header{"Origin": {c.origin}}, Subprotocols: []string{"sinthmux.v1", "sinthmux.ticket." + ticket}})
	if err != nil {
		return nil, response, err
	}
	conn.SetReadLimit(1 << 20)
	t := &terminal{conn: conn, frames: make(chan frame, 8192), done: make(chan struct{})}
	go func() {
		defer close(t.done)
		for {
			kind, data, err := conn.Read(context.Background())
			if err != nil {
				t.err, t.closedAt = err, time.Now()
				t.closeCode = websocket.CloseStatus(err)
				return
			}
			select {
			case t.frames <- frame{at: time.Now(), data: data, text: kind == websocket.MessageText}:
			default: // consumer gone; keep reading so the socket drains
			}
		}
	}()
	return t, response, nil
}

func (t *terminal) send(data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return t.conn.Write(ctx, websocket.MessageBinary, data)
}

func (t *terminal) control(value any) error {
	data, _ := json.Marshal(value)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return t.conn.Write(ctx, websocket.MessageText, data)
}

func (t *terminal) close() { _ = t.conn.CloseNow() }

// firstOutput waits for the first binary frame.
func (t *terminal) firstOutput(timeout time.Duration) (time.Time, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case f := <-t.frames:
			if !f.text {
				return f.at, true
			}
		case <-t.done:
			return time.Time{}, false
		case <-deadline:
			return time.Time{}, false
		}
	}
}

// drain discards output until the terminal is quiet for the given time.
func drainFrames(frames <-chan frame, quiet, max time.Duration) {
	limit := time.After(max)
	for {
		select {
		case <-frames:
		case <-time.After(quiet):
			return
		case <-limit:
			return
		}
	}
}

// waitByte waits for a binary frame containing b and returns its arrival time.
func waitByte(frames <-chan frame, done <-chan struct{}, b byte, timeout time.Duration) (time.Time, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case f := <-frames:
			if !f.text && bytes.IndexByte(f.data, b) >= 0 {
				return f.at, true
			}
		case <-done:
			return time.Time{}, false
		case <-deadline:
			return time.Time{}, false
		}
	}
}

type openTiming struct {
	ticket, handshake, first time.Duration
}

// openTerminal does what the web page does: ticket, WebSocket, resize, first output.
func openTerminal(c *client, deviceID, session string, timeout time.Duration) (*terminal, openTiming, error) {
	var timing openTiming
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	ticket, _, err := c.ticket(ctx, deviceID, session)
	cancel()
	if err != nil {
		return nil, timing, err
	}
	timing.ticket = time.Since(start)
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	t, _, err := dialTerminal(ctx, c, ticket)
	cancel()
	if err != nil {
		return nil, timing, err
	}
	timing.handshake = time.Since(start) - timing.ticket
	_ = t.control(map[string]any{"type": "resize", "cols": 100, "rows": 30})
	at, ok := t.firstOutput(timeout)
	if !ok {
		t.close()
		return nil, timing, errors.New("no first output")
	}
	timing.first = at.Sub(start)
	return t, timing, nil
}

// ---------- network emulator control ----------

func control(base, path string) error {
	response, err := (&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}).Get(base + path)
	if err != nil {
		return err
	}
	response.Body.Close()
	return nil
}

// Profile is a network condition. One-way delays per proxy crossing: browser ↔
// Hub (client), device ↔ Hub (device), and browser ↔ device for SSH, set to
// client+device so both paths have the same total propagation delay.
type Profile struct {
	Name           string
	Client, Device float64
}

var profiles = map[string]Profile{
	"lan": {"lan", 0, 0},
	"wan": {"wan", 20, 10}, // RTT 40 ms phone↔cloud, 20 ms home↔cloud
}

func applyProfile(s Stack, p Profile) error {
	if err := control(s.HubCtl, fmt.Sprintf("/delay?client=%g&device=%g", p.Client, p.Device)); err != nil {
		return err
	}
	return control(s.SSHCtl, fmt.Sprintf("/delay?client=%g", p.Client+p.Device))
}

func selectProfiles(names string) []Profile {
	var result []Profile
	for _, name := range strings.Split(names, ",") {
		if p, ok := profiles[strings.TrimSpace(name)]; ok {
			result = append(result, p)
		}
	}
	return result
}

// ---------- statistics and output ----------

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := p / 100 * float64(len(sorted)-1)
	low := int(math.Floor(rank))
	high := int(math.Ceil(rank))
	return sorted[low] + (sorted[high]-sorted[low])*(rank-float64(low))
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

type table struct {
	mu   sync.Mutex
	file *os.File
	w    *csv.Writer
}

func newTable(s Stack, name string, header ...string) (*table, error) {
	file, err := os.Create(filepath.Join(s.Out, name))
	if err != nil {
		return nil, err
	}
	t := &table{file: file, w: csv.NewWriter(file)}
	t.row(toAny(header)...)
	return t, nil
}

func toAny(values []string) []any {
	result := make([]any, len(values))
	for i, v := range values {
		result[i] = v
	}
	return result
}

func (t *table) row(values ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	record := make([]string, len(values))
	for i, v := range values {
		switch x := v.(type) {
		case float64:
			if math.IsNaN(x) {
				record[i] = ""
			} else {
				record[i] = strconv.FormatFloat(x, 'f', 2, 64)
			}
		case time.Duration:
			record[i] = strconv.FormatFloat(ms(x), 'f', 2, 64)
		default:
			record[i] = fmt.Sprint(v)
		}
	}
	_ = t.w.Write(record)
	t.w.Flush()
}

func (t *table) close() { t.w.Flush(); t.file.Close() }

func logf(format string, args ...any) {
	fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// ---------- process sampling ----------

// cpuTime returns the summed CPU time and RSS (KiB) of the given processes.
func cpuTime(pids []int) (time.Duration, int64) {
	var total time.Duration
	var rss int64
	for _, pid := range pids {
		if cpu, kib, ok := procUsage(pid); ok {
			total += cpu
			rss += kib
		}
	}
	return total, rss
}

// parseCPU parses ps time: [[hh:]mm:]ss.ss
func parseCPU(value string) time.Duration {
	parts := strings.Split(value, ":")
	seconds := 0.0
	for _, part := range parts {
		n, _ := strconv.ParseFloat(part, 64)
		seconds = seconds*60 + n
	}
	return time.Duration(seconds * float64(time.Second))
}

func pgrep(pattern string) []int {
	output, _ := exec.Command("pgrep", "-f", pattern).Output()
	var pids []int
	for _, field := range strings.Fields(string(output)) {
		if n, err := strconv.Atoi(field); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// sampler measures CPU% (of one core) and peak RSS over a window.
type sampler struct {
	pids     []int
	start    time.Time
	cpuStart time.Duration
	peakRSS  int64
	stop     chan struct{}
	wg       sync.WaitGroup
}

func startSampler(pids []int) *sampler {
	s := &sampler{pids: pids, start: time.Now(), stop: make(chan struct{})}
	s.cpuStart, s.peakRSS = cpuTime(pids)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				if _, rss := cpuTime(pids); rss > s.peakRSS {
					s.peakRSS = rss
				}
			}
		}
	}()
	return s
}

func (s *sampler) finish() (cpuPercent float64, rssMiB float64) {
	close(s.stop)
	s.wg.Wait()
	cpu, rss := cpuTime(s.pids)
	if rss > s.peakRSS {
		s.peakRSS = rss
	}
	elapsed := time.Since(s.start)
	return 100 * float64(cpu-s.cpuStart) / float64(elapsed), float64(s.peakRSS) / 1024
}

// pgSampler tracks every postgres process seen during a window, rescanning
// often because the Hub's pool closes idle connections (new backend PIDs), and
// reads the transaction counter of the test database.
type pgSampler struct {
	start    time.Time
	first    map[int]time.Duration
	last     map[int]time.Duration
	xactFrom int64
	dsn      string
	stop     chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
}

func pgXacts(dsn string) int64 {
	output, err := exec.Command("psql", dsn, "-Atc", "select xact_commit+xact_rollback from pg_stat_database where datname=current_database()").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	return n
}

func startPGSampler(dsn string) *pgSampler {
	s := &pgSampler{start: time.Now(), first: map[int]time.Duration{}, last: map[int]time.Duration{}, dsn: dsn, stop: make(chan struct{})}
	s.xactFrom = pgXacts(dsn)
	scan := func() {
		for _, pid := range pgrep("postgres") {
			cpu, _, ok := procUsage(pid)
			if !ok {
				continue
			}
			s.mu.Lock()
			if _, seen := s.first[pid]; !seen {
				// A backend born inside the window started from zero CPU.
				if time.Since(s.start) > 300*time.Millisecond {
					s.first[pid] = 0
				} else {
					s.first[pid] = cpu
				}
			}
			s.last[pid] = cpu
			s.mu.Unlock()
		}
	}
	scan()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				scan()
			}
		}
	}()
	return s
}

// finish returns CPU % of one core and database transactions per second.
func (s *pgSampler) finish() (float64, float64) {
	close(s.stop)
	s.wg.Wait()
	var total time.Duration
	for pid, last := range s.last {
		total += last - s.first[pid]
	}
	elapsed := time.Since(s.start)
	// pg_stat_database counters are flushed with a delay of up to ~1 s.
	time.Sleep(1200 * time.Millisecond)
	xacts := pgXacts(s.dsn) - s.xactFrom
	return 100 * float64(total) / float64(elapsed), float64(xacts) / elapsed.Seconds()
}
