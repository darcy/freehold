// Console + world-mutation action flows for the TUI.
//
// The running-mode flows (login/provision/rotate/revoke/grant) talk to the
// console client in-process. The bootstrap/configure-mode forms reuse the
// FULLY WIRED CLI drivers by exec'ing the freehold binary (this binary) as
// a subprocess — the same pattern the teardown engine uses, which avoids a
// cli<->tui import cycle and cobra re-entrancy while keeping a single
// source of truth for the drivers.
package tui

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/console"
	"freehold/orchestrator/internal/flows"
)

type consoleClient struct {
	client *console.Client
}

type flowKind int

const (
	flowNone flowKind = iota
	flowLogin
	flowProvision
	flowRotate
	flowRevoke
	flowGrant
	flowBootstrap
	flowDeployRelay
	flowDeployCp
	flowTeardown
	flowRebuild
)

type tuiFlow struct {
	Kind   flowKind
	Step   int
	Inputs [6]string
	Field  *textinput.Model
	// Defaults are the prefilled answers per step — sourced from the
	// recorded config when one exists (see flowDefaults). Blank = no
	// recorded value; the prompt's own "(blank = N)" semantics apply.
	Defaults [6]string
}

type flowMsg struct {
	ok  string
	err error
}

// activityStartMsg tells Update to swap the whole screen into a streaming
// subprocess activity (teardown / rebuild / bootstrap / deploys). The form
// dispatch returns it INSTEAD of flowMsg for the world-mutation flows: the
// operator never stares at a blank dashboard while the world changes.
type activityStartMsg struct {
	kind  string
	title string
	args  []string
}

func textInputNew(placeholder string) *textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.Focus()
	return &ti
}

// fieldFor builds one step's input: the label as placeholder, the recorded
// default as the prefilled value (cursor at the end so the operator just
// hits enter — or edits it).
func fieldFor(k flowKind, step int, def string) *textinput.Model {
	ti := textInputNew(promptLabel(k, step))
	if def != "" {
		ti.SetValue(def)
		ti.CursorEnd()
	}
	return ti
}

// flowDefaults reads the recorded config and prefills the form steps that
// have a recorded value. The rebuild form's four config-backed answers:
// operator pubkey, domain, the carved thin-pool (a reused stock pool is
// deliberately NOT recorded — Plane.ThinPool says freehold owns it), and
// the k3s membership: "y" when k3s is managed, else "n" — that prompt's
// blank default is y, so a k3s-off world MUST seed an explicit n (a blank
// would boot k3s). The two size prompts have no config record; blank keeps
// their "(blank = N)" semantics. Absent/unreadable config = no defaults
// (fresh-world behavior, unchanged).
func flowDefaults(m *Model, k flowKind) [6]string {
	var d [6]string
	if k != flowRebuild || m.CfgPath == "" {
		return d
	}
	cfg, err := config.Load(m.CfgPath)
	if err != nil || cfg == nil {
		return d
	}
	d[0] = cfg.OperatorPubkey
	d[1] = cfg.Domain
	if cfg.Plane.ThinPool != nil {
		d[3] = *cfg.Plane.ThinPool
	}
	d[5] = "n"
	for _, role := range cfg.Managed {
		if role == "k3s" {
			d[5] = "y"
			break
		}
	}
	return d
}

func ncols(k flowKind) int {
	switch k {
	case flowProvision, flowRotate, flowGrant:
		return 2
	case flowDeployRelay, flowDeployCp:
		return 3
	case flowBootstrap:
		return 4
	case flowTeardown:
		return 2
	case flowRebuild:
		return 6
	default:
		return 1
	}
}

func promptLabel(k flowKind, step int) string {
	switch k {
	case flowLogin:
		return "console base URL"
	case flowProvision:
		if step == 0 {
			return "runner name"
		}
		return "address (user@host:22)"
	case flowRotate:
		if step == 0 {
			return "runner name"
		}
		return "new secret"
	case flowRevoke:
		return "runner name to revoke"
	case flowGrant:
		if step == 0 {
			return "runner name"
		}
		return "agent pubkey (64-hex)"
	case flowBootstrap:
		switch step {
		case 0:
			return "runner address (blank = 127.0.0.1:8787)"
		case 1:
			return "kind: proxmox-lxc | vultr-vps | hetzner-vps"
		case 2:
			return "domain (the relay's identity)"
		default:
			return "operator pubkey (64-hex)"
		}
	case flowDeployRelay:
		switch step {
		case 0:
			return "owner pubkey (64-hex)"
		case 1:
			return "relay URL (blank = https://<domain>)"
		default:
			return "operator pubkey (64-hex)"
		}
	case flowDeployCp:
		switch step {
		case 0:
			return "local path of the control-plane binary"
		case 1:
			return "relay URL (blank = https://<domain>)"
		default:
			return "operator pubkey (64-hex)"
		}
	case flowTeardown:
		switch step {
		case 0:
			return "destroy tenant data too? (yes | no)"
		default:
			return "CONFIRM destroying the whole world (all LXCs, door key KEPT)? type yes"
		}
	case flowRebuild:
		switch step {
		case 0:
			return "operator pubkey (npub1… or 64-hex)"
		case 1:
			return "domain (the relay's identity)"
		case 2:
			return "tenant LV size GB (blank = 10)"
		case 3:
			return "thin-pool name (blank = reuse detected / carve default)"
		case 4:
			return "new thin-pool size GB (blank = 40, used when carving)"
		default:
			return "boot k3s too? (y/n, blank = y)"
		}
	default:
		return "value"
	}
}

