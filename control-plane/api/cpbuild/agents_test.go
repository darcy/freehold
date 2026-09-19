package cpbuild

import (
	"reflect"
	"testing"

	"freehold/contract/console"
)

// TestReconciledChannels: a reserved department re-derives its fixed channels
// (private); any other agent rejoins the persisted full list, falling back to
// the single recorded channel for legacy rows.
func TestReconciledChannels(t *testing.T) {
	dept, private := reconciledChannels(console.AgentInfo{Name: "security"})
	if !private || len(dept) < 2 || dept[0] != "#freehold" {
		t.Fatalf("department channels = %v private=%v, want #freehold + private #security", dept, private)
	}
	got, priv := reconciledChannels(console.AgentInfo{Name: "custom", Channels: []string{"#a", "#b"}, Private: true})
	if !reflect.DeepEqual(got, []string{"#a", "#b"}) || !priv {
		t.Fatalf("custom channels = %v private=%v, want [#a #b] true", got, priv)
	}
	legacy, priv := reconciledChannels(console.AgentInfo{Name: "legacy", Channel: "#old"})
	if !reflect.DeepEqual(legacy, []string{"#old"}) || priv {
		t.Fatalf("legacy channels = %v private=%v, want [#old] false", legacy, priv)
	}
}

// TestAgentIdentityDir: an explicit identity root wins; empty falls back to the
// state dir (the agent-tools server sets StateDir to its own root). This is what
// keeps a console-side reconcile minting into the same dir as the server.
func TestAgentIdentityDir(t *testing.T) {
	if got := (&Spec{StateDir: "/srv/data/cp/control-plane", AgentIdentityDir: "/srv/data/cp/agent-tools"}).agentIdentityDir(); got != "/srv/data/cp/agent-tools" {
		t.Fatalf("agentIdentityDir = %q, want the explicit root", got)
	}
	if got := (&Spec{StateDir: "/srv/data/cp/agent-tools"}).agentIdentityDir(); got != "/srv/data/cp/agent-tools" {
		t.Fatalf("agentIdentityDir = %q, want the StateDir fallback", got)
	}
}

func TestCPANameOrDefault(t *testing.T) {
	if got := (&Spec{}).cpaNameOrDefault(); got == "" {
		t.Fatal("empty CpaName must default, not stay empty")
	}
	if got := (&Spec{CpaName: "boss"}).cpaNameOrDefault(); got != "boss" {
		t.Fatalf("cpaNameOrDefault = %q, want boss", got)
	}
}
