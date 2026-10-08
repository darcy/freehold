package cpbuild

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"freehold/agents"
	"freehold/contract/console"
	"freehold/contract/crypto"
	"freehold/control-plane/api/agent"
)

// TestReconciledChannels: a reserved department re-derives its fixed channels
// (private); any other agent rejoins the persisted full list, falling back to
// the single recorded channel for legacy rows.
func TestReconciledChannels(t *testing.T) {
	dept, private := reconciledChannels(console.AgentInfo{Name: "network"})
	if !private || len(dept) != 1 || dept[0] != "#freehold" {
		t.Fatalf("department channels = %v private=%v, want just private #freehold", dept, private)
	}
	// The migration case: a department row that still carries stale persisted
	// channels from before the department channels were retired must re-derive
	// the fixed list, never rejoin a retired channel.
	stale, private := reconciledChannels(console.AgentInfo{Name: "network", Channels: []string{"#freehold-network"}})
	if !private || len(stale) != 1 || stale[0] != "#freehold" {
		t.Fatalf("stale department channels = %v private=%v, want just private #freehold", stale, private)
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
	s := &Spec{OwnerPub: "op", CpaName: "boss", AgentIdentityDir: root, StateDir: root + "/agent-tools"}
	for _, name := range append([]string{"boss"}, agents.DepartmentNames()...) {
		if _, err := agent.EnsureIdentity(filepath.Join(root, "agents", sanitizeDir(name))); err != nil {
			t.Fatalf("mint %s identity: %v", name, err)
		}
	}
	cpa := s.identityPubkey("boss")
	if cpa == "" {
		t.Fatal("CPA identity pubkey unreadable")
	}
	// The console identity (the scheduled-jobs fire path) rides every
	// allowlist: seed it under the state root the Spec derives.
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x77
	consolePK, err := crypto.PubkeyFromSecret(consoleSec)
	if err != nil {
		t.Fatal(err)
	}
	consoleDir := filepath.Join(root, "control-plane")
	if err := os.MkdirAll(filepath.Join(consoleDir, "console"), 0o700); err != nil {
		t.Fatal(err)
	}
	idJSON := fmt.Sprintf(`{"nostr_secret_hex":%q}`, hex.EncodeToString(consoleSec))
	if err := os.WriteFile(filepath.Join(consoleDir, "console", "identity.json"), []byte(idJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "op," + cpa + "," + consolePK
	for _, dep := range agents.DepartmentNames() {
		want += "," + s.identityPubkey(dep)
	}
	if got := s.respondAllowlist("network"); got != want {
		t.Fatalf("department allowlist = %q, want %q", got, want)
	}
	if got := s.respondAllowlist("helper"); got != "op,"+cpa+","+consolePK {
		t.Fatalf("custom allowlist = %q, want op + the CPA + the console identity", got)
	}
}
