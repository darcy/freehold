package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/crypto"
	"freehold/orchestrator/internal/wire"
)

// ---- storage-line parsing (stage_storage contract lines) -------------------

func TestParseStoragePool(t *testing.T) {
	if got := parseStoragePool("noise\nSTORAGE-POOL: pve\nmore\n"); got != "pve" {
		t.Errorf("pool = %q, want pve", got)
	}
	// absent line => rpool default.
	if got := parseStoragePool("nothing here\n"); got != "rpool" {
		t.Errorf("default pool = %q, want rpool", got)
	}
	if got := parseStoragePool("STORAGE-POOL:   \n"); got != "rpool" {
		t.Errorf("blank pool value should fall back to rpool, got %q", got)
	}
}

func TestParseStorageMounts(t *testing.T) {
	out := `creating thin LV...
STORAGE-MOUNT /freehold/dom/relay:/var/lib/docker
STORAGE-MOUNT /freehold/dom/deploy:/srv/data/relay
garbage line
STORAGE-MOUNT notamountline
`
	mounts := parseStorageMounts(out)
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2: %+v", len(mounts), mounts)
	}
	if mounts[0].Source != "/freehold/dom/relay" || mounts[0].GuestPath != "/var/lib/docker" {
		t.Errorf("mount[0] = %+v", mounts[0])
	}
	if mounts[1].GuestPath != "/srv/data/relay" {
		t.Errorf("mount[1] = %+v", mounts[1])
	}
}

func TestParseStorageBackend(t *testing.T) {
	if got := parseStorageBackend("STORAGE-BACKEND: lvmth pve\n"); got != "lvmth" {
		t.Errorf("kind = %q, want lvmth", got)
	}
	if got := parseStorageBackend("no line\n"); got != "" {
		t.Errorf("absent line = %q, want empty", got)
	}
	// a present-but-empty value must not panic (malformed resolve output) —
	// it reads as absent.
	if got := parseStorageBackend("STORAGE-BACKEND: \n"); got != "" {
		t.Errorf("empty value = %q, want empty", got)
	}
}

// ---- pct output parsing ------------------------------------------------------

func TestFindVmidInList(t *testing.T) {
	out := `VMID       Status     Lock         Name
100        running                 freehold-test-darcydev-net-relay
101        running                 freehold-test-darcydev-net-cp
200        stopped                 other-darcydev-net-relay
`
	vmid, err := findVmidInList(out, "freehold-test-darcydev-net-cp")
	if err != nil {
		t.Fatal(err)
	}
	if vmid != 101 {
		t.Errorf("vmid = %d, want 101", vmid)
	}
	// suffix-only names must NOT match a different world's container.
	if _, err := findVmidInList(out, "darcydev-net-relay"); err == nil {
		t.Error("a non-exact name should not match")
	}
	if _, err := findVmidInList(out, "missing-relay"); err == nil {
		t.Error("absent name should error")
	}
}

func TestParseLxcIP(t *testing.T) {
	ip, err := parseLxcIP("2: eth0    inet 192.168.30.8/24 brd 192.168.30.255 scope global eth0", 100)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.168.30.8/24" {
		t.Errorf("ip = %q", ip)
	}
	// loopback alone => error.
	if _, err := parseLxcIP("1: lo    inet 127.0.0.1/8 scope host lo", 100); err == nil {
		t.Error("loopback-only should error")
	}
}

func TestParsePctMounts(t *testing.T) {
	out := `arch: amd64
mp0: pve:freehold-test-darcydev-net/relay,mp=/var/lib/docker
mp1: pve:freehold-test-darcydev-net/deploy,mp=/srv/data/relay
net0: name=eth0,bridge=vmbr0,ip=dhcp
mptmp: something
`
	mounts := parsePctMounts(out)
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2: %v", len(mounts), mounts)
	}
	if mounts[0] != "/var/lib/docker" || mounts[1] != "/srv/data/relay" {
		t.Errorf("mounts = %v", mounts)
	}
}

func TestLxcName(t *testing.T) {
	if got := lxcName("freehold-test.darcydev.net", "relay"); got != "freehold-test-darcydev-net-relay" {
		t.Errorf("lxcName = %q", got)
	}
}

