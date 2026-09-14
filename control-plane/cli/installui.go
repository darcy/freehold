// installui.go — `freehold install`'s bubbletea wizard: a multi-step form over
// the rebuild engine (the replacement for the sequential dialoguer-style
// prompts in install.go). The wizard collects EVERY answer up front — including
// relay/CP domains and the proxy IP, which runBootstrap would otherwise
// re-prompt at runtime — then hands the SAME rebuildFlags to the engine.
//
// The sequential collectAnswers/installerUI path stays for non-TTY input
// (tests, piped runs) and is what runInstallWith tests drive; the wizard is the
// interactive front only. The pure identity helpers (mintAgentIdentity,
// writeOperatorIdentity, loadRPubkey, operatorDir) are shared by both.
package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"freehold/contract/crypto"
	"freehold/platform/provisioning/drive"
)

// installStep is one wizard form field.
type installStep struct {
	label string
	def   string
	// masked hides the input (operator nsec).
	masked bool
	// yesno renders a y/n prompt instead of a text field.
	yesno bool
}

var installSteps = []installStep{
	{label: "Proxmox host (address the runner will SSH into)", def: "root@192.168.30.224"},
	{label: "Runner name", def: "proxmox-box"},
	{label: "Runner MCP address (loopback)", def: "127.0.0.1:8787"},
	{label: "Relay domain (its Buzz origin — REQUIRED)", def: ""},
	{label: "Control-plane domain (REQUIRED)", def: ""},
	{label: "proxy static IP (CIDR, e.g. 192.168.30.8/24) — REQUIRED, the one address relay/CP resolve to", def: ""},
	{label: "LXC rootfs size (GB)", def: "16"},
	{label: "LXC memory (MB)", def: "2048"},
	// identity select (1 = have key, 2 = generate) — rendered as a choice.
	{label: "Operator identity: 1) have a Nostr key already  2) generate one for me", def: "1", yesno: true},
	{label: "Your Nostr public key (npub1… or 64 hex)", def: ""},
	{label: "Your nsec (nsec1… — optional; empty = skip persisting)", def: "", masked: true},
	{label: "No existing storage backend — create one (ZFS/LVM-thin)? (y/n)", def: "", yesno: true},
}

type installModel struct {
	step  int
	field textinput.Model
	vals  []string
	err   string
	done  bool
	// collected rebuildFlags, filled on confirm.
	f        rebuildFlags
	identity string // operator identity dir ("" = not persisted)
	// newEngine is the seam (runInstallWith injects a sentinel).
	newEngine func(rebuildFlags) (*rebuildEngine, error)
	out       io.Writer
}

func newInstallModel(newEngine func(rebuildFlags) (*rebuildEngine, error), out io.Writer) *installModel {
	m := &installModel{newEngine: newEngine, out: out}
	m.field = installField(0, "")
	return m
}

func installField(step int, def string) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = installSteps[step].label
	if installSteps[step].masked {
		ti.EchoMode = textinput.EchoPassword
	}
	// Fields start EMPTY (the dashboard pattern): typing appends cleanly, and
	// advance applies the declared default on blank. Prefilling would make the
	// operator's first keystroke append to a stale value.
	ti.Focus()
	return ti
}

func (m *installModel) Init() tea.Cmd { return nil }

func (m *installModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "enter", "tab":
			return m.advance(msg)
		default:
			nf, _ := m.field.Update(msg)
			m.field = nf
			return m, nil
		}
	}
	return m, nil
}

// advance stores the current field's answer and moves to the next step. On the
// last step (storage consent) it collects the identity, builds the flags, and
// hands the engine over (the wizard is the front-end only; the engine runs its
// own pipeline after the wizard quits).
func (m *installModel) advance(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := strings.TrimSpace(m.field.Value())
	step := m.step
	// A blank text field takes its default (dialoguer parity: blank = def).
	if s == "" && installSteps[step].def != "" && !installSteps[step].yesno && !installSteps[step].masked {
		s = installSteps[step].def
	}
	m.vals = append(m.vals, s)
	if step < len(installSteps)-1 {
		m.step = step + 1
		m.field = installField(m.step, installSteps[m.step].def)
		return m, nil
	}
	// Final step: storage consent. Validate + build flags, then quit into the engine.
	if err := m.finish(); err != nil {
		m.err = err.Error()
		return m, tea.Quit
	}
	m.done = true
	return m, tea.Quit
}

