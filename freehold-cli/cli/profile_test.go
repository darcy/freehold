package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"freehold/contract/config"
)

// TestTeardownFailsClosedNoProfiles guards the destructive-path guardrail: with
// zero registered profiles (no legacy single config under the new layout),
// `freehold teardown` must fail closed rather than silently acting on a
// leftover default config.
func TestTeardownFailsClosedNoProfiles(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { config.SetCurrent(nil) })
	if code := Run([]string{"teardown"}); code == 0 {
		t.Fatal("teardown with zero profiles must fail closed (return non-zero)")
	}
}

// TestExecConfigFlagScopesToOwningProfile guards the teardown bug: an explicit
// --config must scope the state dir to the OWNING profile, not the base home.
// A stale base-layout runner package (<base>/runner/<target>) otherwise wins
// the runner-pubkey lookup, and the box signs against the wrong audience
// ("signature does not verify"). The teardown engine always drives its child
// `freehold exec` with --config, so this is the path that broke.
func TestExecConfigFlagScopesToOwningProfile(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { config.SetCurrent(nil) })

	name := "librem3"
	profileCfg := config.NewProfilePath(name)
	if err := (&config.Config{CPURL: "https://cp.example"}).Save(profileCfg); err != nil {
		t.Fatal(err)
	}
	// The profile's real runner package.
	profilePkg := filepath.Join(config.NewProfileState(name), "runner", "proxmox-box")
	if err := mintAgentIdentity(profilePkg); err != nil {
		t.Fatal(err)
	}
	// A stale base-layout runner package that previously shadowed it.
	basePkg := filepath.Join(config.DefaultStateHome(), "runner", "proxmox-box")
	if err := mintAgentIdentity(basePkg); err != nil {
		t.Fatal(err)
	}
	profilePK, _ := loadRPubkey(profilePkg)
	basePK, _ := loadRPubkey(basePkg)
	if profilePK == basePK {
		t.Fatal("test setup: identities must differ")
	}

	cmd := &cobra.Command{Use: "exec"}
	cmd.Flags().String("config", config.ConfigPath(), "config path")
	if err := cmd.Flags().Set("config", profileCfg); err != nil {
		t.Fatal(err)
	}
	if err := resolveExecProfile(cmd); err != nil {
		t.Fatalf("resolveExecProfile: %v", err)
	}
	if got := defaultAgentDir(); !strings.Contains(got, filepath.Join("profiles", name, "control-plane", "agent-ops")) {
		t.Fatalf("agent-dir not scoped to owning profile: %s", got)
	}
	pk, err := resolveRunnerPubkey("proxmox-box", "")
	if err != nil {
		t.Fatal(err)
	}
	if pk == basePK {
		t.Fatalf("runner pubkey resolved to the stale base package (%s) — signature would not verify", basePK)
	}
	if pk != profilePK {
		t.Fatalf("runner pubkey = %s, want the owning profile's %s", pk, profilePK)
	}
}

// TestExecSingleProfileScopesAgentDir guards the BLOCKING review finding: exec
// negotiation must scope BOTH the runner-pubkey lookup AND the signing identity
// dir (--agent-dir) to the negotiated profile, not leave agent-dir on an
// init-time default frozen before any profile was chosen.
func TestExecSingleProfileScopesAgentDir(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { config.SetCurrent(nil) })

	name := "prod"
	if err := (&config.Config{CPURL: "https://cp.example"}).Save(config.NewProfilePath(name)); err != nil {
		t.Fatal(err)
	}
	if config.Resolve(name) == nil {
		t.Fatal("test profile not registered")
	}

	cmd := &cobra.Command{Use: "exec"}
	cmd.Flags().String("config", config.ConfigPath(), "config path")

	if err := resolveExecProfile(cmd); err != nil {
		t.Fatalf("resolveExecProfile: %v", err)
	}
	// Tenant config scoped to the profile.
	if got := config.ConfigPath(); got != config.NewProfilePath(name) {
		t.Fatalf("config path not scoped to profile: %s", got)
	}
	// The signing identity dir resolves into the profile's scoped state dir.
	got := defaultAgentDir()
	if !strings.Contains(got, filepath.Join("profiles", name, "control-plane", "agent-ops")) {
		t.Fatalf("agent-dir not scoped to profile: %s", got)
	}
}
