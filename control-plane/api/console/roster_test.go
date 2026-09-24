package console

import (
	"path/filepath"
	"strings"
	"testing"

	"freehold/control-plane/api/agenttools"
)

// TestResolveAgentRoster pins the console /api/provision rosters contract:
// rosters are agent NAMES resolved through the agent-tools registry (the same
// representation the rebuild re-assertion consumes); an unknown name is a 400,
// never a silently-unresolvable roster entry.
func TestResolveAgentRoster(t *testing.T) {
	dir := t.TempDir()
	reg, err := agenttools.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("ai", "aabb", "ai"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("deployer", "ccdd", "deployer"); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveAgentRoster(dir, []string{"ai", "deployer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 2 || resolved[0] != "aabb" || resolved[1] != "ccdd" {
		t.Fatalf("resolved: %v", resolved)
	}
	if _, err := resolveAgentRoster(dir, []string{"ai", "nobody"}); err == nil ||
		!strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("unknown roster name must be refused, got %v", err)
	}
}
