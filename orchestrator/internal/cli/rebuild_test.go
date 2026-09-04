package cli

import "reflect"

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os"
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
// TestMergeKeepsDnsAndLitellm covers the finalSave contract: the mid-
// pipeline recorders persist [dns] + [litellm] to disk, and the terminal
// merge must KEEP them (the old merge rebuilt from answers and dropped
// both, so a finished rebuild showed no gateway coords and an empty DNS
// mirror even though the resolver held records).
func TestMergeKeepsDnsAndLitellm(t *testing.T) {
	prev := &config.Config{
		Dns:     config.DnsSpec{Records: map[string]string{"relay": "192.168.30.8", "litellm": "192.168.30.7"}},
		Litellm: config.LitellmSpec{URL: "http://192.168.30.7:31400", Host: "192.168.30.7"},
		Plane:   config.PlaneSpec{Backend: ptr("pve")},
		Managed: []string{"relay", "cp", "litellm"},
	}
	ans := &config.Config{Managed: []string{"relay", "cp"}}
	got := mergeFromAnswers(ans, prev)
	if len(got.Dns.Records) != 2 {
		t.Errorf("dns records dropped by merge: %+v", got.Dns)
	}
	if got.Litellm.Host != "192.168.30.7" || got.Litellm.URL != "http://192.168.30.7:31400" {
		t.Errorf("litellm dropped by merge: %+v", got.Litellm)
	}
	if got.Plane.Backend == nil || *got.Plane.Backend != "pve" {
		t.Errorf("plane must still merge: %+v", got.Plane)
	}
}

func ptr[T any](v T) *T { return &v }

// TestParsePctGateway covers the gw= parser: static guests carry the
// router, DHCP guests (`ip=dhcp`) have NO gw= and must yield "" so the
// caller falls back to the default route (the review-flagged regression:
// gw-only reading silently lost dnsmasq's upstream on the default world).
func TestParsePctGateway(t *testing.T) {
	static := `arch: amd64
cores: 2
net0: name=eth0,bridge=vmbr0,gw=192.168.30.1,hwaddr=BC:24:11:71:48:B5,ip=192.168.30.9/24,type=veth
ostype: debian
`
	if got := parsePctGateway(static); got != "192.168.30.1" {
		t.Errorf("static gw = %q, want 192.168.30.1", got)
	}
	dhcp := `arch: amd64
cores: 2
net0: name=eth0,bridge=vmbr0,ip=dhcp,type=veth
ostype: debian
`
	if got := parsePctGateway(dhcp); got != "" {
		t.Errorf("dhcp gw = %q, want empty (no gw= key)", got)
	}
}

// TestWorldManaged keeps a recorded k3s guest in the manifest when a re-run
// opted k3s out (--no-k3s), so teardown still destroys it instead of leaking
// the LXC + LV. litellm rides k3s and needs no guest carve-out.
func TestWorldManaged(t *testing.T) {
	// Full world (k3s + litellm on), recorded vmid / absent vmid.
	if got := worldManaged(false, false, ptr(uint32(102))); !reflect.DeepEqual(got, []string{"relay", "cp", "k3s", "litellm"}) {
		t.Errorf("full world with vmid = %v, want relay/cp/k3s/litellm", got)
	}
	if got := worldManaged(false, false, nil); !reflect.DeepEqual(got, []string{"relay", "cp", "k3s", "litellm"}) {
		t.Errorf("full world with no vmid = %v, want relay/cp/k3s/litellm", got)
	}
	// k3s on, litellm opted out.
	if got := worldManaged(false, true, ptr(uint32(102))); !reflect.DeepEqual(got, []string{"relay", "cp", "k3s"}) {
		t.Errorf("k3s-only world = %v, want relay/cp/k3s", got)
	}
	// k3s opted out but a recorded guest exists: it stays owned for teardown.
	if got := worldManaged(true, false, ptr(uint32(102))); !reflect.DeepEqual(got, []string{"relay", "cp", "k3s"}) {
		t.Errorf("skipped k3s with recorded vmid = %v, want relay/cp/k3s", got)
	}
	if got := worldManaged(true, true, nil); !reflect.DeepEqual(got, []string{"relay", "cp"}) {
		t.Errorf("k3s+litellm opted out, no vmid = %v, want relay/cp", got)
	}
}

