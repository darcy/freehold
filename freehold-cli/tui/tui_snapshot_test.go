package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"freehold/providers/proxmox/drive"
)

// TestSnapshotRollbackLabels pins the rollback form's two prompts: the
// picker step (blank = newest) and the destructive confirm.
func TestSnapshotRollbackLabels(t *testing.T) {
	if got := promptLabel(flowSnapshotRollback, 0); got != "snapshot to roll back to (blank = newest — names listed above)" {
		t.Errorf("rollback step0 label = %q", got)
	}
	if got := promptLabel(flowSnapshotRollback, 1); got != "CONFIRM rolling back ALL durable-plane data (guests stop; ZFS destroys newer snapshots; LVM block-copies)? type yes" {
		t.Errorf("rollback step1 label = %q", got)
	}
	if n := ncols(flowSnapshotRollback); n != 2 {
		t.Errorf("rollback form must be 2 steps, got %d", n)
	}
}

// TestSnapshotRollbackConfirmGates: the confirm step must refuse anything
// but an explicit "yes" — enter alone aborts without dispatching.
func TestSnapshotRollbackConfirmGates(t *testing.T) {
	m := &Model{Mode: ModeRunning}
	m.beginPrompt(flowSnapshotRollback)
	m.Flow.SnapList = []string{"fh-b", "fh-a"}
	m.Flow.Field.SetValue("fh-a")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // step 0 -> confirm
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("the confirm step must return its verdict as a cmd")
	}
	msg := cmd()
	flow, ok := msg.(flowMsg)
	if !ok || flow.err == nil {
		t.Fatalf("a blank confirm must abort with an error, got %T %+v", msg, msg)
	}
	if m.activity != nil {
		t.Fatal("a blank confirm must not start a rollback")
	}
}

// TestSnapshotRollbackDispatch: an explicit yes dispatches the CLI rollback
// with the picked name + --yes (the activity streams the CLI's own output).
func TestSnapshotRollbackDispatch(t *testing.T) {
	m := &Model{Mode: ModeRunning}
	m.beginPrompt(flowSnapshotRollback)
	m.Flow.SnapList = []string{"fh-b", "fh-a"}
	m.Flow.Field.SetValue("fh-a")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Flow.Field.SetValue("yes")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("an explicit yes must dispatch")
	}
	start, ok := cmd().(activityStartMsg)
	if !ok {
		t.Fatal("dispatch must be an activity start")
	}
	want := []string{"snapshot", "rollback", "--to", "fh-a", "--yes"}
	for i, w := range want {
		if start.args[i] != w {
			t.Errorf("rollback args wrong: %v", start.args)
			break
		}
	}
}

// TestParseSnapshotList: the CLI's --json payload (lowercase keys —
// SnapshotInfo's tags) decodes to the full info, consumption included.
func TestParseSnapshotList(t *testing.T) {
	payload := `{"snapshots":[{"name":"fh-20261001T102717Z","created":1791850000,"present":4,"volumes":4,"used":"0.42% of pool"},{"name":"fh-partial","created":1791840000,"present":2,"volumes":4}]}`
	snaps, ok := parseSnapshotList(payload)
	if !ok || len(snaps) != 2 {
		t.Fatalf("parse: ok=%v n=%d", ok, len(snaps))
	}
	if snaps[0].Name != "fh-20261001T102717Z" || snaps[0].Used != "0.42% of pool" || snaps[0].Present != 4 {
		t.Errorf("snapshot info wrong: %+v", snaps[0])
	}
	if _, ok := parseSnapshotList("not json"); ok {
		t.Error("garbage must not parse")
	}
}

// TestRunSelfRefusesInTests: the guard that stopped the fork bomb — runSelf
// inside a test binary must refuse (os.Executable IS the test binary;
// re-running it re-runs the suite, which re-runs it…).
func TestRunSelfRefusesInTests(t *testing.T) {
	if _, err := runSelf("snapshot", "--list"); err == nil {
		t.Fatal("runSelf must refuse inside tests")
	}
}

// TestDataViewRendersSnapshots: the DATA view's second table — name, taken,
// consumed, coverage; a partial is marked; an empty plane gets the hint.
func TestDataViewRendersSnapshots(t *testing.T) {
	m := &Model{Mode: ModeRunning, Domain: "world.test", ActiveView: ViewData,
		DataCap: "vg pve", DataAt: time.Now(),
		Snapshots: []drive.SnapshotInfo{
			{Name: "fh-full", Created: 1791850000, Present: 4, Volumes: 4, Used: "0.42% of pool"},
			{Name: "fh-part", Created: 1791840000, Present: 2, Volumes: 4},
		}}
	out := m.View()
	for _, want := range []string{"fh-full", "0.42% of pool", "4/4", "2/4 PARTIAL", "S rolls back"} {
		if !strings.Contains(out, want) {
			t.Errorf("the DATA view must render %q", want)
		}
	}
	empty := &Model{Mode: ModeRunning, ActiveView: ViewData, DataAt: time.Now()}
	if !strings.Contains(empty.View(), "no snapshots") {
		t.Error("an empty snapshot list must show the hint")
	}
}
