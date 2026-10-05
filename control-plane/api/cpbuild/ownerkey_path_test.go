package cpbuild

import "testing"

// TestSealedOwnerPathIsTheConsoleRoot: the sealed operator identity's ONE home
// is the CONSOLE's world-secrets (where the box's build PutSecrets lands). The
// agent-tools server's Spec is rooted at its OWN state dir — keying the lookup
// off StateDir silently missed the record there and refused every
// tool-created agent on the console-identity fallback (seen live). Both Spec
// roots must resolve the SAME path.
func TestSealedOwnerPathIsTheConsoleRoot(t *testing.T) {
	console := &Spec{StateDir: "/srv/data/cp/control-plane"} // the console's own spec
	agentTools := &Spec{StateDir: "/srv/data/cp/agent-tools"} // the tool server's spec

	want := "/srv/data/cp/control-plane/world-secrets/operator.json"
	if got := console.sealedOwnerPath(); got != want {
		t.Errorf("console-rooted spec: sealed path = %q, want %q", got, want)
	}
	if got := agentTools.sealedOwnerPath(); got != want {
		t.Errorf("agent-tools-rooted spec: sealed path = %q, want %q — a StateDir-keyed lookup misses the record and falls back to the console identity", got, want)
	}
}
