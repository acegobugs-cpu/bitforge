// Package callback is the real runner.Notifier: it POSTs every status change
// to the client's callbackUrl with X-Internal-Auth, retrying a few times.
//
// Reading guide: one type, one public method, one loop. Delivery is
// best-effort by design (plan §3) — if the client is down the event is logged
// and dropped, because the client reconciles by polling GET /instances/{id}.
// That is why nothing here is persisted and why Notify never returns an error.
package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"bitforge/lab-runner/internal/runner"
)

// HeaderInternalAuth is the shared-secret header every internal call carries,
// the same one the gateway/identity/learn services already use.
const HeaderInternalAuth = "X-Internal-Auth"

type Sender struct {
	Client  *http.Client  // default: 5s timeout
	Secret  string        // INTERNAL_GATEWAY_SECRET; sent as-is
	Retries int           // attempts after the first (default 3)
	Backoff time.Duration // between attempts (default 500ms; doubles each time)
	Log     *slog.Logger
}

func New(secret string, log *slog.Logger) *Sender {
	if log == nil {
		log = slog.Default()
	}
	return &Sender{
		Client:  &http.Client{Timeout: 5 * time.Second},
		Secret:  secret,
		Retries: 3,
		Backoff: 500 * time.Millisecond,
		Log:     log,
	}
}

// Notify sends ev to url. It blocks for at most (Retries+1) attempts; callers
// that must not wait (the HTTP handler) run it in a goroutine. ctx bounds the
// whole sequence — pass context.WithoutCancel(reqCtx) from a handler so the
// learner closing the browser does not cancel the delivery.
func (s *Sender) Notify(ctx context.Context, url string, ev runner.Event) {
	body, err := json.Marshal(ev)
	if err != nil {
		s.Log.Error("callback marshal", "err", err)
		return
	}
	wait := s.Backoff
	for attempt := 0; attempt <= s.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				s.Log.Warn("callback abandoned", "url", url, "instance", ev.InstanceID, "err", ctx.Err())
				return
			case <-time.After(wait):
				wait *= 2
			}
		}
		if err := s.post(ctx, url, body); err == nil {
			return
		} else {
			s.Log.Warn("callback failed", "url", url, "instance", ev.InstanceID, "status", ev.Status, "attempt", attempt+1, "err", err)
		}
	}
	s.Log.Error("callback gave up", "url", url, "instance", ev.InstanceID, "status", ev.Status)
}

func (s *Sender) post(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderInternalAuth, s.Secret)
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 2xx = delivered. 4xx = the client rejected it; retrying will not help
	// (wrong secret, unknown session) so treat as delivered-but-logged. 5xx
	// and transport errors retry.
	if resp.StatusCode >= 500 {
		return &statusError{resp.StatusCode}
	}
	if resp.StatusCode >= 400 {
		s.Log.Warn("callback rejected by client", "url", url, "status", resp.StatusCode)
	}
	return nil
}

type statusError struct{ code int }

func (e *statusError) Error() string { return http.StatusText(e.code) }

var _ runner.Notifier = (*Sender)(nil)
