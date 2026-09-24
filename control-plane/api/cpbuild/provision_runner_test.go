package cpbuild

import (
	"path/filepath"
	"strings"
	"testing"

	"freehold/control-plane/api/agent"
	"freehold/control-plane/state"
)

func openTestStore(t *testing.T) *state.StateStore {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestProvisionRunnerValidation pins the mechanical guardrails of the CPA's
// provision_runner flow: every rejection fires BEFORE any state is opened or
// provisioned (the flow only ever creates NEW runners, never widens one).
func TestProvisionRunnerValidation(t *testing.T) {
	spec := &Spec{CpaName: "freehold"}
	fn := BuildProvisionRunner(spec, nil) // reg is never reached by invalid args
	for _, tc := range []struct {
		name string
		args agent.ProvisionArgs
		want string // error substring
	}{
		{"bad name", args("RTX_BOT", "ssh", "a@h", "", "ai"), "kebab-case"},
		{"leading dash", args("-rtx", "ssh", "a@h", "", "ai"), "kebab-case"},
		{"missing kind", args("rtx-ssh-root", "", "a@h", "", "ai"), "kind"},
		{"unknown kind", args("rtx-ssh-root", "smtp", "a@h", "", "ai"), "unsupported kind"},
		{"ssh with inline secret", args("rtx-ssh-root", "ssh", "a@h", "hunter2", "ai"), "mint their own keypair"},
		{"api kind without credential", args("unifi-api-admin", "unifi", "https://u", "", "ai"), "operator-supplied credential"},
		{"no grantee", args("rtx-ssh-root", "ssh", "a@h", ""), "grant_to is required"},
		{"grants to the CPA", args("rtx-ssh-root", "ssh", "a@h", "", "freehold"), "the CPA holds no exec"},
	} {
		if _, err := fn(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error %q, got %v", tc.name, tc.want, err)
		}
	}
}

// args builds ProvisionArgs positionally (secret "" for ssh doors).
func args(name, kind, address, secret string, grantTo ...string) agent.ProvisionArgs {
	return agent.ProvisionArgs{Name: name, Kind: kind, Address: address, Secret: secret, GrantTo: grantTo}
}

// TestDynamicRunnersStageFromRecords pins the record → staging-table mapping:
// an agent-provisioned capability re-stages on every build with its fixed
// port, its own address, and adopt-only behavior.
func TestDynamicRunnersStageFromRecords(t *testing.T) {
	store := openTestStore(t)
	if err := store.InsertCapability("rtx3090-ssh-root", state.CapabilityRecord{
		Kind: "ssh", Address: "darcy@192.168.1.50", Port: 8800,
		Rosters: []string{"ai"}, CreatedAt: 5,
	}); err != nil {
		t.Fatal(err)
	}
	runners := dynamicRunners(store)
	if len(runners) != 1 {
		t.Fatalf("want 1 dynamic runner, got %d", len(runners))
	}
	r := runners[0]
	if r.name != "rtx3090-ssh-root" || r.kind != "ssh" || r.port != 8800 ||
		r.addr != "darcy@192.168.1.50" || !r.dynamic || len(r.rosters) != 1 || r.rosters[0] != "ai" {
		t.Fatalf("dynamic mapping: %+v", r)
	}
}

// TestNextCapabilityPortSkipsOccupied pins port allocation: records start
// above the DNS doors' range and never reuse a port (pod coords must stay
// stable across rebuilds).
func TestNextCapabilityPortSkipsOccupied(t *testing.T) {
	store := openTestStore(t)
	if got := nextCapabilityPort(store); got != 8800 {
		t.Fatalf("first dynamic port = %d, want 8800", got)
	}
	if err := store.InsertCapability("a-ssh-root", state.CapabilityRecord{Port: 8800}); err != nil {
		t.Fatal(err)
	}
	if got := nextCapabilityPort(store); got != 8801 {
		t.Fatalf("after 8800 taken = %d, want 8801", got)
	}
}

// TestAgentRunnerCoordsResolvesFromState pins the coord resolution a pod
// re-apply uses: every runner whose roster includes the agent — static and
// dynamic — with pubkeys read from state; not-yet-staged runners are skipped.
func TestAgentRunnerCoordsResolvesFromState(t *testing.T) {
	spec := &Spec{
		StateDir: t.TempDir(), CpIP: "10.0.0.9",
		CpHost: "cp.example.com", RelayHost: "relay.example.com",
	}
	store := openTestStore(t)
	store.InsertRunner("pve-ssh-root", state.RunnerRecord{NostrPubkey: strings.Repeat("a", 64), Status: state.RunnerActive})
	store.InsertRunner("rtx3090-ssh-root", state.RunnerRecord{NostrPubkey: strings.Repeat("b", 64), Status: state.RunnerActive})
	if err := store.InsertCapability("rtx3090-ssh-root", state.CapabilityRecord{
		Kind: "ssh", Address: "darcy@192.168.1.50", Port: 8800, Rosters: []string{"ai"},
	}); err != nil {
		t.Fatal(err)
	}

	ai := spec.agentRunnerCoords(store, "ai")
	if len(ai) != 1 {
		t.Fatalf("ai holds 1 dynamic door, got %v", ai)
	}
	if ai[0].Target != "rtx3090-ssh-root" || ai[0].Pubkey != strings.Repeat("b", 64) ||
		ai[0].URL != "http://10.0.0.9:8800" || ai[0].Secret != "rtx3090-ssh-root" {
		t.Fatalf("ai coords: %+v", ai[0])
	}
	net := spec.agentRunnerCoords(store, "network")
	if len(net) != 1 || net[0].Target != "pve-ssh-root" || net[0].URL != "http://10.0.0.9:8791" {
		t.Fatalf("network coords: %+v", net)
	}
	if got := spec.agentRunnerCoords(store, "nobody"); len(got) != 0 {
		t.Fatalf("nobody holds nothing, got %v", got)
	}
}

// TestDynamicRecordWithoutPackageFailsLoudly pins the rebuild contract for an
// agent-provisioned runner whose package vanished: staging refuses (the
// credential came from the operator and cannot be re-derived) instead of
// silently re-keying the door.
func TestDynamicRecordWithoutPackageFailsLoudly(t *testing.T) {
	spec := &Spec{StateDir: t.TempDir()}
	store := openTestStore(t)
	if err := store.InsertCapability("rtx3090-ssh-root", state.CapabilityRecord{
		Kind: "ssh", Address: "darcy@192.168.1.50", Port: 8800, Rosters: []string{"ai"},
	}); err != nil {
		t.Fatal(err)
	}
	r := capabilityRunner{name: "rtx3090-ssh-root", kind: "ssh", port: 8800,
		rosters: []string{"ai"}, addr: "darcy@192.168.1.50", dynamic: true}
	if err := spec.ensureCapabilityRunner(store, filepath.Join(spec.StateDir, "cp"), r, ""); err == nil {
		t.Fatal("a dynamic record without its package must fail loudly")
	} else if !strings.Contains(err.Error(), "re-provision") {
		t.Fatalf("error should name the re-provision path: %v", err)
	}
}
