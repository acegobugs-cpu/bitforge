package docker

import (
	"errors"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"bitforge/lab-runner/internal/store"
)

var p8080 = network.MustParsePort("8080/tcp")

func baseSpec() CreateSpec {
	return CreateSpec{
		SessionID:   "sess-1",
		Image:       "ghcr.io/bitforge/labs/hello-flag:1",
		ExposedPort: 8080,
		CPUMillis:   500,
		MemoryMB:    256,
		Env:         map[string]string{"FLAG_1": "FLAG{x}", "A": "1"},
		Labels:      map[string]string{store.LabelOwner: "learn", store.LabelSessionID: "sess-1"},
	}
}

// The isolation table from 04-labs-and-runner.md §4, asserted flag by flag.
func TestHostConfigIsolation(t *testing.T) {
	hc := hostConfig(baseSpec())

	if !reflect.DeepEqual(hc.CapDrop, []string{"ALL"}) {
		t.Errorf("CapDrop = %v, want [ALL]", hc.CapDrop)
	}
	if !reflect.DeepEqual(hc.SecurityOpt, []string{"no-new-privileges"}) {
		t.Errorf("SecurityOpt = %v", hc.SecurityOpt)
	}
	if hc.Resources.NanoCPUs != 500_000_000 {
		t.Errorf("NanoCPUs = %d, want 5e8 (500 millis)", hc.Resources.NanoCPUs)
	}
	if hc.Resources.Memory != 256<<20 || hc.Resources.MemorySwap != hc.Resources.Memory {
		t.Errorf("Memory=%d MemorySwap=%d; swap must equal memory (no swap)", hc.Resources.Memory, hc.Resources.MemorySwap)
	}
	if hc.Resources.PidsLimit == nil || *hc.Resources.PidsLimit != 256 {
		t.Errorf("PidsLimit = %v, want 256", hc.Resources.PidsLimit)
	}
	if len(hc.Resources.Ulimits) != 1 || hc.Resources.Ulimits[0].Name != "nofile" || hc.Resources.Ulimits[0].Hard != 1024 {
		t.Errorf("Ulimits = %+v", hc.Resources.Ulimits)
	}
	if string(hc.NetworkMode) != "bitforge-sess-1" {
		t.Errorf("NetworkMode = %s, want the per-session network", hc.NetworkMode)
	}
	if hc.RestartPolicy.Name != container.RestartPolicyDisabled || hc.AutoRemove {
		t.Errorf("RestartPolicy=%s AutoRemove=%v; must not restart and must stay inspectable after exit", hc.RestartPolicy.Name, hc.AutoRemove)
	}

	// published on loopback only, ephemeral host port
	b := hc.PortBindings[p8080]
	if len(b) != 1 || b[0].HostIP.String() != "127.0.0.1" || b[0].HostPort != "" {
		t.Errorf("PortBindings = %+v, want one 127.0.0.1:<ephemeral> binding", hc.PortBindings)
	}

	// read-only is opt-in
	if hc.ReadonlyRootfs || hc.Tmpfs != nil {
		t.Error("ReadOnly=false must not set ReadonlyRootfs/Tmpfs")
	}
	ro := baseSpec()
	ro.ReadOnly = true
	rhc := hostConfig(ro)
	if !rhc.ReadonlyRootfs || rhc.Tmpfs["/tmp"] != "rw,noexec,nosuid,size=64m" {
		t.Errorf("ReadOnly=true: ReadonlyRootfs=%v Tmpfs=%v", rhc.ReadonlyRootfs, rhc.Tmpfs)
	}
}

func TestContainerConfig(t *testing.T) {
	spec := baseSpec()
	spec.Command = []string{"/entry.sh"}
	cc := containerConfig(spec)

	if cc.Image != spec.Image {
		t.Errorf("Image = %s", cc.Image)
	}
	// env is sorted so configs are deterministic (and diffable in logs)
	if !reflect.DeepEqual(cc.Env, []string{"A=1", "FLAG_1=FLAG{x}"}) {
		t.Errorf("Env = %v, want sorted k=v", cc.Env)
	}
	if _, ok := cc.ExposedPorts[p8080]; !ok || len(cc.ExposedPorts) != 1 {
		t.Errorf("ExposedPorts = %v", cc.ExposedPorts)
	}
	if !reflect.DeepEqual(cc.Cmd, spec.Command) {
		t.Errorf("Cmd = %v", cc.Cmd)
	}
	// labels: client labels preserved (owner stays "learn"), managed marker added
	if cc.Labels[store.LabelOwner] != "learn" {
		t.Errorf("owner label overwritten: %v", cc.Labels)
	}
	if cc.Labels[LabelManaged] != LabelManagedValue {
		t.Errorf("managed marker missing: %v", cc.Labels)
	}
	// the spec's own map must not be mutated
	if _, leaked := spec.Labels[LabelManaged]; leaked {
		t.Error("containerConfig mutated spec.Labels")
	}
}

