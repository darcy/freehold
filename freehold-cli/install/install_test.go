package install

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/config"
	"freehold/platform/provisioning/box"
)

// newEOFUI is an installerUI whose every read hits EOF — a prompt would fail,
// so passing with this ui proves no prompt fired.
func newEOFUI() *installerUI {
	return &installerUI{out: io.Discard, raw: strings.NewReader(""), in: bufio.NewReader(strings.NewReader(""))}
}

// writeProfile registers a named profile with the given config body.
func writeProfile(t *testing.T, name, body string) {
	t.Helper()
	p := config.NewProfilePath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSelectProfileCreatesAndReAdopts covers the life-cycle-aware profile
// selection: a bad name is rejected, a new name pins profiles/<name>/, and an
// already-registered name RE-ADOPTS it (the gate, not selectProfile, refuses a
// live CP).
func TestSelectProfileCreatesAndReAdopts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	config.SetCurrent(nil)

	if err := selectProfile("bad name"); err == nil {
		t.Error("invalid profile name must be rejected")
	}
	if err := selectProfile("demo"); err != nil {
		t.Fatalf("new profile must be accepted: %v", err)
	}
	p := config.Current()
	if p == nil || p.Name != "demo" || p.ConfigPath != config.NewProfilePath("demo") || p.StateDir != config.NewProfileState("demo") {
		t.Fatalf("profile not pinned: %+v", p)
	}

	writeProfile(t, "demo", "relay_url = 'https://relay.example'\n")
	config.SetCurrent(nil)
	if err := selectProfile("demo"); err != nil {
		t.Fatalf("existing profile must re-adopt, got %v", err)
	}
	if p := config.Current(); p == nil || p.Name != "demo" {
		t.Fatalf("re-adopt must pin the existing profile, got %+v", p)
	}
}

// TestGateInstallMatrix is the PR1 gate: no profile mints; an existing profile
// whose recorded CP answers /healthz FAILS (a live world means build/teardown/
// uninstall/login); an existing profile whose CP is absent re-adopts.
func TestGateInstallMatrix(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	config.SetCurrent(nil)

	if a, err := gateInstall("demo"); err != nil || a != lifecycleMint {
		t.Fatalf("no profile must mint: a=%v err=%v", a, err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	writeProfile(t, "demo", "cp_url = '"+srv.URL+"'\nrelay_url = 'https://relay.example'\n")
	if _, err := gateInstall("demo"); err == nil || !strings.Contains(err.Error(), "live control plane") {
		t.Fatalf("a live CP must fail the gate, got %v", err)
	}

	// CP absent: no recorded URL is not-live, and a dead URL is not-live.
	writeProfile(t, "demo", "relay_url = 'https://relay.example'\n")
	if a, err := gateInstall("demo"); err != nil || a != lifecycleReAdopt {
		t.Fatalf("a profile with no live CP must re-adopt: a=%v err=%v", a, err)
	}
	writeProfile(t, "demo", "cp_url = 'http://127.0.0.1:1'\n")
	if a, err := gateInstall("demo"); err != nil || a != lifecycleReAdopt {
		t.Fatalf("a dead CP must re-adopt: a=%v err=%v", a, err)
	}
}

// TestApplyInstallDefaultsGatewayForcing: the mint flags itself for the
// pipeline's derive (the CIDR is derived + L2-probed in RunBootstrap, which
// owns the host SSH the probe needs) — a re-adopt rides the recorded gateway
// (a pre-gateway world stays flat), and a recorded vlan sentinel (-1) is
// never re-seeded.
func TestApplyInstallDefaultsGatewayForcing(t *testing.T) {
	// Mint: no CIDR is derived here — the pipeline derives + probes.
	f := box.Flags{ProxyIP: "192.168.30.8/24"}
	applyInstallDefaults(&f, true)
	if !f.Mint {
		t.Error("mint did not flag itself for the pipeline's subnet derive")
	}
	if f.GatewayCIDR != "" {
		t.Errorf("CLI-side derivation ran: %q (the pipeline owns the derive)", f.GatewayCIDR)
	}
	// Mint with an explicit --gateway-cidr: the flag wins untouched.
	f = box.Flags{ProxyIP: "192.168.30.8/24", GatewayCIDR: "10.99.0.0/24"}
	applyInstallDefaults(&f, true)
	if f.GatewayCIDR != "10.99.0.0/24" {
		t.Errorf("explicit cidr = %q, want 10.99.0.0/24", f.GatewayCIDR)
	}
	// Re-adopt of a pre-gateway world: NOTHING is derived (no mid-life
	// gateway colliding with the live LAN guests) and no mint flag.
	f = box.Flags{ProxyIP: "192.168.30.8/24"}
	applyInstallDefaults(&f, false)
	if f.GatewayCIDR != "" || f.Mint {
		t.Errorf("re-adopt derived a gateway or flagged mint: %q %v", f.GatewayCIDR, f.Mint)
	}
}

// TestSeedFromProfileSkipsVlanSentinel: a recorded vlan of -1 (the parse
// sentinel of a poisoned profile) must not resurrect through the re-adopt
// seed — an explicit --gateway-vlan 0 recovery must stay untagged.
func TestSeedFromProfileSkipsVlanSentinel(t *testing.T) {
	minus1 := -1
	f := box.Flags{}
	seedFromProfile(&f, &config.Config{Gateway: config.GatewaySpec{Vlan: &minus1}})
	if f.GatewayVlan != 0 {
		t.Errorf("sentinel seeded: %d", f.GatewayVlan)
	}
	seven := 7
	f = box.Flags{}
	seedFromProfile(&f, &config.Config{Gateway: config.GatewaySpec{Vlan: &seven}})
	if f.GatewayVlan != 7 {
		t.Errorf("recorded tag not seeded: %d", f.GatewayVlan)
	}
}

// TestResolveOperatorIdentityReAdopt: a re-adopt rides the RECORDED identity
// — no prompts (the EOF reader proves it: any prompt would die on EOF), the
// recorded dir + pubkey come back untouched.
func TestResolveOperatorIdentityReAdopt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	config.SetCurrent(nil)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorded := dir
	ui := newEOFUI()
	pk, opDir, err := resolveOperatorIdentity(ui, &config.Config{
		OperatorPubkey:   "abc",
		OperatorIdentity: &recorded,
	}, box.Flags{})
	if err != nil || pk != "abc" || opDir != dir {
		t.Fatalf("re-adopt must ride the recorded identity: pk=%q dir=%q err=%v", pk, opDir, err)
	}
}

// TestCollectHaveKeyReusesStoredKey: pasting the SAME key as the stored
// identity reuses it (a re-paste is not a conflict); a DIFFERENT key still
// errors.
func TestCollectHaveKeyReusesStoredKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	config.SetCurrent(nil)
	if err := selectProfile("demo"); err != nil {
		t.Fatal(err)
	}
	if err := box.MintIdentity(operatorDir()); err != nil {
		t.Fatal(err)
	}
	pk, err := box.LoadPubkey(operatorDir())
	if err != nil {
		t.Fatal(err)
	}
	ui := &installerUI{out: io.Discard, raw: strings.NewReader(pk + "\n"), in: bufio.NewReader(strings.NewReader(pk + "\n"))}
	gotPk, gotDir, err := collectHaveKey(ui)
	if err != nil || gotPk != pk || gotDir != operatorDir() {
		t.Fatalf("same-key paste must reuse: pk=%q dir=%q err=%v", gotPk, gotDir, err)
	}
	foreign := strings.Repeat("ab", 32)
	ui2 := &installerUI{out: io.Discard, raw: strings.NewReader(foreign + "\n"), in: bufio.NewReader(strings.NewReader(foreign + "\n"))}
	if _, _, err := collectHaveKey(ui2); err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("a different key must conflict, got %v", err)
	}
}

