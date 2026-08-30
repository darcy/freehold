package tui

import (
	"fmt"
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

// TestRebuildFormSteps walks the 6-step rebuild form to completion and
// confirms the inputs land in order (operator pk, domain, LV size,
// thin-pool name, pool size, k3s).
func TestRebuildFormSteps(t *testing.T) {
	m := &Model{Mode: ModeBootstrap}
	if !keyPress(m, "B") {
		t.Fatal("B did not start the rebuild flow in bootstrap mode")
	}
	if m.Flow == nil || m.Flow.Kind != flowRebuild {
		t.Fatal("expected a rebuild flow after pressing B")
	}
	if ncols(flowRebuild) != 6 {
		t.Fatalf("rebuild form should have 6 steps, got %d", ncols(flowRebuild))
	}
	answers := []string{strings.Repeat("a", 64), "world.test", "10", "freehold-thin", "40", "y"}
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
	if got := promptLabel(flowRebuild, 3); got != "thin-pool name (blank = reuse detected / carve default)" {
		t.Errorf("rebuild step3 label = %q", got)
	}
	if got := promptLabel(flowRebuild, 5); got != "boot k3s too? (y/n, blank = y)" {
		t.Errorf("rebuild step5 label = %q", got)
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

// TestDoorKeyWaitingDetection pins the expected-pause detection: the
// door-gate bail is a NORMAL operator-paused state, not a failure.
func TestDoorKeyWaitingDetection(t *testing.T) {
	// The CLI's --yes bail includes stage logs before the message; the
	// extraction must find the marker and return just the instruction.
	out := "provisioning runner...\nthe door needs a NEW ssh key before rebuild can continue — install it on host, then re-run rebuild:\n\n    ssh-ed25519 AAAA... freehold\n\n  (on the host: mkdir -p /root/.ssh && echo 'ssh-ed25519 AAAA...' >> /root/.ssh/authorized_keys)\n"
	got := doorKeyWaiting(out)
	if got == "" {
		t.Fatal("expected the door-gate pause to be detected")
	}
	if !strings.HasPrefix(got, "the door needs a NEW ssh key") {
		t.Errorf("waiting text should start at the marker, got:\n%s", got)
	}
	if !strings.Contains(got, "ssh-ed25519") {
		t.Errorf("waiting text must carry the install line, got:\n%s", got)
	}
	if doorKeyWaiting("rebuild failed: storage resolution failed") != "" {
		t.Error("a real failure must NOT be detected as the door pause")
	}
}

// TestDoorKeyWaitingRenderedNotError: the pause lands as a yellow waiting
// state, NOT the red error path.
func TestDoorKeyWaitingRenderedNotError(t *testing.T) {
	m := &Model{Mode: ModeBootstrap, CfgPath: "/nonexistent/config.toml"}
	wait := "the door needs a NEW ssh key before rebuild can continue — install it on host, then re-run rebuild:\n\n    ssh-ed25519 AAAA freehold\n\n  (on the host: echo it >> authorized_keys)"
	_, _ = m.Update(flowMsg{wait: wait, rebuildArgs: []string{"rebuild", "--yes"}})
	if m.Wait == "" {
		t.Fatal("the wait text must be captured on the model")
	}
	if m.Err != "" {
		t.Errorf("the door pause must not set an error, got %q", m.Err)
	}
	if len(m.rebuildArgs) == 0 {
		t.Error("the pause must keep the rebuild args for the in-place retry")
	}
	view := m.View()
	if !strings.Contains(view, "waiting for the operator") {
		t.Errorf("view must announce the waiting state, got:\n%s", view)
	}
	if !strings.Contains(view, "ssh-ed25519") {
		t.Errorf("view must show the key to install, got:\n%s", view)
	}
	if !strings.Contains(view, "ENTER") {
		t.Errorf("view must offer ENTER to resume the rebuild, got:\n%s", view)
	}

	// A subsequent error clears the wait AND the retry args; a new flow clears both too.
	_, _ = m.Update(flowMsg{err: fmt.Errorf("boom")})
	if m.Wait != "" || m.Err == "" {
		t.Errorf("error must replace the wait: wait=%q err=%q", m.Wait, m.Err)
	}
	if m.rebuildArgs != nil {
		t.Error("an error must clear the retry args")
	}
	m.Wait = "pending"
	m.rebuildArgs = []string{"rebuild", "--yes"}
	m.beginPrompt(flowRebuild)
	if m.Wait != "" {
		t.Error("starting a new flow must clear the waiting state")
	}
	if m.rebuildArgs != nil {
		t.Error("starting a new flow must clear the retry args")
	}
}

// TestDoorGateEnterRetry pins the Rust door flow: the operator stays INSIDE
// the gate. ENTER re-runs the SAME rebuild in place (args preserved — never
// back to the 6-field form), ESC cancels, and a second pause re-arms the gate.
func TestDoorGateEnterRetry(t *testing.T) {
	args := []string{"rebuild", "--yes", "--operator-pubkey", strings.Repeat("a", 64), "--domain", "world.test"}
	m := &Model{Mode: ModeBootstrap, CfgPath: "/nonexistent/config.toml"}
	_, _ = m.Update(flowMsg{wait: "the door needs a NEW ssh key …", rebuildArgs: args})
	if len(m.rebuildArgs) == 0 {
		t.Fatal("the pause must keep the rebuild args for the in-place retry")
	}
	if m.Flow != nil {
		t.Fatal("the pause must not keep a form flow")
	}

	// ENTER: re-runs in place — a cmd is dispatched, the gate state clears,
	// and NO form is reopened.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("ENTER must re-run the rebuild")
	}
	if m.rebuildArgs != nil || m.Wait != "" || m.Msg == "" {
		t.Errorf("ENTER must leave the gate (args=%v wait=%q msg=%q)", m.rebuildArgs, m.Wait, m.Msg)
	}
	if m.Flow != nil {
		t.Error("ENTER must not re-open the form")
	}

	// The resumed run pauses again (key still not installed) → the gate
	// re-arms with the same args.
	_, _ = m.Update(flowMsg{wait: "the door needs its ssh key …", rebuildArgs: args})
	if len(m.rebuildArgs) == 0 || m.Wait == "" {
		t.Fatal("a second pause must re-arm the gate")
	}

	// ESC: cancel — gate cleared, no error, no form.
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.rebuildArgs != nil || m.Wait != "" || m.Flow != nil {
		t.Errorf("esc must clear the door gate (args=%v wait=%q flow=%v)", m.rebuildArgs, m.Wait, m.Flow)
	}
	if m.Err != "" {
		t.Errorf("esc is a cancel, not an error: %q", m.Err)
	}
}

// TestDoorGateSwallowsKeys: while paused at the door, stray keys (including
// B) must not open flows or disarm the gate — only ENTER/esc/q pass.
func TestDoorGateSwallowsKeys(t *testing.T) {
	m := &Model{Mode: ModeBootstrap, rebuildArgs: []string{"rebuild", "--yes"}}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("B")})
	if m.Flow != nil {
		t.Error("B at the door gate must not open the rebuild form")
	}
	if len(m.rebuildArgs) == 0 {
		t.Error("stray keys must not disarm the gate")
	}
}
