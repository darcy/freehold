package cpbuild

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/secret-management"
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
// provisioned (the flow only ever creates NEW runners, never widens one). The
// grantee resolution needs a registry, so the table's registry-backed errors
// live in their own tests.
func TestProvisionRunnerValidation(t *testing.T) {
	spec := &Spec{CpaName: "freehold"}
	fn := BuildProvisionRunner(spec, nil) // reg is never reached by invalid args
	for _, tc := range []struct {
		name string
		args agent.ProvisionArgs
		want string // error substring
	}{
		{"bad name", args("RTX_BOT", "ssh", "a@h", "ai"), "kebab-case"},
		{"leading dash", args("-rtx", "ssh", "a@h", "ai"), "kebab-case"},
		{"missing kind", args("rtx-ssh-root", "", "a@h", "ai"), "kind"},
		{"unknown kind", args("rtx-ssh-root", "smtp", "a@h", "ai"), "unsupported kind"},
		{"reserved dns prefix", args("cloudflare-api-example-com", "unifi", "https://u", "ai"), "reserved"},
		{"no grantee", args("rtx-ssh-root", "ssh", "a@h"), "grant_to is required"},
		{"grants to the CPA", args("rtx-ssh-root", "ssh", "a@h", "freehold"), "the CPA holds no exec"},
	} {
		if _, err := fn(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error %q, got %v", tc.name, tc.want, err)
		}
	}
}

// args builds ProvisionArgs positionally (the tool is credential-blind: no
// secret field exists).
func args(name, kind, address string, grantTo ...string) agent.ProvisionArgs {
	return agent.ProvisionArgs{Name: name, Kind: kind, Address: address, GrantTo: grantTo}
}

// testRegistry is an on-disk registry with the departments + a custom agent,
// the grantee-resolution source for the flow tests.
func testRegistry(t *testing.T) (*agenttools.Registry, string) {
	t.Helper()
	dir := t.TempDir()
	reg, err := agenttools.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct{ name, pk string }{
		{"ai", strings.Repeat("a", 64)},
		{"network", strings.Repeat("b", 64)},
		{"deployer", strings.Repeat("c", 64)},
	} {
		if _, err := reg.RegisterAgent(a.name, a.pk, a.name); err != nil {
			t.Fatal(err)
		}
	}
	return reg, dir
}

