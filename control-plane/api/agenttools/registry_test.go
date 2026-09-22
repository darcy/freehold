package agenttools

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestRegistryPurposeSurvives verifies the E3 path concretely: a created agent's
// purpose is persisted on its registry row and survives a re-open (the rebuild
// reconciler reads it back to recreate the system prompt with the purpose intact).
func TestRegistryPurposeSurvives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RegisterAgent("helper", "aa55", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPurpose("helper", "helps with installs"); err != nil {
		t.Fatal(err)
	}

	// Re-open from disk (a fresh process / rebuild reconciler reading it back).
	r2, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := r2.Agents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Name != "helper" || agents[0].Purpose != "helps with installs" {
		t.Fatalf("purpose not preserved across re-open: %+v", agents)
	}
}

// TestRegistrySetPurposeBeforeRegister rejects setting purpose on an unknown row.
func TestRegistrySetPurposeBeforeRegister(t *testing.T) {
	r, err := OpenRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetPurpose("ghost", "purpose"); err == nil {
		t.Fatal("SetPurpose on an unnamed row should error")
	}
}

// TestRegistryChannelsSurvive: a multi-channel/private create persists its full
// channel list + private flag on the registry row and survives a re-open, so a
// rebuild rejoins every channel (not just the primary) with the right visibility.
func TestRegistryChannelsSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RegisterAgent("helper", "aa55", "#ops"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetChannels("helper", []string{"#ops", "#extra"}, true); err != nil {
		t.Fatal(err)
	}
	r2, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := r2.Agents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || len(agents[0].Channels) != 2 || !agents[0].Private {
		t.Fatalf("channels/private not preserved across re-open: %+v", agents)
	}
	if err := r2.SetChannels("ghost", nil, false); err == nil {
		t.Fatal("SetChannels on an unnamed row should error")
	}
}

// TestWithRegistryLockedExcludesConcurrentRosterWrite pins the property the migration
// queue actually depends on, and which a plain re-read does NOT give: while the
// out-of-band scripts are editing registry.json, no roster write may land. A migration
// script runs as its own process with its own handle and knows nothing about this
// serve's lock, so a roster write interleaving into that window has its save() put
// this process's STALE rows on disk over the script's edit — and the later re-read
// then faithfully loads the clobbered state and reports it converged, the exact
// silent loss the queue exists to prevent. Holding mu across the whole window is what
// forces such a write to wait until the scripts are done.
//
// The competing write is started INSIDE the held window, so this asserts the property
// itself rather than a timing accident: it cannot complete until the window releases.
func TestWithRegistryLockedExcludesConcurrentRosterWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RegisterAgent("cpa", "PUB_CPA", "#freehold"); err != nil {
		t.Fatal(err)
	}
	// The out-of-band writer: a second handle to the same file, standing in for the
	// migration script's `registry import-console`.
	other, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}

	const late = "latecomer"
	wDone := make(chan struct{})
	if err := r.WithRegistryLocked(func() error {
		if _, err := other.RegisterAgent("scripted", "PUB_SCRIPTED", "#freehold-ai"); err != nil {
			return fmt.Errorf("out-of-band write: %w", err)
		}
		go func() {
			if _, err := r.RegisterAgent(late, "PUB_LATE", "#late"); err != nil {
				t.Errorf("blocked RegisterAgent: %v", err)
			}
			close(wDone)
		}()
		// The window is held: the roster write must still be queued. Release the
		// lock and this branch fires.
		select {
		case <-wDone:
			return fmt.Errorf("a roster write landed inside the migration window")
		case <-time.After(200 * time.Millisecond):
		}
		return nil
	}); err != nil {
		t.Fatalf("WithRegistryLocked: %v", err)
	}

	select {
	case <-wDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued roster write never ran once the window was released")
	}

	// The serve's memory ends up holding the script's edit rather than its own
	// pre-window rows — which is what stops its next save() from clobbering it.
	got, err := r.Agents()
	if err != nil {
		t.Fatal(err)
	}
	inMemory := map[string]bool{}
	for _, a := range got {
		inMemory[a.Name] = true
	}
	if !inMemory["scripted"] {
		t.Fatalf("the migration's edit is not in memory: %+v", got)
	}
	// And no one's work was lost on disk: the pre-existing row, the script's, and the
	// write that had to wait for it.
	disk, err := OpenRegistry(path)
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
	for _, want := range []string{"cpa", "scripted", late} {
		if !onDisk[want] {
			t.Fatalf("%s missing from the durable registry: %+v", want, rows)
		}
	}
}

// TestWithRegistryLockedReReadsAfterFailure pins that memory is re-read even when the
// out-of-band work bailed out midway: a queue that stopped at a failing script still
// moved the durable file (the scripts before it got marked), so memory must match the
// file rather than go on serving rows from before the run. The caller's own error is
// what is returned, not the re-read's.
func TestWithRegistryLockedReReadsAfterFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RegisterAgent("cpa", "PUB_CPA", "#freehold"); err != nil {
		t.Fatal(err)
	}
	other, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.RegisterAgent("latecomer", "PUB_LATE", "#late"); err != nil {
		t.Fatal(err)
	}

	sentinel := fmt.Errorf("the script failed halfway")
	if err := r.WithRegistryLocked(func() error { return sentinel }); err != sentinel {
		t.Fatalf("the caller's own error must be returned, got %v", err)
	}
	got, err := r.Agents()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("memory was not re-read after a failed run: %+v", got)
	}
}
