// Package runner is the lab-runner's brain: it owns the lifecycle of one lab
// instance from "a client asked for it" to "the container is gone", using
// store.Store for state and docker.Engine for side effects. It has no HTTP in
// it; the api package translates requests into calls on Service, and the
// reaper calls Service on a timer.
//
// Reading guide — the whole package is one type and five verbs:
//
//	Service.Create    client → STARTING → (pull, start) → RUNNING | FAILED
//	Service.Stop      DELETE → STOPPED           (idempotent)
//	Service.Expire    reaper → EXPIRED           (same mechanics as Stop)
//	Service.Reconcile notice containers that died on their own → FAILED
//	Service.Rebuild   start-up: adopt containers that survived a restart
//
// Every state change goes through one private helper, finish(), so the
// "transition in store → tell the client" sequence is written exactly once.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"bitforge/lab-runner/internal/docker"
	"bitforge/lab-runner/internal/store"
)

// Event is what clients learn about: one status change of one instance.
// It is the payload of the callback POST (step 4) and nothing else.
type Event struct {
	InstanceID string       `json:"instanceId"`
	SessionID  string       `json:"sessionId"`
	Status     store.Status `json:"status"`
	Endpoint   string       `json:"endpoint,omitempty"`
	FailReason string       `json:"failReason,omitempty"`
	ExpiresAt  time.Time    `json:"expiresAt"`
	At         time.Time    `json:"at"`
}

/*
Notifier delivers Events to the owning client. The real one POSTs to the
callbackUrl with retries (step 4); tests use a channel.
*/
type Notifier interface {
	Notify(ctx context.Context, callbackURL string, ev Event)
}

// NopNotifier drops events; handy when callbacks are not configured.
type NopNotifier struct{}

func (NopNotifier) Notify(context.Context, string, Event) {}

// Limits are the operator-set ceilings from the environment (plan §7).
type Limits struct {
	MaxInstances int           // LAB_MAX_INSTANCES — global, counts STARTING+RUNNING
	MaxTTL       time.Duration // LAB_MAX_TTL_SECONDS — requests above are clamped, not rejected
	PublicHost   string        // LAB_PUBLIC_HOST — what learners connect to
	Allowlist    docker.Allowlist
}

// Request is a validated, client-neutral create request. The api package
// builds it from JSON; the runner never sees HTTP types.
type Request struct {
	Owner       string // "learn", "challenge" — becomes the bitforge.owner label
	SessionID   string
	UserID      string
	Image       string
	ExposedPort int
	TTL         time.Duration
	CPUMillis   int
	MemoryMB    int
	Egress      bool
	ReadOnly    bool
	Env         map[string]string
	Labels      map[string]string
	CallbackURL string
}

var (
	ErrInvalid      = errors.New("invalid request")
	ErrNotAllowed   = errors.New("image not in allowlist")
	ErrCapacity     = errors.New("capacity reached")
	ErrNotFound     = store.ErrNotFound
	ErrDockerFailed = errors.New("docker error")
)

type Service struct {
	store    *store.Store
	engine   docker.Engine
	notify   Notifier
	limits   Limits
	log      *slog.Logger
	newID    func() string
	callback map[string]string // instanceID → callbackURL (not a label: URLs may carry secrets)
}

