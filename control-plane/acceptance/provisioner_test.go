package acceptance

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/crypto"
	"freehold/contract/relay"
	"freehold/contract/state"
	"freehold/contract/wire"
	provisioner "freehold/control-plane/secret-management"
)

// provision is the shared B1 happy-path helper.
func provision(t *testing.T, store *state.StateStore, name string, secret []byte, runnerDir string) *provisioner.ProvisionResult {
	t.Helper()
	res, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: name, Kind: "vultr", Address: "api.vultr.com",
		Secret: secret, RunnerDir: runnerDir,
	})
	if err != nil {
		t.Fatalf("provision %s: %v", name, err)
	}
	return res
}

// openSealed opens a package entry under its own map key (aad = name).
func openSealed(t *testing.T, dir, name string) string {
	t.Helper()
	_, encSecret := readRunnerIdentity(t, dir)
	pkg, err := wire.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := hex.DecodeString(pkg.Secrets[name])
	if err != nil {
		t.Fatal(err)
	}
	got, err := crypto.Open(encSecret, []byte(name), blob)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	return string(got)
}

func TestProvisionShipsCiphertextOnly(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	runnerDir := filepath.Join(base, "runner")
	secret := []byte("vultr-api-key-9876")

	res, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: "vultr", Kind: "vultr", Address: "api.vultr.com",
		Secret: secret, RunnerDir: runnerDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(runnerDir, "identity.json")); err != nil {
		t.Fatal("identity.json must ship")
	}
	if _, err := os.Stat(filepath.Join(runnerDir, wire.SECRETS_FILE)); err != nil {
		t.Fatal("secrets.json must ship")
	}
	pkg, err := wire.Load(runnerDir)
	if err != nil {
		t.Fatal(err)
	}
	meta := pkg.Targets["vultr"]
	if meta.Kind != "vultr" || meta.Address != "api.vultr.com" || meta.Secret != "vultr" {
		t.Fatalf("target meta: %+v", meta)
	}
	if strings.Contains(mustRead(t, filepath.Join(runnerDir, wire.SECRETS_FILE)), string(secret)) {
		t.Fatal("plaintext must never land in the runner package")
	}

	stateRaw := mustRead(t, filepath.Join(base, "state", "state.json"))
	if strings.Contains(stateRaw, string(secret)) {
		t.Fatal("plaintext must never land in CP state")
	}
	for _, key := range []string{"nostr_secret", "enc_secret", "private", "secret_hex"} {
		if strings.Contains(stateRaw, key) {
			t.Fatalf("CP state must not serialize private keys (%s)", key)
		}
	}
	if !strings.Contains(stateRaw, res.NostrPubkey) || !strings.Contains(stateRaw, res.EncPubkey) {
		t.Fatal("pubkeys are expected in state")
	}

	// The runner opens its own sealed secret; a different key cannot.
	if got := openSealed(t, runnerDir, "vultr"); got != string(secret) {
		t.Fatalf("runner opens its own secret, got %q", got)
	}
	other := make([]byte, 32)
	for i := range other {
		other[i] = 0x99
	}
	if _, err := crypto.Open(other, []byte("vultr"), mustDecodeHex(t, pkg.Secrets["vultr"])); err == nil {
		t.Fatal("a different key must not open the blob")
	}
}

func TestRotateReencryptsAndReplacesEverywhere(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	runnerDir := filepath.Join(base, "runner")
	provision(t, store, "b2", []byte("old-key-value"), runnerDir)

	before, _ := store.GetSecret("b2")
	if _, err := provisioner.RotateSecret(store, "b2", []byte("new-key-value")); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetSecret("b2")
	if before.CiphertextHex == after.CiphertextHex {
		t.Fatal("rotation must produce fresh ciphertext")
	}
	if after.RotatedAt == nil {
		t.Fatal("rotation must set rotated_at")
	}
	raw := mustRead(t, filepath.Join(runnerDir, wire.SECRETS_FILE))
	if strings.Contains(raw, "old-key-value") || strings.Contains(raw, "new-key-value") {
		t.Fatal("plaintext must never be echoed")
	}
	if got := openSealed(t, runnerDir, "b2"); got != "new-key-value" {
		t.Fatalf("runner opens the rotated value, got %q", got)
	}
}

