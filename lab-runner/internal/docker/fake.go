package docker

import (
	"context"
	"fmt"
	"sync"
)

// FakeEngine is the in-memory Engine every package above this one tests
// against: the reaper, the HTTP layer and the start-up rebuild never need a
// daemon. Knobs make each call fail on demand; recorders let tests assert what
// was asked of the engine.
type FakeEngine struct {
	mu sync.Mutex

	// knobs
	FailCreate  error
	FailStart   error
	FailInspect error
	FailRemove  error
	FailList    error
	FailPing    error
	StartPort   int // host port returned by Start (default 32768)

	// recorders
	Created []CreateSpec
	Removed []string

	containers map[string]*fakeContainer
	seq        int
}

type fakeContainer struct {
	spec    CreateSpec
	running bool
	labels  map[string]string
}

func NewFakeEngine() *FakeEngine {
	return &FakeEngine{containers: map[string]*fakeContainer{}, StartPort: 32768}
}

func (f *FakeEngine) Create(_ context.Context, spec CreateSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailCreate != nil {
		return "", f.FailCreate
	}
	f.seq++
	id := fmt.Sprintf("fake-%d", f.seq)
	f.Created = append(f.Created, spec)
	f.containers[id] = &fakeContainer{spec: spec, labels: containerConfig(spec).Labels}
	return id, nil
}

func (f *FakeEngine) Start(_ context.Context, id string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailStart != nil {
		return 0, f.FailStart
	}
	c, ok := f.containers[id]
	if !ok {
		return 0, ErrNotFound
	}
	c.running = true
	return f.StartPort, nil
}

func (f *FakeEngine) Inspect(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailInspect != nil {
		return false, f.FailInspect
	}
	c, ok := f.containers[id]
	if !ok {
		return false, ErrNotFound // same sentinel as the real client
	}
	return c.running, nil
}

// Remove is idempotent like the real one: removing an unknown id is nil.
func (f *FakeEngine) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailRemove != nil {
		return f.FailRemove
	}
	f.Removed = append(f.Removed, id)
	delete(f.containers, id)
	return nil
}

func (f *FakeEngine) ListManaged(_ context.Context) ([]ManagedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailList != nil {
		return nil, f.FailList
	}
	out := make([]ManagedContainer, 0, len(f.containers))
	for id, c := range f.containers {
		out = append(out, ManagedContainer{ID: id, Labels: c.labels, Running: c.running})
	}
	return out, nil
}

func (f *FakeEngine) Ping(context.Context) error { return f.FailPing }

// --- test helpers ---

// Seed pre-populates a container as if it survived a runner restart.
func (f *FakeEngine) Seed(id string, labels map[string]string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[id] = &fakeContainer{labels: labels, running: running}
}

// Stop simulates the process inside the container exiting on its own.
func (f *FakeEngine) Stop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[id]; ok {
		c.running = false
	}
}

func (f *FakeEngine) Has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.containers[id]
	return ok
}

var _ Engine = (*FakeEngine)(nil)