func (m *installModel) finish() error {
	host, runner, addr := m.vals[0], m.vals[1], m.vals[2]
	relayDomain, cpDomain, proxyIP := m.vals[3], m.vals[4], m.vals[5]
	rootfs := m.vals[6]
	memory := m.vals[7]
	idChoice := m.vals[8]

	if relayDomain == "" {
		return fmt.Errorf("relay domain is required")
	}
	if cpDomain == "" {
		cpDomain = relayDomain
	}
	if proxyIP == "" {
		return fmt.Errorf("proxy static IP is required (the one address relay/CP resolve to)")
	}
	if !strings.Contains(proxyIP, "/") {
		return fmt.Errorf("proxy static IP must be CIDR (host/prefix) — got %q", proxyIP)
	}
	rootfsN, err := strconv.ParseUint(strings.TrimSpace(rootfs), 10, 32)
	if err != nil || rootfsN < 1 {
		return fmt.Errorf("LXC rootfs size must be a number >= 1")
	}
	memN, err := strconv.ParseUint(strings.TrimSpace(memory), 10, 32)
	if err != nil || memN < 1 {
		return fmt.Errorf("LXC memory must be a number >= 1")
	}
	pk, opDir, err := m.resolveIdentity(idChoice)
	if err != nil {
		return err
	}
	consent := strings.EqualFold(strings.TrimSpace(m.vals[11]), "y")

	m.f = rebuildFlags{
		addr:               addr,
		target:             runner,
		host:               host,
		domain:             relayDomain,
		relayDomain:        relayDomain,
		cpDomain:           cpDomain,
		proxyIP:            proxyIP,
		operatorPubkey:     pk,
		operatorIdentity:   opDir,
		sizeGB:             drive.TenantLVSizeGB,
		poolSizeGB:         drive.FreshPoolSizeGB,
		noK3s:              false,
		noLitellm:          false,
		rootfsGB:           uint32(rootfsN),
		memoryMB:           uint32(memN),
		relayGw:            "192.168.30.1",
		bridge:             "vmbr0",
		litellmProviderKey: os.Getenv("FREEHOLD_LITELLM_PROVIDER_KEY"),
		configPath:         defaultConfigPath(),
		confirmStorage:     consent,
	}
	return nil
}

// resolveIdentity resolves the operator pubkey + optional identity dir from the
// have-key (1) / generate (2) choice, persisting when requested (same pure
// helpers as the sequential collectOperatorIdentity).
func (m *installModel) resolveIdentity(choice string) (string, string, error) {
	switch strings.TrimSpace(choice) {
	case "2":
		dir := operatorDir()
		reused := false
		if _, err := os.Stat(opIdentityPath(dir)); err == nil {
			reused = true
		} else if err := mintAgentIdentity(dir); err != nil {
			return "", "", err
		}
		pk, err := loadRPubkey(dir)
		if err != nil {
			return "", "", err
		}
		if !reused {
			fmt.Fprintf(m.out, "  Generated a fresh operator identity (pubkey %s)\n", pk)
		}
		return pk, dir, nil
	default: // have-key
		pk, err := crypto.ParsePubkeyInput(strings.TrimSpace(m.vals[9]))
		if err != nil {
			return "", "", fmt.Errorf("invalid pubkey: %w", err)
		}
		nsec := strings.TrimSpace(m.vals[10])
		if nsec == "" {
			return pk, "", nil
		}
		secret, err := crypto.NsecToSecret(nsec)
		if err != nil {
			return "", "", fmt.Errorf("invalid nsec: %w", err)
		}
		derived, err := crypto.PubkeyFromSecret(secret[:])
		if err != nil {
			return "", "", err
		}
		if derived != pk {
			return "", "", fmt.Errorf("the nsec's pubkey %s does not match the pubkey you entered (%s) — fix one", derived, pk)
		}
		dir := operatorDir()
		if _, err := os.Stat(opIdentityPath(dir)); err == nil {
			return "", "", fmt.Errorf("an operator identity already exists at %s — remove it or reuse that key", dir)
		}
		if err := writeOperatorIdentity(dir, secret[:]); err != nil {
			return "", "", err
		}
		return pk, dir, nil
	}
}

func opIdentityPath(dir string) string { return dir + "/identity.json" }

func (m *installModel) View() string {
	var b strings.Builder
	b.WriteString(installBanner)
	if m.err != "" {
		b.WriteString(styleRed.Render("! "+m.err) + "\n")
	}
	if m.step < len(installSteps) {
		b.WriteString("\n  " + styleYellow.Render(installSteps[m.step].label) + "\n")
		b.WriteString("  " + m.field.View() + "\n")
		b.WriteString("\n  " + styleDim.Render("enter/tab: next · esc: quit"))
	}
	return lipgloss.NewStyle().Render(b.String())
}

// runInstallUI launches the wizard (interactive TTY) and hands the collected
// flags to the engine. The engine runs AFTER the wizard quits; its runtime
// prompts read a fresh reader over the terminal.
func runInstallUI(newEngine func(rebuildFlags) (*rebuildEngine, error), in io.Reader, out io.Writer) error {
	model := newInstallModel(newEngine, out)
	p := tea.NewProgram(model, tea.WithInput(in))
	final, err := p.Run()
	if err != nil {
		return err
	}
	im, ok := final.(*installModel)
	if !ok || !im.done {
		return fmt.Errorf("install aborted")
	}
	applyInstallDefaults(&im.f)
	eng, err := newEngine(im.f)
	if err != nil {
		return err
	}
	eng.out = out
	eng.stdin = bufio.NewReader(in)
	// The wizard filled relay/CP domains + proxy IP, so runBootstrap's promptDomains
	// and proxy prompts skip; the door gate stays interactive (a NEW door key must
	// be authorized + ENTER-resumed, not a silent --yes bail).
	return eng.runBootstrap()
}

var (
	styleRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("211"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)
