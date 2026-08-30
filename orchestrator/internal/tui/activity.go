// The activity view: freehold's full-screen "working on it" mode, modeled on
// bubbletea's send-msg / tui-daemon-combo examples — a spinner + live label
// on top, a result window streaming below it (✓ lines for finished steps,
// placeholder dots for queued ones), and one help line at the bottom.
//
// Anything that takes longer than a heartbeat — the boot-time world check,
// teardown, rebuild, bootstrap, the deploys — runs HERE, replacing the
// dashboard and its keyboard-shortcut footer entirely (no stale shortcuts
// while the world is being rebuilt). Two flavors share one view:
//
//   - step activities (the boot check): ordered named probes, one at a
//     time — "✓ config · ✓ relay · ⠋ checking control plane";
//   - subprocess activities (teardown/rebuild/bootstrap/deploys): the
//     freehold binary runs itself as a child and its stdout streams in
//     line by line while the spinner turns.
//
// Keys while working: swallowed except ctrl+c (kills the child, quits).
// When done: any key returns to the dashboard (success re-checks the world
// visibly; failure surfaces the tail). A rebuild that pauses at the door
// gate renders the key + ENTER-to-resume inside the same view.
package tui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"freehold/orchestrator/internal/config"
)

var (
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleFail    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleCurrent = lipgloss.NewStyle().Foreground(lipgloss.Color("211"))
	styleDots    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

type stepState int

const (
	stepPending stepState = iota
	stepRunning
	stepOK
	stepFail
)

// activity is one full-screen run. EXACTLY one of steps/lines is used.
type activity struct {
	kind  string // boot | teardown | rebuild | bootstrap | deploy
	title string
	args  []string // subprocess args (kept for the door-gate retry)

	// step flavor
	steps    []actStep
	bootFns  []func() (string, bool)
	bootDone func() // settles the mode after the last step

	// subprocess flavor
	lines   []string
	scanner *bufio.Scanner
	proc    *exec.Cmd
	procErr error

	spin spinner.Model
	done bool
	ok   bool
	fail string
	wait string // rebuild door-gate pause text
}

type actStep struct {
	label  string
	state  stepState
	detail string
}

// activity messages.
type actStepMsg struct {
	idx    int
	ok     bool
	detail string
}
type actLineMsg struct {
	a    *activity // the owning activity — stale pumps are dropped
	line string
}
type actDoneMsg struct{ a *activity }

// activityExec builds the subprocess a subprocess-activity streams.
// Injectable so tests never spawn the real freehold binary.
var activityExec = func(bin string, args ...string) *exec.Cmd {
	return exec.Command(bin, args...)
}

func newSpinner() spinner.Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))
	return s
}

// ---- boot-time world check ------------------------------------------------

// startBootActivity replaces the dashboard with the sequential world check.
// It sets m.activity itself and returns the initial command batch (the
// daemon-combo pattern: one re-arming Cmd per step).
func (m *Model) startBootActivity(title string) tea.Cmd {
	cfg, err := config.Load(m.CfgPath)
	if err != nil {
		m.Err = "config: " + err.Error()
		m.activity = nil
		return nil
	}
	if cfg == nil {
		// config gone (e.g. a --data teardown just removed it) — nothing
		// left to check; the dashboard shows bootstrap mode.
		m.Mode, m.HasConfig, m.activity = ModeBootstrap, false, nil
		return nil
	}
	m.HasConfig, m.Domain = true, cfg.Domain

	a := &activity{kind: "boot", title: title, spin: newSpinner()}
	type def struct {
		label string
		fn    func() (string, bool)
	}
	defs := []def{
		{"config", func() (string, bool) {
			return cfg.Domain + " · " + m.CfgPath, true
		}},
		{"runner", func() (string, bool) {
			m.RunnerReach = config.URLReachable("http://" + cfg.Runner.Addr)
			if m.RunnerReach {
				return cfg.Runner.Addr + " reachable", true
			}
			return "no answer on " + cfg.Runner.Addr, false
		}},
		{"relay", func() (string, bool) {
			m.RelayLive = config.RelayLive(cfg)
			if m.RelayLive {
				return "live (/_liveness ok)", true
			}
			return "no answer at " + cfg.RelayURL, false
		}},
		{"control plane", func() (string, bool) {
			m.CPLive = cpLive(cfg)
			if m.CPLive {
				return "healthy (:8080 answered from inside its LXC)", true
			}
			return "down (no :8080 answer through the runner)", false
		}},
		{"k3s", func() (string, bool) {
			if cfg.Lxc.K3s.Ip == nil {
				m.K3sLive = false
				return "not on record — skipped", true
			}
			m.K3sLive = config.K3sLive(cfg)
			if m.K3sLive {
				return "API healthy at " + config.StripCIDR(*cfg.Lxc.K3s.Ip) + ":6443", true
			}
			return "no API answer at " + config.StripCIDR(*cfg.Lxc.K3s.Ip) + ":6443", false
		}},
		{"world state", func() (string, bool) {
			m.buildServices(cfg)
			m.readLocalRunners(cfg)
			m.buildAgents(cfg)
			m.refreshData(cfg)
			return fmt.Sprintf("%d services · %d runners · %d agents",
				len(m.Services), len(m.Runners), len(m.Agents)), true
		}},
	}
	for _, d := range defs {
		a.steps = append(a.steps, actStep{label: d.label})
		a.bootFns = append(a.bootFns, d.fn)
	}
	a.bootDone = func() {
		m.Converged = m.RelayLive && m.CPLive && m.RunnerReach
		if m.Converged {
			m.Mode = ModeRunning
		} else {
			m.Mode = ModeConfigure
		}
		m.LastRef = time.Now()
	}
	m.activity = a
	return tea.Batch(m.runBootStep(0), a.spin.Tick)
}

