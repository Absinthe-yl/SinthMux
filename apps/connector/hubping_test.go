package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/internal/config"
)

// A Hub that accepts the connection and then never answers (frozen path) must
// make run return, so the connector reconnects instead of waiting for TCP.
func TestRunReturnsWhenHubStopsAnswering(t *testing.T) {
	oldInterval, oldTimeout := hubPingInterval, hubPongTimeout
	hubPingInterval, hubPongTimeout = 100*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { hubPingInterval, hubPongTimeout = oldInterval, oldTimeout })

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		// Read the hello, then stop reading: pings are never answered.
		_, _, _ = conn.Read(r.Context())
		<-release
	}))
	t.Cleanup(func() { close(release); server.Close() })

	settings := config.Connector{HubURL: "ws" + strings.TrimPrefix(server.URL, "http"), DevToken: "test", DeviceID: "d", Name: "d"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- run(context.Background(), &settings, false, logger, nil) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Hub 无响应") {
			t.Fatalf("run returned %v, want Hub 无响应", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("took %v", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not notice the unresponsive Hub")
	}
}