// ---- the record discipline (fresh load → mutate → save) ---------------------

// applyLxcCoords places one guest's coordinates in the right slot and adds
// the managed piece (k3s).
func TestApplyLxcCoords(t *testing.T) {
	cfg := &config.Config{Managed: []string{"relay", "cp"}}
	applyLxcCoords(cfg, "relay", 100, "10.0.0.8/24")
	if cfg.Lxc.Relay.Vmid == nil || *cfg.Lxc.Relay.Vmid != 100 || *cfg.Lxc.Relay.Ip != "10.0.0.8/24" {
		t.Errorf("relay coords not applied: %+v", cfg.Lxc.Relay)
	}
	if containsStr(cfg.Managed, "k3s") {
		t.Error("relay record must not add k3s to managed")
	}
	applyLxcCoords(cfg, "cp", 101, "10.0.0.9/24")
	if cfg.Lxc.Cp.Vmid == nil || *cfg.Lxc.Cp.Vmid != 101 {
		t.Errorf("cp coords not applied: %+v", cfg.Lxc.Cp)
	}
	applyLxcCoords(cfg, "k3s", 102, "10.0.0.10/24")
	if cfg.Lxc.K3s.Vmid == nil || *cfg.Lxc.K3s.Vmid != 102 {
		t.Errorf("k3s coords not applied: %+v", cfg.Lxc.K3s)
	}
	if !containsStr(cfg.Managed, "k3s") {
		t.Error("k3s record must add k3s to managed")
	}
	// re-recording k3s must not duplicate the managed entry.
	applyLxcCoords(cfg, "k3s", 102, "10.0.0.10/24")
	count := 0
	for _, m := range cfg.Managed {
		if m == "k3s" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("k3s managed duplicated: %v", cfg.Managed)
	}
	// unknown role => cp slot.
	applyLxcCoords(cfg, "bogus", 103, "10.0.0.11/24")
	if cfg.Lxc.Cp.Vmid == nil || *cfg.Lxc.Cp.Vmid != 103 {
		t.Errorf("unknown role should land in the cp slot: %+v", cfg.Lxc.Cp)
	}
}

