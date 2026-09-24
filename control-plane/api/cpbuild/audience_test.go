package cpbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/state"
)

// TestAppendAgentToolsAudience pins the drift report line: the live agent-tools
// identity vs the pubkey the console state recorded from the box's profile must
// read plainly on every bring-up. A re-minted durable identity strands every
// existing pod's signature (every CP tool call fails "-32001 signature does not
// verify") while the world otherwise looks healthy — the line is what makes
// that world detectable instead of silently broken.
func TestAppendAgentToolsAudience(t *testing.T) {
	dir := t.TempDir()
	// The production layout: StateDir is the agent-tools dir, the console
	// state is its control-plane sibling.
	stateDir := filepath.Join(dir, "agent-tools")
	consoleDir := filepath.Join(dir, "control-plane")
	for _, d := range []string{stateDir, consoleDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	live, stale := "aa11aa11aa11aa11", "bb22bb22bb22bb22"
	spec := &Spec{StateDir: stateDir, Audience: live}

	// A fresh world with no console state: nothing recorded to compare, and
	// the check must not CREATE the state as a side effect of looking.
	line := spec.appendAgentToolsAudience(nil)
	if len(line) != 1 || !strings.Contains(line[0], "no console state") {
		t.Fatalf("no-state report = %q, want one no-console-state line", line)
	}
	if _, err := os.Stat(filepath.Join(consoleDir, state.StateFile)); !os.IsNotExist(err) {
		t.Fatalf("the check must not create the console state as a side effect (stat err: %v)", err)
	}

	store, err := state.Open(consoleDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgentToolsPubkey(&live); err != nil {
		t.Fatal(err)
	}
	if line := spec.appendAgentToolsAudience(nil); len(line) != 1 || !strings.Contains(line[0], "matches") {
		t.Fatalf("match report = %q, want a clean match line", line)
	}

	if err := store.SetAgentToolsPubkey(&stale); err != nil {
		t.Fatal(err)
	}
	line = spec.appendAgentToolsAudience(nil)
	if len(line) != 1 || !strings.HasPrefix(line[0], "WARN:") || !strings.Contains(line[0], "AUDIENCE DRIFT") {
		t.Fatalf("drift report = %q, want a WARN AUDIENCE DRIFT line", line)
	}
	if strings.Contains(line[0], live) || !strings.Contains(line[0], stale[:12]) {
		t.Fatalf("drift report must name the recorded pubkey, not the live one verbatim: %q", line[0])
	}

	// A spec that never carried a live audience and has no durable identity
	// resolves to nothing — silent.
	if got := (&Spec{StateDir: stateDir}).appendAgentToolsAudience(nil); got != nil {
		t.Fatalf("empty-audience spec must stay silent, got %q", got)
	}

	// The console-executor resolution: the live audience comes from the
	// durable agent-tools identity ON DISK (what the serve actually verifies
	// against), never from Spec.Audience — the console executor's Spec.Audience
	// is its own runner-signing identity, and a manifest stamped with it signs
	// a dead audience forever. Mint a fresh identity and the line must name IT.
	disk, err := agent.EnsureIdentity(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	line = spec.appendAgentToolsAudience(nil)
	if len(line) != 1 || !strings.Contains(line[0], agenttools.ShortHex(disk)) {
		t.Fatalf("resolved-audience report = %q, want the disk identity's pubkey named", line)
	}
}
