package migrations

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeScript drops a script into <root>/scripts/<name>.
func writeScript(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.MkdirAll(ScriptsRoot(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ScriptsRoot(root), name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScriptsAscendingAndHelpersIgnored(t *testing.T) {
	root := t.TempDir()
	writeScript(t, root, "1799900002.sh", "two")
	writeScript(t, root, "1799900001.sh", "one")
	writeScript(t, root, "not-a-migration.sh", "helper")

	got, err := Scripts(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1799900001.sh", "1799900002.sh"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Scripts = %v, want %v", got, want)
	}
}

func TestMarkAllLeavesNothingPendingAndRunsNothing(t *testing.T) {
	root := t.TempDir()
	writeScript(t, root, "1799900001.sh", "one")
	writeScript(t, root, "1799900002.sh", "two")

	n, err := MarkAll(root)
	if err != nil || n != 2 {
		t.Fatalf("MarkAll = %d, %v; want 2, nil", n, err)
	}
	if p, _ := Pending(root); len(p) != 0 {
		t.Fatalf("all marked, pending = %v", p)
	}
	// No apply function was ever passed — MarkAll must not run the scripts.
}

func TestRunAppliesPendingInOrderStopsOnFailureAndRetries(t *testing.T) {
	root := t.TempDir()
	writeScript(t, root, "1799900001.sh", "one")
	writeScript(t, root, "1799900002.sh", "two")
	writeScript(t, root, "1799900003.sh", "three")

	var order []string
	run := func(name, path string) error {
		order = append(order, name)
		if name == "1799900002.sh" {
			return fmt.Errorf("boom")
		}
		return nil
	}
	res, err := Run(root, run)
	if err == nil {
		t.Fatal("expected an error when a script fails")
	}
	if fmt.Sprint(order) != fmt.Sprint([]string{"1799900001.sh", "1799900002.sh"}) {
		t.Fatalf("order = %v (must stop at the failure)", order)
	}
	if len(res) != 2 || !res[0].OK || res[1].OK {
		t.Fatalf("results = %+v", res)
	}
	if !Done(root, "1799900001.sh") {
		t.Fatal("the script before the failure must be marked done")
	}
	if Done(root, "1799900002.sh") {
		t.Fatal("the failed script must NOT be marked done")
	}

	// Next run retries only the failed + later scripts.
	order = nil
	if _, err := Run(root, func(name, path string) error { order = append(order, name); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(order) != fmt.Sprint([]string{"1799900002.sh", "1799900003.sh"}) {
		t.Fatalf("retry order = %v, want the two unmarked", order)
	}
}

// TestMarkerSurvivesChannelSwitch proves a marker is a durable per-script file,
// not version-stamped: switching to a different tree's scripts (the same root,
// a replaced scripts/ dir) keeps an already-done script done.
func TestMarkerSurvivesChannelSwitch(t *testing.T) {
	root := t.TempDir()
	writeScript(t, root, "1799900001.sh", "one")
	writeScript(t, root, "1799900002.sh", "two")
	if _, err := Run(root, func(string, string) error { return nil }); err != nil {
		t.Fatal(err)
	}

	// "Switch channel": replace scripts/ with a NEW release's set (001 + a new
	// 003), same marker root.
	if err := os.RemoveAll(ScriptsRoot(root)); err != nil {
		t.Fatal(err)
	}
	writeScript(t, root, "1799900001.sh", "one (new release)")
	writeScript(t, root, "1799900003.sh", "three (new release)")

	if pending, _ := Pending(root); fmt.Sprint(pending) != fmt.Sprint([]string{"1799900003.sh"}) {
		t.Fatalf("pending = %v, want only the new script", pending)
	}
}
