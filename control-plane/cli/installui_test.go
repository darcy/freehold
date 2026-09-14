package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// wizardType feeds runes into the active field.
func wizardType(m *installModel, s string) {
	for _, r := range s {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// wizardAdvance presses enter to move to the next step.
func wizardAdvance(m *installModel) {
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

// TestInstallWizardCollectsAllAnswers walks the wizard to completion and checks
// the collected rebuildFlags — including relay/CP domains + proxy IP, which
// runBootstrap would otherwise re-prompt at runtime.
func TestInstallWizardCollectsAllAnswers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FREEHOLD_HOME", home)

	m := newInstallModel(func(rebuildFlags) (*rebuildEngine, error) {
		return nil, errors.New("engine-handoff")
	}, &bytes.Buffer{})

	steps := []string{
		"root@10.0.0.5",   // host
		"proxmox-box",     // runner
		"127.0.0.1:8787",  // addr
		"world.test",      // relay domain
		"",                // cp domain (blank -> defaults to relay)
		"192.168.30.8/24", // proxy IP
		"16",              // rootfs
		"2048",            // memory
		"2",               // identity: generate
		"",                // pubkey (skipped for generate)
		"",                // nsec (skipped)
		"y",               // storage consent
	}
	for _, s := range steps {
		if s != "" {
			wizardType(m, s)
		}
		wizardAdvance(m)
	}
	if !m.done {
		t.Fatalf("wizard did not finish: err=%q step=%d", m.err, m.step)
	}
	if m.f.relayDomain != "world.test" {
		t.Errorf("relayDomain = %q, want world.test", m.f.relayDomain)
	}
	if m.f.cpDomain != "world.test" {
		t.Errorf("cpDomain = %q, want world.test (blank defaults to relay)", m.f.cpDomain)
	}
	if m.f.proxyIP != "192.168.30.8/24" {
		t.Errorf("proxyIP = %q, want 192.168.30.8/24", m.f.proxyIP)
	}
	if m.f.host != "root@10.0.0.5" || m.f.target != "proxmox-box" {
		t.Errorf("host/target = %q/%q", m.f.host, m.f.target)
	}
	if !m.f.confirmStorage {
		t.Error("storage consent 'y' not recorded")
	}
	if m.f.operatorPubkey == "" {
		t.Error("generate path must mint an operator pubkey")
	}
	if m.f.operatorIdentity == "" {
		t.Error("generate path must record the operator identity dir")
	}
}

// TestInstallWizardDefaultsBlankToDefault confirms a blank text field takes its
// declared default (dialoguer parity), and blank cp domain defaults to relay.
func TestInstallWizardDefaultsBlankToDefault(t *testing.T) {
	m := newInstallModel(func(rebuildFlags) (*rebuildEngine, error) {
		return nil, errors.New("engine-handoff")
	}, &bytes.Buffer{})
	for i := 0; i < len(installSteps); i++ {
		// Blank everything; defaults apply (host/runner/addr/rootfs/memory), cp
		// domain derives from relay, identity=generate (choice default "1" is
		// overridden to "2" here so no key is pasted).
		if installSteps[i].label == "Operator identity: 1) have a Nostr key already  2) generate one for me" {
			wizardType(m, "2")
		}
		wizardAdvance(m)
	}
	if m.f.relayDomain != "" {
		t.Errorf("relay domain blank must stay blank (required), got %q", m.f.relayDomain)
	}
	if m.f.cpDomain != "" {
		t.Errorf("cp domain must default to relay (blank), got %q", m.f.cpDomain)
	}
}

// TestInstallWizardRequiresProxyIP: no proxy IP => the wizard bails with an
// actionable error instead of handing a half-filled flags to the engine.
func TestInstallWizardRequiresProxyIP(t *testing.T) {
	m := newInstallModel(func(rebuildFlags) (*rebuildEngine, error) {
		return nil, errors.New("engine-handoff")
	}, &bytes.Buffer{})
	// Walk every step; proxy IP left blank, identity=generate.
	steps := []string{
		"root@10.0.0.5",  // host
		"",               // runner (default)
		"",               // addr (default)
		"world.test",     // relay domain
		"",               // cp domain (defaults to relay)
		"",               // proxy IP blank -> bail
		"16",             // rootfs
		"2048",           // memory
		"2",              // identity: generate
		"",               // pubkey
		"",               // nsec
		"y",              // storage consent
	}
	for _, s := range steps {
		if s != "" {
			wizardType(m, s)
		}
		wizardAdvance(m)
	}
	if m.done || !strings.Contains(m.err, "proxy static IP") {
		t.Errorf("blank proxy IP must bail with an actionable error, got done=%v err=%q", m.done, m.err)
	}
}
