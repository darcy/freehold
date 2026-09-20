package uninstall

import (
	"os"
	"path/filepath"
	"testing"

	"freehold/contract/config"
)

// TestResolveUninstall covers the uninstall preconditions: --host wins over the
// recorded host; the local-runner path needs the CP vmid; the transient path
// needs a host; neither present refuses.
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
	noHost := *base
	noHost.Host = ""
	if h, err := resolveUninstall(&noHost, ""); err != nil || h != "" {
		t.Fatalf("host is optional: h=%q err=%v", h, err)
	}
	thin := *base
	thin.Runner = config.RunnerRef{}
	if h, err := resolveUninstall(&thin, ""); err != nil || h != "root@host.recorded" {
		t.Fatalf("thin box with a host must resolve transiently: h=%q err=%v", h, err)
	}
	nowhere := *base
	nowhere.Runner = config.RunnerRef{}
	nowhere.Host = ""
	if _, err := resolveUninstall(&nowhere, ""); err == nil {
		t.Fatal("no runner and no host must refuse")
	}
	noCp := *base
	noCp.Lxc = config.LxcSpec{}
	if _, err := resolveUninstall(&noCp, ""); err == nil {
		t.Fatal("a missing CP vmid must refuse on the local-runner path")
	}
	noCpThin := *base
	noCpThin.Runner = config.RunnerRef{}
	noCpThin.Lxc = config.LxcSpec{}
	if _, err := resolveUninstall(&noCpThin, ""); err != nil {
		t.Fatalf("transient path must not require the CP vmid: %v", err)
	}
}

// TestWipeLocalProfileRefusesForeignDir guards against wiping the parent of an
// arbitrary --config: with no profile, only the config file itself is removed.
func TestWipeLocalProfileRefusesForeignDir(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(sibling, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	wipeLocalProfile(cfg, nil)
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatal("config file not removed")
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatal("sibling file must survive")
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
	wipeLocalProfile(filepath.Join(cfgDir, "config.toml"), &config.Profile{Name: "demo", ConfigPath: filepath.Join(cfgDir, "config.toml"), StateDir: stateDir})
	if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
		t.Fatal("profile config dir not wiped")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("profile state dir not wiped")
	}
}
