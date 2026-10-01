package docker

import "context"

type CreateSpec struct {
	SessionID   string
	Name        string
	Image       string
	ExposedPort int
	CPUMillis   int
	MemoryMB    int
	Egress      bool
	ReadOnly    bool
	Env         map[string]string
	Labels      map[string]string
	Command     []string
}

type ManagedContainer struct {
	ID      string
	Labels  map[string]string
	Running bool
}

type Engine interface {
	Create(ctx context.Context, spec CreateSpec) (containerID string, err error) // network + container, not started
	Start(ctx context.Context, containerID string) (hostPort int, err error)     // returns the 127.0.0.1 published port
	Inspect(ctx context.Context, containerID string) (running bool, err error)
	Remove(ctx context.Context, containerID string) error        // container + its network; idempotent, nil if already gone
	ListManaged(ctx context.Context) ([]ManagedContainer, error) // filter label bitforge.owner
	Ping(ctx context.Context) error
}