func TestRevokedRunnerCannotRotateOrReprovision(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	runnerDir := filepath.Join(base, "runner")
	provision(t, store, "vultr", []byte("key"), runnerDir)

	if _, err := provisioner.RevokeRunner(store, "vultr"); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.GetRunner("vultr")
	if rec.Status != state.RunnerRevoked {
		t.Fatalf("status = %s", rec.Status)
	}
	if len(rec.NostrPubkey) != 64 {
		t.Fatal("revocation keeps the record (pubkey still listed)")
	}
	if _, err := provisioner.RotateSecret(store, "vultr", []byte("whatever")); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("rotate revoked runner should fail, got %v", err)
	}
	if _, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: "vultr", Kind: "vultr", Address: "x", Secret: []byte("x"), RunnerDir: runnerDir,
	}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("reprovision revoked runner should fail, got %v", err)
	}
}

func TestDuplicateProvisionRejected(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	runnerDir := filepath.Join(base, "runner")
	provision(t, store, "vultr", []byte("key"), runnerDir)
	_, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: "vultr", Kind: "vultr", Address: "x", Secret: []byte("x"), RunnerDir: runnerDir,
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("got %v", err)
	}
}

func TestSwappedPackageEntriesRejected(t *testing.T) {
	base := t.TempDir()
	runnerDir := filepath.Join(base, "runner")
	encSecret := make([]byte, 32)
	for i := range encSecret {
		encSecret[i] = byte(i + 1)
	}
	encPub, err := crypto.X25519PublicKey(encSecret)
	if err != nil {
		t.Fatal(err)
	}
	ctA, err := crypto.Seal(encPub, []byte("a"), []byte("cred-a"))
	if err != nil {
		t.Fatal(err)
	}
	ctB, err := crypto.Seal(encPub, []byte("b"), []byte("cred-b"))
	if err != nil {
		t.Fatal(err)
	}
	pkg := wire.New(
		map[string]string{"a": hex.EncodeToString(ctA), "b": hex.EncodeToString(ctB)},
		map[string]wire.TargetMeta{}, nil,
	)
	if err := pkg.WriteToDir(runnerDir); err != nil {
		t.Fatal(err)
	}
	loaded, err := wire.Load(runnerDir)
	if err != nil {
		t.Fatal(err)
	}
	blobA := mustDecodeHex(t, loaded.Secrets["a"])
	blobB := mustDecodeHex(t, loaded.Secrets["b"])
	if got, _ := crypto.Open(encSecret, []byte("a"), blobA); string(got) != "cred-a" {
		t.Fatalf("a -> %q", got)
	}
	if got, _ := crypto.Open(encSecret, []byte("b"), blobB); string(got) != "cred-b" {
		t.Fatalf("b -> %q", got)
	}
	if _, err := crypto.Open(encSecret, []byte("b"), blobA); err == nil {
		t.Fatal("swapped entry must fail")
	}
	if _, err := crypto.Open(encSecret, []byte("a"), blobB); err == nil {
		t.Fatal("swapped entry must fail")
	}
}

func TestPackageDirInUseRefused(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	shared := filepath.Join(base, "shared")
	provision(t, store, "a", []byte("key-a"), shared)
	_, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: "b", Kind: "vultr", Address: "x", Secret: []byte("key-b"), RunnerDir: shared,
	})
	if err == nil || !strings.Contains(err.Error(), "already holds a runner") {
		t.Fatalf("got %v", err)
	}
	if got := openSealed(t, shared, "a"); got != "key-a" {
		t.Fatalf("a's package must stay decryptable, got %q", got)
	}
}

func TestRevokeRemovesShippedCredential(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	runnerDir := filepath.Join(base, "runner")
	provision(t, store, "ssh", []byte("key"), runnerDir)

	if _, err := provisioner.RevokeRunner(store, "ssh"); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.GetRunner("ssh")
	if rec.Status != state.RunnerRevoked {
		t.Fatalf("status = %s", rec.Status)
	}
	if _, err := os.Stat(filepath.Join(runnerDir, wire.SECRETS_FILE)); !os.IsNotExist(err) {
		t.Fatal("revoke must remove the shipped credential")
	}
	if _, err := os.Stat(filepath.Join(runnerDir, "identity.json")); err != nil {
		t.Fatal("the runner's own identity stays")
	}
}

func TestRevokeSaveFailureRestoresPriorStatus(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores read-only dir perms")
	}
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	store := openStore(t, stateDir)
	runnerDir := filepath.Join(base, "runner")
	provision(t, store, "ssh", []byte("key"), runnerDir)

	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.RevokeRunner(store, "ssh"); err == nil {
		t.Fatal("revoke must fail when state cannot be saved")
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.GetRunner("ssh")
	if rec.Status != state.RunnerActive {
		t.Fatalf("memory must revert to active, got %s", rec.Status)
	}
	reopened := openStore(t, stateDir)
	rec2, _ := reopened.GetRunner("ssh")
	if rec2.Status != state.RunnerActive {
		t.Fatalf("disk must stay active, got %s", rec2.Status)
	}
}

