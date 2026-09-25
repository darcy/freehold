package cpbuild

import (
	"path/filepath"
	"reflect"
	"testing"

	"freehold/agents"
	"freehold/contract/console"
	"freehold/control-plane/api/agent"
)

// TestReconciledChannels: a reserved department re-derives its fixed channels
// (private); any other agent rejoins the persisted full list, falling back to
// the single recorded channel for legacy rows.
func TestReconciledChannels(t *testing.T) {
	dept, private := reconciledChannels(console.AgentInfo{Name: "network"})
	if !private || len(dept) < 2 || dept[0] != "#freehold" || dept[1] != "#freehold-network" {
		t.Fatalf("department channels = %v private=%v, want #freehold + private #freehold-network", dept, private)
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

// TestRespondAllowlist: a reserved department's respond-to allowlist names the
// operator + every core identity (DepartmentNames order, deduped); a custom
// agent's names its asker (the operator today — the CP cannot see chat
// threads) + the CPA.
func TestRespondAllowlist(t *testing.T) {
	root := t.TempDir()
	s := &Spec{OwnerPub: "op", CpaName: "boss", AgentIdentityDir: root}
	for _, name := range append([]string{"boss"}, agents.DepartmentNames()...) {
		if _, err := agent.EnsureIdentity(filepath.Join(root, "agents", sanitizeDir(name))); err != nil {
			t.Fatalf("mint %s identity: %v", name, err)
		}
	}
	cpa := s.identityPubkey("boss")
	if cpa == "" {
		t.Fatal("CPA identity pubkey unreadable")
	}
	want := "op," + cpa
	for _, dep := range agents.DepartmentNames() {
		want += "," + s.identityPubkey(dep)
	}
	if got := s.respondAllowlist("network"); got != want {
		t.Fatalf("department allowlist = %q, want %q", got, want)
	}
	if got := s.respondAllowlist("helper"); got != "op,"+cpa {
		t.Fatalf("custom allowlist = %q, want op + the CPA", got)
	}
}
