package cli

import (
	"os"
	"path/filepath"
	"testing"

	"freehold/contract/config"
)

// TestResolveUninstall covers the uninstall preconditions: --host wins over the
// recorded host, a missing host / thin box / missing CP vmid each refuse.
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
	if _, err := resolveUninstall(&noHost, ""); err == nil {
		t.Fatal("a missing host must refuse")
	}
	thin := *base
	thin.Runner = config.RunnerRef{}
	if _, err := resolveUninstall(&thin, ""); err == nil {
		t.Fatal("a thin box (no local runner) must refuse — the PR3 path")
	}
	noCp := *base
	noCp.Lxc = config.LxcSpec{}
	if _, err := resolveUninstall(&noCp, ""); err == nil {
		t.Fatal("a missing CP vmid must refuse")
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
