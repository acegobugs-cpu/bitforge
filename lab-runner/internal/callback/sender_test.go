package callback

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"bitforge/lab-runner/internal/runner"
	"bitforge/lab-runner/internal/store"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fast(secret string) *Sender {
	s := New(secret, quiet())
	s.Backoff = time.Millisecond
	return s
}

func TestNotifyDeliversJSONWithSecret(t *testing.T) {
	var got runner.Event
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get(HeaderInternalAuth)
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	ev := runner.Event{InstanceID: "c1", SessionID: "s1", Status: store.Running, Endpoint: "labs.local:1"}
	fast("shh").Notify(context.Background(), srv.URL, ev)

	if header != "shh" {
		t.Errorf("X-Internal-Auth = %q", header)
	}
	if got.InstanceID != "c1" || got.Status != store.Running || got.Endpoint != "labs.local:1" {
		t.Errorf("body = %+v", got)
	}
}

func TestNotifyRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fast("x").Notify(context.Background(), srv.URL, runner.Event{InstanceID: "c1"})
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (two 502s then 200)", calls.Load())
	}
}

func TestNotifyGivesUpAfterRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := fast("x")
	s.Retries = 2
	s.Notify(context.Background(), srv.URL, runner.Event{InstanceID: "c1"}) // must return, not loop
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 1 + 2 retries", calls.Load())
	}
}

func TestNotifyDoesNotRetry4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	fast("x").Notify(context.Background(), srv.URL, runner.Event{InstanceID: "c1"})
	if calls.Load() != 1 {
		t.Errorf("a 4xx is the client's final answer; calls = %d", calls.Load())
	}
}

func TestNotifyStopsWhenContextCancelled(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := fast("x")
	s.Backoff = time.Hour // would wait forever between retries…
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Notify(ctx, srv.URL, runner.Event{InstanceID: "c1"}); close(done) }()
	time.Sleep(20 * time.Millisecond) // let the first attempt fail
	cancel()                          // …unless the context ends
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify did not return after cancel")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d", calls.Load())
	}
}

func TestNotifyUnreachableHostReturns(t *testing.T) {
	s := fast("x")
	s.Retries = 1
	s.Client.Timeout = 200 * time.Millisecond
	start := time.Now()
	s.Notify(context.Background(), "http://127.0.0.1:1/nope", runner.Event{InstanceID: "c1"})
	if time.Since(start) > 2*time.Second {
		t.Error("connection-refused must fail fast")
	}
}
