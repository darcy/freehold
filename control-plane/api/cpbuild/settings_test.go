package cpbuild

import (
	"path/filepath"
	"testing"

	"freehold/control-plane/state"
)

// TestOperatorTZRenamesAcrossExecutors: the settings read must resolve the CP
// state root from BOTH Spec shapes — the console executor's (StateDir IS the
// CP root) and the in-serve agent-tools one (StateDir is <root>/agent-tools,
// the sibling inference agentToolsRoot runs in reverse). The value must read
// FRESH per call: a settings edit lands on the next pod apply without a serve
// restart.
func TestOperatorTZResolvesAcrossExecutors(t *testing.T) {
	root := t.TempDir()
	cpDir := filepath.Join(root, "control-plane")
	atDir := filepath.Join(root, "agent-tools")

	store, err := state.Open(cpDir)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}

	executor := &Spec{StateDir: cpDir}
	if got := executor.operatorTZ(); got != "" {
		t.Fatalf("unset setting read %q, want empty", got)
	}
	inServe := &Spec{StateDir: atDir}
	if got := inServe.operatorTZ(); got != "" {
		t.Fatalf("in-serve unset setting read %q, want empty", got)
	}

	if err := store.SetSettings(&state.Settings{OperatorTZ: "America/Chicago"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	for name, spec := range map[string]*Spec{"executor": executor, "in-serve": inServe} {
		if got := spec.operatorTZ(); got != "America/Chicago" {
			t.Fatalf("%s operatorTZ = %q, want America/Chicago", name, got)
		}
	}

	// Fresh per call: an edit (another process writing state.json) is visible
	// on the next read.
	if err := store.SetSettings(&state.Settings{OperatorTZ: "Europe/Berlin"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := executor.operatorTZ(); got != "Europe/Berlin" {
		t.Fatalf("after edit operatorTZ = %q, want Europe/Berlin", got)
	}
}
