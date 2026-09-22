package cpbuild

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeScript drops one migration script body into dir and returns its path. The
// scripts are read by bash rather than exec'd directly, so the mode is the shipped
// 0644 rather than an executable bit.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunMigrationScriptBoundsAHangingScript pins the reason the queue cannot be
// allowed to run unbounded: world_build invokes it while HOLDING the registry's write
// lock, so however long one script hangs is how long every roster read and write in
// the serve is blocked. A script that never exits on its own must therefore still
// return, as an error — the ordinary queue failure path: unmarked, retried next
// bring-up. `exec` replaces the shell with the sleeper, so the process the timeout
// kills is the one holding the output pipe and the call returns on the deadline
// itself (the orphan case is the next test).
func TestRunMigrationScriptBoundsAHangingScript(t *testing.T) {
	spec := &Spec{StateDir: filepath.Join(t.TempDir(), "agent-tools")}
	path := writeScript(t, t.TempDir(), "9000000000.sh", "exec sleep 60\n")

	start := time.Now()
	err := spec.runMigrationScript(path, os.Environ(), 300*time.Millisecond, 5*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a script that hangs must return an error, not succeed")
	}
	// The generous wait delay must not be what dominates: the timeout does the killing
	// and nothing is left holding the pipe, so this returns on the deadline.
	if elapsed > time.Second {
		t.Fatalf("the script was not bounded by the timeout (took %v): %v", elapsed, err)
	}
}

// TestRunMigrationScriptReturnsDespiteAnOrphanedDescendant is the half a bare context
// timeout does NOT cover: the timeout kills the script's shell, but a backgrounded
// descendant inherits the output pipe and holds its write end open, which keeps
// CombinedOutput blocked past the deadline — and it is blocked inside the critical
// section holding the registry lock. WaitDelay is the escape, so the call returns in
// roughly timeout+waitDelay rather than waiting out the descendant's 30s life.
func TestRunMigrationScriptReturnsDespiteAnOrphanedDescendant(t *testing.T) {
	spec := &Spec{StateDir: filepath.Join(t.TempDir(), "agent-tools")}
	path := writeScript(t, t.TempDir(), "9000000001.sh", "sleep 30 &\nsleep 60\n")

	start := time.Now()
	err := spec.runMigrationScript(path, os.Environ(), 300*time.Millisecond, 300*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a script that hangs must return an error, not succeed")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("returned only after %v — the orphaned descendant still held the output "+
			"pipe, so the registry lock would be pinned for that long too", elapsed)
	}
}