func fieldValue(f *tuiFlow) string {
	if f != nil && f.Field != nil {
		return f.Field.Value()
	}
	return ""
}

func (m *Model) beginPrompt(k flowKind) {
	m.Flow = &tuiFlow{Kind: k, Defaults: flowDefaults(m, k)}
	m.Flow.Field = fieldFor(k, 0, m.Flow.Defaults[0])
}

func (m *Model) handleFlow(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := m.Flow
	if f == nil {
		return m, nil
	}
	if msg.String() == "esc" {
		m.Flow = nil
		m.Msg = "cancelled"
		return m, nil
	}
	nf, _ := f.Field.Update(msg)
	*f.Field = nf
	if msg.String() == "enter" || msg.String() == "tab" {
		f.Inputs[f.Step] = f.Field.Value()
		next := f.Step + 1
		if next < ncols(f.Kind) {
			f.Step = next
			f.Field = fieldFor(f.Kind, next, f.Defaults[next])
			return m, nil
		}
		return m, runFlowAction(m, f)
	}
	return m, nil
}

// selfBin resolves the running freehold binary (the forms exec it as a
// subprocess to reuse the wired CLI drivers).
func selfBin() (string, error) {
	return os.Executable()
}

// runSelf runs the freehold binary with the given subcommand args and
// returns combined stdout/stderr.
func runSelf(args ...string) (string, error) {
	bin, err := selfBin()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	return buf.String(), err
}