func (m *Model) runBootStep(i int) tea.Cmd {
	return func() tea.Msg {
		a := m.activity
		if a == nil || i >= len(a.bootFns) {
			return actStepMsg{idx: i}
		}
		detail, ok := a.bootFns[i]()
		return actStepMsg{idx: i, ok: ok, detail: detail}
	}
}

// ---- subprocess activities (teardown / rebuild / bootstrap / deploys) -----

// startSubprocessActivity streams the freehold binary's own output into the
// full-screen view. Returns the Model + initial command batch.
func (m *Model) startSubprocessActivity(kind, title string, args []string) (tea.Model, tea.Cmd) {
	bin, err := os.Executable()
	if err != nil {
		m.Err = "cannot resolve the freehold binary: " + err.Error()
		return m, nil
	}
	a := &activity{kind: kind, title: title, args: args, spin: newSpinner()}
	pr, pw := io.Pipe()
	a.scanner = bufio.NewScanner(pr)
	a.scanner.Buffer(make([]byte, 64*1024), 512*1024)

	cmd := activityExec(bin, args...)
	cmd.Stdout = pw
	cmd.Stderr = pw
	a.proc = cmd
	m.activity = a

	go func() {
		runErr := cmd.Run()
		a.procErr = runErr // set BEFORE Close: the pump sees it after the EOF
		_ = pw.Close()
	}()
	return m, tea.Batch(m.pumpActLine(a), a.spin.Tick)
}

// pumpActLine reads ONE line from the child (blocking) and re-arms itself
// on every line — the daemon-combo re-arming Cmd; EOF delivers actDoneMsg.
// bubbletea runs each Cmd on its own goroutine, so the blocked read never
// stalls the event loop.
func (m *Model) pumpActLine(a *activity) tea.Cmd {
	return func() tea.Msg {
		if a.scanner.Scan() {
			return actLineMsg{a: a, line: a.scanner.Text()}
		}
		return actDoneMsg{a: a}
	}
}

// ---- Update handling -------------------------------------------------------

// handleActivityMsg drives the activity state machine. Returns (handled,
// model, cmd): handled=false means the message was not an activity message.
func (m *Model) handleActivityMsg(msg tea.Msg) (bool, tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case actStepMsg:
		a := m.activity
		if a == nil || a.kind != "boot" {
			return true, m, nil
		}
		if v.idx < len(a.steps) {
			a.steps[v.idx].detail = v.detail
			if v.ok {
				a.steps[v.idx].state = stepOK
			} else {
				a.steps[v.idx].state = stepFail
			}
		}
		next := v.idx + 1
		if next < len(a.steps) {
			a.steps[next].state = stepRunning
			return true, m, tea.Batch(m.runBootStep(next), a.spin.Tick)
		}
		if a.bootDone != nil {
			a.bootDone()
		}
		// the boot check auto-continues into the dashboard.
		m.activity = nil
		return true, m, nil

	case actLineMsg:
		a := m.activity
		if a == nil || v.a != a {
			return true, m, nil // stale pump from a superseded activity
		}
		if line := strings.TrimRight(v.line, " \t"); line != "" {
			a.lines = append(a.lines, line)
		}
		return true, m, m.pumpActLine(a)

	case actDoneMsg:
		a := m.activity
		if a == nil || v.a != a || a.done || a.kind == "boot" {
			return true, m, nil // stale pump / already classified
		}
		a.done = true
		out := strings.Join(a.lines, "\n")
		switch {
		case a.kind == "rebuild" && a.procErr != nil:
			// the door-gate bail is an EXPECTED pause, not a failure.
			if wait := doorKeyWaiting(out); wait != "" {
				a.wait = wait
				return true, m, nil
			}
			a.fail = "rebuild failed: " + tail(out)
		case a.procErr != nil:
			a.fail = a.kind + " failed: " + tail(out)
		default:
			a.ok = true
		}
		return true, m, nil

	case spinner.TickMsg:
		if m.activity == nil || m.activity.done {
			return true, m, nil
		}
		var cmd tea.Cmd
		m.activity.spin, cmd = m.activity.spin.Update(v)
		return true, m, cmd
	}
	return false, m, nil
}