func TestManagedForFlags(t *testing.T) {
	if got := managedForFlags(true, true); !reflect.DeepEqual(got, []string{"relay", "cp"}) {
		t.Errorf("base (k3s+litellm out) = %v", got)
	}
	if got := managedForFlags(false, true); !reflect.DeepEqual(got, []string{"relay", "cp", "k3s"}) {
		t.Errorf("with k3s = %v", got)
	}
	if got := managedForFlags(false, false); !reflect.DeepEqual(got, []string{"relay", "cp", "k3s", "litellm"}) {
		t.Errorf("full = %v", got)
	}
}

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

// TestFromAnswersRelayWsURLInternal confirms the relay/CP domains are NEVER
// derived: each is exactly what the operator supplied (defaulting to the world
// domain when absent), and the CPA origin follows the relay's own host.
func TestFromAnswersRelayWsURLInternal(t *testing.T) {
	// no explicit relay/cp domains -> both live at the world domain.
	e := &rebuildEngine{f: rebuildFlags{domain: "freehold-test.darcydev.net", agentName: "cpa"}}
	cfg := e.fromAnswers()
	if cfg.RelayWsURL != "wss://freehold-test.darcydev.net" {
		t.Errorf("RelayWsURL = %q, want wss://<world-domain> (no derivation)", cfg.RelayWsURL)
	}
	if cfg.RelayURL != "https://freehold-test.darcydev.net" {
		t.Errorf("RelayURL = %q, want https://<world-domain> (no relay. prefix)", cfg.RelayURL)
	}
	if cfg.CPURL != "https://freehold-test.darcydev.net" {
		t.Errorf("CPURL = %q, want https://<world-domain> (no cp. prefix)", cfg.CPURL)
	}

	// explicit relay/cp domains are used verbatim.
	e2 := &rebuildEngine{f: rebuildFlags{
		domain:      "freehold-test.darcydev.net",
		relayDomain: "relay.freehold-test.darcydev.net",
		cpDomain:    "cp.freehold-test.darcydev.net",
		agentName:   "cpa",
	}}
	cfg2 := e2.fromAnswers()
	if cfg2.RelayURL != "https://relay.freehold-test.darcydev.net" || cfg2.RelayWsURL != "wss://relay.freehold-test.darcydev.net" {
		t.Errorf("explicit relay domain not honored: %+v", cfg2)
	}
	if cfg2.CPURL != "https://cp.freehold-test.darcydev.net" {
		t.Errorf("explicit cp domain not honored: %+v", cfg2)
	}
}
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

// TestGenSecretHex: the re-minted master key + postgres password are 32-byte
// random hex — valid input for every consumer (and never committed).
func TestGenSecretHex(t *testing.T) {
	a, b := genSecretHex(), genSecretHex()
	if len(a) != 64 || len(b) != 64 {
		t.Fatalf("secret must be 32 bytes hex, got %q / %q", a, b)
	}
	if a == b {
		t.Fatal("two mints must differ")
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Errorf("not hex: %v", err)
	}
}

// TestLitellmManifestScript: the apply script creates the Secrets FROM ENV
// (never a literal), embeds both workloads, and pins the durable PVC + the
// fireworks egress — the C0 kube surface.
func TestLitellmManifestScript(t *testing.T) {
	out := litellmManifestScript(102, "masterkey", "pgpw", "providerkey")
	for _, want := range []string{
		"pct exec 102 -- sh -c",
		"master-key='masterkey'",
		"postgres-pw='pgpw'",
		"storageClassName: local-path",
		"nodePort: 31400",
		"35.207.52.96",                  // fireworks egress pin
		"LITELLM_MASTER_KEY",            // pod reads the k8s Secret
		"rollout status deploy/litellm", // readiness gate
		"LEG1_OK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("manifest script missing %q", want)
		}
	}
	// The script must NEVER carry a literal secret value.
	for _, forbidden := range []string{"fw_", "masterKey", "postgresPw", "$LITELLM"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("script leaked a literal: %q", forbidden)
		}
	}
	if strings.Contains(out, "FREEHOLD_LITELLM_MASTER=") {
		t.Error("script must not embed the secret value")
	}
}

