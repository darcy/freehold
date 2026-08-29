package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTeardownFormSteps walks the 1-step teardown form and confirms the
// answer is captured (it only starts in RUNNING mode).
func TestTeardownFormSteps(t *testing.T) {
	m := &Model{Mode: ModeRunning}
	if !keyPress(m, "t") {
		t.Fatal("t did not start the teardown flow")
	}
	if m.Flow == nil || m.Flow.Kind != flowTeardown {
		t.Fatal("expected a teardown flow after pressing t")
	}
	if ncols(flowTeardown) != 1 {
		t.Fatalf("teardown form should have 1 step, got %d", ncols(flowTeardown))
	}
	typeText(m, "yes")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.Flow == nil || m.Flow.Inputs[0] != "yes" {
		t.Fatalf("teardown input not captured: %+v", m.Flow)
	}

	// teardown must NOT start outside running mode (config absent = the
	// teardown CLI would no-op anyway, but the hint isn't offered there).
	for _, mode := range []Mode{ModeBootstrap, ModeConfigure} {
		m2 := &Model{Mode: mode}
		if keyPress(m2, "t") {
			t.Errorf("t must not start a teardown flow in %s", mode)
		}
	}
}

// TestRebuildFormSteps walks the 5-step rebuild form to completion and
// confirms the inputs land in order (operator pk, domain, sizes, k3s).
func TestRebuildFormSteps(t *testing.T) {
	m := &Model{Mode: ModeBootstrap}
	if !keyPress(m, "B") {
		t.Fatal("B did not start the rebuild flow in bootstrap mode")
	}
	if m.Flow == nil || m.Flow.Kind != flowRebuild {
		t.Fatal("expected a rebuild flow after pressing B")
	}
	if ncols(flowRebuild) != 5 {
		t.Fatalf("rebuild form should have 5 steps, got %d", ncols(flowRebuild))
	}
	answers := []string{strings.Repeat("a", 64), "world.test", "10", "40", "y"}
	for i := range answers {
		typeText(m, answers[i])
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if m.Flow == nil {
		t.Fatal("flow vanished before inputs were captured")
	}
	for i, want := range answers {
		if m.Flow.Inputs[i] != want {
			t.Errorf("rebuild inputs[%d] = %q, want %q", i, m.Flow.Inputs[i], want)
		}
	}

	// rebuild must also start in configure mode (half-built worlds).
	m2 := &Model{Mode: ModeConfigure}
	if !keyPress(m2, "B") {
		t.Fatal("B did not start the rebuild flow in configure mode")
	}
	// and NOT in running mode (tear down first — the running footer offers
	// t teardown + B rebuild is intentionally absent there).
	m3 := &Model{Mode: ModeRunning}
	if keyPress(m3, "B") {
		t.Error("B must not start a rebuild flow in running mode")
	}
}

// TestRebuildFormEscCancels confirms esc aborts a rebuild flow.
func TestRebuildFormEscCancels(t *testing.T) {
	m := &Model{Mode: ModeBootstrap}
	keyPress(m, "B")
	if m.Flow == nil {
		t.Fatal("flow not started")
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.Flow != nil {
		t.Error("esc should cancel the rebuild flow")
	}
}

// TestNewFlowLabels sanity-checks the teardown/rebuild prompt labels.
func TestNewFlowLabels(t *testing.T) {
	if got := promptLabel(flowTeardown, 0); got != "destroy tenant data too? (yes | no)" {
		t.Errorf("teardown label = %q", got)
	}
	if got := promptLabel(flowRebuild, 0); got != "operator pubkey (64-hex)" {
		t.Errorf("rebuild step0 label = %q", got)
	}
	if got := promptLabel(flowRebuild, 2); got != "tenant LV size GB (blank = 10)" {
		t.Errorf("rebuild step2 label = %q", got)
	}
	if got := promptLabel(flowRebuild, 4); got != "boot k3s too? (y/n, blank = y)" {
		t.Errorf("rebuild step4 label = %q", got)
	}
}

// TestTeardownRebuildHints confirms the footers + views offer the new keys.
func TestTeardownRebuildHints(t *testing.T) {
	run := (&Model{Mode: ModeRunning}).footer()
	if !strings.Contains(run, "t teardown") {
		t.Errorf("running footer should offer t teardown, got:\n%s", run)
	}
	boot := (&Model{Mode: ModeBootstrap}).footer()
	if !strings.Contains(boot, "B rebuild") {
		t.Errorf("bootstrap footer should offer B rebuild, got:\n%s", boot)
	}
	conf := (&Model{Mode: ModeConfigure}).footer()
	if !strings.Contains(conf, "B rebuild") {
		t.Errorf("configure footer should offer B rebuild, got:\n%s", conf)
	}
	bootView := (&Model{Mode: ModeBootstrap, CfgPath: "/nonexistent/config.toml"}).View()
	if !strings.Contains(bootView, "rebuild the whole world") {
		t.Errorf("bootstrap view should advertise B rebuild, got:\n%s", bootView)
	}
	confView := (&Model{Mode: ModeConfigure, Domain: "example.test"}).View()
	if !strings.Contains(confView, "rebuild the whole world") {
		t.Errorf("configure view should advertise B rebuild, got:\n%s", confView)
	}
}
