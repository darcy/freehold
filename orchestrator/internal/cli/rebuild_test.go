package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"freehold/orchestrator/internal/config"
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
