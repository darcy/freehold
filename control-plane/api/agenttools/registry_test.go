package agenttools

import (
	"path/filepath"
	"testing"
)

// TestRegistryPurposeSurvives verifies the E3 path concretely: a created agent's
// purpose is persisted on its registry row and survives a re-open (the rebuild
// reconciler reads it back to recreate the system prompt with the purpose intact).
func TestRegistryPurposeSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	r, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RegisterAgent("helper", "aa55", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPurpose("helper", "helps with installs"); err != nil {
		t.Fatal(err)
	}

	// Re-open from disk (a fresh process / rebuild reconciler reading it back).
	r2, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := r2.Agents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Name != "helper" || agents[0].Purpose != "helps with installs" {
		t.Fatalf("purpose not preserved across re-open: %+v", agents)
	}
}

// TestRegistrySetPurposeBeforeRegister rejects setting purpose on an unknown row.
func TestRegistrySetPurposeBeforeRegister(t *testing.T) {
	r, err := OpenRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetPurpose("ghost", "purpose"); err == nil {
		t.Fatal("SetPurpose on an unnamed row should error")
	}
}

// TestRegistryChannelsSurvive: a multi-channel/private create persists its full
// channel list + private flag on the registry row and survives a re-open, so a
// rebuild rejoins every channel (not just the primary) with the right visibility.
func TestRegistryChannelsSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RegisterAgent("helper", "aa55", "#ops"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetChannels("helper", []string{"#ops", "#extra"}, true); err != nil {
		t.Fatal(err)
	}
	r2, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := r2.Agents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || len(agents[0].Channels) != 2 || !agents[0].Private {
		t.Fatalf("channels/private not preserved across re-open: %+v", agents)
	}
	if err := r2.SetChannels("ghost", nil, false); err == nil {
		t.Fatal("SetChannels on an unnamed row should error")
	}
}
