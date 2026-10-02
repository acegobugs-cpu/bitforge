package docker

import (
	"fmt"
	"maps"
	"net/netip"
	"sort"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// LabelManaged marks every container and network this runner created, whatever
// the client owner is. ListManaged filters on it; the reaper/rebuild trust it.
const (
	LabelManaged      = "bitforge.managed"
	LabelManagedValue = "lab-runner"
	networkPrefix     = "bitforge-"
	pidsLimit         = int64(256)
	nofileLimit       = int64(1024)
	tmpfsOpts         = "rw,noexec,nosuid,size=64m"
)

func networkName(sessionID string) string {
	return networkPrefix + sessionID
}

// loopback is the only host address a lab is ever published on; LAB_PUBLIC_HOST
// is how learners reach it (reverse proxy / SSH tunnel), never a 0.0.0.0 bind.
var loopback = netip.MustParseAddr("127.0.0.1")

// tcpPort turns the spec's exposed port into the API's typed port.
func tcpPort(port int) (network.Port, error) {
	if port <= 0 || port > 65535 {
		return network.Port{}, fmt.Errorf("exposed port %d out of range", port)
	}
	p, _ := network.PortFrom(uint16(port), network.TCP)
	return p, nil
}

// managedLabels = client labels (already through store.ToLabels, so the
// bitforge.owner / sessionId / expiresAt keys are set) + the managed marker.
// It never overwrites bitforge.owner: that is the client's identity.
func managedLabels(spec CreateSpec) map[string]string {
	labels := make(map[string]string, len(spec.Labels)+1)
	if spec.Labels != nil {
		maps.Copy(labels, spec.Labels)
	}

	labels[LabelManaged] = LabelManagedValue
	return labels
}

func containerConfig(spec CreateSpec) *container.Config {
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(env)

	port, _ := tcpPort(spec.ExposedPort) // validated by the caller; zero port is simply not exposed
	exposedPorts := network.PortSet{port: struct{}{}}

	return &container.Config{
		Image:        spec.Image,
		Env:          env,
		Labels:       managedLabels(spec),
		ExposedPorts: exposedPorts,
		Cmd:          spec.Command,
	}
}

func hostConfig(spec CreateSpec) *container.HostConfig {
	port, _ := tcpPort(spec.ExposedPort)
	portMap := network.PortMap{
		port: []network.PortBinding{{HostIP: loopback, HostPort: ""}}, // "" = ephemeral
	}

	memBytes := int64(spec.MemoryMB) * 1024 * 1024
	nanoCPUs := int64(spec.CPUMillis) * 1e6

	var tmpfs map[string]string
	if spec.ReadOnly {
		tmpfs = map[string]string{"/tmp": tmpfsOpts}
	}

	pids := pidsLimit

	return &container.HostConfig{
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		PortBindings:   portMap,
		ReadonlyRootfs: spec.ReadOnly,
		Tmpfs:          tmpfs,
		NetworkMode:    container.NetworkMode(networkName(spec.SessionID)),
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
		AutoRemove:     false,
		Resources: container.Resources{
			NanoCPUs:   nanoCPUs,
			Memory:     memBytes,
			MemorySwap: memBytes,
			PidsLimit:  &pids,
			Ulimits: []*container.Ulimit{
				{Name: "nofile", Soft: nofileLimit, Hard: nofileLimit},
			},
		},
	}
}

// Egress is blocked by turning off IP masquerade on the session's bridge, NOT
// with Docker's `--internal` flag: internal networks silently skip port
// publishing (inspect shows "80/tcp": null), so the learner could never reach
// the lab. Without masquerade the container keeps its published port (DNAT in)
// but has no NAT out, so connections to the internet time out.
const optMasquerade = "com.docker.network.bridge.enable_ip_masquerade"

func networkCreate(spec CreateSpec) client.NetworkCreateOptions {
	opts := client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: managedLabels(spec),
	}
	if !spec.Egress {
		opts.Options = map[string]string{optMasquerade: "false"}
	}
	return opts
}

func hostPortOf(inspect container.InspectResponse, exposedPort int) (int, error) {
	if inspect.NetworkSettings == nil {
		return 0, fmt.Errorf("container has no network settings yet")
	}
	port, err := tcpPort(exposedPort)
	if err != nil {
		return 0, err
	}
	bindings, ok := inspect.NetworkSettings.Ports[port]
	if !ok || len(bindings) == 0 {
		return 0, fmt.Errorf("no host port binding found for %s", port)
	}

	hostPort, err := network.ParsePort(bindings[0].HostPort)
	if err != nil || hostPort.Num() == 0 {
		return 0, fmt.Errorf("invalid host port binding %q: %v", bindings[0].HostPort, err)
	}
	return int(hostPort.Num()), nil
}
