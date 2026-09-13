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