// TestProvisionRunnerNeverTouchesBuildTimeRunners pins the containment
// boundary the review flagged: a build-time capability runner's name is
// refused outright, and an existing runner with NO capability record (the
// operator/console provisioned it) is refused — the CPA only ever widens its
// OWN doors (re-provisions of recorded capabilities).
func TestProvisionRunnerNeverTouchesBuildTimeRunners(t *testing.T) {
	root := t.TempDir()
	spec := &Spec{StateDir: filepath.Join(root, "cp"), CpaName: "freehold"}
	fn := BuildProvisionRunner(spec, nil)

	// A static-table name is refused even when no package exists yet (the
	// build just hasn't staged it — the name is still operator-owned).
	if _, err := fn(args("pve-ssh-root", "ssh", "root@10.0.0.5", "ai")); err == nil ||
		!strings.Contains(err.Error(), "operator-owned") {
		t.Fatalf("static-runner name must be refused, got %v", err)
	}

	// An existing operator-provisioned runner (no capability record) is
	// refused on the adopt path. The state dirs mirror the CP layout (the
	// agent-tools state dir is the console state dir's sibling).
	store2, err := state.Open(filepath.Join(root, "control-plane"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.ProvisionRunner(store2, &provisioner.ProvisionRequest{
		Name: "ops-unifi", Kind: "unifi", Address: "https://unifi.local",
		Secret: []byte("key"), RunnerDir: runnerPackageDir(filepath.Join(root, "control-plane"), "ops-unifi"),
	}); err != nil {
		t.Fatal(err)
	}
	reg, regDir := testRegistry(t)
	_ = regDir
	fn2 := BuildProvisionRunner(&Spec{StateDir: filepath.Join(root, "agent-tools"), CpaName: "freehold"}, reg)
	if _, err := fn2(args("ops-unifi", "unifi", "https://unifi.local", "ai")); err == nil ||
		!strings.Contains(err.Error(), "not provisioned by an agent") {
		t.Fatalf("operator-provisioned runner must be refused, got %v", err)
	}
}

// TestProvisionRunnerOperatorOriginRefused pins the provenance boundary: a
// capability record the CONSOLE created (origin=operator) is rebuild-safe but
// agent-untouchable — the CPA cannot adopt-grant onto it.
func TestProvisionRunnerOperatorOriginRefused(t *testing.T) {
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "control-plane"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: "ops-unifi", Kind: "unifi", Address: "https://unifi.local",
		Secret: []byte("key"), RunnerDir: runnerPackageDir(filepath.Join(root, "control-plane"), "ops-unifi"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCapability("ops-unifi", state.CapabilityRecord{
		Kind: "unifi", Address: "https://unifi.local", Port: 8800,
		Rosters: []string{"ai"}, Origin: state.OriginOperator,
	}); err != nil {
		t.Fatal(err)
	}
	reg, _ := testRegistry(t)
	fn := BuildProvisionRunner(&Spec{StateDir: filepath.Join(root, "agent-tools"), CpaName: "freehold"}, reg)
	if _, err := fn(args("ops-unifi", "unifi", "https://unifi.local", "ai")); err == nil ||
		!strings.Contains(err.Error(), "operator") {
		t.Fatalf("an operator-origin record must be refused, got %v", err)
	}
}

// TestProvisionRunnerEmptyDoor pins the empty-shell-door flow: an api-kind
// door provisions with NO credential (a "pending" placeholder seals) — the
// credential is filled via the console web UI, never any agent's chat. The
// seal lands before the staging tail (which fails hermetically at the relay
// sync); the report's door link is pinned in TestDoorLink.
func TestProvisionRunnerEmptyDoor(t *testing.T) {
	root := t.TempDir()
	cpState := filepath.Join(root, "control-plane")
	spec := &Spec{
		StateDir: filepath.Join(root, "agent-tools"), CpaName: "freehold",
		CpIP: "10.0.0.9", CpHost: "cp.example.com",
	}
	reg, _ := testRegistry(t)
	fn := BuildProvisionRunner(spec, reg)
	_, _ = fn(args("unifi-api-admin", "unifi", "https://unifi.local", "ai"))
	// The placeholder sealed (the disk is the truth).
	st, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := st.GetSecret("unifi-api-admin")
	if !ok || rec.CiphertextHex == "" {
		t.Fatal("the empty door must seal its placeholder")
	}
	pkg, err := wire.Load(runnerPackageDir(cpState, "unifi-api-admin"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := hex.DecodeString(pkg.Secrets["unifi-api-admin"])
	if err != nil {
		t.Fatal(err)
	}
	// The placeholder round-trips through the door's own key (aad = the name).
	idRaw, err := os.ReadFile(filepath.Join(runnerPackageDir(cpState, "unifi-api-admin"), "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var id struct {
		EncSecretHex string `json:"enc_secret_hex"`
	}
	if err := json.Unmarshal(idRaw, &id); err != nil {
		t.Fatal(err)
	}
	encKey, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := crypto.Open(encKey, []byte("unifi-api-admin"), blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "pending" {
		t.Fatalf("the empty door's placeholder = %q, want pending", plain)
	}
}

// TestDoorLink pins the per-door console URL: public https when the CP host
// is known, LAN IP:8080 otherwise.
func TestDoorLink(t *testing.T) {
	if got := doorLink(&Spec{CpHost: "cp.example.com", CpIP: "10.0.0.9"}, "unifi-api-admin"); got != "https://cp.example.com/runner/unifi-api-admin" {
		t.Fatalf("doorLink = %q", got)
	}
	if got := doorLink(&Spec{CpIP: "10.0.0.9"}, "unifi-api-admin"); got != "http://10.0.0.9:8080/runner/unifi-api-admin" {
		t.Fatalf("doorLink (no host) = %q", got)
	}
}

// TestProvisionRunnerSecretlessAdoptKeepsFilled pins the restart-only
// semantics: a re-provision NEVER re-seals (the tool is credential-blind) —
// the operator's filled credential survives the door's restart; only the
// console's rotate changes the sealed credential.
func TestProvisionRunnerSecretlessAdoptKeepsFilled(t *testing.T) {
	root := t.TempDir()
	cpState := filepath.Join(root, "control-plane")
	store, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: "unifi-api-admin", Kind: "unifi", Address: "https://unifi.local",
		Secret: []byte("old-cred"), RunnerDir: runnerPackageDir(cpState, "unifi-api-admin"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCapability("unifi-api-admin", state.CapabilityRecord{
		Kind: "unifi", Address: "https://unifi.local", Port: 8800, Rosters: []string{"ai"},
	}); err != nil {
		t.Fatal(err)
	}
	// The operator fills the door via the console (rotate).
	if _, err := provisioner.RotateSecret(store, "unifi-api-admin", []byte("filled-by-operator")); err != nil {
		t.Fatal(err)
	}
	filled, _ := store.GetSecret("unifi-api-admin")

	reg, _ := testRegistry(t)
	spec := &Spec{StateDir: filepath.Join(root, "agent-tools"), CpaName: "freehold", CpIP: "10.0.0.9"}
	fn := BuildProvisionRunner(spec, reg)
	_, _ = fn(args("unifi-api-admin", "unifi", "https://unifi.local", "ai"))
	// The flow writes through its own store handle; re-open (the disk is the
	// truth) before asserting.
	disk, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := disk.GetSecret("unifi-api-admin")
	if after.CiphertextHex != filled.CiphertextHex {
		t.Fatal("a re-provision must keep the operator's filled credential (only the console rotates)")
	}
}

// TestProvisionRunnerUnknownGranteeFailsFirst pins the fail-fast contract:
// an unknown grant_to name is resolved BEFORE any side effect — no package,
// no capability record, nothing left behind for a typo'd name.
func TestProvisionRunnerUnknownGranteeFailsFirst(t *testing.T) {
	root := t.TempDir()
	cpState := filepath.Join(root, "control-plane")
	reg, _ := testRegistry(t)
	spec := &Spec{StateDir: filepath.Join(root, "agent-tools"), CpaName: "freehold", CpIP: "10.0.0.9"}
	fn := BuildProvisionRunner(spec, reg)
	if _, err := fn(args("unifi-api-admin", "unifi", "https://unifi.local", "ai", "nobody")); err == nil ||
		!strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("unknown grantee must fail fast, got %v", err)
	}
	if _, err := os.Stat(runnerPackageDir(cpState, "unifi-api-admin")); !os.IsNotExist(err) {
		t.Fatal("no package may exist — the failure came before provisioning")
	}
	st, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetCapability("unifi-api-admin"); ok {
		t.Fatal("no capability record may exist — the failure came before provisioning")
	}
}

// TestDroppedRosters pins the re-provision roster diff: names the previous
// roster held that the new one drops are revoked.
func TestDroppedRosters(t *testing.T) {
	dropped := droppedRosters([]string{"ai", "network"}, []string{"ai", "deployer"})
	if len(dropped) != 1 || dropped[0] != "network" {
		t.Fatalf("dropped = %v", dropped)
	}
	if d := droppedRosters([]string{"ai"}, []string{"ai", "deployer"}); len(d) != 0 {
		t.Fatalf("a grown roster drops nothing: %v", d)
	}
	if d := droppedRosters(nil, []string{"ai"}); len(d) != 0 {
		t.Fatalf("a first provision drops nothing: %v", d)
	}
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
	spec := &Spec{CpHost: "cp.example.com", CpIP: "10.0.0.9"}
	if got := spec.NextCapabilityPort(store); got != 8800 {
		t.Fatalf("first dynamic port = %d, want 8800", got)
	}
	if err := store.InsertCapability("a-ssh-root", state.CapabilityRecord{Port: 8800}); err != nil {
		t.Fatal(err)
	}
	if got := spec.NextCapabilityPort(store); got != 8801 {
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
