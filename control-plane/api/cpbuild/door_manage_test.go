package cpbuild

import (
	"path/filepath"
	"strings"
	"testing"

	"freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

// fillSpec is the hermetic spec: an agent-tools state dir under the test root
// and no relay wiring — exactly the condition door_package's roster read uses
// to fail closed.
func fillSpec(t *testing.T, root string) *Spec {
	t.Helper()
	return &Spec{StateDir: filepath.Join(root, "agent-tools"), CpaName: "freehold", CpIP: "10.0.0.9"}
}

const (
	testNostrPK = "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	testEncPK   = "bb66bb66bb66bb66bb66bb66bb66bb66bb66bb66bb66bb66bb66bb66bb66bb66"
	testCaller  = "cc77cc77cc77cc77cc77cc77cc77cc77cc77cc77cc77cc77cc77cc77cc77cc77"
)

// seedNoSecretDoor seeds an agent-provisioned, declared no-secret door's
// record — the shape provision_runner leaves behind it.
func seedNoSecretDoor(t *testing.T, root, name string, hosted string) {
	t.Helper()
	cpState := filepath.Join(root, "control-plane")
	store, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	rec := state.CapabilityRecord{
		Kind: "api", Address: "http://192.168.30.254:1255", Port: 8800,
		Rosters: []string{"compute"}, Origin: state.OriginAgent,
		NoSecret: true, CreatedAt: 1,
	}
	if hosted == state.HostedSelf {
		rec.Kind = "local"
		rec.Hosted = hosted
		rec.Host = "audio-box"
	}
	if err := store.InsertCapability(name, rec); err != nil {
		t.Fatal(err)
	}
}

// TestFillDoorGuards pins the door-side refusals: the declaration is what
// makes a door agent-sealable, and nothing else — an undeclared door, an
// operator-provisioned one, and the keypair/token kinds are all refused
// before any seal happens.
func TestFillDoorGuards(t *testing.T) {
	root := t.TempDir()
	fn := BuildFillDoor(fillSpec(t, root))

	for _, tc := range []struct{ label, name, want string }{
		{"bad name", "AUDIO_BOX", "kebab-case"},
		{"no record", "never-was-api", "no capability record"},
	} {
		if _, err := fn(tc.name, "http://x"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want %q, got %v", tc.label, tc.want, err)
		}
	}

	cpState := filepath.Join(root, "control-plane")
	store, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	// Operator provenance: agent-unfillable, declaration or not.
	if err := store.InsertCapability("op-api-door", state.CapabilityRecord{
		Kind: "api", Address: "http://x", Port: 8801, Origin: state.OriginOperator, CreatedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fn("op-api-door", "http://x"); err == nil || !strings.Contains(err.Error(), "operator") {
		t.Fatalf("an operator-provisioned door must be refused, got %v", err)
	}
	// Undeclared door: the refusal names the new-door path, not a judgment call.
	if err := store.InsertCapability("secret-api-door", state.CapabilityRecord{
		Kind: "api", Address: "http://x", Port: 8802, Rosters: []string{"compute"},
		Origin: state.OriginAgent, CreatedAt: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fn("secret-api-door", "http://x"); err == nil || !strings.Contains(err.Error(), "not declared no_secret") {
		t.Fatalf("an undeclared door must be refused, got %v", err)
	}
	// ssh and kube carry CP-minted/CP-sourced credentials — never agent-sealable.
	for kind, want := range map[string]string{"ssh": "nothing to fill", "kubernetes": "never agent-sealable"} {
		name := kind + "-door"
		if err := store.InsertCapability(name, state.CapabilityRecord{
			Kind: kind, Address: "x", Port: 8803, Rosters: []string{"compute"},
			Origin: state.OriginAgent, NoSecret: true, CreatedAt: 4,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := fn(name, "x"); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s door must be refused with %q, got %v", kind, want, err)
		}
	}
}

// TestFillDoorSeals pins the seal itself: a declared no-secret door's value
// rides the SAME provisioner path the console uses (the record's ciphertext
// updates, RotatedAt lands), and a resident door's report names door_package
// instead of any operator step. The CP-guest restart fails hermetically and
// is reported soft — the seal stands.
func TestFillDoorSeals(t *testing.T) {
	root := t.TempDir()
	fn := BuildFillDoor(fillSpec(t, root))
	cpState := filepath.Join(root, "control-plane")

	seedNoSecretDoor(t, root, "marantz-api-config", "")
	store, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: "marantz-api-config", Kind: "api", Address: "http://192.168.30.254:1255",
		Secret: []byte("pending"), RunnerDir: cpState + "/runner/marantz-api-config",
		Probe: "GET /get/DeviceReady none",
	}); err != nil {
		t.Fatal(err)
	}
	report, err := fn("marantz-api-config", "192.168.30.254:1255")
	if err != nil {
		t.Fatalf("fill_door on a declared no-secret door must seal: %v", err)
	}
	if !strings.Contains(report, "no-secret") {
		t.Fatalf("the report should name the declaration, got %q", report)
	}
	fresh, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := fresh.GetSecret("marantz-api-config")
	if rec.RotatedAt == nil {
		t.Fatal("the seal did not land (RotatedAt unset)")
	}
	if rec.CiphertextHex == "" {
		t.Fatal("no ciphertext recorded")
	}

	// A resident door: the report hands the package to the GRANTEE via
	// door_package — the operator is out of the chain.
	seedNoSecretDoor(t, root, "audio-box-local", state.HostedSelf)
	store2, _ := state.Open(cpState)
	if _, err := provisioner.EnrollRunner(store2, "audio-box-local", "local",
		"audio-box:1255", testNostrPK, testEncPK, "audio-box:8804"); err != nil {
		t.Fatal(err)
	}
	// The confirm barrier lives with the operation: without it the fill refuses.
	if _, err := fn("audio-box-local", "audio-box:1255"); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("an unconfirmed resident fill must be refused, got %v", err)
	}
	capRec, _ := store2.GetCapability("audio-box-local")
	capRec.EnrollConfirmedAt = &[]uint64{7}[0]
	if err := store2.InsertCapability("audio-box-local", capRec); err != nil {
		t.Fatal(err)
	}
	report, err = fn("audio-box-local", "audio-box:1255")
	if err != nil {
		t.Fatalf("a confirmed resident fill must seal: %v", err)
	}
	if !strings.Contains(report, "door_package") {
		t.Fatalf("the resident report must name the grantee's fetch, got %q", report)
	}
}

// TestDoorPackageGuards pins the fetch's fail-closed shape: only
// agent-provisioned SELF-HOSTED doors with a sealed credential are fetchable,
// and without relay wiring the roster read cannot happen — the fetch refuses
// rather than shipping a package past an unchecked gate.
func TestDoorPackageGuards(t *testing.T) {
	root := t.TempDir()
	fn := BuildDoorPackage(fillSpec(t, root))
	cpState := filepath.Join(root, "control-plane")

	if _, err := fn("never-was", testCaller); err == nil || !strings.Contains(err.Error(), "no capability record") {
		t.Fatalf("an unknown door must be refused, got %v", err)
	}

	seedNoSecretDoor(t, root, "audio-box-local", state.HostedSelf)
	store, err := state.Open(cpState)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.EnrollRunner(store, "audio-box-local", "local",
		"audio-box:1255", testNostrPK, testEncPK, "audio-box:8804"); err != nil {
		t.Fatal(err)
	}
	// Not sealed yet: the pending placeholder is not a package anyone should
	// ship — the refusal names the fill that must come first.
	if _, err := fn("audio-box-local", testCaller); err == nil || !strings.Contains(err.Error(), "not sealed yet") {
		t.Fatalf("an unsealed door must be refused, got %v", err)
	}
	// Seal it (the CPA's own fill path), then the roster read fails closed
	// (no relay wiring in the hermetic spec).
	fill := BuildFillDoor(fillSpec(t, root))
	capRec, _ := store.GetCapability("audio-box-local")
	capRec.EnrollConfirmedAt = &[]uint64{7}[0]
	if err := store.InsertCapability("audio-box-local", capRec); err != nil {
		t.Fatal(err)
	}
	if _, err := fill("audio-box-local", "audio-box:1255"); err != nil {
		t.Fatal(err)
	}
	if _, err := fn("audio-box-local", testCaller); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("a fetch with an unreadable roster must fail closed, got %v", err)
	}

	// A CP-guest door: the CP ships its package itself — nothing to fetch.
	if err := store.InsertCapability("guest-api-door", state.CapabilityRecord{
		Kind: "api", Address: "http://x", Port: 8805, Rosters: []string{"compute"},
		Origin: state.OriginAgent, NoSecret: true, CreatedAt: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fn("guest-api-door", testCaller); err == nil || !strings.Contains(err.Error(), "nothing to fetch") {
		t.Fatalf("a CP-guest door must be refused, got %v", err)
	}
}
