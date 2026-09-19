package cli

import (
	"os"
	"path/filepath"
	"testing"

	"freehold/contract/config"
)

// TestResolveUninstall covers the uninstall preconditions: --host wins over the
// recorded host; the local-runner path needs the CP vmid; the transient path
// (thin box / dead CP) needs a host; neither present refuses.
func TestResolveUninstall(t *testing.T) {
	vmid := uint32(200)
	base := &config.Config{
		Host:   "root@host.recorded",
		Runner: config.RunnerRef{Addr: "127.0.0.1:8787", Pubkey: "aa"},
		Lxc:    config.LxcSpec{Cp: config.LxcGuest{Vmid: &vmid}},
	}
	if h, err := resolveUninstall(base, "root@host.flag"); err != nil || h != "root@host.flag" {
		t.Fatalf("--host must win: h=%q err=%v", h, err)
	}
	if h, err := resolveUninstall(base, ""); err != nil || h != "root@host.recorded" {
		t.Fatalf("recorded host must be used: h=%q err=%v", h, err)
	}
	// Host is optional (informational): a recorded runner + CP vmid suffice.
	noHost := *base
	noHost.Host = ""
	if h, err := resolveUninstall(&noHost, ""); err != nil || h != "" {
		t.Fatalf("host is optional: h=%q err=%v", h, err)
	}
	// A thin box with a recorded host now takes the TRANSIENT path.
	thin := *base
	thin.Runner = config.RunnerRef{}
	if h, err := resolveUninstall(&thin, ""); err != nil || h != "root@host.recorded" {
		t.Fatalf("thin box with a host must resolve transiently: h=%q err=%v", h, err)
	}
	// Neither a runner nor a host: refuse.
	nowhere := *base
	nowhere.Runner = config.RunnerRef{}
	nowhere.Host = ""
	if _, err := resolveUninstall(&nowhere, ""); err == nil {
		t.Fatal("no runner and no host must refuse")
	}
	// The local-runner path still needs the CP vmid.
	noCp := *base
	noCp.Lxc = config.LxcSpec{}
	if _, err := resolveUninstall(&noCp, ""); err == nil {
		t.Fatal("a missing CP vmid must refuse on the local-runner path")
	}
	// The transient path does not need the CP vmid (it lists guests by name).
	noCpThin := *base
	noCpThin.Runner = config.RunnerRef{}
	noCpThin.Lxc = config.LxcSpec{}
	if _, err := resolveUninstall(&noCpThin, ""); err != nil {
		t.Fatalf("transient path must not require the CP vmid: %v", err)
	}
}

// TestWipeLocalProfile proves the local half removes BOTH the profile config dir
// and the scoped state dir.
func TestWipeLocalProfile(t *testing.T) {
	home := t.TempDir()
	cfgDir := filepath.Join(home, "profiles", "demo")
	stateDir := filepath.Join(home, "state", "profiles", "demo")
	for _, d := range []string{cfgDir, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wipeLocalProfile(filepath.Join(cfgDir, "config.toml"), &config.Profile{Name: "demo", StateDir: stateDir})
	if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
		t.Fatal("profile config dir not wiped")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("profile state dir not wiped")
	}
}

// TestRunnerKeyRefsFallback: with no readable package (thin box) the ref is
// the runner TARGET (the provision comment), never the Nostr pubkey.
func TestRunnerKeyRefsFallback(t *testing.T) {
	cfg := &config.Config{Runner: config.RunnerRef{Target: "proxmox-box", Pubkey: "deadbeef"}}
	refs := runnerKeyRefs(cfg, nil)
	if len(refs) != 1 || refs[0] != "proxmox-box" {
		t.Errorf("refs = %v, want [proxmox-box]", refs)
	}
}