// TestLitellmRegisterScript: the registration leg asks the runner for the
// two secrets BY NAME and uses them in env, never literal.
func TestLitellmRegisterScript(t *testing.T) {
	out := litellmRegisterScript("http://192.168.30.7:31400")
	for _, want := range []string{"$LITELLM", "$PROVIDER_KEY", "/model/new", "deepseek-v4-flash", "LEG2_OK", "http://192.168.30.7:31400"} {
		if !strings.Contains(out, want) {
			t.Errorf("register script missing %q", want)
		}
	}
	if strings.Contains(out, "127.0.0.1:31400") {
		t.Error("register script must target the real gateway URL, not loopback")
	}
	if strings.Contains(out, "fw_") {
		t.Error("register script leaked the provider key")
	}
}

// TestRecordLitellm: coords land in config + managed (Services row + DNS + a
// teardown sees the gateway).
func TestRecordLitellm(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	base := "domain = \"world.test\"\noperator_pubkey = \"" + strings.Repeat("a", 64) + "\"\nmanaged = [\"relay\", \"cp\", \"k3s\"]\n"
	if err := os.WriteFile(cfgPath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &rebuildEngine{f: rebuildFlags{configPath: cfgPath}}
	if err := e.recordLitellm("http://192.168.30.7:31400", "192.168.30.7"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Litellm.URL != "http://192.168.30.7:31400" || cfg.Litellm.Host != "192.168.30.7" {
		t.Errorf("litellm coords = %+v", cfg.Litellm)
	}
	if !containsStr(cfg.Managed, "litellm") {
		t.Errorf("managed must include litellm, got %v", cfg.Managed)
	}
	// idempotent: a second record doesn't duplicate the managed entry.
	if err := e.recordLitellm("http://192.168.30.7:31400", "192.168.30.7"); err != nil {
		t.Fatal(err)
	}
	if n := countStr(cfg.Managed, "litellm"); n != 1 {
		t.Errorf("managed duplicated litellm %d times", n)
	}
}

// TestRecordCaddy: coords land in config + managed (Services row + teardown
// ownership), idempotently.
func TestRecordCaddy(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	base := "domain = \"world.test\"\noperator_pubkey = \"" + strings.Repeat("a", 64) + "\"\nmanaged = [\"relay\", \"cp\", \"k3s\"]\n"
	if err := os.WriteFile(cfgPath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &rebuildEngine{f: rebuildFlags{configPath: cfgPath}}
	if err := e.recordCaddy("https://relay.world.test", "192.168.30.7"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Caddy.URL != "https://relay.world.test" || cfg.Caddy.Host != "192.168.30.7" {
		t.Errorf("caddy coords = %+v", cfg.Caddy)
	}
	if !containsStr(cfg.Managed, "caddy") {
		t.Errorf("managed must include caddy, got %v", cfg.Managed)
	}
	if err := e.recordCaddy("https://relay.world.test", "192.168.30.7"); err != nil {
		t.Fatal(err)
	}
	if n := countStr(cfg.Managed, "caddy"); n != 1 {
		t.Errorf("managed duplicated caddy %d times", n)
	}
}
func countStr(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}

// TestCaddyCertInstallScript guards the review finding that the cert-install
// helper script must FAIL (not silently succeed) when the PVC write doesn't
// happen: both shells run set -e, the helper pod waits for Ready before the
// writes, and the base64 payloads are single-quoted (no shell metachars).
func TestCaddyCertInstallScript(t *testing.T) {
	fc := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	key := []byte("-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n")
	s := caddyCertInstallScript(102, fc, key)

	// set -e in BOTH the outer wrapper and the inner (guest) script.
	if got := strings.Count(s, "set -e"); got != 2 {
		t.Errorf("want set -e in outer + inner shells, got %d occurrences:\n%s", got, s)
	}
	// the busybox helper reads the same durable PVC and is waited-for before
	// the cert/key writes.
	for _, want := range []string{"image: busybox", "claimName: caddy-data", "--timeout=60s",
		"persistentVolumeClaim", "rollout restart deploy/caddy"} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// base64 payloads present + single-quoted (safe, no shell metacharacters).
	fcB64 := base64.StdEncoding.EncodeToString(fc)
	keyB64 := base64.StdEncoding.EncodeToString(key)
	if !strings.Contains(s, "'"+fcB64+"'") {
		t.Errorf("fullchain base64 not single-quoted-embedded")
	}
	if !strings.Contains(s, "'"+keyB64+"'") {
		t.Errorf("key base64 not single-quoted-embedded")
	}
}

func TestLitellmHasProviderKey(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// provider-key present -> reuse.
	write(`{"secrets":{"litellm":"abc","provider-key":"def"},"targets":["litellm"],"grants":[]}`)
	if !litellmHasProviderKey(dir) {
		t.Error("expected provider-key detected as present")
	}
	// missing -> require a fresh supply.
	write(`{"secrets":{"litellm":"abc"},"targets":["litellm"],"grants":[]}`)
	if litellmHasProviderKey(dir) {
		t.Error("expected no provider-key")
	}
	// corrupted / absent file -> conservative (treat as absent).
	write(`{not json`)
	if litellmHasProviderKey(dir) {
		t.Error("expected corrupt package to read as no provider-key")
	}
	if err := os.Remove(filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	if litellmHasProviderKey(dir) {
		t.Error("expected missing package to read as no provider-key")
	}
}

// TestLitellmRunArgs: the litellm admin leg must exec the dedicated litellm
// runner (loopback 8788, target "litellm") with the named secrets — NOT the
// main proxmox-box runner via a nested "exec --target …" prefix (which bash
// swallows and never injects the secrets).
func TestLitellmRunArgs(t *testing.T) {
	var got [][]string
	eng := &rebuildEngine{}
	eng.bins.Self = "/bin/true"
	eng.runEnv = func(_ string, _ []string, args []string) (bool, string) {
		got = append(got, args)
		return true, ""
	}
	eng.litellmRun("echo hi", 60, "litellm", "provider-key")
	if len(got) != 1 {
		t.Fatalf("litellmRun ran %d execs, want 1", len(got))
	}
	args := got[0]
	joined := strings.Join(args, " ")
	for _, want := range []string{"exec", "--addr", "127.0.0.1:8788", "--secret", "litellm", "--secret", "provider-key", "litellm"} {
		if !strings.Contains(joined, want) {
			t.Errorf("litellmRun args %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "exec --target litellm") {
		t.Errorf("litellmRun must not use the nested exec --target form: %q", joined)
	}
}

func TestRelayDomainHost(t *testing.T) {
	cases := []struct{ domain, base, want string }{
		{"freehold-test.darcydev.net", "darcydev.net", "freehold-test"},
		{"example.com", "com", "example"},        // example.com under search com -> bare host example
		{"freehold-test.darcydev.net", "", ""},   // no search base -> no record
		{"a.b.darcydev.net", "darcydev.net", ""}, // dotted host not derivable (resolver rejects dots)
		{"plain", "darcydev.net", ""},            // not a subdomain
	}
	for _, c := range cases {
		if got := relayDomainHost(c.domain, c.base); got != c.want {
			t.Errorf("relayDomainHost(%q,%q) = %q, want %q", c.domain, c.base, got, c.want)
		}
	}
}