func TestRevokeIsIdempotentAndNeverGrants(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	runnerDir := filepath.Join(base, "runner")
	provision(t, store, "ssh", []byte("key"), runnerDir)
	if _, err := provisioner.RevokeRunner(store, "ssh"); err != nil {
		t.Fatal(err)
	}
	// A reappeared secrets.json must be cleaned by re-revoking.
	if err := wire.New(nil, nil, nil).WriteToDir(runnerDir); err != nil {
		t.Fatal(err)
	}
	rec, err := provisioner.RevokeRunner(store, "ssh")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != state.RunnerRevoked {
		t.Fatalf("status = %s", rec.Status)
	}
	if _, err := os.Stat(filepath.Join(runnerDir, wire.SECRETS_FILE)); !os.IsNotExist(err) {
		t.Fatal("re-revoke must clean a reappeared secrets.json")
	}
	if _, err := provisioner.RotateSecret(store, "ssh", []byte("x")); err == nil {
		t.Fatal("still revoked")
	}
}

func TestInvalidNamesRejected(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	runnerDir := filepath.Join(base, "runner")
	for _, bad := range []string{"", "../x", "a/b", ".hidden"} {
		_, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
			Name: bad, Kind: "vultr", Address: "x", Secret: []byte("x"), RunnerDir: runnerDir,
		})
		if err == nil || !strings.Contains(err.Error(), "invalid runner name") {
			t.Fatalf("%q -> %v", bad, err)
		}
	}
}

func TestUnknownSecretRotateFails(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	if _, err := provisioner.RotateSecret(store, "nope", []byte("x")); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
}

func TestProvisionShipsGrantsAndGrantAddsLive(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	runnerDir := filepath.Join(base, "runner")
	agentA := hex64('a')
	agentB := hex64('b')
	if _, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: "vultr", Kind: "vultr", Address: "api.vultr.com",
		Secret: []byte("key"), RunnerDir: runnerDir, Grants: []string{agentA},
	}); err != nil {
		t.Fatal(err)
	}
	if pkg, _ := wire.Load(runnerDir); len(pkg.Grants) != 1 || pkg.Grants[0] != agentA {
		t.Fatalf("shipped grants: %v", pkg.Grants)
	}
	grants, err := provisioner.GrantAgent(store, "vultr", agentB)
	if err != nil || len(grants) != 2 {
		t.Fatalf("grant: %v %v", grants, err)
	}
	if _, err := provisioner.GrantAgent(store, "vultr", agentB); err != nil {
		t.Fatal(err)
	}
	if pkg, _ := wire.Load(runnerDir); len(pkg.Grants) != 2 || !containsStr(pkg.Grants, agentB) {
		t.Fatalf("re-shipped grants: %v", pkg.Grants)
	}
	if _, err := provisioner.RotateSecret(store, "vultr", []byte("new-key")); err != nil {
		t.Fatal(err)
	}
	if pkg, _ := wire.Load(runnerDir); len(pkg.Grants) != 2 {
		t.Fatalf("rotate must preserve grants, got %v", pkg.Grants)
	}
}

func TestInvalidGrantPubkeysRejected(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	runnerDir := filepath.Join(base, "runner")
	provision(t, store, "vultr", []byte("key"), runnerDir)
	if _, err := provisioner.GrantAgent(store, "vultr", "not-hex"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("got %v", err)
	}
}

func TestStatePersistsAcrossReopen(t *testing.T) {
	base := t.TempDir()
	store := openStore(t, filepath.Join(base, "state"))
	provision(t, store, "ssh", []byte("key"), filepath.Join(base, "runner"))

	reopened := openStore(t, filepath.Join(base, "state"))
	snap := reopened.Snapshot()
	if _, ok := snap.Runners["ssh"]; !ok {
		t.Fatal("runner must survive reopen")
	}
	if snap.Runners["ssh"].Status != state.RunnerActive {
		t.Fatalf("status = %s", snap.Runners["ssh"].Status)
	}
	orig, _ := store.GetSecret("ssh")
	if snap.Secrets["ssh"].CiphertextHex != orig.CiphertextHex {
		t.Fatal("ciphertext must survive reopen")
	}
}

