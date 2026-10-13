package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// runLoad measures Hub CPU and memory as connections grow. Devices are virtual
// connectors (simdev.go) because macOS has 511 ptys in total; the Hub cannot
// tell them from real ones. Each level runs an idle window (heartbeats, ping,
// the 2 s permission check per terminal) and an active window where every
// terminal types one key per second and measures the echo through the Hub.
func runLoad(s Stack, flags *flag.FlagSet, args []string) error {
	levels := flags.String("levels", "10x0,100x0,500x0,1000x0,100x1,100x5,250x4,500x4", "devices x terminals per device")
	idle := flags.Duration("idle", 30*time.Second, "idle measurement window")
	active := flags.Duration("active", 30*time.Second, "active measurement window")
	streamRate := flags.Int("stream", 0, "extra output per terminal in bytes/s during an extra 'stream' window (0 = skip)")
	_ = flags.Parse(args)

	owner, err := login(s.Hub, s.Proxy, s.Owner)
	if err != nil {
		return err
	}
	space := owner.spaces[0].ID
	out, err := newTable(s, "load.csv", "devices", "terminals", "phase", "hub_cpu_pct", "hub_rss_mib", "pg_cpu_pct", "db_xact_per_s", "echo_n", "echo_fail", "echo_p50_ms", "echo_p95_ms", "echo_p99_ms", "open_p50_ms", "open_p95_ms")
	if err != nil {
		return err
	}
	defer out.close()

	var pool []*simDevice
	for _, level := range strings.Split(*levels, ",") {
		parts := strings.Split(strings.TrimSpace(level), "x")
		devicesN, _ := strconv.Atoi(parts[0])
		perDevice, _ := strconv.Atoi(parts[1])
		if len(pool) < devicesN {
			more, err := createSimDevices(owner, space, fmt.Sprintf("exp-load-%d", len(pool)), devicesN-len(pool), 8)
			if err != nil {
				return err
			}
			pool = append(pool, more...)
		}
		devices := pool[:devicesN]
		if err := connectAll(devices, s.Hub, 32); err != nil {
			return err
		}
		ids := make([]string, len(devices))
		for i, d := range devices {
			ids[i] = d.ID
		}
		if err := waitOnline(owner, ids, 60*time.Second); err != nil {
			return err
		}
		logf("load %s: %d devices online", level, devicesN)

		// Open terminals, 16 at a time, recording first-screen time under load.
		var terminals []*terminal
		var tmu sync.Mutex
		var opens []float64
		slots := make(chan struct{}, 16)
		var wg sync.WaitGroup
		var openFailed atomic.Int64
		for _, d := range devices {
			for j := 0; j < perDevice; j++ {
				wg.Add(1)
				slots <- struct{}{}
				go func(d *simDevice) {
					defer wg.Done()
					defer func() { <-slots }()
					t, timing, err := openTerminal(owner, d.ID, "s", 10*time.Second)
					if err != nil {
						openFailed.Add(1)
						return
					}
					tmu.Lock()
					terminals = append(terminals, t)
					opens = append(opens, ms(timing.first))
					tmu.Unlock()
				}(d)
			}
		}
		wg.Wait()
		if openFailed.Load() > 0 {
			logf("load %s: %d terminals failed to open", level, openFailed.Load())
		}
		openP50, openP95 := percentile(opens, 50), percentile(opens, 95)
		dsn := os.Getenv("SINTHMUX_TEST_DATABASE_URL")
		hub := []int{s.HubPID}

		// Idle window.
		time.Sleep(3 * time.Second)
		hs, ps := startSampler(hub), startPGSampler(dsn)
		time.Sleep(*idle)
		hubCPU, hubRSS := hs.finish()
		pgCPU, xps := ps.finish()
		out.row(devicesN, len(terminals), "idle", hubCPU, hubRSS, pgCPU, xps, 0, 0, "", "", "", openP50, openP95)
		logf("load %s idle: hub %.1f%% %.0f MiB, pg %.1f%% %.0f xact/s", level, hubCPU, hubRSS, pgCPU, xps)

		if len(terminals) > 0 {
			window := func(phase string) {
				var lat []float64
				var lmu sync.Mutex
				var failed atomic.Int64
				ctx, cancel := context.WithTimeout(context.Background(), *active)
				hs, ps := startSampler(hub), startPGSampler(dsn)
				var wg sync.WaitGroup
				for _, t := range terminals {
					wg.Add(1)
					go func(t *terminal) {
						defer wg.Done()
						// Spread the first key over one second.
						time.Sleep(time.Duration(rand.Int63n(int64(time.Second))))
						for i := 0; ctx.Err() == nil; i++ {
							key := keys[i%len(keys)]
							start := time.Now()
							if t.send([]byte{key}) != nil {
								failed.Add(1)
								return
							}
							at, ok := waitByte(t.frames, t.done, key, 3*time.Second)
							if ok {
								lmu.Lock()
								lat = append(lat, ms(at.Sub(start)))
								lmu.Unlock()
							} else {
								failed.Add(1)
							}
							select {
							case <-ctx.Done():
							case <-time.After(time.Second - time.Since(start)):
							}
						}
					}(t)
				}
				wg.Wait()
				cancel()
				hubCPU, hubRSS := hs.finish()
				pgCPU, xps := ps.finish()
				out.row(devicesN, len(terminals), phase, hubCPU, hubRSS, pgCPU, xps, len(lat), failed.Load(), percentile(lat, 50), percentile(lat, 95), percentile(lat, 99), openP50, openP95)
				logf("load %s %s: hub %.1f%% %.0f MiB, echo p95 %.1f ms (%d, %d failed)", level, phase, hubCPU, hubRSS, percentile(lat, 95), len(lat), failed.Load())
			}
			window("typing")
			if *streamRate > 0 {
				for _, d := range devices {
					d.streamRate = *streamRate
				}
				// streamRate applies to streams opened from now on: reopen.
				for _, t := range terminals {
					t.close()
				}
				terminals = terminals[:0]
				for _, d := range devices {
					for j := 0; j < perDevice; j++ {
						if t, _, err := openTerminal(owner, d.ID, "s", 10*time.Second); err == nil {
							terminals = append(terminals, t)
						}
					}
				}
				// The reader goroutine drops frames when the buffer is full, so
				// the echo can still be found among the stream.
				window(fmt.Sprintf("typing+stream%dKiB", *streamRate>>10))
				for _, d := range devices {
					d.streamRate = 0
				}
			}
		}
		for _, t := range terminals {
			t.close()
		}
		time.Sleep(2 * time.Second)
	}
	for _, d := range pool {
		d.close()
	}
	return nil
}
