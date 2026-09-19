package oplogin

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/config"
	"freehold/contract/crypto"
)

// TestOperatorLedgerRoundTrip verifies the operator's nsec persists to the
// operator dir (isolated via FREEHOLD_HOME) and that first-run-wins holds.
func TestOperatorLedgerRoundTrip(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())

	secret := [32]byte{}
	secret[0] = 7
	wrote, err := Save(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("first save should write")
	}
	got, err := SecretHex()
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(secret[:]); got != want {
		t.Fatalf("secret mismatch: want %s got %s", want, got)
	}
	// First-run-wins: a second Save must not clobber.
	other := [32]byte{}
	other[0] = 9
	if wrote, _ = Save(other); wrote {
		t.Fatal("second save should be refused (first-run-wins)")
	}
	if got, _ = SecretHex(); got != hex.EncodeToString(secret[:]) {
		t.Fatal("operator identity was clobbered")
	}
}

// TestNsecToSecret accepts both nsec1 bech32 and bare hex.
func TestNsecToSecret(t *testing.T) {
	hex64 := "b9100ce43b1deac5c5189f48bc6d97b287e122211d5f271488c0bcae2b350ad6"
	s, err := NsecToSecret(hex64)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(s[:]) != hex64 {
		t.Fatal("hex nsec misparsed")
	}
	if _, err := NsecToSecret("not-a-nsec"); err == nil {
		t.Fatal("invalid nsec should error")
	}
}

// TestAuthorizeDoorNeedsToolsetCoords guards the door flow's guard rails: with
// an agent-ops identity minted but no agent-tools coords recorded (a CP that
// predates the world toolset), AuthorizeDoor must fail cleanly and name the
// miss rather than attempt a network call or panic.
func TestAuthorizeDoorNeedsToolsetCoords(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	if _, err := EnsureOpsIdentity(); err != nil {
		t.Fatal(err)
	}
	c := &config.Config{}
	if err := AuthorizeDoor(c); err == nil {
		t.Fatal("AuthorizeDoor with no agent-tools coords must error")
	} else if !strings.Contains(err.Error(), "freehold-agent-tools") {
		t.Fatalf("error should name the missing coords, got: %v", err)
	}
}

// TestInteractiveSeedsWorldProfile drives `freehold login` end to end against a
// mock CP: challenge -> login (Set-Cookie) -> world summary; the local desire
// profile is seeded under a NAMED profile (no legacy/default) and the operator
// identity is persisted, all root-free.
func TestInteractiveSeedsWorldProfile(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const nsecHex = "b9100ce43b1deac5c5189f48bc6d97b287e122211d5f271488c0bcae2b350ad6"
	wantOpPK, err := crypto.PubkeyFromSecret(mustHex(t, nsecHex))
	if err != nil {
		t.Fatal(err)
	}
	const cpPubkey = "00aa11bb22cc33dd44ee55ff6677889900112233445566778899aabbccddeeff"

	srv := mockCP(t, cpPubkey)
	defer srv.Close()

	// Feed: CP address, then a blank profile name (accept the default = host
	// slug), then the nsec — all over piped stdin (no terminal, no l / c).
	feedStdin(t, srv.URL, "", nsecHex)

	if err := Interactive(); err != nil {
		t.Fatalf("login failed: %v", err)
	}

	// One named profile was created; there is NO legacy/default profile.
	profiles := config.List()
	if len(profiles) != 1 {
		t.Fatalf("login must create exactly one profile, got %d", len(profiles))
	}
	if profiles[0].Name == "default" {
		t.Fatal("no implicit default profile under option B")
	}
	got, err := config.Load(profiles[0].ConfigPath)
	if err != nil || got == nil {
		t.Fatalf("seeded config: %v", err)
	}
	if got.CPURL != srv.URL {
		t.Errorf("cp_url not seeded: %s", got.CPURL)
	}
	if got.RelayURL != "https://relay.example" {
		t.Errorf("relay_url not seeded: %s", got.RelayURL)
	}
	if got.RelayWsURL != "wss://relay.example" {
		t.Errorf("relay_ws_url not seeded: %s", got.RelayWsURL)
	}
	if got.OperatorPubkey != wantOpPK {
		t.Errorf("operator_pubkey not seeded from nsec: %s", got.OperatorPubkey)
	}
	if got.CpPubkey != cpPubkey {
		t.Errorf("cp_pubkey not seeded: %s", got.CpPubkey)
	}
	if got.AgentToolsURL != "http://10.0.0.5:8089" {
		t.Errorf("agent_tools_url not seeded: %s", got.AgentToolsURL)
	}
	if got.AgentToolsPubkey != "22222" {
		t.Errorf("agent_tools_pubkey not seeded: %s", got.AgentToolsPubkey)
	}
	// The box's own provisioning identity is materialized on disk (the actor a
	// CP-side grant binds), but login does NOT fabricate a [runner] block — a
	// box has no deployed runner until `freehold build` authors one.
	opsPK, err := OpsPubkey()
	if err != nil {
		t.Fatalf("box ops identity not materialized: %v", err)
	}
	if opsPK == "" {
		t.Fatal("box ops identity pubkey empty")
	}
	if got.Runner.Pubkey != "" {
		t.Errorf("login must not fabricate a runner pubkey (no deployed runner yet): %s", got.Runner.Pubkey)
	}
	// Operator identity ledger persisted, scoped to the profile's state dir.
	if gotSec, err := SecretHex(); err != nil || gotSec != nsecHex {
		t.Errorf("operator identity not persisted: %q err=%v", gotSec, err)
	}
	if !strings.HasPrefix(identityHome(), profiles[0].StateDir) {
		t.Errorf("operator identity must be scoped to the profile state dir, got %s", identityHome())
	}
}

