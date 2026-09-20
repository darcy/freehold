package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestBootstrapModeView confirms the bootstrap-mode view is a STATUS view that
// points at `freehold build` — the TUI no longer hosts the build flow.
func TestBootstrapModeView(t *testing.T) {
	m := &Model{Mode: ModeBootstrap, CfgPath: "/nonexistent/config.toml"}
	out := m.View()
	if !strings.Contains(out, "freehold build") {
		t.Errorf("bootstrap view should point at `freehold build`, got:\n%s", out)
	}
	if strings.Contains(out, "press") || strings.Contains(out, "to bootstrap a target") || strings.Contains(out, "to rebuild the whole world") {
		t.Errorf("bootstrap view must not offer keypress build/bootstrap, got:\n%s", out)
	}
}

// TestConfigureModeView confirms the configure-mode view is a STATUS view that
// points at `freehold build` (no deploy-relay / deploy-cp / rebuild keys).
func TestConfigureModeView(t *testing.T) {
	m := &Model{Mode: ModeConfigure, Domain: "example.test"}
	out := m.View()
	if !strings.Contains(out, "freehold build") {
		t.Errorf("configure view should point at `freehold build`, got:\n%s", out)
	}
	for _, banned := range []string{"deploy-relay", "deploy-cp", " B rebuild", "to deploy the relay", "to deploy the control plane"} {
		if strings.Contains(out, banned) {
			t.Errorf("configure view must not offer keypress deploy/rebuild %q, got:\n%s", banned, out)
		}
	}
}

// TestBootstrapFormSteps walks the 4-step bootstrap form to completion and
// confirms the inputs are collected in order.

// keyPress returns true if the key started a flow.
func keyPress(m *Model, s string) bool {
	before := m.Flow
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return m.Flow != nil && before == nil
}

// typeText feeds a string into the active text input.
func typeText(m *Model, s string) {
	for _, r := range s {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// TestFlowEscCancels confirms esc aborts a flow.

// TestPromptLabels sanity-checks the form prompt labels.
func TestPromptLabels(t *testing.T) {
	if promptLabel(flowBootstrap, 1) != "kind: proxmox-lxc | vultr-vps | hetzner-vps" {
		t.Errorf("bootstrap step1 label wrong: %q", promptLabel(flowBootstrap, 1))
	}
	if promptLabel(flowDeployCp, 0) != "local path of the control-plane binary" {
		t.Errorf("deploy-cp step0 label wrong: %q", promptLabel(flowDeployCp, 0))
	}
}
