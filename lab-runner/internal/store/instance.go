package store

import (
	"errors"
	"fmt"
	"maps"
	"time"
)

// Status is the runner's view of one lab container. Transitions follow
// docs/plan 01/04-labs-and-runner.md §3:
//
//	STARTING → RUNNING | STOPPED | FAILED
//	RUNNING  → STOPPED | EXPIRED | FAILED
//	STOPPED, EXPIRED, FAILED are terminal.
//
// STOPPED from STARTING is a cancel (DELETE while the image is still pulling).
type Status string

const (
	Starting Status = "STARTING"
	Running  Status = "RUNNING"
	Stopped  Status = "STOPPED"
	Expired  Status = "EXPIRED"
	Failed   Status = "FAILED"
)

// Container labels are the source of truth for "what is running": the runner
// keeps no database and rebuilds its table from these on start-up. The prefix
// is the product name so the labels stay meaningful outside this repo.
const (
	LabelOwner     = "bitforge.owner"     // which client created it: "learn", "challenge"
	LabelSessionID = "bitforge.sessionId" // the client's session id (opaque to the runner)
	LabelUserID    = "bitforge.userId"    // the learner (opaque; for ops filtering only)
	LabelExpiresAt = "bitforge.expiresAt" // RFC 3339; the reaper works off this
	LabelImage     = "bitforge.image"
	LabelEndpoint  = "bitforge.endpoint"
)

type Instance struct {
	ID        string `json:"instanceId"`
	Owner     string `json:"owner"`
	SessionID string `json:"sessionId"`
	UserID    string `json:"userId,omitempty"`
	Image     string `json:"image"`
	Endpoint  string `json:"endpoint,omitempty"`
	Status    Status `json:"status"`

	FailReason string            `json:"failReason,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`

	LaunchedAt time.Time `json:"launchedAt,omitzero"`
	ExpiresAt  time.Time `json:"expiresAt"`
	EndedAt    time.Time `json:"endedAt,omitzero"` // set on any terminal transition
}

// IsTerminal reports whether no further transition is possible.
func (s Status) IsTerminal() bool {
	return s == Stopped || s == Expired || s == Failed
}

func (s Status) CanTransitionTo(target Status) bool {
	switch s {
	case Starting:
		return target == Running || target == Stopped || target == Failed
	case Running:
		return target == Stopped || target == Expired || target == Failed
	default:
		return false
	}
}

var (
	ErrInvalidTransition = errors.New("invalid status transition")
	ErrMissingSessionID  = errors.New("missing " + LabelSessionID + " label")
	ErrMissingOwner      = errors.New("missing " + LabelOwner + " label")
	ErrBadExpiresAt      = errors.New("missing or unparsable " + LabelExpiresAt + " label")
)

// ToLabels serialises the fields the runner needs to rebuild an Instance, on
// top of any client-supplied labels (which may not override the reserved keys).
func ToLabels(inst Instance) map[string]string {
	labels := make(map[string]string, len(inst.Labels)+6)
	maps.Copy(labels, inst.Labels)

	labels[LabelOwner] = inst.Owner
	labels[LabelSessionID] = inst.SessionID
	labels[LabelExpiresAt] = inst.ExpiresAt.UTC().Format(time.RFC3339)
	if inst.UserID != "" {
		labels[LabelUserID] = inst.UserID
	}
	if inst.Image != "" {
		labels[LabelImage] = inst.Image
	}
	if inst.Endpoint != "" {
		labels[LabelEndpoint] = inst.Endpoint
	}
	return labels
}

// FromLabels reconstructs an Instance from a container's labels (rebuild on
// start-up). It is strict on purpose: a container we cannot attribute or
// cannot expire is one we must not silently adopt — the caller decides
// whether to remove it.
func FromLabels(containerID string, labels map[string]string, running bool) (Instance, error) {
	owner := labels[LabelOwner]
	if owner == "" {
		return Instance{}, ErrMissingOwner
	}
	sessionID := labels[LabelSessionID]
	if sessionID == "" {
		return Instance{}, ErrMissingSessionID
	}
	expiresAt, err := time.Parse(time.RFC3339, labels[LabelExpiresAt])
	if err != nil {
		return Instance{}, fmt.Errorf("%w: %v", ErrBadExpiresAt, err)
	}

	status := Stopped
	if running {
		status = Running
	}

	instLabels := make(map[string]string, len(labels))
	maps.Copy(instLabels, labels)

	return Instance{
		ID:        containerID,
		Owner:     owner,
		SessionID: sessionID,
		UserID:    labels[LabelUserID],
		Image:     labels[LabelImage],
		Endpoint:  labels[LabelEndpoint],
		Status:    status,
		ExpiresAt: expiresAt,
		Labels:    instLabels,
	}, nil
}
