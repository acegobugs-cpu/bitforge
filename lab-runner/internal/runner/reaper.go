package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// randomID is the provisional instance id used between "slot reserved" and
// "container created"; afterwards the record is rekeyed to the container id.
func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "pending-" + hex.EncodeToString(b[:])
}

// Reaper runs ReapOnce and Reconcile on a fixed cadence until ctx is done.
// It is the only goroutine the runner owns besides the HTTP server.
//
// Reading guide: a ticker, a select, two calls. Everything interesting lives
// in Service; the reaper only decides *when*.
type Reaper struct {
	Service      *Service
	Interval     time.Duration // how often to look (default 5s)
	KeepTerminal time.Duration // how long STOPPED/EXPIRED/FAILED stay queryable (default 10m)
}

// Run blocks until ctx is cancelled. Call it with `go reaper.Run(ctx)`.
func (r Reaper) Run(ctx context.Context) {
	if r.Interval <= 0 {
		r.Interval = 5 * time.Second
	}
	if r.KeepTerminal <= 0 {
		r.KeepTerminal = 10 * time.Minute
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick(ctx)
		}
	}
}

// Tick is one iteration, exposed so tests drive it without a real clock.
func (r Reaper) Tick(ctx context.Context) {
	if n := r.Service.ReapOnce(ctx); n > 0 {
		r.Service.log.Info("reaped expired instances", "count", n)
	}
	r.Service.Reconcile(ctx, r.KeepTerminal)
}
