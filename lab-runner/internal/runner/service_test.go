package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"bitforge/lab-runner/internal/docker"
	"bitforge/lab-runner/internal/store"
)

// ---------------------------------------------------------------- harness

// rig wires a Service to a FakeEngine, a frozen clock and a channel notifier.
// Every test starts here; read it once and the tests read as prose.
type rig struct {
	svc    *Service
	eng    *docker.FakeEngine
	st     *store.Store
	now    time.Time
	events chan Event
}

type chanNotifier struct{ ch chan Event }

func (c chanNotifier) Notify(_ context.Context, _ string, ev Event) { c.ch <- ev }

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		eng:    docker.NewFakeEngine(),
		now:    time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC),
		events: make(chan Event, 16),
	}
	r.st = store.NewStore(func() time.Time { return r.now })
	r.svc = New(r.st, r.eng, chanNotifier{r.events}, Limits{
		MaxInstances: 2,
		MaxTTL:       time.Hour,
		PublicHost:   "labs.local",
		Allowlist:    docker.ParseAllowlist("ghcr.io/bitforge/labs/"),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return r
}

func (r *rig) request(session string) Request {
	return Request{
		Owner: "learn", SessionID: session, UserID: "u-1",
		Image: "ghcr.io/bitforge/labs/hello-flag:1", ExposedPort: 8080,
		TTL: 30 * time.Minute, CallbackURL: "http://learn/internal/" + session,
		Env: map[string]string{"FLAG_1": "FLAG{x}"},
	}
}

func (r *rig) event(t *testing.T) Event {
	t.Helper()
	select {
	case ev := <-r.events:
		return ev
	default:
		t.Fatal("expected a callback event, got none")
		return Event{}
	}
}

func (r *rig) noEvent(t *testing.T) {
	t.Helper()
	select {
	case ev := <-r.events:
		t.Fatalf("unexpected event %+v", ev)
	default:
	}
}

// ---------------------------------------------------------------- Create

func TestCreateHappyPath(t *testing.T) {
	r := newRig(t)
	inst, err := r.svc.Create(context.Background(), r.request("s1"))
	if err != nil {
		t.Fatal(err)
	}
	if inst.Status != store.Running || inst.Endpoint != "labs.local:32768" {
		t.Errorf("got %+v", inst)
	}
	if inst.ID != "fake-1" {
		t.Errorf("store id must be the container id after create, got %s", inst.ID)
	}
	if !inst.ExpiresAt.Equal(r.now.Add(30 * time.Minute)) {
		t.Errorf("ExpiresAt = %v, want now+30m", inst.ExpiresAt)
	}
	// exactly one event: RUNNING (STARTING is the initial state, not a transition)
	ev := r.event(t)
	if ev.Status != store.Running || ev.Endpoint != "labs.local:32768" || ev.SessionID != "s1" {
		t.Errorf("event %+v", ev)
	}
	r.noEvent(t)

	// the engine was asked for the right thing
	if len(r.eng.Created) != 1 {
		t.Fatalf("Created = %d", len(r.eng.Created))
	}
	spec := r.eng.Created[0]
	if spec.Labels[store.LabelOwner] != "learn" || spec.Labels[store.LabelSessionID] != "s1" || spec.Env["FLAG_1"] != "FLAG{x}" {
		t.Errorf("spec %+v", spec)
	}
	if spec.Labels[store.LabelExpiresAt] == "" {
		t.Error("expiry must be stamped into labels so Rebuild can reap after a restart")
	}
}

func TestCreateValidationAndAllowlist(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	bad := r.request("s1")
	bad.Image = "docker.io/library/alpine"
	if _, err := r.svc.Create(ctx, bad); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("want ErrNotAllowed, got %v", err)
	}

	for name, mutate := range map[string]func(*Request){
		"no owner":        func(q *Request) { q.Owner = "" },
		"no session":      func(q *Request) { q.SessionID = "" },
		"no image":        func(q *Request) { q.Image = "" },
		"port 0":          func(q *Request) { q.ExposedPort = 0 },
		"port 70000":      func(q *Request) { q.ExposedPort = 70000 },
		"negative memory": func(q *Request) { q.MemoryMB = -1 },
		"reserved label":  func(q *Request) { q.Labels = map[string]string{store.LabelOwner: "spoof"} },
	} {
		q := r.request("s1")
		mutate(&q)
		if _, err := r.svc.Create(ctx, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if len(r.eng.Created) != 0 {
		t.Error("invalid requests must never reach the engine")
	}
	if r.st.Len() != 0 {
		t.Error("invalid requests must not reserve a slot")
	}
}

func TestCreateCapacityAndOneActivePerSession(t *testing.T) {
	r := newRig(t) // MaxInstances = 2
	ctx := context.Background()

	if _, err := r.svc.Create(ctx, r.request("s1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Create(ctx, r.request("s1")); !errors.Is(err, ErrInvalid) {
		t.Errorf("second instance for the same session: want ErrInvalid, got %v", err)
	}
	if _, err := r.svc.Create(ctx, r.request("s2")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Create(ctx, r.request("s3")); !errors.Is(err, ErrCapacity) {
		t.Errorf("third instance: want ErrCapacity, got %v", err)
	}

	// freeing one slot admits the next
	if err := r.svc.Stop(ctx, "fake-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Create(ctx, r.request("s3")); err != nil {
		t.Errorf("after Stop the slot must be free: %v", err)
	}
	// a terminal instance does not block its session from starting again
	if _, err := r.svc.Create(ctx, r.request("s1")); !errors.Is(err, ErrCapacity) {
		t.Errorf("s1 again: expected only the capacity error now, got %v", err)
	}
}

func TestCreateClampsTTL(t *testing.T) {
	r := newRig(t)
	q := r.request("s1")
	q.TTL = 5 * time.Hour // above MaxTTL (1h)
	inst, _ := r.svc.Create(context.Background(), q)
	if !inst.ExpiresAt.Equal(r.now.Add(time.Hour)) {
		t.Errorf("TTL not clamped: %v", inst.ExpiresAt)
	}
	q = r.request("s2")
	q.TTL = 0 // unspecified → max
	inst, _ = r.svc.Create(context.Background(), q)
	if !inst.ExpiresAt.Equal(r.now.Add(time.Hour)) {
		t.Errorf("zero TTL must default to max: %v", inst.ExpiresAt)
	}
}

func TestCreateFailsAtEngineCreate(t *testing.T) {
	r := newRig(t)
	r.eng.FailCreate = errors.New("pull: not found")
	inst, err := r.svc.Create(context.Background(), r.request("s1"))
	if err != nil {
		t.Fatalf("a Docker failure is reported as a FAILED instance, not an error: %v", err)
	}
	if inst.Status != store.Failed || inst.FailReason != "create: pull: not found" {
		t.Errorf("got %+v", inst)
	}
	ev := r.event(t)
	if ev.Status != store.Failed || ev.FailReason == "" {
		t.Errorf("client must be told: %+v", ev)
	}
	if n := r.svc.active(); n != 0 {
		t.Errorf("FAILED must release the slot, active=%d", n)
	}
}

func TestCreateFailsAtEngineStartCleansUp(t *testing.T) {
	r := newRig(t)
	r.eng.FailStart = errors.New("oom")
	inst, _ := r.svc.Create(context.Background(), r.request("s1"))
	if inst.Status != store.Failed || inst.FailReason != "start: oom" {
		t.Errorf("got %+v", inst)
	}
	if len(r.eng.Removed) != 1 || r.eng.Removed[0] != "fake-1" {
		t.Errorf("a container that failed to start must be removed, Removed=%v", r.eng.Removed)
	}
	if inst.ID != "fake-1" {
		t.Errorf("record must be keyed by container id even on failure, got %s", inst.ID)
	}
}

// ---------------------------------------------------------------- Stop / Expire

func TestStopIsIdempotentAndCancelsStarting(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.svc.Create(ctx, r.request("s1"))
	<-r.events

	if err := r.svc.Stop(ctx, "fake-1"); err != nil {
		t.Fatal(err)
	}
	inst, _ := r.svc.Get("fake-1")
	if inst.Status != store.Stopped || inst.EndedAt.IsZero() {
		t.Errorf("got %+v", inst)
	}
	if r.event(t).Status != store.Stopped {
		t.Error("client must learn about STOPPED")
	}
	if r.eng.Has("fake-1") {
		t.Error("container must be removed")
	}

	// again: nothing happens, no error, no event
	if err := r.svc.Stop(ctx, "fake-1"); err != nil {
		t.Error(err)
	}
	if err := r.svc.Stop(ctx, "never-existed"); err != nil {
		t.Error("unknown id must be nil (idempotent DELETE)")
	}
	r.noEvent(t)

	// a STARTING record (slot reserved, container not yet created) can be cancelled
	r.st.Put(store.Instance{ID: "pending-x", Status: store.Starting, SessionID: "s9", ExpiresAt: r.now.Add(time.Hour)})
	if err := r.svc.Stop(ctx, "pending-x"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.svc.Get("pending-x"); got.Status != store.Stopped {
		t.Errorf("STARTING → STOPPED expected, got %s", got.Status)
	}
}

func TestStopKeepsRecordWhenRemoveFails(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.svc.Create(ctx, r.request("s1"))
	<-r.events
	r.eng.FailRemove = errors.New("daemon busy")

	if err := r.svc.Stop(ctx, "fake-1"); !errors.Is(err, ErrDockerFailed) {
		t.Fatalf("want ErrDockerFailed, got %v", err)
	}
	if inst, _ := r.svc.Get("fake-1"); inst.Status != store.Running {
		t.Errorf("record must stay RUNNING so the reaper retries; got %s", inst.Status)
	}
	r.noEvent(t)
}

// ---------------------------------------------------------------- Reap

func TestReapOnceExpiresOnlyDueInstances(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	short := r.request("s1")
	short.TTL = 10 * time.Minute
	r.svc.Create(ctx, short)
	r.svc.Create(ctx, r.request("s2")) // 30m
	<-r.events
	<-r.events

	if n := r.svc.ReapOnce(ctx); n != 0 {
		t.Errorf("nothing due yet, reaped %d", n)
	}
	r.now = r.now.Add(11 * time.Minute)
	if n := r.svc.ReapOnce(ctx); n != 1 {
		t.Fatalf("one due, reaped %d", n)
	}
	a, _ := r.svc.Get("fake-1")
	b, _ := r.svc.Get("fake-2")
	if a.Status != store.Expired || a.FailReason != "ttl expired" || b.Status != store.Running {
		t.Errorf("a=%s b=%s", a.Status, b.Status)
	}
	if ev := r.event(t); ev.Status != store.Expired || ev.InstanceID != "fake-1" {
		t.Errorf("event %+v", ev)
	}
	// second pass is a no-op
	if n := r.svc.ReapOnce(ctx); n != 0 {
		t.Errorf("already expired must not be reaped again, got %d", n)
	}
}

func TestReapOnceContinuesPastAStuckContainer(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.svc.Create(ctx, r.request("s1"))
	r.svc.Create(ctx, r.request("s2"))
	<-r.events
	<-r.events
	r.now = r.now.Add(time.Hour)

	r.eng.FailRemove = errors.New("busy")
	if n := r.svc.ReapOnce(ctx); n != 0 {
		t.Errorf("remove failing → nothing reaped, got %d", n)
	}
	r.eng.FailRemove = nil
	if n := r.svc.ReapOnce(ctx); n != 2 {
		t.Errorf("next tick must catch up, got %d", n)
	}
}

// ---------------------------------------------------------------- Reconcile

func TestReconcileMarksDeadContainersFailedAndForgetsOldTerminals(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.svc.Create(ctx, r.request("s1"))
	r.svc.Create(ctx, r.request("s2"))
	<-r.events
	<-r.events

	r.eng.Stop("fake-1") // the lab process crashed
	r.svc.Reconcile(ctx, 10*time.Minute)

	a, _ := r.svc.Get("fake-1")
	b, _ := r.svc.Get("fake-2")
	if a.Status != store.Failed || a.FailReason != "container exited" {
		t.Errorf("crashed container must be FAILED, got %+v", a)
	}
	if b.Status != store.Running {
		t.Errorf("healthy container must be untouched, got %s", b.Status)
	}
	if r.event(t).Status != store.Failed {
		t.Error("client must learn about the crash")
	}

	// a container someone removed behind our back
	r.eng.Removed = nil
	_ = r.eng.Remove(ctx, "fake-2")
	r.svc.Reconcile(ctx, 10*time.Minute)
	if b, _ = r.svc.Get("fake-2"); b.Status != store.Failed || b.FailReason != "container vanished" {
		t.Errorf("vanished container must be FAILED, got %+v", b)
	}

	// terminal records are kept for keepTerminal, then forgotten
	r.now = r.now.Add(9 * time.Minute)
	r.svc.Reconcile(ctx, 10*time.Minute)
	if r.st.Len() != 2 {
		t.Errorf("records inside the keep window must stay, len=%d", r.st.Len())
	}
	r.now = r.now.Add(2 * time.Minute)
	r.svc.Reconcile(ctx, 10*time.Minute)
	if r.st.Len() != 0 {
		t.Errorf("old terminal records must be forgotten, len=%d", r.st.Len())
	}
}

// ---------------------------------------------------------------- Rebuild

func TestRebuildAdoptsRunningRemovesDeadAndUnattributable(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	good := store.ToLabels(store.Instance{Owner: "learn", SessionID: "s1", ExpiresAt: r.now.Add(time.Hour)})
	good[docker.LabelManaged] = docker.LabelManagedValue
	dead := store.ToLabels(store.Instance{Owner: "learn", SessionID: "s2", ExpiresAt: r.now.Add(time.Hour)})
	broken := map[string]string{docker.LabelManaged: docker.LabelManagedValue} // no owner/session/expiry

	r.eng.Seed("c-good", good, true)
	r.eng.Seed("c-dead", dead, false)
	r.eng.Seed("c-broken", broken, true)

	adopted, removed, err := r.svc.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if adopted != 1 || removed != 2 {
		t.Errorf("adopted=%d removed=%d", adopted, removed)
	}
	inst, ok := r.svc.Get("c-good")
	if !ok || inst.Status != store.Running || inst.SessionID != "s1" || inst.Owner != "learn" {
		t.Errorf("adopted instance wrong: %+v ok=%v", inst, ok)
	}
	if r.eng.Has("c-dead") || r.eng.Has("c-broken") {
		t.Error("dead and unattributable containers must be removed")
	}
	if _, ok := r.svc.Get("c-dead"); ok {
		t.Error("dead container must not be adopted")
	}

	// the adopted instance is reaped on schedule like any other
	r.now = r.now.Add(2 * time.Hour)
	if n := r.svc.ReapOnce(ctx); n != 1 {
		t.Errorf("adopted instance must expire, reaped %d", n)
	}
}

// ---------------------------------------------------------------- Reaper

func TestReaperTickDrivesBothVerbs(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.svc.Create(ctx, r.request("s1")) // 30m
	long := r.request("s2")
	long.TTL = time.Hour
	r.svc.Create(ctx, long)
	<-r.events
	<-r.events
	reaper := Reaper{Service: r.svc, KeepTerminal: time.Minute}

	r.eng.Stop("fake-2")                // crashed → Reconcile
	r.now = r.now.Add(31 * time.Minute) // s1 expired → ReapOnce; s2 still has 29m
	reaper.Tick(ctx)

	a, _ := r.svc.Get("fake-1")
	b, _ := r.svc.Get("fake-2")
	if a.Status != store.Expired || b.Status != store.Failed {
		t.Errorf("a=%s b=%s", a.Status, b.Status)
	}

	// Run exits when the context is cancelled
	ctx2, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { reaper.Run(ctx2); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
