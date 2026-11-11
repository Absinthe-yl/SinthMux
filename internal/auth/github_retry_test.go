package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoWithRetryRecoversFromDroppedConnection(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			panic(http.ErrAbortHandler)
		}
		io.WriteString(w, "ok")
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := doWithRetry(ctx, server.Client(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	})
	if err != nil {
		t.Fatalf("retry did not recover from a dropped connection: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ok" || atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("body=%q hits=%d, want ok after 2 attempts", body, hits)
	}
}

func TestDoWithRetryGivesUpAfterThreeAttempts(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := doWithRetry(ctx, server.Client(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	}); err == nil {
		t.Fatal("expected an error after every attempt failed")
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("hits=%d, want 3 attempts", got)
	}
}

func TestDoWithRetryStopsWhenContextEnds(t *testing.T) {
	var hits int32
	firstHit := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case firstHit <- struct{}{}:
		default:
		}
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := doWithRetry(ctx, server.Client(), func(ctx context.Context) (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		})
		done <- err
	}()
	<-firstHit
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits=%d, want the cancelled context to stop retries", got)
	}
}