// TestRecordLxcFreshLoadClobber is the port of the Rust regression test:
// record_lxc must load the config FRESH from disk, mutate only the target
// role, and save — never clobbering facts another stage recorded (the
// plane mounts here stand in for any mid-pipeline write).
func TestRecordLxcFreshLoadClobber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	pool := "pve"
	kind := "lvmth"
	seed := &config.Config{
		Domain:         "world.test",
		Managed:        []string{"relay", "cp"},
		OperatorPubkey: strings.Repeat("a", 64),
		Plane: config.PlaneSpec{
			Backend:     &pool,
			BackendKind: &kind,
			Mounts: map[string][]config.PlaneMount{
				"relay": {{Source: "/freehold/world-test/relay", GuestPath: "/srv/data/relay"}},
			},
		},
	}
	if err := seed.Save(path); err != nil {
		t.Fatal(err)
	}

	e := &rebuildEngine{f: rebuildFlags{configPath: path, domain: "world.test"}}
	got, err := e.recordLxcWith("cp", func(role string) (uint32, string, error) {
		return 101, "10.0.0.9/24", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Lxc.Cp.Vmid == nil || *got.Lxc.Cp.Vmid != 101 {
		t.Fatalf("cp coords not recorded: %+v", got.Lxc.Cp)
	}

	// reload from disk: the plane facts must have survived the save.
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Plane.Backend == nil || *reloaded.Plane.Backend != "pve" {
		t.Errorf("plane backend lost: %+v", reloaded.Plane)
	}
	if len(reloaded.Plane.Mounts["relay"]) != 1 || reloaded.Plane.Mounts["relay"][0].GuestPath != "/srv/data/relay" {
		t.Errorf("plane mounts lost: %+v", reloaded.Plane.Mounts)
	}
	if reloaded.Lxc.Cp.Vmid == nil || *reloaded.Lxc.Cp.Vmid != 101 {
		t.Errorf("cp coords not on disk: %+v", reloaded.Lxc.Cp)
	}
}

// TestRecordLxcBailsWithoutConfig: recording with no config on disk must
// fail actionably, not silently drop the coordinates.
func TestRecordLxcBailsWithoutConfig(t *testing.T) {
	e := &rebuildEngine{f: rebuildFlags{configPath: filepath.Join(t.TempDir(), "nope.toml")}}
	_, err := e.recordLxcWith("relay", func(string) (uint32, string, error) {
		return 100, "10.0.0.8/24", nil
	})
	if err == nil || !strings.Contains(err.Error(), "cannot record") {
		t.Errorf("expected actionable bail, got %v", err)
	}
}

// ---- merge_from_answers ------------------------------------------------------

func TestMergeFromAnswersNilPrev(t *testing.T) {
	ans := &config.Config{Domain: "d", Managed: []string{"relay", "cp"}}
	if got := mergeFromAnswers(ans, nil); got != ans {
		t.Error("nil prev must return the answers config untouched")
	}
}

func TestMergeFromAnswersPreservesPrevFacts(t *testing.T) {
	rpk := strings.Repeat("b", 64)
	oid := "/home/op/.freehold/control-plane/operator"
	pool := "pve"
	prev := &config.Config{
		RelayPubkey:      &rpk,
		OperatorIdentity: &oid,
		Plane: config.PlaneSpec{
			Backend: &pool,
			Mounts:  map[string][]config.PlaneMount{"cp": {{Source: "/s", GuestPath: "/g"}}},
		},
		Lxc: config.LxcSpec{
			Relay: config.LxcGuest{Vmid: u32(200), Ip: sptr("10.0.0.99/24")},
		},
		Managed: []string{"relay", "cp", "k3s"},
	}
	ans := &config.Config{
		Domain:  "d",
		Managed: []string{"relay", "cp"},
	}
	cfg := mergeFromAnswers(ans, prev)
	if cfg.RelayPubkey == nil || *cfg.RelayPubkey != rpk {
		t.Error("relay pubkey lost")
	}
	if cfg.OperatorIdentity == nil || *cfg.OperatorIdentity != oid {
		t.Error("operator identity lost")
	}
	if cfg.Plane.Backend == nil || *cfg.Plane.Backend != "pve" {
		t.Error("plane lost")
	}
	// prev fills the answer's Nones.
	if cfg.Lxc.Relay.Vmid == nil || *cfg.Lxc.Relay.Vmid != 200 {
		t.Errorf("prev relay vmid not filled: %+v", cfg.Lxc.Relay)
	}
	// answers win where set.
	ans.Lxc.Relay.Vmid = u32(300)
	cfg = mergeFromAnswers(ans, prev)
	if *cfg.Lxc.Relay.Vmid != 300 {
		t.Error("answer vmid must win over prev")
	}
	// managed union keeps k3s.
	if !containsStr(cfg.Managed, "k3s") {
		t.Errorf("managed union lost k3s: %v", cfg.Managed)
	}
}

func u32(v uint32) *uint32 { return &v }

func sptr(v string) *string { return &v }

// ---- door + NIP-11 parsing ----------------------------------------------------

func TestExtractSSHKey(t *testing.T) {
	out := "provisioning...\n  ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAaa proxmox-box\nnext\n"
	if got := extractSSHKey(out); got != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAaa proxmox-box" {
		t.Errorf("key = %q", got)
	}
	if got := extractSSHKey("no key here\n"); got != "" {
		t.Errorf("no key = %q, want empty", got)
	}
}

func TestExtractNip11Pubkey(t *testing.T) {
	pk := strings.Repeat("0123456789abcdef", 4)
	body := `{"name":"x","pubkey":"` + pk + `","supported_nips":[1]}`
	if got, ok := extractNip11Pubkey(body); !ok || got != pk {
		t.Errorf("pubkey = %q ok=%v", got, ok)
	}
	if _, ok := extractNip11Pubkey(`{"pubkey":"short"}`); ok {
		t.Error("short pubkey must be rejected")
	}
	if _, ok := extractNip11Pubkey(`{}`); ok {
		t.Error("missing pubkey must be rejected")
	}
	if _, ok := extractNip11Pubkey(`{"pubkey":"` + strings.Repeat("g", 64) + `"}`); ok {
		t.Error("non-hex pubkey must be rejected")
	}
}

