package cli

import (
	"strings"
	"testing"

	"freehold/contract/config"
)

// TestCpFirstTeardownBailsWithoutSession proves the CP-first teardown hand-off
// is best-effort: on a box with no operator session it returns an actionable
// error (so teardownCmd warns and still proceeds), it never fabricates one.
func TestCpFirstTeardownBailsWithoutSession(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	cfg := &config.Config{CPURL: "https://cp.example"}
	err := cpFirstTeardown(cfg)
	if err == nil {
		t.Fatal("cpFirstTeardown must fail without an operator session")
	}
	if !strings.Contains(err.Error(), "no operator session") {
		t.Fatalf("expected an actionable no-session error, got: %v", err)
	}
}

// TestCpFirstTeardownNeedsCPURL proves a missing CP coordinate is caught early.
func TestCpFirstTeardownNeedsCPURL(t *testing.T) {
	if err := cpFirstTeardown(&config.Config{}); err == nil {
		t.Fatal("cpFirstTeardown must require a CP URL")
	}
}
