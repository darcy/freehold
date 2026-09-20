package cli

import (
	"fmt"
	"net"
	"time"

	"github.com/spf13/cobra"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/identity"
	"freehold/freehold-cli/cli/login"
)

// worldCmd is the box's post-login world surface: every world op goes through
// the CP's unified api/ (the roster-gated freehold-agent-tools server), signed
// as the box's ops identity — the box holds no runner/door of its own for these.
// status = the single inventory read; build/teardown/migrate = the CP driving
// the world through its co-located runner.
var worldCmd = &cobra.Command{
	Use:   "world <status|build|teardown|migrate>",
	Short: "drive the CP's world actions through the unified api (roster-gated)",
	Long: "world drives the CP's world actions over the agent-tools MCP server on the\n" +
		"control plane (signed as this box's ops identity — the same call shape the\n" +
		"build dogfoods). status is the single inventory read (agents + runners +\n" +
		"DNS); build/teardown/migrate run the CP-owned world stages through its\n" +
		"co-located runner.",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("world needs a subcommand: status|build|teardown|migrate")
		}
		ok, err := negotiateProfile(cmd, "world "+args[0])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
		}
		return worldAction(args[0])
	},
}

func init() {
	rootCmd.AddCommand(worldCmd)
	worldCmd.Flags().String("config", config.ConfigPath(), "Tenant config path to drive the world against")
}

