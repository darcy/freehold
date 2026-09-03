package tui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTeardownFormSteps walks the 2-step teardown form (data? then the
// world-destroy CONFIRM) and confirms the answers are captured, the confirm
// GATES the dispatch, and teardown only starts in RUNNING mode.
func TestTeardownFormSteps(t *testing.T) {
	m := &Model{Mode: ModeRunning}
	if !keyPress(m, "t") {
		t.Fatal("t did not start the teardown flow")
	}
	if m.Flow == nil || m.Flow.Kind != flowTeardown {
		t.Fatal("expected a teardown flow after pressing t")
	}
	if ncols(flowTeardown) != 2 {
		t.Fatalf("teardown form should have 2 steps, got %d", ncols(flowTeardown))
	}
	typeText(m, "yes")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.Flow == nil || m.Flow.Inputs[0] != "yes" {
		t.Fatalf("teardown input not captured: %+v", m.Flow)
	}

	// (the form is already at step 1 from the walk above)
	typeText(m, "no") // confirm = NOT yes -> abort
	var cmd tea.Cmd
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if cmd == nil {
		t.Fatal("the confirm step must produce a dispatch outcome")
	}
	if start, ok := cmd().(activityStartMsg); ok {
		t.Fatalf("a non-yes confirm must NOT dispatch teardown, got %+v", start)
	}
	msg := cmd() // resolve the flowMsg
	m.Update(msg)
	if m.Flow != nil {
		t.Fatal("the aborted teardown flow should be finished after the flowMsg")
	}

	// an explicit "yes" on the confirm step dispatches teardown --yes.
	m2 := &Model{Mode: ModeRunning}
	_ = keyPress(m2, "t")
	typeText(m2, "no") // data
	_, _ = m2.Update(tea.KeyMsg{Type: tea.KeyTab})
	typeText(m2, "yes") // confirm
	_, cmd = m2.Update(tea.KeyMsg{Type: tea.KeyTab})
	if cmd == nil {
		t.Fatal("a confirmed teardown must dispatch")
	}
	start, ok := cmd().(activityStartMsg)
	if !ok {
		t.Fatalf("dispatched a %T, want activityStartMsg", cmd())
	}
	if strings.Join(start.args, " ") != "teardown --yes" {
		t.Errorf("args = %v, want teardown --yes (no --data)", start.args)
	}

	// teardown must NOT start outside running mode (config absent = the
	// teardown CLI would no-op anyway, but the hint isn't offered there).
	for _, mode := range []Mode{ModeBootstrap, ModeConfigure} {
		m3 := &Model{Mode: mode}
		if keyPress(m3, "t") {
			t.Errorf("t must not start a teardown flow in %s", mode)
		}
	}
}

// TestRebuildFormSteps walks the 7-step rebuild form to completion and
// confirms the inputs land in order (operator pk, domain, LV size,
// thin-pool name, pool size, k3s, CPA agent name).
func TestRebuildFormSteps(t *testing.T) {
	m := &Model{Mode: ModeBootstrap}
	if !keyPress(m, "B") {
		t.Fatal("B did not start the rebuild flow in bootstrap mode")
	}
	if m.Flow == nil || m.Flow.Kind != flowRebuild {
		t.Fatal("expected a rebuild flow after pressing B")
	}
	if ncols(flowRebuild) != 7 {
		t.Fatalf("rebuild form should have 7 steps, got %d", ncols(flowRebuild))
	}
	answers := []string{strings.Repeat("a", 64), "world.test", "10", "freehold-thin", "40", "y", "my-cpa"}
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
	// Completing the form dispatches the rebuild subprocess. With k3s on, it
	// must wire the litellm gateway (the CPA needs it to reason) and forward
	// the named CPA agent — and never disable k3s.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if cmd != nil {
		if start, ok := cmd().(activityStartMsg); ok {
			got := strings.Join(start.args, " ")
			for _, want := range []string{"--with-litellm", "--agent-name", "my-cpa"} {
				if !strings.Contains(got, want) {
					t.Errorf("rebuild args %q missing %q", got, want)
				}
			}
			if strings.Contains(got, "--with-k3s=false") {
				t.Errorf("rebuild args must keep k3s on when not opted out: %q", got)
			}
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
	if got := promptLabel(flowTeardown, 1); got != "CONFIRM destroying the whole world (all LXCs, door key KEPT)? type yes" {
		t.Errorf("teardown step1 label = %q", got)
	}
	if got := promptLabel(flowRebuild, 0); got != "operator pubkey (npub1… or 64-hex)" {
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
	if got := promptLabel(flowRebuild, 6); got != "CPA agent name (blank = freehold)" {
		t.Errorf("rebuild step6 label = %q", got)
	}
}

// TestRebuildFormSeededFromConfig: when a config exists, pressing B opens
// the rebuild form with the RECORDED answers prefilled — operator pubkey,
// domain, the carved thin-pool (a reused stock pool is never recorded),
// and k3s membership. The two size prompts stay blank: the config records
// nothing about them, so their "(blank = N)" semantics hold.
func TestRebuildFormSeededFromConfig(t *testing.T) {
	op := strings.Repeat("b", 64)
	cfgPath := writeRebuildCfg(t,
		"domain = \"world.test\"\n"+
			"operator_pubkey = \""+op+"\"\n"+
			"managed = [\"relay\", \"cp\", \"k3s\"]\n"+
			"[plane]\nthin_pool = \"freehold-thin\"\n")

	m := &Model{Mode: ModeBootstrap, CfgPath: cfgPath}
	if !keyPress(m, "B") {
		t.Fatal("B did not start the rebuild flow")
	}

	want := [7]string{op, "world.test", "", "freehold-thin", "", "y", ""}
	for i := 0; i < 7; i++ {
		if m.Flow == nil {
			t.Fatalf("flow vanished at step %d", i)
		}
		if got := m.Flow.Field.Value(); got != want[i] {
			t.Errorf("step %d prefilled %q, want %q", i, got, want[i])
		}
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if m.Flow == nil {
		t.Fatal("flow vanished before inputs were captured")
	}
	for i := 0; i < 7; i++ {
		if m.Flow.Inputs[i] != want[i] {
			t.Errorf("inputs[%d] = %q, want %q", i, m.Flow.Inputs[i], want[i])
		}
	}
}

// TestRebuildFormSeedCanBeEdited: the prefills are EDITABLE, not locked —
// appended typing changes the answer (cursor sits at the end of the seed).
func TestRebuildFormSeedCanBeEdited(t *testing.T) {
	cfgPath := writeRebuildCfg(t, "domain = \"world.test\"\noperator_pubkey = \""+strings.Repeat("b", 64)+"\"\n")
	m := &Model{Mode: ModeBootstrap, CfgPath: cfgPath}
	keyPress(m, "B")
	if m.Flow == nil {
		t.Fatal("B did not start the rebuild flow")
	}
	typeText(m, "-new")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := m.Flow.Inputs[0]; !strings.HasSuffix(got, "-new") {
		t.Errorf("edited seed should keep the prefix + the appended text, got %q", got)
	}
}

// TestRebuildFormAcceptsSeededDefaults: six bare enters on a seeded form
// must launch the rebuild with the RECORDED world — same args the operator
// would have typed by hand.
func TestRebuildFormAcceptsSeededDefaults(t *testing.T) {
	op := strings.Repeat("b", 64)
	cfgPath := writeRebuildCfg(t,
		"domain = \"world.test\"\n"+
			"operator_pubkey = \""+op+"\"\n"+
			"managed = [\"relay\", \"cp\", \"k3s\"]\n"+
			"[plane]\nthin_pool = \"freehold-thin\"\n")
	m := &Model{Mode: ModeBootstrap, CfgPath: cfgPath}
	keyPress(m, "B")
	var msg tea.Cmd
	for i := 0; i < 7; i++ {
		_, msg = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if msg == nil {
		t.Fatal("no activity message was dispatched")
	}
	start, ok := msg().(activityStartMsg)
	if !ok {
		t.Fatalf("dispatched a %T, want activityStartMsg", msg())
	}
	joined := strings.Join(start.args, " ")
	for _, want := range []string{"--operator-pubkey " + op, "--domain world.test", "--thin-pool freehold-thin"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rebuild args %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "--with-k3s=false") {
		t.Errorf("k3s is managed in the config — the args must not disable it: %q", joined)
	}
}

// TestRebuildFormAgentNameRoundTrip: a config with cpa_name seeds the
// agent-name step, and seven bare enters dispatch --agent-name with that
// recorded value — the name the operator chose at install round-trips into
// the rebuild args (A1: persist the CPA name).
func TestRebuildFormAgentNameRoundTrip(t *testing.T) {
	op := strings.Repeat("b", 64)
	cfgPath := writeRebuildCfg(t,
		"domain = \"world.test\"\n"+
			"operator_pubkey = \""+op+"\"\n"+
			"cpa_name = \"waldo\"\n"+
			"managed = [\"relay\", \"cp\", \"k3s\"]\n")
	m := &Model{Mode: ModeBootstrap, CfgPath: cfgPath}
	keyPress(m, "B")
	if m.Flow == nil {
		t.Fatal("B did not start the rebuild flow")
	}
	if got := m.Flow.Defaults[6]; got != "waldo" {
		t.Fatalf("agent-name step seeded %q, want %q", got, "waldo")
	}
	var msg tea.Cmd
	for i := 0; i < 7; i++ {
		_, msg = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if msg == nil {
		t.Fatal("no activity message was dispatched")
	}
	start, ok := msg().(activityStartMsg)
	if !ok {
		t.Fatalf("dispatched a %T, want activityStartMsg", msg())
	}
	if got := strings.Join(start.args, " "); !strings.Contains(got, "--agent-name waldo") {
		t.Errorf("rebuild args %q missing --agent-name waldo", got)
	}
}

// TestRebuildFormNoSeedWithoutConfig: no config = the old fresh-world
// behavior (nothing prefilled), and a config WITHOUT k3s seeds "n" — that
// prompt's blank default is y, so blank would boot k3s on a k3s-off world.
func TestRebuildFormNoSeedWithoutConfig(t *testing.T) {
	m := &Model{Mode: ModeBootstrap, CfgPath: "/nonexistent/config.toml"}
	keyPress(m, "B")
	if m.Flow == nil {
		t.Fatal("B did not start the rebuild flow")
	}
	if got := m.Flow.Field.Value(); got != "" {
		t.Errorf("no config = nothing prefilled, got %q", got)
	}

	cfgPath := writeRebuildCfg(t, "domain = \"world.test\"\noperator_pubkey = \""+strings.Repeat("b", 64)+"\"\n")
	m2 := &Model{Mode: ModeBootstrap, CfgPath: cfgPath}
	keyPress(m2, "B")
	if m2.Flow == nil {
		t.Fatal("B did not start the rebuild flow")
	}
	if got := flowDefaults(m2, flowRebuild)[5]; got != "n" {
		t.Errorf("config without k3s must seed the k3s answer as %q, got %q", "n", got)
	}
}

// TestRebuildFormK3sOffWorldStaysOff: six bare enters on a k3s-off world
// must dispatch --with-k3s=false — the seeded "n" has to round-trip into
// the args (a blank would have booted k3s: the prompt default is y).
func TestRebuildFormK3sOffWorldStaysOff(t *testing.T) {
	cfgPath := writeRebuildCfg(t,
		"domain = \"world.test\"\n"+
			"operator_pubkey = \""+strings.Repeat("b", 64)+"\"\n"+
			"managed = [\"relay\", \"cp\"]\n")
	m := &Model{Mode: ModeBootstrap, CfgPath: cfgPath}
	keyPress(m, "B")
	var msg tea.Cmd
	for i := 0; i < 7; i++ {
		_, msg = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if msg == nil {
		t.Fatal("no activity message was dispatched")
	}
	start, ok := msg().(activityStartMsg)
	if !ok {
		t.Fatalf("dispatched a %T, want activityStartMsg", msg())
	}
	joined := strings.Join(start.args, " ")
	if !strings.Contains(joined, "--with-k3s=false") {
		t.Errorf("k3s-off world must dispatch --with-k3s=false, got %q", joined)
	}
}

// writeRebuildCfg writes a scratch config.toml and returns its path.
func writeRebuildCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
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

// TestDoorKeyWaitingRenderedNotError: the pause lands INSIDE the activity
// view as a yellow waiting state, NOT the red error path. The rebuild
// subprocess exits non-zero with the door-key block on stdout — that is
// the EXPECTED operator-paused state.
func TestDoorKeyWaitingRenderedNotError(t *testing.T) {
	m := &Model{Mode: ModeBootstrap, CfgPath: "/nonexistent/config.toml"}
	args := []string{"rebuild", "--yes"}
	a := &activity{kind: "rebuild", title: "rebuilding", args: args, spin: newSpinner()}
	a.lines = []string{
		"provisioning runner…",
		"the door needs a NEW ssh key before rebuild can continue — install it on host, then re-run rebuild:",
		"",
		"    ssh-ed25519 AAAA freehold",
	}
	a.procErr = errors.New("exit status 1")
	m.activity = a

	_, _ = m.Update(actDoneMsg{a: a})
	if a.wait == "" {
		t.Fatal("the door pause must be captured on the activity")
	}
	if !a.done || a.ok {
		t.Errorf("the pause is done but NOT ok (done=%v ok=%v)", a.done, a.ok)
	}
	if m.Err != "" {
		t.Errorf("the door pause must not set a dashboard error, got %q", m.Err)
	}
	view := m.View()
	if !strings.Contains(view, "waiting for the operator") {
		t.Errorf("activity view must announce the waiting state, got:\n%s", view)
	}
	if !strings.Contains(view, "ssh-ed25519") {
		t.Errorf("activity view must show the key to install, got:\n%s", view)
	}
	if !strings.Contains(view, "ENTER") {
		t.Errorf("activity view must offer ENTER to resume the rebuild, got:\n%s", view)
	}
}

// TestDoorGateEnterRetry pins the Rust door flow in the activity view: the
// operator stays INSIDE the gate. ENTER re-runs the SAME rebuild in place
// (a fresh streaming activity with the SAME args — never back to the
// 6-field form), ESC cancels, and a second pause re-arms the gate.
func TestDoorGateEnterRetry(t *testing.T) {
	oldExec := activityExec
	activityExec = func(bin string, args ...string) *exec.Cmd { return exec.Command("true") }
	defer func() { activityExec = oldExec }()

	args := []string{"rebuild", "--yes", "--operator-pubkey", strings.Repeat("a", 64), "--domain", "world.test"}
	m := &Model{Mode: ModeBootstrap, CfgPath: "/nonexistent/config.toml"}
	pause := func(a *activity, out string) {
		a.lines = []string{out}
		a.procErr = errors.New("exit status 1")
		_, _ = m.Update(actDoneMsg{a: a})
	}
	a := &activity{kind: "rebuild", title: "rebuilding", args: args, spin: newSpinner()}
	m.activity = a
	pause(a, "the door needs a NEW ssh key …")
	if a.wait == "" || m.Flow != nil {
		t.Fatal("the pause must hold the gate without a form")
	}

	// ENTER: re-runs in place — a fresh streaming rebuild activity, the
	// SAME args, no form.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("ENTER must re-run the rebuild")
	}
	if m.activity == nil || m.activity == a || m.activity.kind != "rebuild" {
		t.Fatal("ENTER must open a fresh rebuild activity in place")
	}
	if !reflect.DeepEqual(m.activity.args, args) {
		t.Errorf("the resumed rebuild must keep the EXACT args, got %v", m.activity.args)
	}
	if m.activity.wait != "" || m.Flow != nil || m.Msg == "" {
		t.Errorf("ENTER must leave the gate (wait=%q flow=%v msg=%q)", m.activity.wait, m.Flow, m.Msg)
	}

	// The resumed run pauses again (key still not installed) → the gate
	// re-arms on the SAME activity, same args.
	a2 := m.activity
	pause(a2, "the door needs its ssh key …")
	if m.activity != a2 || a2.wait == "" {
		t.Fatal("a second pause must re-arm the gate in place")
	}

	// ESC: cancel — gate cleared, no error, no form.
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.activity != nil || m.Flow != nil {
		t.Errorf("esc must clear the door gate (activity=%v flow=%v)", m.activity, m.Flow)
	}
	if m.Err != "" {
		t.Errorf("esc is a cancel, not an error: %q", m.Err)
	}
}

// TestDoorGateSwallowsKeys: while paused at the door, stray keys (including
// B) must not open flows or disarm the gate — only ENTER/esc pass.
func TestDoorGateSwallowsKeys(t *testing.T) {
	m := &Model{Mode: ModeBootstrap}
	a := &activity{kind: "rebuild", args: []string{"rebuild", "--yes"}, spin: newSpinner(), done: true, wait: "the door needs …"}
	m.activity = a
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("B")})
	if m.Flow != nil {
		t.Error("B at the door gate must not open the rebuild form")
	}
	if m.activity != a || a.wait == "" {
		t.Error("stray keys must not disarm the gate")
	}
}

// TestActivityReplacesDashboard: while an activity runs, View shows ONLY
// the activity surface — no dashboard tabs, no shortcut footer.
func TestActivityReplacesDashboard(t *testing.T) {
	m := &Model{Mode: ModeRunning, Domain: "world.test"}
	a := &activity{kind: "teardown", title: "tearing down the world", spin: newSpinner()}
	a.lines = []string{"destroyed relay LXC 100"}
	m.activity = a
	out := m.View()
	for _, gone := range []string{"Tab/Shift-Tab", "t teardown", "[Services]"} {
		if strings.Contains(out, gone) {
			t.Errorf("the activity view must not leak dashboard chrome %q:\n%s", gone, out)
		}
	}
	if !strings.Contains(out, "destroyed relay LXC 100") {
		t.Errorf("the streamed line must be visible:\n%s", out)
	}
	if !strings.Contains(out, "ctrl+c aborts") {
		t.Errorf("the working view must show its ONLY key:\n%s", out)
	}
}

// TestActivityStreamsLines: lines land one at a time and the pump re-arms
// until EOF, then the done state classifies success.
func TestActivityStreamsLines(t *testing.T) {
	oldExec := activityExec
	activityExec = func(bin string, args ...string) *exec.Cmd {
		return exec.Command("printf", "one\\ntwo\\nthree\\n")
	}
	defer func() { activityExec = oldExec }()

	m := &Model{Mode: ModeRunning, Domain: "world.test"}
	_, _ = m.startSubprocessActivity("teardown", "tearing down the world", []string{"anything"})
	if m.activity == nil {
		t.Fatal("startSubprocessActivity must arm the activity")
	}
	// drive the pump exactly like bubbletea would (each cmd → msg → Update);
	// the batch wrapper from tea.Batch is opaque, so take the pump directly.
	cmd := m.pumpActLine(m.activity)
	for i := 0; i < 8; i++ {
		msg := cmd()
		_, next := m.Update(msg)
		if _, isDone := msg.(actDoneMsg); isDone {
			break
		}
		cmd = next
		if cmd == nil {
			break
		}
	}
	if m.activity == nil {
		t.Fatal("the activity must survive until the done key")
	}
	if !m.activity.done || !m.activity.ok {
		t.Fatalf("clean exit must classify ok (done=%v ok=%v)", m.activity.done, m.activity.ok)
	}
	if strings.Join(m.activity.lines, ",") != "one,two,three" {
		t.Errorf("every line must stream in order, got %v", m.activity.lines)
	}
}

// TestActivityScannerErrorNotCleanDone: a line over the scanner's 512 KiB
// cap makes Scan() return false with ErrTooLong WHILE the child keeps
// running — that must classify as a FAILURE, never a clean "done" (the
// stream is partial and the child is still live). The child is this test
// binary itself (a 600 KiB ARGV would blow ARG_MAX, so it must be stdout).
func TestActivityScannerErrorNotCleanDone(t *testing.T) {
	oldExec := activityExec
	activityExec = func(bin string, args ...string) *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=TestActivityScannerBigLineHelper")
		c.Env = append(os.Environ(), "FREEHOLD_TEST_BIG_LINE=1")
		return c
	}
	defer func() { activityExec = oldExec }()

	m := &Model{Mode: ModeRunning, Domain: "world.test"}
	_, _ = m.startSubprocessActivity("teardown", "tearing down the world", []string{"anything"})
	if m.activity == nil {
		t.Fatal("startSubprocessActivity must arm the activity")
	}
	cmd := m.pumpActLine(m.activity)
	for i := 0; i < 8; i++ {
		msg := cmd()
		_, next := m.Update(msg)
		if _, isDone := msg.(actDoneMsg); isDone {
			break
		}
		cmd = next
		if cmd == nil {
			break
		}
	}
	if m.activity == nil {
		t.Fatal("the activity must survive until the done key")
	}
	if !m.activity.done {
		t.Fatal("the scanner error must still deliver the done state")
	}
	if m.activity.ok {
		t.Fatal("a scanner error must NOT classify as a clean done")
	}
	if !strings.Contains(m.activity.fail, "stream broke") {
		t.Errorf("failure must surface the scanner error, got %q", m.activity.fail)
	}
}

// TestActivityScannerBigLineHelper is the child: emit a single line over the
// pump scanner's 512 KiB cap (a real argument would blow ARG_MAX).
func TestActivityScannerBigLineHelper(t *testing.T) {
	if os.Getenv("FREEHOLD_TEST_BIG_LINE") != "1" {
		return
	}
	if _, err := os.Stdout.WriteString(strings.Repeat("x", 600*1024) + "\n"); err != nil {
		t.Errorf("write failed: %v", err)
	}
}

// TestTeardownStepsFromConfig: starting a teardown activity seeds one
// checkbox slot per MANAGED LXC from the recorded config (label = role +
// vmid) — the "checkboxes" the operator sees, same shape as the boot view.
func TestTeardownStepsFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	tomlBody := "domain = \"world.test\"\nmanaged = [\"relay\", \"cp\", \"k3s\"]\n" +
		"[lxc.relay]\nvmid = 100\n[lxc.cp]\nvmid = 101\n[lxc.k3s]\nvmid = 102\n"
	if err := os.WriteFile(cfgPath, []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	m := &Model{Mode: ModeRunning, Domain: "world.test", CfgPath: cfgPath}
	a := &activity{kind: "teardown", title: "tearing down the world", spin: newSpinner()}
	m.seedTeardownSteps(a)

	if len(a.steps) != 3 {
		t.Fatalf("one slot per managed LXC, got %v", a.steps)
	}
	for i, want := range []string{"relay LXC 100", "cp LXC 101", "k3s LXC 102"} {
		if a.steps[i].label != want || a.steps[i].state != stepPending {
			t.Errorf("step %d: want pending %q, got %+v", i, want, a.steps[i])
		}
	}
}

// TestTeardownLinesFlipCheckboxes: the streamed subprocess lines drive the
// slots — "destroying relay LXC 100" flips it to RUNNING, "destroyed relay
// LXC 100" (or already-gone / never-created) flips it to ✓; non-LXC lines
// leave every slot untouched.
func TestTeardownLinesFlipCheckboxes(t *testing.T) {
	a := &activity{kind: "teardown", spin: newSpinner()}
	a.steps = []actStep{
		{label: "relay LXC 100", state: stepPending},
		{label: "cp LXC 101", state: stepPending},
		{label: "k3s LXC 102", state: stepPending},
	}

	a.feedTeardownLine("door verified (proxmox-box)")
	if a.steps[0].state != stepPending {
		t.Errorf("a non-LXC line must not touch any slot: %+v", a.steps[0])
	}

	a.feedTeardownLine("destroying relay LXC 100")
	if a.steps[0].state != stepRunning {
		t.Errorf("'destroying' must flip the slot to running: %+v", a.steps[0])
	}
	if a.steps[1].state != stepPending || a.steps[2].state != stepPending {
		t.Errorf("only the relay slot may move: %+v", a.steps)
	}

	a.feedTeardownLine("destroyed relay LXC 100")
	if a.steps[0].state != stepOK {
		t.Errorf("'destroyed' must flip the slot to ✓: %+v", a.steps[0])
	}

	a.feedTeardownLine("cp LXC 101: already gone")
	a.feedTeardownLine("k3s LXC: never created (no vmid recorded)")
	if a.steps[1].state != stepOK || a.steps[1].detail != "already gone" {
		t.Errorf("'already gone' must ✓ the cp slot with its reason: %+v", a.steps[1])
	}
	if a.steps[2].state != stepOK || a.steps[2].detail != "never created" {
		t.Errorf("'never created' must ✓ the k3s slot with its reason: %+v", a.steps[2])
	}

	// the CLI's Live hook indents every streamed line ("  " + line) — the
	// parser must match the bare text or the checkboxes never flip.
	a.steps[0].state = stepPending
	a.feedTeardownLine("  destroying relay LXC 100")
	if a.steps[0].state != stepRunning {
		t.Errorf("indented 'destroying' must still flip to running: %+v", a.steps[0])
	}
	a.feedTeardownLine("  destroyed relay LXC 100")
	if a.steps[0].state != stepOK {
		t.Errorf("indented 'destroyed' must still flip to ✓: %+v", a.steps[0])
	}
}

// TestTeardownViewRendersCheckboxRows: the activity view shows the ✓ rows
// and the in-flight slot while teardown runs — the active-checkbox UX.
func TestTeardownViewRendersCheckboxRows(t *testing.T) {
	m := &Model{Mode: ModeRunning, Domain: "world.test"}
	a := &activity{kind: "teardown", title: "tearing down the world", spin: newSpinner()}
	a.steps = []actStep{
		{label: "relay LXC 100", state: stepOK},
		{label: "cp LXC 101", state: stepRunning},
		{label: "k3s LXC 102", state: stepPending},
	}
	m.activity = a
	out := m.View()

	if !strings.Contains(out, "✓ relay LXC 100") {
		t.Errorf("finished slot must render a ✓ row:\n%s", out)
	}
	if !strings.Contains(out, "cp LXC 101…") {
		t.Errorf("in-flight slot must render its label:\n%s", out)
	}
	if !strings.Contains(out, strings.Repeat(".", 30)) {
		t.Errorf("queued slot must render placeholder dots:\n%s", out)
	}
	if !strings.Contains(out, "destroying cp LXC 101…") {
		t.Errorf("the spinner line must name the LXC being destroyed:\n%s", out)
	}
}

// TestTeardownStreamDrivesSteps end-to-end: a teardown activity seeded from
// the config flips its slots as the subprocess stream lands — the full
// pump → Update loop, no fakes.
func TestTeardownStreamDrivesSteps(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	tomlBody := "domain = \"world.test\"\nmanaged = [\"relay\", \"cp\"]\n" +
		"[lxc.relay]\nvmid = 100\n[lxc.cp]\nvmid = 101\n"
	if err := os.WriteFile(cfgPath, []byte(tomlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	oldExec := activityExec
	activityExec = func(bin string, args ...string) *exec.Cmd {
		return exec.Command("printf",
			"door verified (proxmox-box)\\ndestroying relay LXC 100\\ndestroyed relay LXC 100\\ndestroying cp LXC 101\\ndestroyed cp LXC 101\\nconfig KEPT INTACT\\n")
	}
	defer func() { activityExec = oldExec }()

	m := &Model{Mode: ModeRunning, Domain: "world.test", CfgPath: cfgPath}
	_, _ = m.startSubprocessActivity("teardown", "tearing down the world", []string{"anything"})
	if m.activity == nil || len(m.activity.steps) != 2 {
		t.Fatalf("teardown must seed its checkbox slots, got %+v", m.activity)
	}

	cmd := m.pumpActLine(m.activity)
	for i := 0; i < 12; i++ {
		msg := cmd()
		_, next := m.Update(msg)
		if _, isDone := msg.(actDoneMsg); isDone {
			break
		}
		cmd = next
		if cmd == nil {
			break
		}
	}

	a := m.activity
	if a == nil || !a.done || !a.ok {
		t.Fatalf("clean teardown must finish ok (a=%v)", a)
	}
	for i, s := range a.steps {
		if s.state != stepOK {
			t.Errorf("step %d must end ✓, got %+v", i, s)
		}
	}
}

// TestFailTailKeepsTheCause pins the failure-detail contract the operator
// hit (2026-08-30 — "! teardown failed:  — the pool is NOT removed"):
// teardown's pool step embeds the child's stderr INSIDE its error, so the
// actionable cause ("unknown flag: --addr") is NOT the last line — a
// single-line tail returned only the trailing clause and the cause was
// invisible. tail must keep the last 3 non-empty lines so the cause shows.
func TestFailTailKeepsTheCause(t *testing.T) {
	out := "thin-pool teardown FAILED: dataset destroy for relay failed:\n" +
		"unknown flag: --addr\n" +
		" — the pool is NOT removed\n"
	got := tail(out)
	if !strings.Contains(got, "unknown flag: --addr") {
		t.Errorf("tail must keep the actionable cause, got %q", got)
	}
	if !strings.Contains(got, "the pool is NOT removed") {
		t.Errorf("tail must keep the trailing clause too, got %q", got)
	}
}
