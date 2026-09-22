package cpbuild

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"freehold/control-plane/api/agenttools"
)

// TestBuildMigratorExcludesRosterWritesUnderTheLock pins the wiring the
// world_migrate tool depends on — the path `freehold update` drives, which has no
// world_build around it to provide the guarantee. The Registry-level test proves the
// critical section works; this proves migrationRunner actually ENTERS it, since the
// queue's scripts edit the very file the serve holds in memory. Drop the
// WithRegistryLocked call from migrationRunner and the concurrent roster write lands
// mid-queue, saving the serve's pre-migration rows and dropping the script's import.
func TestBuildMigratorExcludesRosterWritesUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "agent-tools")
	consoleDir := filepath.Join(dir, "control-plane")
	for _, d := range []string{stateDir, consoleDir, filepath.Join(dir, "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// cpGuestDirs derives binDir from filepath.Dir(StateDir).
	stub := filepath.Join(dir, "bin", "freehold-agent-tools")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nset -eu\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	reg, err := agenttools.OpenRegistry(filepath.Join(stateDir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("cpa", "PUB_CPA", "#freehold"); err != nil {
		t.Fatal(err)
	}

	// The script edits the registry out-of-band (its own write, exactly as
	// `registry import-console` does), announces it is mid-flight, then holds on so
	// a competing roster write can be observed against the held lock.
	started := filepath.Join(dir, "started")
	scripts := filepath.Join(consoleDir, "migrations", "scripts")
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(
		"echo '{\"cpa\":{\"name\":\"cpa\",\"pubkey\":\"PUB_CPA\"},\"imported\":{\"name\":\"imported\",\"pubkey\":\"PUB_IMPORTED\"}}' > \"$REGISTRY\"\n"+
			"touch %q\n"+
			"sleep 3\n", started)
	if err := os.WriteFile(filepath.Join(scripts, "1800000000.sh"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := &Spec{StateDir: stateDir, AgentRegistry: reg}
	done := make(chan struct{})
	var merr error
	go func() {
		defer close(done)
		_, merr = BuildMigrator(spec, consoleDir)()
	}()

	// Wait until the script has actually done its write, so the race window is real
	// rather than inferred from a sleep.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the migration script never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A roster write now in flight must wait for the queue, not interleave.
	wDone := make(chan struct{})
	go func() {
		if _, err := reg.RegisterAgent("late", "PUB_LATE", "#late"); err != nil {
			t.Errorf("blocked RegisterAgent: %v", err)
		}
		close(wDone)
	}()
	select {
	case <-wDone:
		t.Fatal("a roster write landed while the migration scripts were editing the registry")
	case <-time.After(400 * time.Millisecond):
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the migrator never returned")
	}
	if merr != nil {
		t.Fatalf("BuildMigrator: %v", merr)
	}
	select {
	case <-wDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the queued roster write never ran once the queue released the lock")
	}

	// The queue's own re-read is what keeps the serve serving the script's import.
	mem, err := reg.Agents()
	if err != nil {
		t.Fatal(err)
	}
	inMem := map[string]bool{}
	for _, a := range mem {
		inMem[a.Name] = true
	}
	if !inMem["imported"] {
		t.Fatalf("the serve did not re-read the script's import: %+v", mem)
	}
	// And the durable file kept every contributor's work.
	disk, err := agenttools.OpenRegistry(filepath.Join(stateDir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := disk.Agents()
	if err != nil {
		t.Fatal(err)
	}
	onDisk := map[string]bool{}
	for _, a := range rows {
		onDisk[a.Name] = true
	}
	for _, want := range []string{"cpa", "imported", "late"} {
		if !onDisk[want] {
			t.Fatalf("%s missing from the durable registry — its write was clobbered: %+v", want, rows)
		}
	}
}

// TestBuildMigratorWithoutAnInProcessRegistryStillRuns pins the other branch: the
// console-executor build path carries no AgentRegistry (its serve is a separate
// process that is restarted after the queue), so the queue must run WITHOUT trying to
// take a lock it does not have. A nil-guard regression here would silently skip the
// whole queue on that path — the same silent-success shape as the original bug.
func TestBuildMigratorWithoutAnInProcessRegistryStillRuns(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "agent-tools")
	consoleDir := filepath.Join(dir, "control-plane")
	for _, d := range []string{stateDir, consoleDir, filepath.Join(dir, "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "freehold-agent-tools"),
		[]byte("#!/bin/sh\nset -eu\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	scripts := filepath.Join(consoleDir, "migrations", "scripts")
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(dir, "ran")
	body := fmt.Sprintf("touch %q\n", ran)
	if err := os.WriteFile(filepath.Join(scripts, "1800000001.sh"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := BuildMigrator(&Spec{StateDir: stateDir}, consoleDir)()
	if err != nil {
		t.Fatalf("BuildMigrator with no in-process registry: %v", err)
	}
	if len(res) != 1 || !res[0].OK || !res[0].Applied {
		t.Fatalf("the queue did not run on the no-registry path: %+v", res)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("the script never executed: %v", err)
	}
}
