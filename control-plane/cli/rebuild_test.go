package cli

import "reflect"

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/wire"
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

// TestRecordPostWorld: after world_build brings up the world, the box records
// the relay/k3s coords it read back (DHCP re-lease aware) + the deterministic
// litellm/caddy coords, so the config/TUI/teardown agree with the CP-built
// world. The runBin mock answers the pct-list + ip readback probes.
func TestRecordPostWorld(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	domain := "relay.librem.freehold.technology"
	cfg := &config.Config{
		RelayURL: "https://" + domain,
		CPURL:    "https://cp.librem.freehold.technology",
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	relayName := lxcName(domain, "relay")
	k3sName := lxcName(domain, "k3s")
	e := &rebuildEngine{
		f: rebuildFlags{
			configPath:     cfgPath,
			relayDomain:    domain,
			proxyIP:        "192.168.30.8/24",
			operatorPubkey: strings.Repeat("ab", 32),
		},
		bins: rebuildBins{Self: "freehold"},
		out:  &bytes.Buffer{},
		runBin: func(bin string, args []string) (bool, string) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "pct list"):
				return true, "VMID Status Name\n100 running " + relayName + "\n102 running " + k3sName + "\n"
			case strings.Contains(joined, "pct exec 100 -- ip -4 -o addr show eth0"):
				return true, "2: eth0    inet 192.168.30.220/24 brd 192.168.30.255 scope global eth0"
			case strings.Contains(joined, "pct exec 102 -- ip -4 -o addr show eth0"):
				return true, "2: eth0    inet 192.168.30.8/24 brd 192.168.30.255 scope global eth0"
			case strings.Contains(joined, "relayPubkeyNip11") || strings.Contains(joined, "curl"):
				return true, ""
			}
			return false, "unexpected: " + joined
		},
	}
	if err := e.recordPostWorld(); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Lxc.Relay.Vmid == nil || *got.Lxc.Relay.Vmid != 100 || got.Lxc.Relay.Ip == nil || *got.Lxc.Relay.Ip != "192.168.30.220/24" {
		t.Errorf("relay coords not recorded: %+v", got.Lxc.Relay)
	}
	if got.Lxc.K3s.Vmid == nil || *got.Lxc.K3s.Vmid != 102 || got.Lxc.K3s.Ip == nil || *got.Lxc.K3s.Ip != "192.168.30.8/24" {
		t.Errorf("k3s coords not recorded: %+v", got.Lxc.K3s)
	}
	if got.Litellm.URL != "http://192.168.30.8:31400" || got.Litellm.Host != "192.168.30.8" {
		t.Errorf("litellm coords not recorded: %+v", got.Litellm)
	}
	if got.Caddy.Host != "192.168.30.8" {
		t.Errorf("caddy coords not recorded: %+v", got.Caddy)
	}
	if !containsStr(got.Managed, "litellm") || !containsStr(got.Managed, "caddy") {
		t.Errorf("managed missing litellm/caddy: %v", got.Managed)
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
		RelayURL:       "https://world.test",
		CPURL:          "https://cp.world.test",
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
	ans := &config.Config{RelayURL: "https://relay.d", CPURL: "https://cp.d", Managed: []string{"relay", "cp"}}
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
		RelayURL: "https://relay.d",
		CPURL:    "https://cp.d",
		Managed:  []string{"relay", "cp"},
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

// TestFromAnswersRelayWsURLInternal confirms relay + CP domains are NEVER
// derived: each is exactly what the operator supplies, and the CPA origin
// follows the relay's own host. There is no world/base domain anymore.
func TestFromAnswersRelayWsURLInternal(t *testing.T) {
	e := &rebuildEngine{f: rebuildFlags{
		relayDomain: "relay.freehold-test.darcydev.net",
		cpDomain:    "cp.freehold-test.darcydev.net",
		agentName:   "cpa",
	}}
	cfg := e.fromAnswers()
	if cfg.RelayURL != "https://relay.freehold-test.darcydev.net" {
		t.Errorf("RelayURL = %q, want the supplied relay host", cfg.RelayURL)
	}
	if cfg.RelayWsURL != "wss://relay.freehold-test.darcydev.net" {
		t.Errorf("RelayWsURL = %q, want wss://<relay-domain>", cfg.RelayWsURL)
	}
	if cfg.CPURL != "https://cp.freehold-test.darcydev.net" {
		t.Errorf("CPURL = %q, want the supplied cp host", cfg.CPURL)
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

func TestBootstrapStaticIPProxyOnly(t *testing.T) {
	// Only the proxy node ("k3s") can be static; its value is f.proxyIP first,
	// else the recorded cfg.Proxy.Ip, else DHCP.
	if got := bootstrapStaticIP("k3s", rebuildFlags{proxyIP: "192.168.30.7/24"}, &config.Config{}); got != "192.168.30.7/24" {
		t.Errorf("proxy flag should win, got %q", got)
	}
	ip := "192.168.30.7/24"
	cfg := &config.Config{Proxy: config.ProxySpec{Ip: &ip}}
	if got := bootstrapStaticIP("k3s", rebuildFlags{}, cfg); got != "192.168.30.7/24" {
		t.Errorf("recorded proxy ip should ride again, got %q", got)
	}
	if got := bootstrapStaticIP("k3s", rebuildFlags{}, &config.Config{}); got != "" {
		t.Errorf("absent proxy ip should be DHCP, got %q", got)
	}
	// relay/cp are ALWAYS DHCP behind the proxy, even with a recorded LXC ip.
	if got := bootstrapStaticIP("relay", rebuildFlags{proxyIP: "x"}, cfg); got != "" {
		t.Errorf("relay must stay DHCP, got %q", got)
	}
	if got := bootstrapStaticIP("cp", rebuildFlags{}, cfg); got != "" {
		t.Errorf("cp must stay DHCP, got %q", got)
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

// TestApplyConfigDefaults: `freehold rebuild` with no flags must pull the
// recorded operator key, relay/CP hosts, thin-pool, agent name, and proxy IP
// from the stored config — a smooth rebuild, no forced re-entry. Explicit flags
// win over the config.
func TestApplyConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	op := strings.Repeat("e", 64)
	pool := "fh-thin"
	proxy := "192.168.30.7/24"
	if err := (&config.Config{
		RelayURL:       "https://relay.world.test",
		RelayWsURL:     "wss://relay.world.test",
		CPURL:          "https://cp.world.test",
		OperatorPubkey: op,
		Proxy:          config.ProxySpec{Ip: &proxy},
		Plane:          config.PlaneSpec{ThinPool: &pool},
		CPAName:        "waldo",
	}).Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	f := &rebuildFlags{}
	if err := applyConfigDefaults(f, cfgPath); err != nil {
		t.Fatal(err)
	}
	if f.operatorPubkey != op || f.relayDomain != "relay.world.test" || f.cpDomain != "cp.world.test" {
		t.Errorf("defaults = %q / %q / %q", f.operatorPubkey, f.relayDomain, f.cpDomain)
	}
	if f.thinPool != "fh-thin" || f.agentName != "waldo" || f.proxyIP != proxy {
		t.Errorf("defaults = tp:%q agent:%q proxy:%q", f.thinPool, f.agentName, f.proxyIP)
	}
	// explicit flags win
	f2 := &rebuildFlags{operatorPubkey: "y", relayDomain: "z", cpDomain: "w"}
	if err := applyConfigDefaults(f2, cfgPath); err != nil {
		t.Fatal(err)
	}
	if f2.operatorPubkey != "y" || f2.relayDomain != "z" {
		t.Errorf("explicit flags must win: %q/%q", f2.operatorPubkey, f2.relayDomain)
	}
	// a recorded [dns.manager] managed=true seeds --manage-dns so a rebuild of
	// an already-DNS-managed world skips the y/n prompt.
	if err := (&config.Config{
		RelayURL: "https://relay.world.test",
		CPURL:    "https://cp.world.test",
		Dns: config.DnsSpec{Manager: &config.DnsManager{
			Provider: "cloudflare", Managed: true, IP: "192.168.30.7",
		}},
	}).Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	f3 := &rebuildFlags{}
	if err := applyConfigDefaults(f3, cfgPath); err != nil {
		t.Fatal(err)
	}
	if !f3.manageDNS {
		t.Error("recorded dns.manager.managed=true must seed manageDNS=true (no re-prompt)")
	}
}
