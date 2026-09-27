package store

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("instance not found")
)

// Store is the in-memory instance table. It is rebuilt from container labels
// on start-up (see FromLabels) and is the only thing the HTTP layer, the
// reaper and the callback sender read. All methods are safe for concurrent use
// and hand out copies, never internal pointers.
type Store struct {
	mu        sync.RWMutex
	instances map[string]*Instance
	clock     func() time.Time
}

// NewStore takes a clock so tests can freeze time; nil means time.Now.
func NewStore(clock func() time.Time) *Store {
	if clock == nil {
		clock = time.Now
	}
	return &Store{
		instances: make(map[string]*Instance),
		clock:     clock,
	}
}

// Now is the store's notion of the current time (injected in tests).
func (s *Store) Now() time.Time { return s.clock() }

// Put inserts or replaces an instance wholesale (creation and start-up rebuild).
// Status changes on an existing instance must go through Transition.
func (s *Store) Put(inst Instance) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cp := copyInstance(&inst)
	s.instances[inst.ID] = &cp
}

func (s *Store) Get(id string) (Instance, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	inst, ok := s.instances[id]
	if !ok {
		return Instance{}, false
	}
	return copyInstance(inst), true
}

// Delete forgets an instance (after the container is gone). Idempotent.
func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, id)
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.instances)
}

// List returns instances carrying labelKey=labelValue, or all of them when
// labelKey is empty. Sorted by LaunchedAt then ID so responses are stable.
func (s *Store) List(labelKey, labelValue string) []Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]Instance, 0, len(s.instances))
	for _, inst := range s.instances {
		if labelKey == "" || inst.Labels[labelKey] == labelValue {
			result = append(result, copyInstance(inst))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].LaunchedAt.Equal(result[j].LaunchedAt) {
			return result[i].LaunchedAt.Before(result[j].LaunchedAt)
		}
		return result[i].ID < result[j].ID
	})
	return result
}

// Transition moves an instance to next if the state machine allows it, then
// applies mutate (endpoint, fail reason…). mutate runs on a copy and cannot
// change Status, ID or the timestamps the store owns; a terminal transition
// stamps EndedAt. On an illegal transition nothing is changed.
func (s *Store) Transition(id string, next Status, mutate func(*Instance)) (Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inst, ok := s.instances[id]
	if !ok {
		return Instance{}, ErrNotFound
	}
	if !inst.Status.CanTransitionTo(next) {
		return copyInstance(inst), fmt.Errorf("%w: %s → %s", ErrInvalidTransition, inst.Status, next)
	}

	updated := copyInstance(inst)
	if mutate != nil {
		mutate(&updated)
	}
	updated.ID = inst.ID
	updated.LaunchedAt = inst.LaunchedAt
	updated.Status = next
	if next.IsTerminal() {
		updated.EndedAt = s.clock()
	} else {
		updated.EndedAt = inst.EndedAt
	}

	*inst = updated
	return copyInstance(inst), nil
}

// Expired returns RUNNING instances whose TTL has passed according to the
// store's clock — the reaper's work list. STARTING instances are left alone
// (the create path owns them); terminal ones are already done.
func (s *Store) Expired() []Instance {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := s.clock()
	var expired []Instance
	for _, inst := range s.instances {
		if inst.Status == Running && !inst.ExpiresAt.After(now) {
			expired = append(expired, copyInstance(inst))
		}
	}
	return expired
}

func copyInstance(inst *Instance) Instance {
	cp := *inst
	if inst.Labels != nil {
		cp.Labels = make(map[string]string, len(inst.Labels))
		maps.Copy(cp.Labels, inst.Labels)
	}
	return cp
}