func TestNetworkCreate(t *testing.T) {
	nc := networkCreate(baseSpec())
	if nc.Driver != "bridge" || nc.Internal {
		t.Errorf("must be a plain bridge, never --internal (internal networks do not publish ports): %+v", nc)
	}
	if nc.Options[optMasquerade] != "false" {
		t.Errorf("default (no egress) must disable IP masquerade: %v", nc.Options)
	}
	if nc.Labels[LabelManaged] != LabelManagedValue || nc.Labels[store.LabelSessionID] != "sess-1" {
		t.Errorf("network labels must carry the marker and session so it is reaped with the container: %v", nc.Labels)
	}

	eg := baseSpec()
	eg.Egress = true
	if _, set := networkCreate(eg).Options[optMasquerade]; set {
		t.Error("Egress=true must leave masquerade at its default (on)")
	}
}

func TestHostPortOf(t *testing.T) {
	inspect := container.InspectResponse{
		NetworkSettings: &container.NetworkSettings{
			Ports: network.PortMap{
				p8080: []network.PortBinding{{HostIP: loopback, HostPort: "32771"}},
			},
		},
	}
	port, err := hostPortOf(inspect, 8080)
	if err != nil || port != 32771 {
		t.Errorf("hostPortOf = %d, %v", port, err)
	}
	if _, err := hostPortOf(inspect, 9090); err == nil {
		t.Error("unpublished port must be an error")
	}
	inspect.NetworkSettings.Ports[p8080][0].HostPort = "not-a-port"
	if _, err := hostPortOf(inspect, 8080); err == nil {
		t.Error("garbage host port must be an error")
	}
	if _, err := hostPortOf(container.InspectResponse{}, 8080); err == nil {
		t.Error("nil NetworkSettings must be an error, not a panic")
	}
	if _, err := tcpPort(0); err == nil {
		t.Error("port 0 must be rejected")
	}
	if _, err := tcpPort(70000); err == nil {
		t.Error("port > 65535 must be rejected")
	}
}

func TestAllowlist(t *testing.T) {
	al := ParseAllowlist(" ghcr.io/bitforge/labs/ , , alpine ")
	cases := map[string]bool{
		"ghcr.io/bitforge/labs/hello-flag:1":   true,
		"ghcr.io/bitforge/labs/x/y@sha256:abc": true,
		"ghcr.io/bitforge/labsx:1":             false,
		"ghcr.io/bitforge/labs/../evil":        false,
		"alpine":                               true, // "alpine" prefix normalises to docker.io/library/alpine
		"alpine:3.20":                          true,
		"docker.io/library/alpine:3.20":        true,
		"index.docker.io/library/alpine":       true,
		"alpinex":                              true,  // prefix match, as documented: be specific with a trailing "/" or ":"
		"library/alpine":                       true,  // docker.io/library/alpine
		"evil/alpine":                          false, // docker.io/evil/alpine
		"":                                     false,
		"ghcr.io/bitforge/labs/x y":            false,
	}
	for img, want := range cases {
		if got := al.Allows(img); got != want {
			t.Errorf("Allows(%q) = %v, want %v", img, got, want)
		}
	}
	if (Allowlist{}).Allows("ghcr.io/bitforge/labs/hello-flag:1") {
		t.Error("empty allowlist must deny everything (fail closed)")
	}
	if ParseAllowlist("") != nil {
		t.Error("blank env must parse to an empty allowlist")
	}
}

// The fake honours the same contract the real client must: idempotent Remove,
// labels flow through, Start flips Running.
func TestFakeEngineContract(t *testing.T) {
	ctx := t.Context()
	f := NewFakeEngine()
	id, err := f.Create(ctx, baseSpec())
	if err != nil || id == "" {
		t.Fatalf("Create: %s, %v", id, err)
	}
	if running, _ := f.Inspect(ctx, id); running {
		t.Error("created but not started must not be running")
	}
	if port, err := f.Start(ctx, id); err != nil || port != 32768 {
		t.Errorf("Start = %d, %v", port, err)
	}
	if running, _ := f.Inspect(ctx, id); !running {
		t.Error("started must be running")
	}
	list, _ := f.ListManaged(ctx)
	if len(list) != 1 || list[0].Labels[store.LabelOwner] != "learn" || !list[0].Running {
		t.Errorf("ListManaged = %+v", list)
	}
	if err := f.Remove(ctx, id); err != nil || f.Has(id) {
		t.Error("Remove failed")
	}
	if err := f.Remove(ctx, id); err != nil {
		t.Error("second Remove must be nil (idempotent)")
	}
	if _, err := f.Start(ctx, id); err == nil {
		t.Error("Start on removed container must fail")
	}

	// knobs
	f.FailCreate = errors.New("boom")
	if _, err := f.Create(ctx, baseSpec()); err == nil {
		t.Error("FailCreate knob ignored")
	}
}
