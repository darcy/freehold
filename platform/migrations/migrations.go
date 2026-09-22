// Package migrations runs the CP's repair/catch-up scripts — the Omarchy
// pattern, followed closely:
//
//   - one `<epoch>.sh` file per migration (epoch = the timestamp/filename
//     identity), run with `bash -euo pipefail`, no shebang, mode 0644;
//   - a completion MARKER file named for the script (an empty file), so a
//     migration done on one channel/version stays done when you switch;
//   - strictly ascending epoch order; a failure exits non-zero, stays
//     unmarked, stops the queue, and is retried next run.
//
// Layout under the console's state dir:
//
//	<root>/scripts/<epoch>.sh   the shipped scripts (install/update copy them)
//	<root>/<epoch>.sh           the completion marker (same filename)
//
// There is no verify gate and no JSON ledger: apply success IS done, so "ran"
// and "converged" are not distinguished (Omarchy's rule). Every world runs every
// shipped script exactly once — a FRESH install included, at the end of world
// bring-up (never at CP deploy, when the relay, k3s, the agent identities and the
// agent-tools serve do not exist yet to operate on). A script therefore MUST be
// idempotent: safe to re-run, and safe on a world where the thing it trues up was
// never absent. Nothing marks a script done without running it.
package migrations

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ScriptsSubdir is the subdir of the migrations root that holds the shipped
// scripts; markers live directly in the root.
const ScriptsSubdir = "scripts"

// ScriptsRoot returns the directory the shipped scripts live in.
func ScriptsRoot(root string) string { return filepath.Join(root, ScriptsSubdir) }

// Scripts enumerates `<root>/scripts/*.sh` in ascending filename order. Only
// decimal-epoch filenames are migrations; any other helper `.sh` is ignored.
func Scripts(root string) ([]string, error) {
	dir := ScriptsRoot(root)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".sh") || !epochOnly(strings.TrimSuffix(n, ".sh")) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// epochOnly reports whether a basename is a pure decimal epoch.
func epochOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Done reports whether the completion marker for name exists.
func Done(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

// Mark touches the completion marker for name (empty file, 0600).
func Mark(root, name string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, name), nil, 0o600)
}

// Pending returns the scripts (ascending) whose marker is absent.
func Pending(root string) ([]string, error) {
	all, err := Scripts(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range all {
		if !Done(root, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// PendingCount returns len(Pending(root)).
func PendingCount(root string) (int, error) {
	p, err := Pending(root)
	return len(p), err
}

// Result is one migration's outcome for a run.
type Result struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Err  string `json:"error,omitempty"`
	// Applied reports whether the script actually ran this round (vs already done).
	Applied bool `json:"applied,omitempty"`
}

// Run executes every pending script in ascending order via run, marking each on
// success and stopping at the first failure (which stays unmarked, so the next
// run retries it). run receives the script name and its absolute path.
func Run(root string, run func(name, path string) error) ([]Result, error) {
	pending, err := Pending(root)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(pending))
	for _, name := range pending {
		res := Result{Name: name, Applied: true}
		if err := run(name, filepath.Join(ScriptsRoot(root), name)); err != nil {
			res.Err = err.Error()
			results = append(results, res)
			return results, fmt.Errorf("%s: %w", name, err)
		}
		if err := Mark(root, name); err != nil {
			res.Err = err.Error()
			results = append(results, res)
			return results, fmt.Errorf("%s: mark: %w", name, err)
		}
		res.OK = true
		results = append(results, res)
	}
	return results, nil
}
