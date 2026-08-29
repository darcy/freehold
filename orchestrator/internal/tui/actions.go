package tui

import (
	"fmt"
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
)

type tuiFlow struct {
	Kind   flowKind
	Step   int
	Inputs [2]string
	Field  *textinput.Model
}

type flowMsg struct {
	ok  string
	err error
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
