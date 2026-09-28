package cpbuild

import (
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/wire"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/state"
)

// revokeSpec is the hermetic build spec: an agent-tools state dir under the
// test's own root (consoleStateDir resolves it to the sibling control-plane dir)
// and no substrate/relay wiring, which is exactly the condition the flow uses
// to decide "freehold hosted this unit" — so the unit leg must report
// state-only, never a claimed stop.
func revokeSpec(t *testing.T, root string) *Spec {
	t.Helper()
	return &Spec{StateDir: filepath.Join(root, "agent-tools"), CpaName: "freehold", CpIP: "10.0.0.9"}
}

// seedDoor writes the state a real provision would have left: the capability
// record with its roster, which is what both the build's re-grant and every
// pod's coords feed read.
func seedDoor(t *testing.T, root, name, kind, addr string, rosters ...string) {
	t.Helper()
	cpState := filepath.Join(root, "control-plane")
	store, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCapability(name, state.CapabilityRecord{
		Kind: kind, Address: addr, Port: 8800,
		Rosters: append([]string(nil), rosters...), Origin: state.OriginAgent, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// reopen reads the disk through a fresh handle: the flow writes through its own
// store, so the on-disk truth is what a later build or the console would see.
func reopen(t *testing.T, root string) *state.StateStore {
	t.Helper()
	store, err := state.Open(filepath.Join(root, "control-plane"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestRevokeRunnerNameGuards pins the name-level refusals — they fire before any
// state is opened, so a nil registry proves it: a build-time capability runner
// and a per-zone DNS door are operator-owned and unreachable from the agent
// surface in either direction.
func TestRevokeRunnerNameGuards(t *testing.T) {
	fn := BuildRevokeRunner(&Spec{CpaName: "freehold"}, nil)
	for _, tc := range []struct{ label, name, want string }{
		{"bad name", "RTX_BOX", "kebab-case"},
		{"build-time door", "pve-ssh-root", "operator-owned"},
		{"dns door", "cloudflare-api-example-com", "reserved"},
	} {
		_, err := fn(agent.RetireArgs{Name: tc.name})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error %q, got %v", tc.label, tc.want, err)
		}
	}
}

// TestRevokeRunnerOwnershipGuards pins the take-away mirror of the provision
// side's ownership rules: no record (a build/console-provisioned runner) and an
// operator-origin record are both refused — an agent may only take back what the
// agent flow handed out.
func TestRevokeRunnerOwnershipGuards(t *testing.T) {
	root := t.TempDir()
	reg, _ := testRegistry(t)
	fn := BuildRevokeRunner(revokeSpec(t, root), reg)

	if _, err := fn(agent.RetireArgs{Name: "never-was-ssh-root"}); err == nil ||
		!strings.Contains(err.Error(), "no capability record") {
		t.Fatalf("a runner with no capability record must be refused, got %v", err)
	}

	cpState := filepath.Join(root, "control-plane")
	store, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCapability("ops-box-ssh-root", state.CapabilityRecord{
		Kind: "ssh", Address: "darcy@10.0.0.5", Port: 8801,
		Rosters: []string{"ai"}, Origin: state.OriginOperator,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fn(agent.RetireArgs{Name: "ops-box-ssh-root"}); err == nil ||
		!strings.Contains(err.Error(), "operator") {
		t.Fatalf("an operator-origin record must be refused, got %v", err)
	}
}

// TestRevokeRunnerSingleRemovalOfNonGranteeIsNoOp pins the guard that keeps a
// typo from taking a live door down: naming an agent that is not on the roster
// changes nothing at all — the door keeps its record, its roster, and stays out
// of the retirement guard.
func TestRevokeRunnerSingleRemovalOfNonGranteeIsNoOp(t *testing.T) {
	root := t.TempDir()
	seedDoor(t, root, "rtx-ssh-root", "ssh", "darcy@10.0.0.55", "ai")
	reg, _ := testRegistry(t)

	report, err := BuildRevokeRunner(revokeSpec(t, root), reg)(agent.RetireArgs{
		Name: "rtx-ssh-root", RevokeFrom: []string{"network"},
	})
	if err != nil {
		t.Fatalf("a non-grantee removal is a no-op, not an error: %v", err)
	}
	if !strings.Contains(report, "nothing to take") {
		t.Fatalf("the no-op must be reported as such: %s", report)
	}
	disk := reopen(t, root)
	rec, ok := disk.GetCapability("rtx-ssh-root")
	if !ok || len(rec.Rosters) != 1 || rec.Rosters[0] != "ai" {
		t.Fatalf("the roster must be untouched, got %+v (ok=%v)", rec, ok)
	}
	if _, retired := disk.GetRetired("rtx-ssh-root"); retired {
		t.Fatal("a no-op must not retire the door")
	}
}

// TestRevokeRunnerSingleRemovalEditsTheRecord is the rebuild-safety contract:
// the capability record's roster is what the next build re-grants and re-feeds
// into every pod's coords, so a removal that only wrote the relay would be
// silently reversed. The named agent must leave the record; the door and its
// remaining grantees must survive.
func TestRevokeRunnerSingleRemovalEditsTheRecord(t *testing.T) {
	root := t.TempDir()
	seedDoor(t, root, "rtx-ssh-root", "ssh", "darcy@10.0.0.55", "ai", "network")
	reg, _ := testRegistry(t)

	report, err := BuildRevokeRunner(revokeSpec(t, root), reg)(agent.RetireArgs{
		Name: "rtx-ssh-root", RevokeFrom: []string{"ai"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "removed from the roster of [ai]") {
		t.Fatalf("the report must name whose grant went: %s", report)
	}
	if !strings.Contains(report, "untouched by design") {
		t.Fatalf("a single removal must leave the door standing, said plainly: %s", report)
	}
	disk := reopen(t, root)
	rec, ok := disk.GetCapability("rtx-ssh-root")
	if !ok {
		t.Fatal("a from-the-roster removal must NOT drop the door")
	}
	if strings.Join(rec.Rosters, ",") != "network" {
		t.Fatalf("the removed agent must be out of the recorded roster, got %v", rec.Rosters)
	}
	if _, retired := disk.GetRetired("rtx-ssh-root"); retired {
		t.Fatal("a from-the-roster removal must not retire the door")
	}
}

// TestRevokeRunnerWholeRetireRemovesRecordAndGuardsName pins the retirement:
// the record leaves the table (so no build re-stages it and no agent's coords
// resolve it) and the guard note lands with its provenance and its last roster —
// the record of who the capability was taken away from. With no host executor
// wired the unit leg must say state-only rather than claim a stop.
func TestRevokeRunnerWholeRetireRemovesRecordAndGuardsName(t *testing.T) {
	root := t.TempDir()
	seedDoor(t, root, "rtx-ssh-root", "ssh", "darcy@10.0.0.55", "ai", "network")
	reg, _ := testRegistry(t)

	report, err := BuildRevokeRunner(revokeSpec(t, root), reg)(agent.RetireArgs{Name: "rtx-ssh-root"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "capability door rtx-ssh-root retired") {
		t.Fatalf("the report must name the retirement: %s", report)
	}
	if !strings.Contains(report, "state-only") {
		t.Fatalf("with no host executor the unit leg must be honest about being state-only: %s", report)
	}
	// Hermetic has no relay and no k3s, so the two legs needing them CANNOT be
	// verified here — pinning that they are marked so is the "never claim a
	// teardown you did not check" contract, stated where it can be checked.
	if !strings.Contains(report, "[UNVERIFIED] roster") || !strings.Contains(report, "[UNVERIFIED] coords") {
		t.Fatalf("unverifiable legs must be marked UNVERIFIED, not skipped: %s", report)
	}
	if !strings.Contains(report, "verdict:") || !strings.Contains(report, "did NOT verify") {
		t.Fatalf("the verdict must name what did not verify: %s", report)
	}
	// A revocation the auditor can no longer follow is not a revocation: the
	// retire folds the runner off its own roster but KEEPS the NIP-29 channel so
	// the roster history and the door's kind-48001 audit stream stay queryable.
	if !strings.Contains(report, "stays live and read-only") {
		t.Fatalf("the report must promise the audit channel survives the retire: %s", report)
	}
	disk := reopen(t, root)
	if _, ok := disk.GetCapability("rtx-ssh-root"); ok {
		t.Fatal("the capability record must be gone, or a build re-stages the door")
	}
	note, retired := disk.GetRetired("rtx-ssh-root")
	if !retired {
		t.Fatal("the retirement guard note must be recorded")
	}
	if note.RetiredBy != state.OriginAgent || note.RevokedAt == 0 {
		t.Fatalf("the note must carry who/when: %+v", note)
	}
	if strings.Join(note.LastRoster, ",") != "ai,network" {
		t.Fatalf("the note must record the roster it took away: %+v", note.LastRoster)
	}
}

// TestRevokeRunnerRetiredNameIsRefusedToBothFlows pins the guard's whole point:
// the half that takes capability away and the half that hands it out must not be
// the same caller's two hands. Both the agent's revoke and the agent's provision
// of the retired name are refused, and only the operator's console-side record
// write (InsertCapability) re-enables it.
func TestRevokeRunnerRetiredNameIsRefusedToBothFlows(t *testing.T) {
	root := t.TempDir()
	seedDoor(t, root, "rtx-ssh-root", "ssh", "darcy@10.0.0.55", "ai")
	spec := revokeSpec(t, root)
	reg, _ := testRegistry(t)

	if _, err := BuildRevokeRunner(spec, reg)(agent.RetireArgs{Name: "rtx-ssh-root"}); err != nil {
		t.Fatal(err)
	}
	// The take-away is idempotent to a refusal, not a repeat of the teardown.
	if _, err := BuildRevokeRunner(spec, reg)(agent.RetireArgs{Name: "rtx-ssh-root"}); err == nil ||
		!strings.Contains(err.Error(), "already retired") {
		t.Fatalf("a second revoke of a retired door must be refused, got %v", err)
	}
	// The re-mint hole: the same agent may not simply provision the name back.
	if _, err := BuildProvisionRunner(spec, reg)(agent.ProvisionArgs{
		Name: "rtx-ssh-root", Kind: "ssh", Address: "darcy@10.0.0.55", GrantTo: []string{"ai"},
	}); err == nil || !strings.Contains(err.Error(), "may not re-mint") {
		t.Fatalf("a retired name must be refused to provision_runner, got %v", err)
	}
	// The operator's console-side re-provision clears the guard (the record write
	// is the re-enable verb).
	disk := reopen(t, root)
	if err := disk.InsertCapability("rtx-ssh-root", state.CapabilityRecord{
		Kind: "ssh", Address: "darcy@10.0.0.55", Port: 8800, Rosters: []string{"ai"}, Origin: state.OriginOperator,
	}); err != nil {
		t.Fatal(err)
	}
	if _, retired := disk.GetRetired("rtx-ssh-root"); retired {
		t.Fatal("the operator's record write must re-enable the name")
	}
}

// TestRevokeRunnerWholeRetireErasesTheCredential is the reason the retire goes
// through the audited revoke verb rather than a bare state edit: "revoked" as a
// status is a label, whereas a CP that still holds the sealed package can still
// serve the credential. So the proof is the file — sealed here, and required to
// be unreadable afterwards, both in the report and on disk.
func TestRevokeRunnerWholeRetireErasesTheCredential(t *testing.T) {
	root := t.TempDir()
	seedDoor(t, root, "rtx-ssh-root", "ssh", "darcy@10.0.0.55", "ai")

	cpState := filepath.Join(root, "control-plane")
	pkgDir := runnerPackageDir(cpState, "rtx-ssh-root")
	pkg := wire.New(map[string]string{"rtx-ssh-root": "deadbeef"}, nil, []string{"ai"})
	if err := pkg.WriteToDir(pkgDir); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	store.InsertRunner("rtx-ssh-root", state.RunnerRecord{
		NostrPubkey: strings.Repeat("a", 64), EncPubkey: strings.Repeat("b", 64),
		Status: state.RunnerActive, PackageDir: pkgDir, CreatedAt: 1,
	})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	reg, _ := testRegistry(t)
	report, err := BuildRevokeRunner(revokeSpec(t, root), reg)(agent.RetireArgs{Name: "rtx-ssh-root"})
	if err != nil {
		t.Fatal(err)
	}
	// "is gone", not the other credential branch's "no sealed package exists":
	// that one is what a never-provisioned door says, and it would let this test
	// pass with the erasure path never having run.
	if !strings.Contains(report, "[verified] credential") || !strings.Contains(report, "is gone") ||
		strings.Contains(report, "no sealed package exists") {
		t.Fatalf("the credential leg must report an erasure it verified: %s", report)
	}
	// The state says revoked AND the sealed bytes are gone: a door that is
	// "revoked" while its package still loads is the failure this guards.
	disk := reopen(t, root)
	rec, ok := disk.GetRunner("rtx-ssh-root")
	if !ok || rec.Status != state.RunnerRevoked {
		t.Fatalf("the runner row must be revoked, got %+v (ok=%v)", rec, ok)
	}
	if _, err := wire.Load(pkgDir); err == nil {
		t.Fatal("the sealed package survived the retirement — the CP can still serve the credential")
	}
}

// TestRevokeRunnerKeepsThePodsOtherDoors pins the difference between taking one
// door away and disabling an agent: the pod re-apply rebuilds the department's
// WHOLE FREEHOLD_RUNNER_* feed from this map, so the cut must be resolved from
// state rather than replacing it with only what this call touched. Otherwise
// revoking one door leaves the department with no exec at all.
func TestRevokeRunnerKeepsThePodsOtherDoors(t *testing.T) {
	root := t.TempDir()
	seedDoor(t, root, "rtx-ssh-root", "ssh", "darcy@10.0.0.55", "ai")
	seedDoor(t, root, "unifi-api-admin", "unifi", "http://10.0.0.1", "ai")
	cpState := filepath.Join(root, "control-plane")
	store, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	// Both doors must be LIVE in state (a runner row carrying its identity) or
	// agentRunnerCoords resolves neither and the feed would be empty for the
	// wrong reason, proving nothing.
	store.InsertRunner("rtx-ssh-root", state.RunnerRecord{
		NostrPubkey: strings.Repeat("a", 64), EncPubkey: strings.Repeat("b", 64),
		Status: state.RunnerActive, PackageDir: runnerPackageDir(cpState, "rtx-ssh-root"), CreatedAt: 1,
	})
	store.InsertRunner("unifi-api-admin", state.RunnerRecord{
		NostrPubkey: strings.Repeat("c", 64), EncPubkey: strings.Repeat("d", 64),
		Status: state.RunnerActive, PackageDir: runnerPackageDir(cpState, "unifi-api-admin"), CreatedAt: 1,
	})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	spec := revokeSpec(t, root)
	reg, _ := testRegistry(t)
	if _, err := BuildRevokeRunner(spec, reg)(agent.RetireArgs{Name: "rtx-ssh-root"}); err != nil {
		t.Fatal(err)
	}

	got := []string{}
	for _, c := range spec.DepartmentRunners["ai"] {
		got = append(got, c.Target)
	}
	if strings.Join(got, ",") != "unifi-api-admin" {
		t.Fatalf("the revoked door must be cut while its sibling survives, got [%s]", strings.Join(got, ", "))
	}
}
