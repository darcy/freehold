package provisioner

import (
	"strings"
	"testing"

	"freehold/contract/wire"
	"freehold/control-plane/state"
)

// TestProvisionReAddressRotatesTarget pins the re-adopt contract: a live
// runner re-provisioned against a ROTATED substrate keeps its identity +
// sealed credential and moves its target address (the state row + the
// package's targets entry, together); an unchanged address stays the reuse
// error; a revoked runner never re-addresses.
func TestProvisionReAddressRotatesTarget(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	req := func(addr string) *ProvisionRequest {
		return &ProvisionRequest{
			Name: "pve-ssh-root", Kind: "ssh", Address: addr,
			Secret: []byte("door-key"), RunnerDir: dir,
			Grants: []string{strings.Repeat("b", 64)},
		}
	}
	res, err := ProvisionRunner(store, req("root@10.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	first, ok := store.GetSecret("pve-ssh-root")
	if !ok || first.Address != "root@10.0.0.1" {
		t.Fatalf("provisioned secret row: %+v", first)
	}

	// Unchanged address: the caller's reuse semantics — refused, not no-op.
	if _, err := ProvisionRunner(store, req("root@10.0.0.1")); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unchanged re-provision: err %v, want already-exists", err)
	}

	// Rotated address: re-addressed in place — identity + ciphertext intact.
	res2, err := ProvisionRunner(store, req("root@10.0.0.2"))
	if err != nil {
		t.Fatal(err)
	}
	if res2.NostrPubkey != res.NostrPubkey || res2.EncPubkey != res.EncPubkey || res2.PackageDir != res.PackageDir {
		t.Fatalf("re-address changed the runner identity: %+v vs %+v", res, res2)
	}
	sec, ok := store.GetSecret("pve-ssh-root")
	if !ok || sec.Address != "root@10.0.0.2" || sec.CiphertextHex != first.CiphertextHex {
		t.Fatalf("re-addressed row: %+v (want address moved, ciphertext untouched)", sec)
	}
	pkg, err := wire.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Targets["pve-ssh-root"].Address != "root@10.0.0.2" {
		t.Fatalf("package target address = %q, want root@10.0.0.2", pkg.Targets["pve-ssh-root"].Address)
	}

	// A declared-kind swap cannot smuggle an empty address past the guard:
	// the re-address keys on the kind the runner actually IS.
	if _, err := ProvisionRunner(store, &ProvisionRequest{
		Name: "pve-ssh-root", Kind: "local", Address: "",
		Secret: []byte("x"), RunnerDir: dir,
	}); err == nil || !strings.Contains(err.Error(), "wipe the door's endpoint") {
		t.Fatalf("empty address via a local-declared kind: err %v, want refused", err)
	}

	// A capability door's record is the REBUILD source — its address moves
	// with the re-address, or the next build re-asserts the old one.
	cap := state.CapabilityRecord{Kind: "ssh", Address: "root@10.0.0.1", Port: 8811}
	if err := store.InsertCapability("pve-ssh-root", cap); err != nil {
		t.Fatal(err)
	}
	if _, err := ProvisionRunner(store, req("root@10.0.0.4")); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetCapability("pve-ssh-root"); got.Address != "root@10.0.0.4" {
		t.Fatalf("capability record address = %q, want root@10.0.0.4", got.Address)
	}

	// A revoked runner never re-addresses.
	if _, err := RevokeRunner(store, "pve-ssh-root"); err != nil {
		t.Fatal(err)
	}
	if _, err := ProvisionRunner(store, req("root@10.0.0.3")); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked re-provision: err %v, want revoked", err)
	}
}
