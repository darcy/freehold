package stages

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// Regression (operator report 2026-08-30 — "! teardown failed: — the pool is
// NOT removed"): teardown's DestroyPool drives this binary with the shared
// self-staged flags (--addr / --agent-dir / --transient / --host). Every
// `storage` subcommand — including destroy — must register them, or the
// --data teardown dies on the pool step with "unknown flag: --addr".
func TestStorageSubcommandsAcceptSelfStageFlags(t *testing.T) {
	for _, sc := range []*cobra.Command{
		storageResolveCmd, storageEnsureCmd, storageDestroyCmd,
		storageDestroyPoolCmd, storageInfoCmd,
	} {
		for _, flag := range []string{"addr", "agent-dir", "target", "transient", "host"} {
			if sc.Flags().Lookup(flag) == nil {
				t.Errorf("storage %s must register --%s", sc.Name(), flag)
			}
		}
	}
}

// TestStateRootForDerivesProfileRoot proves a self-staged child recovers the
// profile state root from the agent-dir it is handed, and falls back to the
// default root for a non-conventional dir.
func TestStateRootForDerivesProfileRoot(t *testing.T) {
	root := filepath.Join("/srv", "data", "profiles", "librem2")
	if got := stateRootFor(filepath.Join(root, "control-plane", "agent-ops")); got != root {
		t.Errorf("stateRootFor = %q, want %q", got, root)
	}
	t.Setenv("FREEHOLD_HOME", "/tmp/fh-state-root")
	if got := stateRootFor("/custom/identity"); got != "/tmp/fh-state-root" {
		t.Errorf("non-conventional agent-dir must fall back to the default root, got %q", got)
	}
}