func runFlowAction(m *Model, f *tuiFlow) tea.Cmd {
	return func() tea.Msg {
		if f.Kind == flowLogin {
			agentDir := freeholdStateDir() + "/agent-ops"
			auth, err := flows.AgentAuth(agentDir)
			if err != nil {
				return flowMsg{err: fmt.Errorf("login: no agent identity: %w", err)}
			}
			defer auth.Zero()
			c, err := console.Login(f.Inputs[0], auth.Secret[:], 15*time.Second)
			if err != nil {
				return flowMsg{err: err}
			}
			m.console = &consoleClient{client: c}
			return flowMsg{ok: "console login ok — cookie " + c.Cookie()[:10] + "..."}
		}

		// The five world-mutation forms all become FULL-SCREEN streaming
		// activities: the freehold binary re-execs itself and its stdout
		// lands in the activity view line by line (send-msg pattern) —
		// the operator never watches a frozen dashboard mid-mutation.
		switch f.Kind {
		case flowBootstrap:
			addr := f.Inputs[0]
			if addr == "" {
				addr = "127.0.0.1:8787"
			}
			kind, domain, op := f.Inputs[1], f.Inputs[2], f.Inputs[3]
			if kind == "" || domain == "" || op == "" {
				return flowMsg{err: fmt.Errorf("bootstrap needs kind, domain and operator pubkey")}
			}
			return activityStartMsg{kind: "bootstrap", title: "bootstrapping " + domain, args: []string{
				"bootstrap",
				"--addr", addr,
				"--kind", kind,
				"--domain", domain,
				"--operator-pubkey", op,
			}}
		case flowDeployRelay:
			owner, url, op := f.Inputs[0], f.Inputs[1], f.Inputs[2]
			if owner == "" || op == "" {
				return flowMsg{err: fmt.Errorf("deploy-relay needs owner pubkey and operator pubkey")}
			}
			if url == "" {
				if m.Domain == "" {
					return flowMsg{err: fmt.Errorf("no domain known — give an explicit relay URL")}
				}
				url = "https://" + m.Domain
			}
			args := []string{"deploy-relay",
				"--owner-pubkey", owner,
				"--relay-url", url,
				"--operator-pubkey", op,
			}
			if m.Domain != "" {
				args = append(args, "--domain", m.Domain)
			}
			return activityStartMsg{kind: "deploy", title: "deploying the relay", args: args}
		case flowDeployCp:
			binary, url, op := f.Inputs[0], f.Inputs[1], f.Inputs[2]
			if binary == "" || url == "" {
				return flowMsg{err: fmt.Errorf("deploy-cp needs the CP binary path and relay URL")}
			}
			args := []string{"deploy-cp",
				"--binary", binary,
				"--relay-url", url,
			}
			if op != "" {
				args = append(args, "--operator-pubkey", op)
			}
			return activityStartMsg{kind: "deploy", title: "deploying the control plane", args: args}
		case flowTeardown:
			// The whole-world teardown destroys LXCs (+ datasets with yes);
			// with --data it wipes door key + local home + config. The
			// SECOND step is the world-destroy confirm: anything but an
			// explicit "yes" aborts — t+Enter must not tear down the world
			// by accident (the CLI's own --yes silent path is not
			// reachable).
			if !strings.EqualFold(strings.TrimSpace(f.Inputs[1]), "yes") {
				return flowMsg{err: fmt.Errorf("teardown cancelled: type yes to confirm destroying the whole world")}
			}
			args := []string{"teardown", "--yes"}
			if strings.EqualFold(f.Inputs[0], "yes") {
				args = append(args, "--data")
			}
			return activityStartMsg{kind: "teardown", title: "tearing down the world", args: args}
		case flowRebuild:
			op, domain := f.Inputs[0], f.Inputs[1]
			if op == "" || domain == "" {
				return flowMsg{err: fmt.Errorf("rebuild needs operator pubkey and domain")}
			}
			args := []string{"rebuild", "--yes",
				"--operator-pubkey", op,
				"--domain", domain,
			}
			if f.Inputs[2] != "" {
				args = append(args, "--size-gb", f.Inputs[2])
			}
			if f.Inputs[3] != "" {
				args = append(args, "--thin-pool", f.Inputs[3])
			}
			if f.Inputs[4] != "" {
				args = append(args, "--pool-size-gb", f.Inputs[4])
			}
			if strings.EqualFold(strings.TrimSpace(f.Inputs[5]), "n") {
				args = append(args, "--with-k3s=false")
			}
			return activityStartMsg{kind: "rebuild", title: "rebuilding " + domain, args: args}
		}
		if m.console == nil || m.console.client == nil {
			return flowMsg{err: fmt.Errorf("not logged into a console — press l first")}
		}
		c := m.console.client
		switch f.Kind {
		case flowProvision:
			_, err := c.Provision(&console.ProvisionReq{Name: f.Inputs[0], Kind: "ssh", Address: f.Inputs[1]})
			if err != nil {
				return flowMsg{err: err}
			}
			return flowMsg{ok: "provisioned " + f.Inputs[0]}
		case flowRotate:
			_, err := c.Rotate(&console.SecretReq{Name: f.Inputs[0], Secret: f.Inputs[1]})
			if err != nil {
				return flowMsg{err: err}
			}
			return flowMsg{ok: "rotated " + f.Inputs[0]}
		case flowRevoke:
			_, err := c.Revoke(f.Inputs[0])
			if err != nil {
				return flowMsg{err: err}
			}
			return flowMsg{ok: "revoked " + f.Inputs[0]}
		case flowGrant:
			_, err := c.Grant(f.Inputs[0], f.Inputs[1])
			if err != nil {
				return flowMsg{err: err}
			}
			return flowMsg{ok: "granted " + f.Inputs[1] + " on " + f.Inputs[0]}
		default:
			return flowMsg{err: fmt.Errorf("unhandled flow")}
		}
	}
}

// tail returns the last NON-EMPTY lines of a command's output joined with
// " · " (up to 3, capped at 200 chars). One line is not enough when the
// CLI embeds a subprocess's stderr mid-message — the actionable cause
// ("unknown flag: --addr") must not be swallowed by a trailing clause.
func tail(s string) string {
	var kept []string
	for _, line := range splitLines(s) {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) > 3 {
		kept = kept[len(kept)-3:]
	}
	last := strings.Join(kept, " · ")
	if len(last) > 200 {
		return last[:200] + "…"
	}
	return last
}

func splitLines(s string) []string {
	return strings.Split(s, "\n")
}

// doorKeyWaiting extracts rebuild's EXPECTED door-gate pause from the
// subprocess output and returns the install instruction ("" = not the
// pause) so the TUI renders it as a waiting state, not an error.
// Two --yes bails produce it:
//   - fresh provision:  "the door needs a NEW ssh key before rebuild..."
//   - reuse + auth fail: "the door needs its ssh key before rebuild..."
//
// The shared "the door needs" prefix anchors both.
func doorKeyWaiting(out string) string {
	const marker = "the door needs"
	i := strings.Index(out, marker)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(out[i:])
}

func (m *Model) refreshLocal() {
	m.refreshRunners(m.cfg)
}