// handleActivityKey is the key discipline inside the activity view: while
// working, everything is swallowed except ctrl+c (kill the child, quit);
// a rebuild paused at the door gate takes ENTER (resume) / esc (cancel);
// a finished activity returns to the dashboard on any key.
func (m *Model) handleActivityKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	a := m.activity
	if k.Type == tea.KeyCtrlC {
		if a.proc != nil && a.proc.Process != nil {
			_ = a.proc.Process.Kill()
		}
		return m, tea.Quit
	}
	if !a.done {
		return m, nil // swallow: no accidental quits mid-teardown
	}
	if a.wait != "" {
		switch k.Type {
		case tea.KeyEnter:
			args := a.args
			m.activity = nil
			m.Msg = "re-testing the door and resuming rebuild…"
			return m.startSubprocessActivity("rebuild", "rebuilding "+m.Domain, args)
		case tea.KeyEsc:
			m.activity = nil
			m.Msg = "rebuild cancelled at the door — the key is still printed above"
			return m, nil
		}
		return m, nil
	}
	// done: any key returns to the dashboard.
	ok := a.ok
	m.activity = nil
	if ok {
		// success: re-check the world so the dashboard is honest.
		return m, m.startBootActivity("checking the world")
	}
	m.Err = a.fail
	return m, nil
}

// ---- View ------------------------------------------------------------------

func (m *Model) activityView() string {
	a := m.activity
	var b strings.Builder
	head := styleTitle.Render(" freehold ")
	if m.Domain != "" {
		head += styleDim.Render(" " + m.Domain + " ·")
	}
	b.WriteString(head + styleDim.Render(" "+a.title) + "\n")

	if a.done {
		b.WriteString("\n")
		switch {
		case a.wait != "":
			b.WriteString(styleYellow.Render("waiting for the operator:\n"+a.wait) + "\n\n")
			b.WriteString(styleYellow.Render("install the key, then press ENTER to re-test the door and resume — esc to cancel") + "\n")
		case a.ok:
			b.WriteString(styleOK.Render("✓ "+a.title+" — done") + "\n\n")
			b.WriteString(styleDots.Render("press any key to return to the dashboard") + "\n")
		default:
			b.WriteString(styleFail.Render("! "+a.fail) + "\n\n")
			b.WriteString(styleDots.Render("press any key to return to the dashboard") + "\n")
		}
		return lipgloss.NewStyle().Render(b.String())
	}

	// the send-msg top line: spinner + what is happening right now.
	b.WriteString("\n" + a.spin.View() + " " + styleCurrent.Render(a.liveLabel()) + "\n\n")

	for _, s := range a.steps {
		switch s.state {
		case stepOK:
			line := styleOK.Render("✓") + " " + s.label
			if s.detail != "" {
				line += " " + styleDots.Render(s.detail)
			}
			b.WriteString("  " + line + "\n")
		case stepFail:
			line := styleFail.Render("✗") + " " + styleFail.Render(s.label)
			if s.detail != "" {
				line += " " + styleDots.Render(s.detail)
			}
			b.WriteString("  " + line + "\n")
		case stepPending:
			// send-msg's empty result slots: placeholder dots.
			b.WriteString("  " + styleDots.Render(strings.Repeat(".", 30)) + "\n")
		}
	}
	for _, l := range lastLines(a.lines, 12) {
		b.WriteString("  " + l + "\n")
	}

	b.WriteString("\n" + styleDots.Render("ctrl+c aborts — the dashboard returns when this finishes"))
	return lipgloss.NewStyle().Render(b.String())
}

// liveLabel is the spinner's current-action text.
func (a *activity) liveLabel() string {
	if a.kind == "boot" {
		for _, s := range a.steps {
			if s.state == stepRunning {
				return "checking " + s.label + "…"
			}
		}
	}
	return a.title + "…"
}

func lastLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}