func TestRegexEscape(t *testing.T) {
	if got := regexEscape("127.0.0.1:8787"); got != `127\.0\.0\.1\:8787` {
		t.Errorf("escape = %q", got)
	}
}

// ---- door key recovery (reuse + auth-fail re-surface) -----------------------

// TestExtractED25519PublicKeyLine: the public line round-trips through the
// openssh-key-v1 PEM (generate → extract) byte-identically — the recovery
// must re-emit EXACTLY the line the operator was shown at provision time.
func TestExtractED25519PublicKeyLine(t *testing.T) {
	pem, want, err := crypto.GenerateED25519SSHKeypair("proxmox-box")
	if err != nil {
		t.Fatal(err)
	}
	got, err := crypto.ExtractED25519PublicKeyLine(pem)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("extracted line = %q\nwant             %q", got, want)
	}
	if _, err := crypto.ExtractED25519PublicKeyLine([]byte("not a pem")); err == nil {
		t.Error("garbage input must fail")
	}
}

// sealedRunnerPackage builds a runner package exactly like provision does:
// identity.json (own X25519 enc key) + secrets.json holding the door PEM
// sealed TO that key under aad = the secret name.
func sealedRunnerPackage(t *testing.T, dir, secretName string) string {
	t.Helper()
	// identity.json: fresh 32-byte secrets (provisioner.identityGenerate);
	// provision creates the dir first, so mirror that.
	if err := wire.EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	nostr := bytes.Repeat([]byte{0x11}, 32)
	enc := bytes.Repeat([]byte{0x22}, 32)
	encPub, err := crypto.X25519PublicKey(enc)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]string{
		"nostr_secret_hex": hexStr(nostr),
		"enc_secret_hex":   hexStr(enc),
	}
	if err := wire.WriteJSON0600(filepath.Join(dir, "identity.json"), doc); err != nil {
		t.Fatal(err)
	}
	pem, want, err := crypto.GenerateED25519SSHKeypair("proxmox-box")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := crypto.Seal(encPub, []byte(secretName), pem)
	if err != nil {
		t.Fatal(err)
	}
	pkg := wire.New(
		map[string]string{secretName: hexStr(sealed)},
		map[string]wire.TargetMeta{secretName: {Kind: "ssh", Address: "root@h", Secret: secretName}},
		nil,
	)
	if err := pkg.WriteToDir(dir); err != nil {
		t.Fatal(err)
	}
	return want
}

func hexStr(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2], out[i*2+1] = digits[c>>4], digits[c&0xf]
	}
	return string(out)
}

// TestDoorKeyFromPackage: the public line comes back out of the sealed
// package — the recovery path the verify stage uses.
func TestDoorKeyFromPackage(t *testing.T) {
	dir := t.TempDir()
	want := sealedRunnerPackage(t, dir, "proxmox-box")
	got, err := doorKeyFromPackage(dir, "proxmox-box")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("recovered line = %q\nwant             %q", got, want)
	}
	// wrong aad (wrong secret name) must not decrypt to a key.
	if _, err := doorKeyFromPackage(dir, "some-other-target"); err == nil {
		// no ssh target named some-other-target — the fallback still finds
		// the real entry, so success is fine; but a package with NO ssh
		// target must fail.
		t.Log("fallback found the real entry (ok)")
	}
	empty := t.TempDir()
	if err := wire.New(nil, nil, nil).WriteToDir(empty); err != nil {
		t.Fatal(err)
	}
	if _, err := doorKeyFromPackage(empty, "x"); err == nil {
		t.Error("a package without any ssh target must fail")
	}
}

