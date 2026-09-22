package common

import (
	"net"
	"strings"
	"testing"

	"freehold/contract/config"
	"freehold/contract/console"
)

// TestAddrReachable: a listening socket is reachable; a closed port is not.
func TestAddrReachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !AddrReachable(l.Addr().String()) {
		t.Fatalf("listening addr %s must be reachable", l.Addr())
	}
	l.Close()
	if AddrReachable(l.Addr().String()) {
		t.Fatalf("closed addr %s must not be reachable", l.Addr())
	}
}

// TestAdoptAgentToolsCoords: a --data rebuild's fresh agent-tools pubkey is
// adopted (with its URL); an unchanged or empty summary is a no-op.
func TestAdoptAgentToolsCoords(t *testing.T) {
	oldPK := strings.Repeat("a", 64)
	cfg := &config.Config{AgentToolsURL: "http://old:8089", AgentToolsPubkey: oldPK}

	if AdoptAgentToolsCoords(cfg, &console.WorldSummary{AgentToolsURL: cfg.AgentToolsURL, AgentToolsPubkey: oldPK}) {
		t.Fatal("unchanged coords must not report a change")
	}

	newPK := strings.Repeat("b", 64)
	if !AdoptAgentToolsCoords(cfg, &console.WorldSummary{AgentToolsURL: "http://new:8089", AgentToolsPubkey: newPK}) {
		t.Fatal("a fresh agent-tools pubkey must report a change")
	}
	if cfg.AgentToolsPubkey != newPK || cfg.AgentToolsURL != "http://new:8089" {
		t.Fatalf("adopted coords = %q / %q, want %q / http://new:8089", cfg.AgentToolsPubkey, cfg.AgentToolsURL, newPK)
	}

	if AdoptAgentToolsCoords(cfg, &console.WorldSummary{AgentToolsURL: "http://x:1"}) {
		t.Fatal("an empty pubkey (pre-/api/world CP) must not be adopted")
	}
	if cfg.AgentToolsURL != "http://new:8089" {
		t.Fatalf("empty-pubkey summary must leave the URL alone, got %q", cfg.AgentToolsURL)
	}
}

// TestRunnerKeyRefsNoPackage: with no readable package (thin box) there are no
// exact body refs — the caller's target-comment sweep removes the key.
func TestRunnerKeyRefsNoPackage(t *testing.T) {
	cfg := &config.Config{Runner: config.RunnerRef{Target: "proxmox-box", Pubkey: "deadbeef"}}
	if refs := RunnerKeyRefs(cfg, nil); len(refs) != 0 {
		t.Errorf("refs = %v, want none (target sweep handles it)", refs)
	}
}