// TestCollectAnswersCarriesFlags: the guided flow's prompt answers plus the
// operator's explicit flags must BOTH land in the result — a flag field
// dropped by the return (port/gateway) made --local-port/--gateway-cidr
// silently vanish on the guided path.
func TestCollectAnswersCarriesFlags(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	config.SetCurrent(nil)
	if err := selectProfile("demo"); err != nil {
		t.Fatal(err)
	}
	if err := box.MintIdentity(operatorDir()); err != nil {
		t.Fatal(err)
	}
	pk, err := box.LoadPubkey(operatorDir())
	if err != nil {
		t.Fatal(err)
	}
	// Prompts answered with bare Enters (defaults): host, relay, cp, proxy
	// (required — answered), rootfs, memory, display name, storage consent.
	// The operator block is short-circuited by the pubkey flag over the live
	// ledger.
	stdin := strings.Repeat("\n", 3) + "192.0.2.8/24\n" + strings.Repeat("\n", 4)
	ui := &installerUI{out: io.Discard, raw: strings.NewReader(stdin), in: bufio.NewReader(strings.NewReader(stdin))}
	flags := box.Flags{
		OperatorPubkey: pk,
		Host:           "root@192.0.2.10",
		RelayDomain:    "chat.example.net",
		CpDomain:       "home.example.net",
		LocalPort:      9100,
		Addr:           "127.0.0.1:9100",
		GatewayCIDR:    "10.99.0.0/24",
		GatewayVlan:    7,
	}
	got, err := collectAnswers(ui, nil, flags)
	if err != nil {
		t.Fatalf("collectAnswers: %v", err)
	}
	if got.LocalPort != 9100 || got.Addr != "127.0.0.1:9100" {
		t.Errorf("--local-port dropped: port=%d addr=%q", got.LocalPort, got.Addr)
	}
	if got.GatewayCIDR != "10.99.0.0/24" || got.GatewayVlan != 7 {
		t.Errorf("gateway flags dropped: cidr=%q vlan=%d", got.GatewayCIDR, got.GatewayVlan)
	}
	if got.Host == "" || got.RelayDomain == "" || got.ProxyIP == "" {
		t.Errorf("prompt answers missing: %+v", got)
	}
	if got.OperatorName != "Operator" {
		t.Errorf("the display-name prompt default missing: %q", got.OperatorName)
	}
}