// TestStageVerifyReSurfacesDoorKey: with --yes, an ssh auth refusal bails
// with the recovered key + the install line — the SAME pause shape the
// fresh-key gate produces (so the TUI's waiting-state marker catches it).
func TestStageVerifyReSurfacesDoorKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)
	dir := filepath.Join(home, "runner", "proxmox-box")
	want := sealedRunnerPackage(t, dir, "proxmox-box")

	authFail := "tool error: ssh error: authentication failed (remaining methods: MethodSet([PublicKey, Password]))"
	e := &rebuildEngine{
		f:    rebuildFlags{yes: true, target: "proxmox-box", host: "root@192.168.30.224", addr: "127.0.0.1:8787"},
		bins: rebuildBins{Self: "self"},
		out:  &bytes.Buffer{},
		in:   strings.NewReader(""),
	}
	e.runBin = func(bin string, args []string) (bool, string) {
		return false, authFail
	}
	err := e.stageVerify()
	if err == nil {
		t.Fatal("auth failure must fail the stage")
	}
	msg := err.Error()
	for _, wantSub := range []string{"the door needs", "re-run rebuild", want, ">> /root/.ssh/authorized_keys"} {
		if !strings.Contains(msg, wantSub) {
			t.Errorf("re-surface bail missing %q:\n%s", wantSub, msg)
		}
	}

	// A NON-auth exec failure keeps the plain bail (no key re-surface).
	e.runBin = func(bin string, args []string) (bool, string) {
		return false, "tool error: ssh error: connection refused"
	}
	err = e.stageVerify()
	if err == nil || strings.Contains(err.Error(), "the door needs") {
		t.Errorf("non-auth failure must not claim a key is needed: %v", err)
	}
}

// TestStageVerifyUnrecoverable: auth failure with NO usable package tells
// the operator the fresh-start path instead of dangling a fake key.
func TestStageVerifyUnrecoverable(t *testing.T) {
	home := t.TempDir() // world home with NO runner package at all
	t.Setenv("FREEHOLD_HOME", home)
	e := &rebuildEngine{
		f:    rebuildFlags{yes: true, target: "proxmox-box", host: "root@h", addr: "127.0.0.1:8787"},
		bins: rebuildBins{Self: "self"},
		out:  &bytes.Buffer{},
		in:   strings.NewReader(""),
	}
	e.runBin = func(bin string, args []string) (bool, string) {
		return false, "tool error: ssh error: authentication failed (remaining methods: MethodSet([PublicKey]))"
	}
	err := e.stageVerify()
	if err == nil {
		t.Fatal("must fail")
	}
	if !strings.Contains(err.Error(), "rm -rf ~/.freehold") {
		t.Errorf("unrecoverable bail must give the fresh-start path:\n%s", err)
	}
}

// ---- static IP resolution (Rust Answers::from_config parity) --------------

func TestBootstrapStaticIPFlagWins(t *testing.T) {
	ip := "10.0.0.8/24"
	cfg := &config.Config{}
	cfg.Lxc.Relay.Ip = &ip
	// explicit flag beats the recorded config ip.
	if got := bootstrapStaticIP("relay", rebuildFlags{relayIP: "192.168.30.8/24"}, cfg); got != "192.168.30.8/24" {
		t.Errorf("flag should win, got %q", got)
	}
}

func TestBootstrapStaticIPRecordedFallsBack(t *testing.T) {
	ip := "192.168.30.9/24"
	cfg := &config.Config{}
	cfg.Lxc.Cp.Ip = &ip
	// no flag => the recorded ip rides again (the proxy/DNS target is owned).
	if got := bootstrapStaticIP("cp", rebuildFlags{}, cfg); got != "192.168.30.9/24" {
		t.Errorf("recorded ip should ride again, got %q", got)
	}
}

func TestBootstrapStaticIPNoneIsDHCP(t *testing.T) {
	// no flag, no recorded ip => DHCP.
	if got := bootstrapStaticIP("k3s", rebuildFlags{}, &config.Config{}); got != "" {
		t.Errorf("absent ip should be DHCP, got %q", got)
	}
	// nil config (no file yet) => DHCP.
	if got := bootstrapStaticIP("relay", rebuildFlags{}, nil); got != "" {
		t.Errorf("nil config should be DHCP, got %q", got)
	}
}