// TestInteractivePreservesSurvivingRunner guards the reviewer-flagged clobber:
// on an already-built box, a re-login into the SAME named profile must NOT
// overwrite [runner] with the box's agent-ops (caller) identity — [runner].pubkey
// is the DEPLOYED runner's own identity, the audience of every signed call.
func TestInteractivePreservesSurvivingRunner(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const nsecHex = "b9100ce43b1deac5c5189f48bc6d97b287e122211d5f271488c0bcae2b350ad6"
	srv := mockCP(t, "00aa11bb22cc33dd44ee55ff6677889900112233445566778899aabbccddeeff")
	defer srv.Close()

	// A world that was already `build`-deployed, with the runner's OWN identity
	// recorded (distinct from the box's agent-ops caller identity) under a NAMED
	// profile.
	deployedPub := "9999999999999999999999999999999999999999999999999999999999999999"
	pre := &config.Config{}
	pre.CPURL = srv.URL
	pre.CpPubkey = "00aa11bb22cc33dd44ee55ff6677889900112233445566778899aabbccddeeff"
	pre.Runner = config.RunnerRef{Addr: "127.0.0.1:8787", Pubkey: deployedPub, Target: "proxmox-box"}
	if err := pre.Save(config.NewProfilePath("demo")); err != nil {
		t.Fatal(err)
	}

	// Re-login into that same profile: CP address, profile name "demo", nsec.
	feedStdin(t, srv.URL, "demo", nsecHex)

	if err := Interactive(); err != nil {
		t.Fatalf("login failed: %v", err)
	}
	got, err := config.Load(config.NewProfilePath("demo"))
	if err != nil || got == nil {
		t.Fatalf("config: %v", err)
	}
	if got.Runner.Pubkey != deployedPub {
		t.Errorf("re-login clobbered the deployed runner pubkey: got %q want %q", got.Runner.Pubkey, deployedPub)
	}
	if got.Runner.Target != "proxmox-box" {
		t.Errorf("re-login clobbered runner target: %q", got.Runner.Target)
	}
	if config.Resolve("demo") == nil {
		t.Fatal("profile demo must exist after re-login")
	}
}

// TestEnsureOpsIdentityFirstRunWins proves the box provisioning identity is
// minted once and never clobbered (idempotent across re-logins).
func TestEnsureOpsIdentityFirstRunWins(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	pk1, err := EnsureOpsIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if pk1 == "" {
		t.Fatal("empty ops pubkey")
	}
	// Second call (re-login) must return the SAME identity, not a new one.
	pk2, err := EnsureOpsIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if pk2 != pk1 {
		t.Fatalf("ops identity not first-run-wins: %s vs %s", pk1, pk2)
	}
	// Logout clears the operator LEDGER only — the box's own identity survives
	// (it is not the operator's nsec; the box stays a durable actor).
	if err := Logout(); err != nil {
		t.Fatal(err)
	}
	pk3, err := EnsureOpsIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if pk3 != pk1 {
		t.Fatalf("logout must not erase the box's own provisioning identity")
	}
}

