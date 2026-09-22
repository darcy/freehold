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
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/crypto"
	"freehold/contract/identity"
	"freehold/freehold-cli/login"
	"freehold/platform/services/certificates/letsencrypt"
)

type consoleClient struct {
	client *console.Client
}

// loginMsg reports the outcome of a background auto-login (from a persisted
// operator identity on TUI start). auth=false means no persisted identity.
type loginMsg struct {
	auth   bool
	client *console.Client
	pubkey string
	err    error
}

// cfgURL is the console base URL to log into: the recorded CP URL (the only
// console URL freehold records). Empty when none is configured.
func (m *Model) cfgURL() string {
	if m.cfg != nil && m.cfg.CPURL != "" {
		return m.cfg.CPURL
	}
	return ""
}

// autoLoginCmd silently re-establishes the console session on TUI start from a
// persisted operator identity (installed via `l` or `freehold --login`). No-op
// when none is recorded or no console is configured. The result arrives as a
// loginMsg.
func (m *Model) autoLoginCmd() tea.Cmd {
	if m.cfg == nil || m.cfg.CPURL == "" {
		return nil
	}
	hexStr, err := oplogin.SecretHex()
	if err != nil {
		return nil
	}
	secret := [32]byte{}
	if b, derr := hex.DecodeString(hexStr); derr == nil && len(b) == 32 {
		copy(secret[:], b)
	} else {
		return nil
	}
	return func() tea.Msg {
		c, err := oplogin.Login(m.cfg.CPURL, secret)
		if err != nil {
			return loginMsg{auth: true, err: err}
		}
		pk, _ := crypto.PubkeyFromSecret(secret[:])
		return loginMsg{auth: true, client: c, pubkey: pk}
	}
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
	Inputs [11]string
	Field  *textinput.Model
	// Defaults are the prefilled answers per step — sourced from the
	// recorded config when one exists (see flowDefaults). Blank = no
	// recorded value; the prompt's own "(blank = N)" semantics apply.
	Defaults [11]string
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
// hits enter — or edits it). The login nsec step is masked (a secret, like a
// password) and never prefilled from disk.
func fieldFor(k flowKind, step int, def string) *textinput.Model {
	ti := textInputNew(promptLabel(k, step))
	if def != "" && !(k == flowLogin && step == 0) {
		ti.SetValue(def)
		ti.CursorEnd()
	}
	if k == flowLogin && step == 0 {
		ti.EchoMode = textinput.EchoPassword
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
func flowDefaults(m *Model, k flowKind) [11]string {
	var d [11]string
	cfg, cerr := config.Load(m.CfgPath)
	if cerr != nil || cfg == nil {
		return d
	}
	if k == flowLogin {
		// The login URL defaults to the recorded CP URL (the operator's nsec
		// is never prefilled).
		d[1] = cfg.CPURL
		return d
	}
	if k != flowRebuild || m.CfgPath == "" {
		// No config: nothing to prefill. k3s (d[6]) and litellm (d[8]) stay
		// BLANK, which the arg builder reads as "y" — the full desired world
		// reconciles by default; opting out is an explicit "n".
		return d
	}
	d[0] = cfg.OperatorPubkey
	// relay + CP hosts are seeded from config (never derived).
	d[1] = cfg.RelayHost()
	d[2] = cfg.CPHost()
	if cfg.Plane.ThinPool != nil {
		d[4] = *cfg.Plane.ThinPool
	} // The desired world is FULL (reconcile-always): k3s (d[6]) and litellm
	// (d[8]) are left blank ("y" when dispatched) regardless of what the
	// recorded config listed, so a partial world is pulled up to the whole by
	// default. Opting out is an explicit "n".
	if cfg.CPAName != "" {
		d[7] = cfg.CPAName
	}
	return d
}

func ncols(k flowKind) int {
	switch k {
	case flowProvision, flowRotate, flowGrant:
		return 2
	case flowLogin:
		return 2
	case flowDeployRelay, flowDeployCp:
		return 3
	case flowBootstrap:
		return 4
	case flowTeardown:
		return 2
	case flowRebuild:
		return 11
	default:
		return 1
	}
}

func promptLabel(k flowKind, step int) string {
	switch k {
	case flowLogin:
		if step == 0 {
			return "your nsec (nsec1… or 64-hex) — logs you into the CP and is saved locally"
		}
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
			return "relay domain (its Buzz origin — REQUIRED)"
		case 2:
			return "control-plane domain (REQUIRED)"
		case 3:
			return "tenant LV size GB (blank = 10)"
		case 4:
			return "thin-pool name (blank = reuse detected / carve default)"
		case 5:
			return "new thin-pool size GB (blank = 40, used when carving)"
		case 6:
			return "boot k3s too? (y/n, blank = y)"
		case 7:
			return "CPA agent name (blank = freehold)"
		case 8:
			return "deploy litellm gateway + CPA model? (y/n, blank = y)"
		case 9:
			return "DNS provider for the certs (e.g. route53; blank = reuse stored)"
		default:
			return "DNS env KEY=VAL,KEY=VAL (blank = auto-detect, e.g. ~/.aws)"
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
		// Rebuild: build the args and dispatch. DNS provider credentials are a
		// VISIBLE form step (after litellm); a blank provider reuses any stored
		// credential. Any provided DNS info is stored (pre-verified) here.
		if f.Kind == flowRebuild {
			args, err := rebuildArgs(f)
			if err != nil {
				return m, func() tea.Msg { return flowMsg{err: err} }
			}
			return m, func() tea.Msg {
				if err := storeRebuildDNS(f); err != nil {
					return flowMsg{err: err}
				}
				return activityStartMsg{kind: "rebuild", title: "rebuilding " + strings.TrimSpace(f.Inputs[1]), args: args}
			}
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
			url := strings.TrimSpace(f.Inputs[1])
			if url == "" {
				url = m.cfgURL()
			}
			secret, err := oplogin.NsecToSecret(strings.TrimSpace(f.Inputs[0]))
			if err != nil {
				return flowMsg{err: fmt.Errorf("login: bad nsec: %w", err)}
			}
			pk, err := crypto.PubkeyFromSecret(secret[:])
			if err != nil {
				return flowMsg{err: err}
			}
			c, err := oplogin.Login(url, secret)
			if err != nil {
				return flowMsg{err: fmt.Errorf("login to %s: %w", url, err)}
			}
			// Only a SUCCESSFUL login is persisted as "the operator" — a
			// mistyped/wrong nsec never poisons the auto-login ledger.
			if _, err := oplogin.Save(secret); err != nil {
				return flowMsg{err: fmt.Errorf("login: save operator identity: %w", err)}
			}
			m.console = &consoleClient{client: c}
			m.consolePK = pk
			return flowMsg{ok: fmt.Sprintf("console login ok — operator %.12s", pk)}
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
			// Whole-world: `teardown` is CP-preserving; wanting the durable
			// plane gone too is `uninstall --remove-data` (the CP + this box's
			// doors + local state go with it). The SECOND step is the
			// destroy confirm: anything but an explicit "yes" aborts — t+Enter
			// must not tear down the world by accident (the CLI's own --yes
			// silent path is not reachable).
			if !strings.EqualFold(strings.TrimSpace(f.Inputs[1]), "yes") {
				return flowMsg{err: fmt.Errorf("teardown cancelled: type yes to confirm destroying the whole world")}
			}
			if strings.EqualFold(f.Inputs[0], "yes") {
				return activityStartMsg{kind: "uninstall", title: "uninstalling (removing the CP + plane)", args: []string{"uninstall", "--remove-data", "--yes"}}
			}
			return activityStartMsg{kind: "teardown", title: "tearing down the world (CP preserved)", args: []string{"teardown", "--yes"}}
		case flowRebuild:
			args, err := rebuildArgs(f)
			if err != nil {
				return flowMsg{err: err}
			}
			return activityStartMsg{kind: "rebuild", title: "rebuilding " + strings.TrimSpace(f.Inputs[1]), args: args}
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
	m.buildAgents(m.cfg)
	// buildAgents fetched the world facts (plane/certs/domains) — re-run the
	// Certs + DATA views so a management/login-only box renders them from the
	// CP (the boot check runs before auto-login, when the facts aren't loaded
	// yet). On the deployer box refreshData still does the live plane probe.
	m.buildCerts(m.cfg)
	m.refreshData(m.cfg)
}

// applyCPWorldHealth is the SINGLE-SOURCE world hook: for any box with a console
// session, the CP is the authority for the whole world — services, DNS, relay,
// and the pillar/flags. There is NO "management vs owner" divergence once a CP
// session exists; the box's own local config is only an offline fallback. Only
// ever turns a pillar green from a live CP answer; never fabricates an "up".
func (m *Model) applyCPWorldHealth() {
	if m.console == nil || m.console.client == nil || m.cfg == nil {
		return
	}
	w, err := m.console.client.World()
	if err != nil {
		return
	}
	m.cpWorld = w
	m.worldSvc = make(map[string]bool, len(w.Services))
	for _, s := range w.Services {
		m.worldSvc[s.Kind] = s.Up
	}
	// Pillar flags come straight from the CP's services report — the CP probes
	// relay/cp/k3s/litellm/caddy co-located and reports them all on /api/world,
	// so a box turns pillars green from the CP's live answer, never a local
	// probe of local config.
	for consts := range m.worldSvc {
		m.applyPillarFlag(m.worldSvc, consts)
	}
	// Still adopt the relay + cp coords into cfg so non-TUI consumers (exec,
	// door, world verbs) dial the CP-named relay, not a stale login snapshot.
	if w.RelayURL != "" && w.RelayURL != m.cfg.RelayURL {
		m.cfg.RelayURL = w.RelayURL
		if w.RelayWsURL != "" {
			m.cfg.RelayWsURL = w.RelayWsURL
		}
	}
	// DNS: the CP resolver is authoritative (no local runner exec needed).
	if rows, ok := m.cpDnsRows(); ok {
		m.DNS = rows
	}
	// Header domain: prefer the CP's recorded relay host (the public domain)
	// — set once at deploy and authoritative. Only when the console predates
	// serving relay_host does the resolver's explicit `relay.<domain>` record
	// derive it instead. Both beat the LAN IP a box dialed.
	if w.RelayHost != "" && w.RelayHost != m.cfg.RelayHost() {
		m.Domain = w.RelayHost
	} else if d := m.relayPublicHost(); d != "" && d != m.Domain {
		m.Domain = d
	}
	m.buildServices(m.cfg)
	m.buildCerts(m.cfg)
}

// applyPillarFlag maps a CP world-service kind to its live flag, so the world
// strip + converged() reflect the CP's truth for every pillar the CP reports.
func (m *Model) applyPillarFlag(worldSvc map[string]bool, kind string) {
	switch kind {
	case "relay":
		m.RelayLive = worldSvc[kind]
	case "cp":
		m.CPLive = worldSvc[kind]
	case "k3s":
		m.K3sLive = worldSvc[kind]
	case "litellm":
		m.LitellmLive = worldSvc[kind]
	case "caddy":
		m.CaddyLive = worldSvc[kind]
	}
}

// cpDnsRows fills the DNS view from the console's /api/dns (the CP resolver),
// used by a management box with no local runner. Returns whether rows exist.
func (m *Model) cpDnsRows() ([]DnsRow, bool) {
	if m.console == nil || m.console.client == nil {
		return nil, false
	}
	v, err := m.console.client.ListDNS()
	if err != nil || len(v.DNS) == 0 {
		return nil, false
	}
	rows := make([]DnsRow, 0, len(v.DNS))
	for _, d := range v.DNS {
		rows = append(rows, DnsRow{Name: d.Name, IP: d.IP, Source: "CP resolver"})
	}
	return rows, true
}

// relayPublicHost derives the relay's public hostname from the resolver's
// explicit `relay.<domain>` A record (the CP DNS the management box just read).
// A bare `relay` record or an IP-only name is skipped; a dotted hostname with
// letters is the public relay host — the domain a deployer box shows from its
// own relay_url, recovered on a management box without needing a CP console
// that serves relay_host.
func (m *Model) relayPublicHost() string {
	for _, d := range m.DNS {
		if !strings.HasPrefix(d.Name, "relay.") || !hostnameHasLetters(d.Name) {
			continue
		}
		return d.Name
	}
	return ""
}

func hostnameHasLetters(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

// --- CP-sourced boot status (management box) --------------------------------

// cpWorldSummary is the "control plane" boot step's report on a management box:
// the CP's world facts fetched on connect. The relay/k3s/litellm/caddy/dns
// steps read from the same snapshot, so the check reflects the CP, never stale
// local config.
func (m *Model) cpWorldSummary() string {
	if m.cpWorld == nil {
		return "healthy (world facts waiting on auto-login)"
	}
	s := fmt.Sprintf("healthy — %d services · %d dns · %d runners · %d agents",
		len(m.cpWorld.Services), len(m.DNS), len(m.Runners), len(m.Agents))
	if v := m.cpWorld.Version.Version; v != "" {
		s += " · " + v
	}
	return s
}

func (m *Model) cpRelayStatus() (string, bool) {
	if m.cpWorld == nil {
		return "awaiting CP world facts (auto-login)", true
	}
	if m.RelayLive {
		return "live via CP at " + m.cfg.RelayURL, true
	}
	return "no answer at " + m.cfg.RelayURL + " (CP-served)", false
}

func (m *Model) cpDnsStatus() (string, bool) {
	if m.cpWorld == nil {
		return "awaiting CP world facts (auto-login)", true
	}
	return fmt.Sprintf("%d resolver records (CP)", len(m.DNS)), true
}

// rebuildArgs builds the `freehold rebuild --yes` args from a completed
// rebuild form (shared by the flow dispatcher and the DNS pre-flow).
func rebuildArgs(f *tuiFlow) ([]string, error) {
	op, relay := strings.TrimSpace(f.Inputs[0]), strings.TrimSpace(f.Inputs[1])
	cp := strings.TrimSpace(f.Inputs[2])
	if op == "" || relay == "" || cp == "" {
		return nil, fmt.Errorf("rebuild needs operator pubkey, relay domain, and control-plane domain")
	}
	args := []string{"rebuild", "--yes",
		"--operator-pubkey", op,
		"--relay-domain", relay,
		"--cp-domain", cp,
	}
	if v := strings.TrimSpace(f.Inputs[3]); v != "" {
		args = append(args, "--size-gb", v)
	}
	if v := strings.TrimSpace(f.Inputs[4]); v != "" {
		args = append(args, "--thin-pool", v)
	}
	if v := strings.TrimSpace(f.Inputs[5]); v != "" {
		args = append(args, "--pool-size-gb", v)
	}
	if strings.EqualFold(strings.TrimSpace(f.Inputs[6]), "n") {
		args = append(args, "--no-k3s")
	}
	if name := strings.TrimSpace(f.Inputs[7]); name != "" {
		args = append(args, "--agent-name", name)
	}
	if strings.EqualFold(strings.TrimSpace(f.Inputs[8]), "n") {
		args = append(args, "--no-litellm")
	}
	return args, nil
}

// edgeNeedsDNS reports whether the rebuild form's world includes the Caddy edge
// (k3s on + relay/CP domains set), i.e. it will need DNS provider credentials.
func edgeNeedsDNS(f *tuiFlow) bool {
	return strings.TrimSpace(f.Inputs[1]) != "" &&
		strings.TrimSpace(f.Inputs[2]) != "" &&
		!strings.EqualFold(strings.TrimSpace(f.Inputs[6]), "n")
}

// storeRebuildDNS seals the DNS provider credential the operator typed into the
// rebuild form (fields 9/10) into BOTH relay + cp slots (pre-verified against
// the relay host), so the headless rebuild reuses it. A blank provider leaves any
// stored credential untouched (the rebuild reuses the one already on disk).
func storeRebuildDNS(f *tuiFlow) error {
	provider := strings.TrimSpace(f.Inputs[9])
	if provider == "" {
		return nil
	}
	if !cert.IsProvider(provider) {
		return fmt.Errorf("unknown DNS provider %q", provider)
	}
	env := map[string]string{}
	if v := strings.TrimSpace(f.Inputs[10]); v != "" {
		for _, kv := range strings.Split(v, ",") {
			k, val, ok := strings.Cut(kv, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return fmt.Errorf("DNS env expects KEY=VAL,KEY=VAL — bad entry %q", kv)
			}
			env[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	}
	relayHost := strings.TrimSpace(f.Inputs[1])
	if relayHost != "" {
		if err := cert.Verify(relayHost, provider, env); err != nil {
			return fmt.Errorf("DNS pre-verify failed: %w", err)
		}
	}
	id, err := identity.Load(freeholdStateDir() + "/agent-ops")
	if err != nil {
		return err
	}
	secret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return err
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return err
	}
	seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
	for _, slot := range []string{"relay", "cp"} {
		if err := cert.SaveCreds(filepath.Join(freeholdStateDir(), "dns-provider-"+slot+".json"), provider, env, seal, pub, "cert-dns-"+slot); err != nil {
			return fmt.Errorf("storing %s DNS credential: %w", slot, err)
		}
	}
	return nil
}
