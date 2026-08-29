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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/textinput"

	"freehold/orchestrator/internal/console"
	"freehold/orchestrator/internal/flows"
	"freehold/orchestrator/internal/state"
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
)

type tuiFlow struct {
	Kind   flowKind
	Step   int
	Inputs [6]string
	Field  *textinput.Model
}

type flowMsg struct {
	ok     string
	err    error
	reload bool
}

func textInputNew(placeholder string) *textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.Focus()
	return &ti
}

func ncols(k flowKind) int {
	switch k {
	case flowProvision, flowRotate, flowGrant:
		return 2
	case flowDeployRelay, flowDeployCp:
		return 3
	case flowBootstrap:
		return 4
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
	m.Flow = &tuiFlow{Kind: k, Field: textInputNew(promptLabel(k, 0))}
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
			f.Field = textInputNew(promptLabel(f.Kind, next))
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

		// The three world-mutation forms reuse the wired CLI drivers.
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
			out, err := runSelf("bootstrap",
				"--addr", addr,
				"--kind", kind,
				"--domain", domain,
				"--operator-pubkey", op,
			)
			if err != nil {
				return flowMsg{err: fmt.Errorf("bootstrap failed: %w (%s)", err, tail(out))}
			}
			return flowMsg{ok: "bootstrap ok — " + tail(out), reload: true}
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
			out, err := runSelf(args...)
			if err != nil {
				return flowMsg{err: fmt.Errorf("deploy-relay failed: %w (%s)", err, tail(out))}
			}
			return flowMsg{ok: "relay deployed — " + tail(out), reload: true}
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
			out, err := runSelf(args...)
			if err != nil {
				return flowMsg{err: fmt.Errorf("deploy-cp failed: %w (%s)", err, tail(out))}
			}
			return flowMsg{ok: "CP deployed — " + tail(out), reload: true}
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

// tail returns the last non-empty line of a command's output (the CLI's
// success/error detail line).
func tail(s string) string {
	var last string
	for _, line := range splitLines(s) {
		if line != "" {
			last = line
		}
	}
	if len(last) > 200 {
		return last[:200] + "…"
	}
	return last
}

func splitLines(s string) []string {
	return strings.Split(s, "\n")
}

func (m *Model) refreshLocal() {
	st, err := state.Open(freeholdStateDir())
	if err != nil {
		return
	}
	m.Runners = nil
	for name, rec := range st.Snapshot().Runners {
		addr := "—"
		if rec.McpAddr != nil {
			addr = *rec.McpAddr
		}
		m.Runners = append(m.Runners, RunnerRow{Name: name, Status: string(rec.Status), Pubkey: rec.NostrPubkey, Addr: addr})
	}
	if len(m.Runners) == 0 {
		m.Runners = []RunnerRow{{Name: "(no runners)"}}
	}
}
