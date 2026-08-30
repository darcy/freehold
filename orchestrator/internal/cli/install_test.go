package cli

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/orchestrator/internal/crypto"
)

// Two valid scalars with DIFFERENT pubkeys (used for the mismatch bail).
const (
	testSecretA = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	testSecretB = "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40"
)

func testPubkey(t *testing.T, secretHex string) string {
	t.Helper()
	secret, err := hex.DecodeString(secretHex)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := crypto.PubkeyFromSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

// ui feeds stdin through the SAME single bufio.Reader the real command uses.
func ui(stdin string) (*installerUI, *bytes.Buffer) {
	var buf bytes.Buffer
	return &installerUI{out: &buf, raw: strings.NewReader(stdin), in: bufio.NewReader(strings.NewReader(stdin))}, &buf
}

// the six world-detail defaults (blank = default) every collectAnswers test
// consumes before the operator-identity select.
const defaultAnswers = "\n\n\n\n\n\n"

func opIdentityFile(home string) string {
	return filepath.Join(home, "control-plane", "operator", "identity.json")
}

func TestCollectHaveKeyPersistsNsec(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)

	pk := testPubkey(t, testSecretA)
	stdin := defaultAnswers + "1\n" + pk + "\n" + testSecretA + "\ny\n"
	u, _ := ui(stdin)
	f, err := collectAnswers(u)
	if err != nil {
		t.Fatalf("collectAnswers: %v", err)
	}
	if f.operatorPubkey != pk {
		t.Errorf("operatorPubkey = %s, want %s", f.operatorPubkey, pk)
	}
	if want := filepath.Join(home, "control-plane", "operator"); f.operatorIdentity != want {
		t.Errorf("operatorIdentity = %q, want %q", f.operatorIdentity, want)
	}
	if !f.confirmStorage {
		t.Error("consent answer missing from flags")
	}

	// the persisted identity: the operator's OWN nostr secret + a fresh enc key.
	id, err := agentIdentity(f.operatorIdentity)
	if err != nil {
		t.Fatalf("identity.json unreadable: %v", err)
	}
	if id.NostrSecretHex != testSecretA {
		t.Errorf("nostr_secret_hex = %s, want the pasted secret", id.NostrSecretHex)
	}
	if _, err := hex.DecodeString(id.EncSecretHex); err != nil || len(id.EncSecretHex) != 64 {
		t.Errorf("enc_secret_hex %q is not a 32-byte hex", id.EncSecretHex)
	}
	info, err := os.Stat(opIdentityFile(home))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("identity.json must exist 0600 (got %v, %v)", info, err)
	}
}

func TestCollectHaveKeySkipNsec(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)

	pk := testPubkey(t, testSecretA)
	stdin := defaultAnswers + "1\n" + pk + "\n\nn\n" // blank nsec = skip, consent no
	u, _ := ui(stdin)
	f, err := collectAnswers(u)
	if err != nil {
		t.Fatalf("collectAnswers: %v", err)
	}
	if f.operatorPubkey != pk {
		t.Errorf("operatorPubkey = %s, want %s", f.operatorPubkey, pk)
	}
	if f.operatorIdentity != "" {
		t.Errorf("skipped nsec must not record an operator dir, got %q", f.operatorIdentity)
	}
	if _, err := os.Stat(opIdentityFile(home)); !os.IsNotExist(err) {
		t.Errorf("skipped nsec must not write identity.json (stat err = %v)", err)
	}
}

func TestCollectHaveKeyNsecMismatchBails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)

	pkA := testPubkey(t, testSecretA)
	// paste pk of A but the nsec of B — must bail BEFORE writing anything.
	stdin := defaultAnswers + "1\n" + pkA + "\n" + testSecretB + "\n"
	u, _ := ui(stdin)
	if _, err := collectAnswers(u); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("want nsec/pubkey mismatch bail, got %v", err)
	}
	if _, err := os.Stat(opIdentityFile(home)); !os.IsNotExist(err) {
		t.Error("mismatched nsec must not write identity.json")
	}

}

func TestCollectHaveKeyRefusesExistingIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)
	existing := opIdentityFile(home)
	if err := os.MkdirAll(filepath.Dir(existing), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte(`{"nostr_secret_hex":"aa"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	pk := testPubkey(t, testSecretA)
	stdin := defaultAnswers + "1\n" + pk + "\n" + testSecretA + "\n"
	u, _ := ui(stdin)
	if _, err := collectAnswers(u); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want already-exists refusal, got %v", err)
	}
}

func TestCollectHaveKeyRepromptsBadPubkey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)

	pk := testPubkey(t, testSecretA)
	// first pubkey attempt is garbage (bad bech32-ish + bad hex), then valid.
	stdin := defaultAnswers + "1\nnot-a-key\n" + pk + "\n\nn\n"
	u, out := ui(stdin)
	f, err := collectAnswers(u)
	if err != nil {
		t.Fatalf("collectAnswers: %v", err)
	}
	if f.operatorPubkey != pk {
		t.Errorf("operatorPubkey = %s, want %s after re-prompt", f.operatorPubkey, pk)
	}
	if !strings.Contains(out.String(), "invalid pubkey") {
		t.Error("bad pubkey must re-prompt with the reason")
	}
}

func TestCollectGenerateMints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)

	stdin := defaultAnswers + "2\nn\n" // generate, consent no
	u, out := ui(stdin)
	f, err := collectAnswers(u)
	if err != nil {
		t.Fatalf("collectAnswers: %v", err)
	}
	dir := filepath.Join(home, "control-plane", "operator")
	if f.operatorIdentity != dir {
		t.Errorf("operatorIdentity = %q, want %q", f.operatorIdentity, dir)
	}
	pk, err := loadRPubkey(dir)
	if err != nil {
		t.Fatalf("minted identity unreadable: %v", err)
	}
	if f.operatorPubkey != pk {
		t.Errorf("operatorPubkey = %s, want minted %s", f.operatorPubkey, pk)
	}
	if !strings.Contains(out.String(), "Generated a fresh identity") {
		t.Error("generate path must announce the mint")
	}
}

func TestCollectGenerateReusesExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)
	dir := filepath.Join(home, "control-plane", "operator")
	if err := mintAgentIdentity(dir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(opIdentityFile(home))
	if err != nil {
		t.Fatal(err)
	}
	pk, err := loadRPubkey(dir)
	if err != nil {
		t.Fatal(err)
	}

	stdin := defaultAnswers + "2\nn\n"
	u, out := ui(stdin)
	f, err := collectAnswers(u)
	if err != nil {
		t.Fatalf("collectAnswers: %v", err)
	}
	if f.operatorPubkey != pk {
		t.Errorf("operatorPubkey = %s, want existing %s", f.operatorPubkey, pk)
	}
	after, err := os.ReadFile(opIdentityFile(home))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("generate must never clobber an existing identity.json")
	}
	if !strings.Contains(out.String(), "Reusing") {
		t.Error("existing identity must be announced as reused, not minted")
	}
}

func TestCollectDefaultsCarryThrough(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)

	stdin := defaultAnswers + "2\nn\n"
	u, _ := ui(stdin)
	f, err := collectAnswers(u)
	if err != nil {
		t.Fatalf("collectAnswers: %v", err)
	}
	if f.host != "root@192.168.30.224" || f.target != "proxmox-box" ||
		f.addr != "127.0.0.1:8787" || f.domain != "freehold-test.darcydev.net" {
		t.Errorf("blank answers must take the defaults: %+v", f)
	}
	if f.rootfsGB != 16 || f.memoryMB != 2048 {
		t.Errorf("size defaults lost: rootfs=%d memory=%d", f.rootfsGB, f.memoryMB)
	}
	if !f.withK3s {
		t.Error("install must boot k3s by default (parity with rebuild)")
	}
}

func TestRunInstallAbortBeforeEngine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)

	// defaults + generate + consent-no + Proceed?=no → abort, engine NEVER built.
	stdin := defaultAnswers + "2\nn\nn\n"
	called := false
	err := runInstallWith(strings.NewReader(stdin), &bytes.Buffer{},
		func(rebuildFlags) (*rebuildEngine, error) {
			called = true
			return nil, errors.New("engine must not be built")
		})
	if err != nil {
		t.Fatalf("abort must be a clean exit, got %v", err)
	}
	if called {
		t.Error("Proceed?=no must not construct the engine")
	}
	if _, err := os.Stat(opIdentityFile(home)); os.IsNotExist(err) {
		t.Error("the generate path must mint BEFORE the proceed gate (Rust parity: collect runs first)")
	}
}

func TestRunInstallHandsCollectedFlagsToEngine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)

	pk := testPubkey(t, testSecretA)
	// custom host + defaults for the rest, have-key w/ nsec, consent yes, proceed yes.
	stdin := "root@10.0.0.5\n\n\nworld.test\n\n\n" + "1\n" + pk + "\n" + testSecretA + "\ny\ny\n"

	var got rebuildFlags
	var out bytes.Buffer
	err := runInstallWith(strings.NewReader(stdin), &out,
		func(f rebuildFlags) (*rebuildEngine, error) {
			got = f
			return nil, errors.New("engine-handoff")
		})
	if err == nil || err.Error() != "engine-handoff" {
		t.Fatalf("want the constructor's sentinel, got %v", err)
	}

	want := rebuildFlags{
		addr:             "127.0.0.1:8787",
		target:           "proxmox-box",
		host:             "root@10.0.0.5",
		domain:           "world.test",
		operatorPubkey:   pk,
		operatorIdentity: filepath.Join(home, "control-plane", "operator"),
		rootfsGB:         16,
		memoryMB:         2048,
		relayGw:          "192.168.30.1",
		withK3s:          true,
		confirmStorage:   true,
		configPath:       defaultConfigPath(),
	}
	if got.sizeGB == 0 || got.poolSizeGB == 0 {
		t.Error("install must carry the storage size defaults from drive")
	}
	got.sizeGB, got.poolSizeGB = 0, 0 // asserted above; compare the rest exactly
	if got != want {
		t.Errorf("handoff flags:\n got %+v\nwant %+v", got, want)
	}
	if !strings.Contains(out.String(), "world.test") {
		t.Error("the summary block must show the collected domain")
	}
}
