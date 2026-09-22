package cpdeploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"freehold/contract/client"
)

// shipRecorder models the CP guest FS for the migration-shipping surface. It
// records BOTH paths a ship can take: the base64 write ShipMigrations uses for a
// script, and the `for f in *.sh; … : > "<root>/$f"` touch the pre-fix install
// used to mark every shipped script done without running it. Recording the
// second is what lets the invariant below fail on that behaviour.
type shipRecorder struct {
	files map[string]string
}

var (
	reShipWrite   = regexp.MustCompile(`printf %s "[A-Za-z0-9+/=]+" \| base64 -d > ([^ ]+)`)
	reMarkerTouch = regexp.MustCompile(`: > "([^"]+)"`)
)

func (s *shipRecorder) Exec(cmd string, _ uint64) (*client.ExecOutcome, error) {
	code := 0
	if m := reShipWrite.FindStringSubmatch(cmd); m != nil {
		s.files[m[1]] = "shipped"
		return &client.ExecOutcome{ExitCode: &code}, nil
	}
	// The marker loop writes one path per pending script, straight into the
	// migrations root. Record each: a path there means "declared already run".
	if strings.Contains(cmd, "for f in *.sh") {
		for _, seg := range strings.Split(cmd, ";") {
			if m := reMarkerTouch.FindStringSubmatch(seg); m != nil {
				s.files[m[1]] = "marker"
			}
		}
	}
	return &client.ExecOutcome{ExitCode: &code}, nil
}

func (s *shipRecorder) Upload(_, _ string, _ uint64) (uint64, error) { return 0, nil }

// TestShipMigrationsShipsScriptsOnly pins the rule that replaced "mark everything
// done": install copies the scripts and touches NOTHING outside the scripts
// subdir. A marker asserts "this world ran this script", and nothing has run at
// deploy time — the relay, k3s, the agent identities and the agent-tools serve do
// not exist yet, so a channel/registry script could only no-op. Marking them done
// there is what silently froze a world's channel shape: the queue saw nothing
// pending, so the script never ran and no report ever said so.
func TestShipMigrationsShipsScriptsOnly(t *testing.T) {
	local := t.TempDir()
	for _, n := range []string{"1799900001.sh", "1799900002.sh"} {
		if err := os.WriteFile(filepath.Join(local, n), []byte("echo hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A non-epoch helper .sh is not a migration and must not ship.
	if err := os.WriteFile(filepath.Join(local, "helpers.sh"), []byte("noop\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &shipRecorder{files: map[string]string{}}
	vmid := uint32(101)
	spec := &DeployCpSpec{StateDir: "/srv/data/cp/control-plane", LXc: &vmid}
	if err := ShipMigrations(rec, spec, local); err != nil {
		t.Fatalf("ShipMigrations: %v", err)
	}

	scriptsDir := "/srv/data/cp/control-plane/migrations/scripts"
	for _, n := range []string{"1799900001.sh", "1799900002.sh"} {
		if _, ok := rec.files[scriptsDir+"/"+n]; !ok {
			t.Fatalf("script %s never shipped (guest has %v)", n, mapKeys(rec.files))
		}
	}
	if _, ok := rec.files[scriptsDir+"/helpers.sh"]; ok {
		t.Fatal("a non-epoch helper .sh must not ship as a migration")
	}
	// The invariant: every byte install writes goes under the scripts dir.
	// Anything else is a completion marker — a claim about a run that never
	// happened.
	for path := range rec.files {
		if !strings.HasPrefix(path, scriptsDir+"/") {
			t.Fatalf("install wrote %q outside %s (a marker asserts a run that did not happen); guest has %v",
				path, scriptsDir, mapKeys(rec.files))
		}
	}
}

// TestShipMigrationsMissingLocalDirIsNoOp pins that an absent local scripts dir is
// silence, not failure: a release built without scripts (or a box run with no
// --migrations-dir) still deploys the CP.
func TestShipMigrationsMissingLocalDirIsNoOp(t *testing.T) {
	rec := &shipRecorder{files: map[string]string{}}
	vmid := uint32(101)
	spec := &DeployCpSpec{StateDir: "/srv/data/cp/control-plane", LXc: &vmid}
	if err := ShipMigrations(rec, spec, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("an absent migrations dir must be a no-op, got %v", err)
	}
	if len(rec.files) != 0 {
		t.Fatalf("nothing should have shipped, got %v", mapKeys(rec.files))
	}
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
