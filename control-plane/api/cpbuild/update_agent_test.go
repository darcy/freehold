package cpbuild

import (
	"path/filepath"
	"strings"
	"testing"

	"freehold/agents"
	"freehold/contract/console"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
)

// TestUpdateAgentCoreIdentitiesRefused pins the locked two-tier boundary on
// the agent-side edit surface: the CPA and the four departments are
// repo-defined (name-keyed prompts, department tables, respond gates), so
// update_agent and the pod-retiring remove refuse them outright. The nil
// registry proves the refusal fires before any row is read.
func TestUpdateAgentCoreIdentitiesRefused(t *testing.T) {
	fn := BuildUpdateAgentFn(&Spec{CpaName: "freehold"}, nil)
	for _, name := range append([]string{"freehold"}, agents.DepartmentNames()...) {
		if _, err := fn(agent.UpdateArgs{Name: name, Purpose: "x"}); err == nil || !strings.Contains(err.Error(), "core") {
			t.Fatalf("update of core identity %s must be refused, got %v", name, err)
		}
	}
	rfn := BuildRemoveAgentFn(&Spec{CpaName: "freehold"})
	for _, name := range append([]string{"freehold"}, agents.DepartmentNames()...) {
		if _, err := rfn(name); err == nil || !strings.Contains(err.Error(), "core") {
			t.Fatalf("remove of core identity %s must be refused, got %v", name, err)
		}
	}
}

// TestUpdateAgentRowGuards pins the row-level refusals, each firing before any
// infra leg runs: an unknown source row, a taken destination name, a rename
// whose pod name would collide with the CPA's, a rename onto another row's
// pod name, and a model outside the create allowlist. The registry holds only
// non-core rows (testRegistry's names are departments).
func TestUpdateAgentRowGuards(t *testing.T) {
	root := t.TempDir()
	reg, err := agenttools.OpenRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct{ name, pk string }{
		{"helper", strings.Repeat("a", 64)},
		{"other", strings.Repeat("b", 64)},
		{"my-agent", strings.Repeat("c", 64)},
	} {
		if _, err := reg.RegisterAgent(a.name, a.pk, "#ops"); err != nil {
			t.Fatal(err)
		}
	}
	fn := BuildUpdateAgentFn(&Spec{StateDir: filepath.Join(root, "agent-tools"), CpaName: "freehold"}, reg)
	for _, tc := range []struct {
		label, want string
		args        agent.UpdateArgs
	}{
		{"unknown row", "unknown agent", agent.UpdateArgs{Name: "ghost"}},
		{"taken destination", "already registered", agent.UpdateArgs{Name: "helper", Rename: "other"}},
		{"CPA pod collision", "collides with the control plane agent", agent.UpdateArgs{Name: "helper", Rename: "Freehold!"}},
		{"row pod collision", "collides with my-agent", agent.UpdateArgs{Name: "helper", Rename: "my agent"}},
		{"bad model", "model must be one of", agent.UpdateArgs{Name: "helper", Model: "Bogus"}},
	} {
		if _, err := fn(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error %q, got %v", tc.label, tc.want, err)
		}
	}
}

// TestResolvedUpdateKeepsAbsentFields pins the absent-means-keep contract in
// its pure form: a purpose/channel edit must not silently re-resolve a
// Code/ExtraThinking agent onto General (the row's model rides through), and
// a field edit keeps the row's channel list + visibility.
func TestResolvedUpdateKeepsAbsentFields(t *testing.T) {
	row := console.AgentInfo{
		Name: "helper", Pubkey: "aa", Purpose: "helps",
		Channels: []string{"#ops", "#extra"}, Private: true, Model: "Code",
	}
	name, purpose, channels, private, model := resolvedUpdate(row, agent.UpdateArgs{Name: "helper"})
	if name != "helper" || purpose != "helps" || model != "Code" || private != true || len(channels) != 2 {
		t.Fatalf("absent args must keep the row: %q %q model=%s private=%v channels=%v", name, purpose, model, private, channels)
	}
	// A purpose edit keeps the model.
	_, purpose, _, _, model = resolvedUpdate(row, agent.UpdateArgs{Name: "helper", Purpose: "new purpose"})
	if purpose != "new purpose" || model != "Code" {
		t.Fatalf("a purpose edit must keep the row's model: purpose=%q model=%s", purpose, model)
	}
	// A model switch keeps the purpose.
	_, purpose, _, _, model = resolvedUpdate(row, agent.UpdateArgs{Name: "helper", Model: "ExtraThinking"})
	if purpose != "helps" || model != "ExtraThinking" {
		t.Fatalf("a model switch must keep the row's purpose: purpose=%q model=%s", purpose, model)
	}
	// A channel replace takes the new list (Private applies only then).
	_, _, channels, private, _ = resolvedUpdate(row, agent.UpdateArgs{Name: "helper", Channels: []string{"#solo"}})
	if len(channels) != 1 || channels[0] != "#solo" || private != false {
		t.Fatalf("a channel replace must take the new list: %v private=%v", channels, private)
	}
	// And a rename resolves the new name through the same merge.
	name, _, _, _, _ = resolvedUpdate(row, agent.UpdateArgs{Name: "helper", Rename: "renamed"})
	if name != "renamed" {
		t.Fatalf("rename must resolve the new name: %q", name)
	}
}