func New(st *store.Store, eng docker.Engine, n Notifier, limits Limits, log *slog.Logger) *Service {
	if n == nil {
		n = NopNotifier{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store:    st,
		engine:   eng,
		notify:   n,
		limits:   limits,
		log:      log,
		newID:    randomID,
		callback: map[string]string{},
	}
}

// ---------------------------------------------------------------- Create

// Create validates, reserves a STARTING slot in the store (so capacity is
// counted immediately), then does the slow Docker work. The returned Instance
// is the STARTING one; the client learns about RUNNING/FAILED via callback or
// by polling Get.
//
// Create blocks until the container is RUNNING or FAILED. The HTTP layer can
// choose to respond 201 with the STARTING instance from a goroutine; here the
// synchronous shape keeps the sequence readable and testable.
func (s *Service) Create(ctx context.Context, req Request) (store.Instance, error) {
	if err := s.validate(req); err != nil {
		return store.Instance{}, err
	}
	if !s.limits.Allowlist.Allows(req.Image) {
		return store.Instance{}, fmt.Errorf("%w: %s", ErrNotAllowed, req.Image)
	}
	if s.active() >= s.limits.MaxInstances {
		return store.Instance{}, fmt.Errorf("%w: %d instances", ErrCapacity, s.limits.MaxInstances)
	}

	ttl := req.TTL
	if ttl <= 0 || ttl > s.limits.MaxTTL {
		ttl = s.limits.MaxTTL
	}
	now := s.store.Now()
	inst := store.Instance{
		ID:         s.newID(),
		Owner:      req.Owner,
		SessionID:  req.SessionID,
		UserID:     req.UserID,
		Image:      req.Image,
		Status:     store.Starting,
		LaunchedAt: now,
		ExpiresAt:  now.Add(ttl),
	}
	// The stored label set is exactly what the container will carry: client
	// labels plus the reserved bitforge.* keys. That is what makes
	// List("bitforge.owner", "learn") work before and after a restart alike.
	inst.Labels = req.Labels
	inst.Labels = store.ToLabels(inst)
	// Reserve the slot before touching Docker: a burst of requests must not
	// all pass the capacity check while the first pull is still running.
	s.store.Put(inst)
	if req.CallbackURL != "" {
		s.callback[inst.ID] = req.CallbackURL
	}

	containerID, err := s.engine.Create(ctx, docker.CreateSpec{
		SessionID:   req.SessionID,
		Name:        "bitforge-" + inst.ID,
		Image:       req.Image,
		ExposedPort: req.ExposedPort,
		CPUMillis:   req.CPUMillis,
		MemoryMB:    req.MemoryMB,
		Egress:      req.Egress,
		ReadOnly:    req.ReadOnly,
		Env:         req.Env,
		Labels:      inst.Labels,
	})
	if err != nil {
		return s.finish(ctx, inst.ID, store.Failed, func(i *store.Instance) { i.FailReason = "create: " + err.Error() })
	}
	// From here on the store's ID is the container ID: the two must agree so
	// that Rebuild (which only has container IDs) finds the same record.
	s.rekey(inst.ID, containerID)

	port, err := s.engine.Start(ctx, containerID)
	if err != nil {
		_ = s.engine.Remove(context.WithoutCancel(ctx), containerID)
		return s.finish(ctx, containerID, store.Failed, func(i *store.Instance) { i.FailReason = "start: " + err.Error() })
	}
	endpoint := fmt.Sprintf("%s:%d", s.limits.PublicHost, port)
	return s.finish(ctx, containerID, store.Running, func(i *store.Instance) { i.Endpoint = endpoint })
}

func (s *Service) validate(req Request) error {
	switch {
	case req.Owner == "":
		return fmt.Errorf("%w: owner is required", ErrInvalid)
	case req.SessionID == "":
		return fmt.Errorf("%w: sessionId is required", ErrInvalid)
	case req.Image == "":
		return fmt.Errorf("%w: image is required", ErrInvalid)
	case req.ExposedPort <= 0 || req.ExposedPort > 65535:
		return fmt.Errorf("%w: exposedPort must be 1..65535", ErrInvalid)
	case req.CPUMillis < 0 || req.MemoryMB < 0:
		return fmt.Errorf("%w: limits must be >= 0", ErrInvalid)
	}
	for k := range req.Labels {
		if k == store.LabelOwner || k == store.LabelSessionID || k == store.LabelExpiresAt || k == docker.LabelManaged {
			return fmt.Errorf("%w: label %s is reserved", ErrInvalid, k)
		}
	}
	for _, inst := range s.store.List(store.LabelSessionID, req.SessionID) {
		if !inst.Status.IsTerminal() {
			return fmt.Errorf("%w: session %s already has an active instance", ErrInvalid, req.SessionID)
		}
	}
	return nil
}

// active counts the instances that hold a capacity slot.
func (s *Service) active() int {
	n := 0
	for _, inst := range s.store.List("", "") {
		if !inst.Status.IsTerminal() {
			n++
		}
	}
	return n
}

// rekey moves a STARTING record from the provisional id to the container id.
func (s *Service) rekey(from, to string) {
	inst, ok := s.store.Get(from)
	if !ok {
		return
	}
	s.store.Delete(from)
	inst.ID = to
	s.store.Put(inst)
	if cb, ok := s.callback[from]; ok {
		delete(s.callback, from)
		s.callback[to] = cb
	}
}

// ---------------------------------------------------------------- Get / List

func (s *Service) Get(id string) (store.Instance, bool) { return s.store.Get(id) }

func (s *Service) List(labelKey, labelValue string) []store.Instance {
	return s.store.List(labelKey, labelValue)
}

// ---------------------------------------------------------------- Stop / Expire

// Stop is the client's DELETE. Idempotent: unknown or already-terminal ids
// return nil. STARTING instances are cancelled (→ STOPPED) as well.
func (s *Service) Stop(ctx context.Context, id string) error {
	return s.tearDown(ctx, id, store.Stopped, "")
}

// Expire is the reaper's verb: identical mechanics, different terminal state
// so the client can tell "learner clicked stop" from "TTL ran out".
func (s *Service) Expire(ctx context.Context, id string) error {
	return s.tearDown(ctx, id, store.Expired, "ttl expired")
}

func (s *Service) tearDown(ctx context.Context, id string, final store.Status, reason string) error {
	inst, ok := s.store.Get(id)
	if !ok || inst.Status.IsTerminal() {
		return nil
	}
	if err := s.engine.Remove(ctx, id); err != nil {
		// Container is still there; keep the record non-terminal so the reaper
		// retries on the next tick rather than leaking it.
		return fmt.Errorf("%w: remove %s: %v", ErrDockerFailed, id, err)
	}
	_, err := s.finish(ctx, id, final, func(i *store.Instance) {
		if reason != "" {
			i.FailReason = reason
		}
	})
	return err
}

// ---------------------------------------------------------------- Reap / Reconcile

// ReapOnce expires every RUNNING instance past its TTL. Returns how many it
// handled; errors are logged per instance so one stuck container does not
// stop the others from being reaped. Called by the reaper loop (reaper.go).
func (s *Service) ReapOnce(ctx context.Context) int {
	n := 0
	for _, inst := range s.store.Expired() {
		if err := s.Expire(ctx, inst.ID); err != nil {
			s.log.Warn("reap failed", "instance", inst.ID, "err", err)
			continue
		}
		n++
	}
	return n
}

// Reconcile notices containers that stopped on their own (crash, OOM, the lab
// process exiting) and marks them FAILED so the client stops showing a dead
// endpoint. It also forgets terminal records older than keepTerminal so the
// in-memory table does not grow forever.
func (s *Service) Reconcile(ctx context.Context, keepTerminal time.Duration) {
	now := s.store.Now()
	for _, inst := range s.store.List("", "") {
		switch {
		case inst.Status == store.Running:
			running, err := s.engine.Inspect(ctx, inst.ID)
			if err != nil && !errors.Is(err, docker.ErrNotFound) {
				s.log.Warn("reconcile inspect failed", "instance", inst.ID, "err", err)
				continue
			}
			if err == nil && running {
				continue
			}
			reason := "container exited"
			if err != nil {
				reason = "container vanished"
			}
			_ = s.engine.Remove(ctx, inst.ID) // frees the network; nil if already gone
			if _, err := s.finish(ctx, inst.ID, store.Failed, func(i *store.Instance) { i.FailReason = reason }); err != nil {
				s.log.Warn("reconcile transition failed", "instance", inst.ID, "err", err)
			}
		case inst.Status.IsTerminal() && !inst.EndedAt.IsZero() && now.Sub(inst.EndedAt) > keepTerminal:
			s.store.Delete(inst.ID)
			delete(s.callback, inst.ID)
		}
	}
}

// ---------------------------------------------------------------- Rebuild

// Rebuild is called once at start-up. Containers that carry our labels are
// adopted into the store (RUNNING or, if they have exited, removed and not
// adopted — a dead lab is useless and the client was already told, or will
// learn from a 404). Containers with broken labels are removed: we cannot
// expire what we cannot attribute. Returns adopted and removed counts.
func (s *Service) Rebuild(ctx context.Context) (adopted, removed int, err error) {
	list, err := s.engine.ListManaged(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range list {
		inst, err := store.FromLabels(c.ID, c.Labels, c.Running)
		if err != nil || !c.Running {
			if rmErr := s.engine.Remove(ctx, c.ID); rmErr != nil {
				s.log.Warn("rebuild remove failed", "container", c.ID, "err", rmErr)
				continue
			}
			s.log.Info("rebuild removed container", "container", c.ID, "reason", reasonOf(err, c.Running))
			removed++
			continue
		}
		// We do not know the host port any more; the reaper and Stop do not
		// need it, and Learn keeps the endpoint it was told at RUNNING time.
		inst.LaunchedAt = s.store.Now()
		s.store.Put(inst)
		adopted++
	}
	return adopted, removed, nil
}

func reasonOf(err error, running bool) string {
	if err != nil {
		return err.Error()
	}
	if !running {
		return "not running"
	}
	return ""
}

// ---------------------------------------------------------------- the one place state changes

// finish applies a transition and tells the client. Every status change in
// this package goes through here, so "update store, then notify" can never
// be done in the wrong order or forgotten.
func (s *Service) finish(ctx context.Context, id string, next store.Status, mutate func(*store.Instance)) (store.Instance, error) {
	inst, err := s.store.Transition(id, next, mutate)
	if err != nil {
		return inst, err
	}
	if cb := s.callback[id]; cb != "" {
		s.notify.Notify(ctx, cb, Event{
			InstanceID: inst.ID,
			SessionID:  inst.SessionID,
			Status:     inst.Status,
			Endpoint:   inst.Endpoint,
			FailReason: inst.FailReason,
			ExpiresAt:  inst.ExpiresAt,
			At:         s.store.Now(),
		})
	}
	s.log.Info("instance "+string(next), "instance", inst.ID, "session", inst.SessionID, "owner", inst.Owner)
	return inst, nil
}

/*
1. The package comment at the top of service.go. It lists the five verbs and says every state change goes through finish(). Hold onto that; it's the whole design.

2. The types (Event, Notifier, Limits, Request). Request is the HTTP body after JSON parsing and before anything Docker-ish — the runner never sees net/http. Notifier is the callback-POST seam; tests plug a channel in, step 4 plugs in real HTTP. Limits are the LAB_* env vars.

3. Create — the only long function. Read it as five beats:

validate (shape, reserved labels, one active instance per session) → allowlist → capacity;
reserve the slot first: store.Put(STARTING) before touching Docker — otherwise ten requests all pass the capacity check during one slow image pull;
engine.Create → on failure finish(FAILED);
rekey: the store record moves from a provisional pending-… id to the container id, so Rebuild (which only has container ids) finds the same record later;
engine.Start → finish(RUNNING, endpoint) or remove-and-finish(FAILED).
Note it returns a FAILED instance, not an error — a Docker failure is a fact about the instance the client must be told, not a 500.
4. Stop / Expire → tearDown. Same mechanics, different terminal state (so Learn can distinguish "learner clicked stop" from "TTL ran out"). Idempotent: unknown or already-terminal ids return nil. If Remove fails, the record stays RUNNING on purpose — the reaper retries next tick rather than leaking a container.

5. ReapOnce / Reconcile / Rebuild — the three maintenance verbs:

ReapOnce: store.Expired() → Expire each; logs and continues past a stuck one.
Reconcile: catches containers that died on their own (crash, OOM) → FAILED "container exited", or vanished behind our back → FAILED "container vanished"; also forgets terminal records older than keepTerminal so memory is bounded.
Rebuild (start-up): ListManaged → adopt RUNNING ones via store.FromLabels; remove exited or unattributable ones — a dead lab is useless, and we must never keep a container we can't expire.
6. finish() at the bottom — transition in the store, then notify the client. Written once, so the order can't be wrong anywhere.

7. reaper.go is just when: a ticker, a select, Tick() calls ReapOnce then Reconcile. Tick is public so tests drive it with a frozen clock.

8. service_test.go — start with newRig: a FakeEngine, a frozen r.now the store reads, a channel Notifier. After that each test reads as a sentence: "capacity reached → ErrCapacity; stop one → slot free", "start fails → container removed, record FAILED, client told", "advance clock 11m → exactly one reaped", "crashed container → FAILED; terminal records vanish after keepTerminal", "rebuild adopts the running one, removes the dead and the unlabelled".
*/
