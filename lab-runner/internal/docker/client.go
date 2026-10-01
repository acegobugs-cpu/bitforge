package docker

import (
	"context"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Client is the only type in the runner that talks to the Docker daemon. It
// implements Engine by composing the pure configs in spec.go with SDK calls
// (github.com/moby/moby/client — the semver successor of github.com/docker/docker).
// Everything above it is tested against FakeEngine.
type Client struct {
	api *client.Client
}

// New connects using the standard DOCKER_HOST / DOCKER_CERT_PATH env; the new
// client negotiates the API version by default so one binary spans daemons.
func New() (*Client, error) {
	api, err := client.New(client.FromEnv, client.WithUserAgent("bitforge-lab-runner/0.1"))
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{api: api}, nil
}

func (c *Client) Close() error { return c.api.Close() }

// Create makes the per-session network, ensures the image is present, and
// creates (does not start) the container. On any failure after the network
// exists it cleans the network up so a failed Create leaves nothing behind.
func (c *Client) Create(ctx context.Context, spec CreateSpec) (string, error) {
	if _, err := tcpPort(spec.ExposedPort); err != nil {
		return "", err
	}
	netName := networkName(spec.SessionID)
	if _, err := c.api.NetworkCreate(ctx, netName, networkCreate(spec)); err != nil {
		return "", fmt.Errorf("create network %s: %w", netName, err)
	}
	fail := func(err error) (string, error) {
		_, _ = c.api.NetworkRemove(context.WithoutCancel(ctx), netName, client.NetworkRemoveOptions{})
		return "", err
	}

	if err := c.ensureImage(ctx, spec.Image); err != nil {
		return fail(err)
	}

	resp, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       spec.Name,
		Config:     containerConfig(spec),
		HostConfig: hostConfig(spec),
	})
	if err != nil {
		return fail(fmt.Errorf("create container: %w", err))
	}
	return resp.ID, nil
}

// ensureImage pulls only when the image is absent locally. Wait blocks until
// the daemon has finished the pull (the stream is lazy otherwise).
func (c *Client) ensureImage(ctx context.Context, ref string) error {
	if _, err := c.api.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect image %s: %w", ref, err)
	}
	resp, err := c.api.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", ref, err)
	}
	defer resp.Close()
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull image %s: %w", ref, err)
	}
	return nil
}

// Start runs the container and returns the loopback host port Docker chose.
func (c *Client) Start(ctx context.Context, id string) (int, error) {
	if _, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return 0, fmt.Errorf("start container: %w", err)
	}
	res, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return 0, fmt.Errorf("inspect after start: %w", err)
	}
	exposed, err := exposedPortOf(res.Container)
	if err != nil {
		return 0, err
	}
	return hostPortOf(res.Container, exposed)
}

// exposedPortOf recovers the single port the container config exposes, so
// Start does not need the CreateSpec again.
func exposedPortOf(inspect container.InspectResponse) (int, error) {
	if inspect.Config == nil || len(inspect.Config.ExposedPorts) != 1 {
		return 0, fmt.Errorf("expected exactly one exposed port, got %v", inspect.Config)
	}
	for p := range inspect.Config.ExposedPorts {
		return int(p.Num()), nil
	}
	return 0, errors.New("unreachable")
}

func (c *Client) Inspect(ctx context.Context, id string) (bool, error) {
	res, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("inspect container: %w", err)
	}
	return res.Container.State != nil && res.Container.State.Running, nil
}

// ErrNotFound is returned by Inspect when the daemon no longer knows the id.
var ErrNotFound = errors.New("container not found")

// Remove force-removes the container and then its per-session network.
// Both steps treat "already gone" as success so the reaper and a client
// DELETE can race without error.
func (c *Client) Remove(ctx context.Context, id string) error {
	var netName string
	if res, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{}); err == nil {
		if res.Container.HostConfig != nil {
			netName = string(res.Container.HostConfig.NetworkMode)
		}
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect before remove: %w", err)
	}

	_, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove container: %w", err)
	}
	if netName != "" {
		if _, err := c.api.NetworkRemove(ctx, netName, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("remove network %s: %w", netName, err)
		}
	}
	return nil
}

func managedFilter() client.Filters {
	return make(client.Filters).Add("label", LabelManaged+"="+LabelManagedValue)
}

// ListManaged returns every container this runner created, running or not,
// so the store can be rebuilt after a restart and stale ones reaped.
func (c *Client) ListManaged(ctx context.Context) ([]ManagedContainer, error) {
	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: managedFilter()})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]ManagedContainer, 0, len(res.Items))
	for _, ctr := range res.Items {
		out = append(out, ManagedContainer{ID: ctr.ID, Labels: ctr.Labels, Running: ctr.State == container.StateRunning})
	}
	return out, nil
}

// RemoveOrphanNetworks deletes managed networks whose container is gone (a
// crash between ContainerRemove and NetworkRemove). Called once at start-up.
func (c *Client) RemoveOrphanNetworks(ctx context.Context) (int, error) {
	res, err := c.api.NetworkList(ctx, client.NetworkListOptions{Filters: managedFilter()})
	if err != nil {
		return 0, fmt.Errorf("list networks: %w", err)
	}
	removed := 0
	for _, n := range res.Items {
		insp, err := c.api.NetworkInspect(ctx, n.ID, client.NetworkInspectOptions{})
		if err != nil || len(insp.Network.Containers) > 0 {
			continue
		}
		if _, err := c.api.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); err == nil {
			removed++
		}
	}
	return removed, nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.api.Ping(ctx, client.PingOptions{})
	return err
}

var _ Engine = (*Client)(nil)