// TestResolveCPPubkeyNormalizes proves the recorded trust anchor is always the
// CP's self-report in normalized lowercase 64-hex (npub1<bech32>, hex, or a
// cased/whitespace variety all collapse to the same hex), and a blank report is
// blank.
func TestResolveCPPubkeyNormalizes(t *testing.T) {
	const hexPK = "3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"
	const npubPK = "npub180cvv07tjdrrgpa0j7j7tmnyl2yr6yr7l8j4s3evf6u64th6gkwsyjh6w6"
	const upperPK = "3BF0C63FCB93463407AF97A5E5EE64FA883D107EF9E558472C4EB9AAAEFA459D"

	if got := resolveCPPubkey(npubPK); got != hexPK {
		t.Fatalf("npub report should normalize to hex, got %q", got)
	}
	if got := resolveCPPubkey(hexPK); got != hexPK {
		t.Fatalf("hex report should stay hex, got %q", got)
	}
	if got := resolveCPPubkey(upperPK); got != hexPK {
		t.Fatalf("uppercase hex should normalize to lowercase, got %q", got)
	}
	if got := resolveCPPubkey(""); got != "" {
		t.Fatalf("blank report should yield a blank anchor, got %q", got)
	}
}

// TestInteractiveAdoptsCPReportOverStaleCpPubkey proves the old footgun is gone:
// even when the config holds a stale/wrong cp_pubkey (here, one that disagrees
// with what the CP reports about itself), login does NOT force an operator-typed
// pubkey through a cross-check — the recorded anchor is simply the CP's report.
// The stale value is overwritten, never allowed to hard-abort login.
func TestInteractiveAdoptsCPReportOverStaleCpPubkey(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const nsecHex = "b9100ce43b1deac5c5189f48bc6d97b287e122211d5f271488c0bcae2b350ad6"
	const worldPub = "00aa11bb22cc33dd44ee55ff6677889900112233445566778899aabbccddeeff"
	srv := mockCP(t, worldPub)
	defer srv.Close()

	// A stale/wrong anchor from an earlier UI iteration (e.g. the operator's own
	// key pasted into a "CP pubkey" prompt). A fresh login into the SAME named
	// profile must adopt the CP's real report instead of either trusting this
	// stale value or dying on it.
	pre := &config.Config{}
	pre.CPURL = srv.URL
	pre.CpPubkey = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := pre.Save(config.NewProfilePath("demo")); err != nil {
		t.Fatal(err)
	}

	// Re-login into "demo": CP address, profile name, nsec.
	feedStdin(t, srv.URL, "demo", nsecHex)

	if err := Interactive(); err != nil {
		t.Fatalf("login with a stale cp_pubkey must succeed and adopt the CP report: %v", err)
	}
	got, err := config.Load(config.NewProfilePath("demo"))
	if err != nil || got == nil {
		t.Fatalf("seeded config: %v", err)
	}
	if got.CpPubkey != worldPub {
		t.Errorf("stale cp_pubkey must be overwritten by the CP's report, got %q want %q", got.CpPubkey, worldPub)
	}
}

// TestLogoutClearsOnlyLocalLedger verifies logout drops the box's operator
// identity while touching nothing else.
func TestLogoutClearsOnlyLocalLedger(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	secret := [32]byte{}
	secret[0] = 5
	if _, err := Save(secret); err != nil {
		t.Fatal(err)
	}
	if err := Logout(); err != nil {
		t.Fatal(err)
	}
	if _, err := SecretHex(); err == nil {
		t.Fatal("operator identity should be gone after logout")
	}
	if _, err := os.Stat(filepath.Join(Dir(), identityFile)); !os.IsNotExist(err) {
		t.Fatal("identity file should not exist after logout")
	}
	// Logout is idempotent (missing file is fine).
	if err := Logout(); err != nil {
		t.Fatalf("repeat logout should be a no-op, got: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// mockCP is a minimal stand-in control plane: NIP-98 challenge/login session +
// a /api/world summary reporting `cpPubkey` as its own identity.
func mockCP(t *testing.T, cpPubkey string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/challenge":
			writeJSON(w, map[string]string{"nonce": "deadbeef"})
		case "/api/auth/login":
			w.Header().Set("Set-Cookie", "fh_session=tok123; Path=/; HttpOnly")
			writeJSON(w, map[string]string{"ok": "true"})
		case "/api/world":
			writeJSON(w, map[string]string{
				"relay_url":          "https://relay.example",
				"relay_ws_url":       "wss://relay.example",
				"relay_pubkey":       "11111",
				"cp_url":             srv.URL,
				"cp_pubkey":          cpPubkey,
				"operator_pubkey":    "operator",
				"agent_tools_url":    "http://10.0.0.5:8089",
				"agent_tools_pubkey": "22222",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// feedStdin feeds one line per interactive prompt over piped stdin (no
// terminal), restoring the original stdin after the test.
func feedStdin(t *testing.T, lines ...string) {
	t.Helper()
	oldStdin := os.Stdin
	pipeR, pipeW, _ := os.Pipe()
	os.Stdin = pipeR
	go func() {
		for _, l := range lines {
			pipeW.WriteString(l + "\n")
		}
		pipeW.Close()
	}()
	t.Cleanup(func() { os.Stdin = oldStdin })
}
