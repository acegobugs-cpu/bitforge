//go:build integration
// +build integration

package docker

// Runs against the local Docker daemon:  go test -tags integration ./internal/docker
// Pulls docker.io/library/nginx:alpine on first run.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"bitforge/lab-runner/internal/store"
)

func TestClientLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c, err := New()
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	defer c.Close()
	if err := c.Ping(ctx); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}

	sessionID := fmt.Sprintf("it-%d", time.Now().UnixNano())
	spec := CreateSpec{
		SessionID:   sessionID,
		Image:       "nginx:alpine",
		ExposedPort: 80,
		CPUMillis:   500,
		MemoryMB:    128,
		Egress:      false,
		Labels: store.ToLabels(store.Instance{
			Owner: "test", SessionID: sessionID, ExpiresAt: time.Now().Add(time.Minute),
		}),
	}

	id, err := c.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove(context.Background(), id) })

	if running, err := c.Inspect(ctx, id); err != nil || running {
		t.Fatalf("after Create: running=%v err=%v; want created-not-started", running, err)
	}

	port, err := c.Start(ctx, id)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if port <= 0 {
		t.Fatalf("Start returned port %d", port)
	}

	// the network is internal (no egress) and carries our labels
	nets, err := c.api.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range nets.Items {
		if n.Name == networkName(sessionID) {
			found = true
			if !n.Internal {
				t.Error("network must be internal by default")
			}
			if n.Labels[LabelManaged] != LabelManagedValue {
				t.Error("network missing managed label")
			}
		}
	}
	if !found {
		t.Fatal("per-session network not created")
	}

	// reachable on loopback only
	if err := waitHTTP(fmt.Sprintf("http://127.0.0.1:%d/", port), 20*time.Second); err != nil {
		t.Fatalf("lab not reachable: %v", err)
	}

	// visible to the rebuild path with the right labels
	list, err := c.ListManaged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, m := range list {
		if m.ID == id {
			seen = true
			if !m.Running || m.Labels[store.LabelSessionID] != sessionID {
				t.Errorf("ListManaged entry wrong: %+v", m)
			}
			if _, err := store.FromLabels(m.ID, m.Labels, m.Running); err != nil {
				t.Errorf("labels do not rebuild into an Instance: %v", err)
			}
		}
	}
	if !seen {
		t.Error("ListManaged did not include our container")
	}

	// remove is idempotent and takes the network with it
	if err := c.Remove(ctx, id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := c.Remove(ctx, id); err != nil {
		t.Errorf("second Remove must be nil, got %v", err)
	}
	if _, err := c.Inspect(ctx, id); err != ErrNotFound {
		t.Errorf("Inspect after remove: want ErrNotFound, got %v", err)
	}
	nets, _ = c.api.NetworkList(ctx, client.NetworkListOptions{})
	for _, n := range nets.Items {
		if n.Name == networkName(sessionID) {
			t.Error("network survived Remove")
		}
	}
}

func TestClientCreateRollsBackNetworkOnBadImage(t *testing.T) {
	ctx := context.Background()
	c, err := New()
	if err != nil || c.Ping(ctx) != nil {
		t.Skip("docker not available")
	}
	defer c.Close()

	sessionID := fmt.Sprintf("it-bad-%d", time.Now().UnixNano())
	_, err = c.Create(ctx, CreateSpec{
		SessionID: sessionID, Image: "ghcr.io/bitforge/does-not-exist:never", ExposedPort: 80,
		Labels: store.ToLabels(store.Instance{Owner: "test", SessionID: sessionID, ExpiresAt: time.Now()}),
	})
	if err == nil {
		t.Fatal("expected pull failure")
	}
	nets, _ := c.api.NetworkList(ctx, client.NetworkListOptions{})
	for _, n := range nets.Items {
		if n.Name == networkName(sessionID) {
			_, _ = c.api.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
			t.Error("failed Create left its network behind")
		}
	}
}

func waitHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", url)
}
