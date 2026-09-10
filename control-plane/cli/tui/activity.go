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
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"freehold/contract/config"
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
	// stepSkip is a legitimate no-op/skipped probe (not deployed, not on
	// record, unreachable) — rendered with a neutral marker, never a green ✓
	// that looks like a success.
	stepSkip
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
	scanErr error // scanner failure (token over cap / read error) — distinct from a clean EOF

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
	m.HasConfig, m.Domain = true, cfg.RelayHost()

	a := &activity{kind: "boot", title: title, spin: newSpinner()}
	type def struct {
		label string
		fn    func() (string, bool)
	}
	defs := []def{
		{"config", func() (string, bool) {
			return cfg.RelayHost() + " · " + m.CfgPath, true
		}}, {"runner", func() (string, bool) {
			if cfg.Runner.Addr == "" {
				// A login-only box has no LOCAL provisioning runner — operating
				// through the CP, so reachability is the console session's.
				m.RunnerReach = true
				return "no local runner (login-only — operating through the CP)", true
			}
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
			if cfg.Runner.Addr == "" {
				// Login-only: the CP's liveness is its own console session —
				// /api/overview answers through the NIP-98 session, not the
				// (absent) local runner.
				m.CPLive = cpConsoleLive(m, cfg)
				if m.CPLive {
					return "healthy (console session answered /api/overview)", true
				}
				return "down (no console session answer)", false
			}
			m.CPLive = cpLive(cfg)
			if m.CPLive {
				return "healthy (:8080 answered from inside its LXC)", true
			}
			return "down (no :8080 answer through the runner)", false
		}},
		{"k3s", func() (string, bool) {
			if cfg.Lxc.K3s.Ip == nil {
				// Management box (no local coords): the CP-served health
				// (applyCPWorldHealth) is authoritative. Do NOT reset the flag
				// to false — a boot probe landing after the CP hook would
				// clobber green back to red. Leave it; the CP hook owns it.
				return "not on record — skipped", true
			}
			m.K3sLive = config.K3sLive(cfg)
			if m.K3sLive {
				return "API healthy at " + config.StripCIDR(*cfg.Lxc.K3s.Ip) + ":6443", true
			}
			return "no API answer at " + config.StripCIDR(*cfg.Lxc.K3s.Ip) + ":6443", false
		}},
		{"litellm", func() (string, bool) {
			if cfg.Litellm.URL == "" {
				return "not deployed (no gateway coords)", true
			}
			m.LitellmLive = config.LitellmLive(cfg)
			if m.LitellmLive {
				return "gateway healthy at " + cfg.Litellm.URL, true
			}
			return "no answer at " + cfg.Litellm.URL, false
		}},
		{"caddy", func() (string, bool) {
			if cfg.Caddy.URL == "" || cfg.Caddy.Host == "" {
				return "not deployed (no TLS edge coords)", true
			}
			m.CaddyLive = config.URLReachable("https://" + cfg.Caddy.Host)
			if m.CaddyLive {
				return "edge reachable at https://" + cfg.Caddy.Host, true
			}
			return "no TLS answer at https://" + cfg.Caddy.Host, false
		}},
		{"dns", func() (string, bool) {
			// The CP resolver is authoritative: pull the live records once
			// per refresh, keep the old snapshot on failure (fallback to the
			// config mirror stays in buildServices when there is no snapshot).
			if live := dnsRowsLive(cfg); len(live) > 0 {
				m.DNS = live
				return fmt.Sprintf("%d resolver records", len(live)), true
			}
			return "resolver unreachable (mirror fallback)", len(m.DNS) > 0
		}},
		{"world state", func() (string, bool) {
			m.buildServices(cfg)
			m.refreshRunners(cfg)
			// buildAgents refreshes m.Facts (the world facts) — buildCerts +
			// refreshData must run AFTER it so the Certs/DATA views render the
			// CURRENT facts on a manual `r` refresh (not one cycle stale).
			m.buildAgents(cfg)
			m.buildCerts(cfg)
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
		m.Converged = converged(cfg.Runner.Addr, m.RelayLive, m.CPLive, m.RunnerReach)
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

// converged settles the running/configure decision. A box WITH a local runner
// is a converging world: every pillar must be live. A runnerless box is a
// login-only OPERATOR — there is no local world to converge; it is operable as
// soon as the CP console it logged into answers. The relay may be unseeded on a
// fresh login box (the CP's /api/world feeds it once the deployed CP carries
// its relay coords), so an unknown/unreachable relay must not lock the operator
// out of the CP console — the probe row still shows it honestly.
func converged(runnerAddr string, relayLive, cpLive, runnerReach bool) bool {
	if runnerAddr == "" {
		return cpLive
	}
	return relayLive && cpLive && runnerReach
}

// isSkipDetail reports whether a boot-probe detail is a legitimate no-op/skip
// rather than an actual success — so the view renders a neutral marker instead
// of a green check that reads as "up".
func isSkipDetail(detail string) bool {
	for _, p := range []string{
		"not deployed", "not on record", "not managed",
		"no gateway coords", "no TLS edge coords", "no :8080 answer",
		"resolver unreachable", "unreachable (mirror", "not recorded",
		"live probe failed", "no live snapshot",
	} {
		if strings.Contains(detail, p) {
			return true
		}
	}
	return false
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
	if kind == "teardown" {
		m.seedTeardownSteps(a)
	}
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

// seedTeardownSteps pre-populates the teardown activity's checkbox slots
// from the recorded managed LXCs (same config facts teardown reads). Each
// slot fills live as the subprocess stream announces the LXC:
// "destroying relay LXC 100" flips it to running, "destroyed relay LXC
// 100" (or "already gone" / "never created") flips it to ✓.
func (m *Model) seedTeardownSteps(a *activity) {
	cfg, err := config.Load(m.CfgPath)
	if err != nil || cfg == nil {
		return // the stream window shows everything anyway
	}
	vmidOf := map[string]*uint32{"relay": cfg.Lxc.Relay.Vmid, "cp": cfg.Lxc.Cp.Vmid, "k3s": cfg.Lxc.K3s.Vmid}
	for _, role := range cfg.Managed {
		label := role + " LXC"
		if v := vmidOf[role]; v != nil {
			label += fmt.Sprintf(" %d", *v)
		}
		a.steps = append(a.steps, actStep{label: label, state: stepPending})
	}
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
		// Scan()==false is CLEAN EOF only when Err() is nil — a token over
		// the cap or a read error must not be classified as a clean done.
		a.scanErr = a.scanner.Err()
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
			switch {
			case !v.ok:
				a.steps[v.idx].state = stepFail
			case isSkipDetail(v.detail):
				a.steps[v.idx].state = stepSkip
			default:
				a.steps[v.idx].state = stepOK
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
			a.feedTeardownLine(line)
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
		case a.scanErr != nil:
			// scanner broke (token over cap / read error) while the child
			// kept running: never report a clean done for a partial stream.
			a.fail = a.kind + " stream broke: " + a.scanErr.Error()
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
			m.Msg = "rebuild cancelled at the door — note the key was shown only in this full-screen view (no scrollback); re-run rebuild (B) to re-surface it through the same gate"
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
		case stepSkip:
			line := styleDots.Render("–") + " " + s.label
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
			// A queued slot: show its label dimmed (and a placeholder) so the
			// list of steps being checked is visible — not anonymous dots.
			label := s.label
			if label == "" {
				label = "…"
			}
			b.WriteString("  " + styleDots.Render("· "+label+" ·") + "\n")
		case stepRunning:
			line := a.spin.View() + " " + styleCurrent.Render(s.label+"…")
			if s.detail != "" {
				line += " " + styleDots.Render(s.detail)
			}
			b.WriteString("  " + line + "\n")
		}
	}
	for _, l := range lastLines(a.lines, 12) {
		b.WriteString("  " + l + "\n")
	}

	b.WriteString("\n" + styleDots.Render("ctrl+c aborts — the dashboard returns when this finishes"))
	return lipgloss.NewStyle().Render(b.String())
}

// teardownLineRe matches the per-LXC progress lines the teardown CLI
// streams: `destroying relay LXC 100` at the start of each destroy and
// `destroyed relay LXC 100` (or `already gone` / `never created`) when it
// finishes. The TUI turns them into checkbox state; everything else stays
// in the stream window.
var teardownLineRe = regexp.MustCompile(`^(destroying|destroyed) (\S+) LXC (\d+)`)
var teardownNoopRe = regexp.MustCompile(`^(\S+) LXC(?: \d+)?: (already gone|never created)`)

// feedTeardownLine drives the teardown checkbox slots from the streamed
// subprocess lines (steps-only for teardown activities).
func (a *activity) feedTeardownLine(line string) {
	if a.kind != "teardown" {
		return
	}
	// the CLI's Live hook indents every streamed line ("  " + line), so the
	// subprocess bytes arrive with a leading indent — match the bare text.
	line = strings.TrimSpace(line)
	if m := teardownLineRe.FindStringSubmatch(line); m != nil {
		if i := a.stepIndex(m[2]); i >= 0 {
			if m[1] == "destroying" {
				a.steps[i].state = stepRunning
			} else {
				a.steps[i].state = stepOK
			}
			// the label already carries the vmid (seeded from the config);
			// no detail — "relay LXC 100" would render "relay LXC 100 LXC 100".
		}
		return
	}
	if m := teardownNoopRe.FindStringSubmatch(line); m != nil {
		if i := a.stepIndex(m[1]); i >= 0 {
			a.steps[i].state = stepOK
			a.steps[i].detail = m[2]
		}
	}
}

func (a *activity) stepIndex(role string) int {
	for i, s := range a.steps {
		if s.label == role+" LXC" || strings.HasPrefix(s.label, role+" LXC ") {
			return i
		}
	}
	return -1
}

// liveLabel is the spinner's current-action text.
func (a *activity) liveLabel() string {
	for _, s := range a.steps {
		if s.state != stepRunning {
			continue
		}
		if a.kind == "boot" {
			return "checking " + s.label + "…"
		}
		if a.kind == "teardown" {
			return "destroying " + s.label + "…"
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
