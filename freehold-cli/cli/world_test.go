package cli

import (
	"strings"
	"testing"

	"freehold/contract/config"
	"freehold/contract/console"
)

// TestAdoptAgentToolsCoords: a --data rebuild's fresh agent-tools pubkey is
// adopted (with its URL); an unchanged or empty summary is a no-op.
func TestAdoptAgentToolsCoords(t *testing.T) {
	oldPK := strings.Repeat("a", 64)
	cfg := &config.Config{AgentToolsURL: "http://old:8089", AgentToolsPubkey: oldPK}

	if adoptAgentToolsCoords(cfg, &console.WorldSummary{AgentToolsURL: cfg.AgentToolsURL, AgentToolsPubkey: oldPK}) {
		t.Fatal("unchanged coords must not report a change")
	}

	newPK := strings.Repeat("b", 64)
	if !adoptAgentToolsCoords(cfg, &console.WorldSummary{AgentToolsURL: "http://new:8089", AgentToolsPubkey: newPK}) {
		t.Fatal("a fresh agent-tools pubkey must report a change")
	}
	if cfg.AgentToolsPubkey != newPK || cfg.AgentToolsURL != "http://new:8089" {
		t.Fatalf("adopted coords = %q / %q, want %q / http://new:8089", cfg.AgentToolsPubkey, cfg.AgentToolsURL, newPK)
	}

	if adoptAgentToolsCoords(cfg, &console.WorldSummary{AgentToolsURL: "http://x:1"}) {
		t.Fatal("an empty pubkey (pre-/api/world CP) must not be adopted")
	}
	if cfg.AgentToolsURL != "http://new:8089" {
		t.Fatalf("empty-pubkey summary must leave the URL alone, got %q", cfg.AgentToolsURL)
	}
}