// worldAction issues one world_* tool call to the CP's agent-tools server,
// signed as the box's ops identity (login materializes it). No local runner or
// door is needed — world ops are API calls (the CP drives the world through its
// co-located runner).
func worldAction(action string) error {
	switch action {
	case "status", "build", "teardown", "migrate":
	default:
		return fmt.Errorf("unknown world action %q (status|build|teardown|migrate)", action)
	}
	cfg, err := config.Load(configPath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// status is a READ — served by the console's public /api/world (the same
	// single-inventory source the TUI uses), reachable by any logged-in box via
	// the public CP URL. build/teardown/migrate are roster-gated world actions
	// through the toolset.
	if action == "status" {
		return worldStatusFromConsole(cfg)
	}
	if action == "teardown" {
		// Pure alias of `freehold teardown`, INCLUDING its confirmation gate:
		// the whole-world teardown is destructive, so it prompts (there is no
		// silent path here) — scripts use `freehold teardown --yes`.
		return runWholeWorldTeardown(cfg, configPath(), false, false)
	}
	mc, err := worldMcp(cfg)
	if err != nil {
		return err
	}
	tool := "world_" + action
	text, err := callAgentToolsText(mc, tool, map[string]interface{}{})
	if err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	fmt.Println(text)
	return nil
}

// worldStatusFromConsole renders the CP's single inventory off the public
// /api/world console route (session authed as the operator), instead of the
// LAN-bound agent-tools MCP server — so a remote box's `world status` works.
func worldStatusFromConsole(cfg *config.Config) error {
	sec, err := oplogin.SecretHex()
	if err != nil {
		return fmt.Errorf("no operator identity (run `freehold login`): %v", err)
	}
	key, err := oplogin.NsecToSecret(sec)
	if err != nil {
		return fmt.Errorf("operator secret not valid: %v", err)
	}
	c, err := oplogin.Login(cfg.CPURL, key)
	if err != nil {
		return fmt.Errorf("console login: %v", err)
	}
	w, err := c.World()
	if err != nil {
		return fmt.Errorf("world status: %v", err)
	}
	return printWorldSummary(w)
}

func printWorldSummary(w *console.WorldSummary) error {
	fmt.Printf("CP pubkey: %s\n", w.CPPubkey)
	fmt.Printf("agents (%d):\n", len(w.Agents))
	for _, a := range w.Agents {
		fmt.Printf("  %-20s %s", a.Name, a.Pubkey)
		if a.Purpose != "" {
			fmt.Printf("  (%s)", a.Purpose)
		}
		fmt.Println()
	}
	runners := w.RunnersList()
	fmt.Printf("runners (%d):\n", len(runners))
	for _, r := range runners {
		fmt.Printf("  %-20s %s\n", r.Name, r.NostrPubkey)
	}
	dns := w.DNSRecords()
	fmt.Printf("dns (%d):\n", len(dns))
	for _, d := range dns {
		fmt.Printf("  %s -> %s\n", d.Name, d.IP)
	}
	return nil
}

// worldMcp builds the agent-tools MCP client for the world verb. It signs as
// the OPERATOR identity (the persisted nsec — a console-admin, in the toolset
// roster), NOT the box's agent-ops identity: a fresh login-only box's
// agent-ops is minted locally and is NOT a toolset-roster member, so signing
// with it would get world_* denied with -32001. The operator key is the
// credential (0.4.9) — the same identity the TUI's Agents view uses.
//
// It refreshes the recorded agent-tools coords from the console FIRST: a
// `--data` rebuild mints a fresh agent-tools identity, so the recorded audience
// goes stale and every signed call fails "signature does not verify" until a
// re-login. This keeps door/world signing current without one.
func worldMcp(cfg *config.Config) (*client.McpClient, error) {
	refreshAgentToolsCoords(cfg)
	if cfg == nil || cfg.AgentToolsURL == "" || cfg.AgentToolsPubkey == "" {
		return nil, fmt.Errorf("no freehold-agent-tools coords recorded (run `freehold login` against the CP)")
	}
	auth, err := identity.AgentAuth(oplogin.Dir())
	if err != nil {
		return nil, fmt.Errorf("this box has no operator identity at %s (run `freehold login` to materialize it): %v", oplogin.Dir(), err)
	}
	return client.New(client.ConnectURL(cfg.AgentToolsURL), auth, cfg.AgentToolsPubkey)
}

// refreshAgentToolsCoords re-reads the CP's live agent-tools URL + pubkey from
// the console's public /api/world and adopts them into cfg, so a `--data`
// rebuild (which mints a fresh agent-tools identity, wiping the durable
// /srv/data copy) does not leave the box signing with a stale audience. Best
// effort: an unreachable console or an absent operator session leaves the
// recorded values in place (the pre-existing behavior).
func refreshAgentToolsCoords(cfg *config.Config) {
	if cfg == nil || cfg.CPURL == "" {
		return
	}
	sec, err := oplogin.SecretHex()
	if err != nil {
		return
	}
	key, err := oplogin.NsecToSecret(sec)
	if err != nil {
		return
	}
	c, err := oplogin.Login(cfg.CPURL, key)
	if err != nil {
		return
	}
	w, err := c.World()
	if err != nil {
		return
	}
	if adoptAgentToolsCoords(cfg, w) {
		_ = cfg.Save(configPath())
	}
}

// adoptAgentToolsCoords copies the CP-reported agent-tools URL + pubkey into
// cfg and reports whether anything changed (a non-empty pubkey is required —
// a CP that predates /api/world's agent_tools fields reports neither).
func adoptAgentToolsCoords(cfg *config.Config, w *console.WorldSummary) bool {
	if cfg == nil || w == nil || w.AgentToolsPubkey == "" {
		return false
	}
	changed := false
	if w.AgentToolsPubkey != cfg.AgentToolsPubkey {
		cfg.AgentToolsPubkey = w.AgentToolsPubkey
		changed = true
	}
	if w.AgentToolsURL != "" && w.AgentToolsURL != cfg.AgentToolsURL {
		cfg.AgentToolsURL = w.AgentToolsURL
		changed = true
	}
	return changed
}

// noLocalRunner reports whether this box is THIN: such a box drives the world —
// including exec — through the CP. A recorded runner addr is not enough: a
// transient install records `[runner] addr` but leaves no runner serving, so the
// addr must actually answer (a short dial) or the box is treated as thin.
func noLocalRunner() bool {
	cfg, err := config.Load(configPath())
	if err != nil || cfg == nil || cfg.Runner.Addr == "" {
		return true
	}
	return !addrReachable(cfg.Runner.Addr)
}

// addrReachable reports whether a TCP address accepts a connection within a
// short timeout.
func addrReachable(addr string) bool {
	c, derr := net.DialTimeout("tcp", addr, 700*time.Millisecond)
	if derr != nil {
		return false
	}
	_ = c.Close()
	return true
}

// worldExecThroughCP runs cmd on the CP's co-located runner via the world_exec
// tool (the drive-through-CP exec surface), so a thin login box has the build
// box's exec capability without hosting a runner. target is passed through so
// the CP validates the box asked for its own runner (never a silent mismatch).
// Signed as the operator.
func worldExecThroughCP(target, cmd string, secrets []string, timeoutS uint64) error {
	cfg, err := config.Load(configPath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	mc, err := worldMcp(cfg)
	if err != nil {
		return err
	}
	args := map[string]interface{}{"target": target, "cmd": cmd}
	if timeoutS > 0 {
		args["timeout_s"] = timeoutS
	}
	if len(secrets) > 0 {
		args["secrets"] = secrets
	}
	text, err := callAgentToolsText(mc, "world_exec", args)
	if err != nil {
		return fmt.Errorf("world_exec: %w", err)
	}
	if text != "" {
		fmt.Print(text)
	}
	return nil
}

// configPath resolves the config path for the world verb: the negotiated
// profile's config when one is active, else the CLI's default.
func configPath() string {
	return config.ConfigPath()
}
