package cli

import (
	"strings"
	"testing"

	"freehold/contract/config"
)

// TestRunWholeWorldTeardownBailsWithoutSession proves the CP-driven
// whole-world teardown is fail-closed: on a box with no operator session it
// returns an actionable error rather than fabricating one (the CP is the only
// path — a thin box has no local runner).
func TestRunWholeWorldTeardownBailsWithoutSession(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	cfg := &config.Config{CPURL: "https://cp.example"}
	err := runWholeWorldTeardown(cfg, false, true)
	if err == nil {
		t.Fatal("runWholeWorldTeardown must fail without an operator session")
	}
	if !strings.Contains(err.Error(), "no operator session") {
		t.Fatalf("expected an actionable no-session error, got: %v", err)
	}
}