func TestGrantAndRevokeAreChannelMembershipCommands(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)
	runnerDir := filepath.Join(base, "runner")
	provision(t, store, "relaybox", []byte("sekrit"), runnerDir)
	rec, _ := store.GetRunner("relaybox")

	relayURL, relayState := spawnRelay(t)
	consoleSecret, _ := ensureConsoleIdentity(t, cpDir)

	a := "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"
	b := "2221222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"

	if err := provisioner.SyncRunnerChannel(store, relayURL, "relaybox", cpDir); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.GrantAgent(store, "relaybox", a); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.GrantAgent(store, "relaybox", b); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.PutUserMembership(store, relayURL, "relaybox", a, cpDir); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.PutUserMembership(store, relayURL, "relaybox", b, cpDir); err != nil {
		t.Fatal(err)
	}

	puts := 0
	for _, e := range relayState.Events() {
		if e.Kind == wire.PutUser {
			puts++
		}
	}
	if puts != 3 { // the runner + a + b
		t.Fatalf("expected 3 kind-9000 puts, got %d", puts)
	}

	roster, err := relay.QueryChannelRoster(relayURL, RelayPubkey(), rec.NostrPubkey, consoleSecret)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{a, b, rec.NostrPubkey} {
		if !containsStr(roster, want) {
			t.Fatalf("roster missing %s: %v", want, roster)
		}
	}

	if err := provisioner.RemoveUserMembership(store, relayURL, "relaybox", b, cpDir); err != nil {
		t.Fatal(err)
	}
	removes := 0
	for _, e := range relayState.Events() {
		if e.Kind == wire.RemoveUser {
			removes++
		}
	}
	if removes != 1 {
		t.Fatalf("expected 1 kind-9001 remove, got %d", removes)
	}
	roster, err = relay.QueryChannelRoster(relayURL, RelayPubkey(), rec.NostrPubkey, consoleSecret)
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(roster, b) {
		t.Fatal("b must be revoked from the roster")
	}
	if !containsStr(roster, a) {
		t.Fatal("a must stay a member")
	}
}

func TestAdoptRegistersExistingPackageWithoutReshipping(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)

	packageDir := filepath.Join(base, "runner")
	nostrSecret := make([]byte, 32)
	for i := range nostrSecret {
		nostrSecret[i] = byte(i + 1)
	}
	encSecret := make([]byte, 32)
	for i := range encSecret {
		encSecret[i] = byte(i + 40)
	}
	nostrPub, err := crypto.PubkeyFromSecret(nostrSecret)
	if err != nil {
		t.Fatal(err)
	}
	encPub, err := crypto.X25519PublicKey(encSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	idDoc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(nostrSecret),
		"enc_secret_hex":   hex.EncodeToString(encSecret),
	}
	idRaw, _ := json.Marshal(idDoc)
	if err := os.WriteFile(filepath.Join(packageDir, "identity.json"), idRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	sealed, err := crypto.Seal(encPub, []byte("relay-box"), []byte("the-ssh-key"))
	if err != nil {
		t.Fatal(err)
	}
	agent := hex64('a')
	pkg := wire.New(
		map[string]string{"relay-box": hex.EncodeToString(sealed)},
		map[string]wire.TargetMeta{"relay-box": {Kind: "ssh", Address: "root@192.168.30.224", Secret: "relay-box"}},
		[]string{agent},
	)
	if err := pkg.WriteToDir(packageDir); err != nil {
		t.Fatal(err)
	}

	mcp := "127.0.0.1:8787"
	runner, err := provisioner.AdoptRunner(store, "proxmox-box", "ssh", "root@192.168.30.224", packageDir, &mcp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if runner.NostrPubkey != nostrPub || runner.EncPubkey != hex.EncodeToString(encPub) {
		t.Fatal("adopt must read the package identity")
	}
	if runner.Status != state.RunnerActive {
		t.Fatalf("status = %s", runner.Status)
	}
	if runner.McpAddr == nil || *runner.McpAddr != mcp {
		t.Fatalf("mcp addr = %v", runner.McpAddr)
	}
	if runner.PackageDir != packageDir {
		t.Fatalf("package dir = %s", runner.PackageDir)
	}
	rec, _ := store.GetRunner("proxmox-box")
	if rec.NostrPubkey != runner.NostrPubkey {
		t.Fatal("state must record the adopted pubkey")
	}
	pkgAgain, _ := wire.Load(packageDir)
	if len(pkgAgain.Grants) != 1 {
		t.Fatalf("grants read from the package: %v", pkgAgain.Grants)
	}
	sec, _ := store.GetSecret("proxmox-box")
	if sec.CiphertextHex != pkgAgain.Secrets["relay-box"] {
		t.Fatal("the package is untouched by adopt")
	}
	if _, err := provisioner.AdoptRunner(store, "proxmox-box", "ssh", "root@192.168.30.224", packageDir, nil, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("re-adopt should be refused, got %v", err)
	}
}

// ---- small helpers ----

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// (end)
