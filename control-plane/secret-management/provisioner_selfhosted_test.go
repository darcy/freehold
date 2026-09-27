package provisioner

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/state"
)

// TestEnrollRunnerPinsPresentedKeys pins the self-hosted enroll contract: the
// CP records the PRESENTED pubkeys verbatim (no keypair minted, no package
// dir), seals a "pending" placeholder TO the presented key, and marks the
// record active with the local risk default.
func TestEnrollRunnerPinsPresentedKeys(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	encSecret := []byte(strings.Repeat("k", 32))
	encPub, err := crypto.X25519PublicKey(encSecret)
	if err != nil {
		t.Fatal(err)
	}
	nostrPK, encPK := strings.Repeat("a", 64), hex.EncodeToString(encPub)
	rec, err := EnrollRunner(store, "dev-local-lxcadmin", "local", "lxcadmin@192.168.30.50",
		nostrPK, encPK, "192.168.30.50:8800")
	if err != nil {
		t.Fatal(err)
	}
	if rec.NostrPubkey != nostrPK || rec.EncPubkey != encPK || rec.PackageDir != "" ||
		rec.McpAddr == nil || *rec.McpAddr != "192.168.30.50:8800" || rec.Status != state.RunnerActive {
		t.Fatalf("enrolled record: %+v", rec)
	}
	if rec.RiskLevel == nil || *rec.RiskLevel != "safe" {
		t.Fatalf("local default risk = %v, want safe", rec.RiskLevel)
	}
	// The placeholder round-trips through the PRESENTED key only.
	sec, ok := store.GetSecret("dev-local-lxcadmin")
	if !ok || sec.CiphertextHex == "" {
		t.Fatal("enroll must seal its placeholder")
	}
	blob, err := hex.DecodeString(sec.CiphertextHex)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := crypto.Open(encSecret, []byte("dev-local-lxcadmin"), blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "pending" {
		t.Fatalf("placeholder = %q, want pending", plain)
	}

	// A second enroll under the same name is refused (new capability = NEW
	// runner), and a revoked one never re-enrolls.
	if _, err := EnrollRunner(store, "dev-local-lxcadmin", "local", "x", nostrPK, encPK, ""); err == nil {
		t.Fatal("a second enroll under one name must be refused")
	}
}

// TestRotateSecretSelfHostedRoundTrip pins the fill flow for a self-hosted
// runner: rotate seals the NEW credential to the presented key and returns
// the package JSON (no CP-side package dir write); the guest decrypts it with
// its own key. A CP-guest runner refuses the self-hosted rotate (the console
// re-ships its package instead).
func TestRotateSecretSelfHostedRoundTrip(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	encSecret := []byte(strings.Repeat("j", 32))
	encPub, err := crypto.X25519PublicKey(encSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnrollRunner(store, "dev-local-lxcadmin", "local", "lxcadmin@h",
		strings.Repeat("a", 64), hex.EncodeToString(encPub), "192.168.30.50:8800"); err != nil {
		t.Fatal(err)
	}
	// The confirm gate lives in the OPERATION: the fill is refused until the
	// operator confirmed the presented pubkeys (the door page) — then it seals.
	if _, _, err := RotateSecretSelfHosted(store, "dev-local-lxcadmin", []byte("git-deploy-key")); err == nil ||
		!strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("an unconfirmed self-hosted fill must be refused, got %v", err)
	}
	if err := store.InsertCapability("dev-local-lxcadmin", state.CapabilityRecord{
		Kind: "local", Address: "lxcadmin@h", Rosters: []string{"deployer"},
		Hosted: state.HostedSelf, Host: "192.168.30.50",
		EnrollConfirmedAt: &[]uint64{7}[0],
	}); err != nil {
		t.Fatal(err)
	}
	rec, pkgJSON, err := RotateSecretSelfHosted(store, "dev-local-lxcadmin", []byte("git-deploy-key"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.CiphertextHex == "" || rec.RotatedAt == nil {
		t.Fatalf("rotated record: %+v", rec)
	}
	// The returned package is the exact file the guest writes beside its
	// identity.json — serde-compatible shape, decryptable with the
	// presented key.
	var pkg wire.SecretPackage
	if err := json.Unmarshal(pkgJSON, &pkg); err != nil {
		t.Fatal(err)
	}
	blob, err := hex.DecodeString(pkg.Secrets["dev-local-lxcadmin"])
	if err != nil {
		t.Fatal(err)
	}
	plain, err := crypto.Open(encSecret, []byte("dev-local-lxcadmin"), blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "git-deploy-key" {
		t.Fatalf("decrypted %q, want git-deploy-key", plain)
	}
	if pkg.Targets["dev-local-lxcadmin"].Kind != "local" {
		t.Fatalf("package target meta: %+v", pkg.Targets)
	}

	// A CP-guest runner (a real package dir) refuses the self-hosted rotate.
	if _, err := ProvisionRunner(store, &ProvisionRequest{
		Name: "ops-unifi", Kind: "unifi", Address: "https://u",
		Secret: []byte("k"), RunnerDir: store.Dir() + "/runner/ops-unifi",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RotateSecretSelfHosted(store, "ops-unifi", []byte("x")); err == nil ||
		!strings.Contains(err.Error(), "not self-hosted") {
		t.Fatalf("CP-guest rotate must refuse the self-hosted path, got %v", err)
	}
}
