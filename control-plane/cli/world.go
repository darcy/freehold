package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/control-plane/cli/flows"
	"freehold/control-plane/cli/login"
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
		return worldAction(args[0])
	},
}

func init() {
	rootCmd.AddCommand(worldCmd)
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
	mc, err := worldMcp(cfg)
	if err != nil {
		return err
	}
	tool := "world_" + action
	text, err := callAgentToolsText(mc, tool, map[string]interface{}{})
	if err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	// world_status returns a JSON inventory; the mutating world tools return a
	// plain report. Print the inventory readably, the report verbatim.
	if action == "status" {
		return printWorldStatus(text)
	}
	fmt.Println(text)
	return nil
}

func printWorldStatus(text string) error {
	var out struct {
		CPPubkey string `json:"cp_pubkey"`
		Agents   []struct {
			Name    string `json:"name"`
			Pubkey  string `json:"pubkey"`
			Purpose string `json:"purpose"`
		} `json:"agents"`
		Runners []struct {
			Name       string `json:"name"`
			NostrPubkey string `json:"nostr_pubkey"`
			McpAddr    string `json:"mcp_addr"`
		} `json:"runners"`
		DNS []struct {
			Name string `json:"name"`
			IP   string `json:"ip"`
		} `json:"dns"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		fmt.Println(text)
		return nil
	}
	fmt.Printf("CP pubkey: %s\n", out.CPPubkey)
	fmt.Printf("agents (%d):\n", len(out.Agents))
	for _, a := range out.Agents {
		fmt.Printf("  %-20s %s", a.Name, a.Pubkey)
		if a.Purpose != "" {
			fmt.Printf("  (%s)", a.Purpose)
		}
		fmt.Println()
	}
	fmt.Printf("runners (%d):\n", len(out.Runners))
	for _, r := range out.Runners {
		fmt.Printf("  %-20s %s\n", r.Name, r.NostrPubkey)
	}
	fmt.Printf("dns (%d):\n", len(out.DNS))
	for _, d := range out.DNS {
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
func worldMcp(cfg *config.Config) (*client.McpClient, error) {
	if cfg == nil || cfg.AgentToolsURL == "" || cfg.AgentToolsPubkey == "" {
		return nil, fmt.Errorf("no freehold-agent-tools coords recorded (run `freehold login` against the CP)")
	}
	auth, err := flows.AgentAuth(oplogin.Dir())
	if err != nil {
		return nil, fmt.Errorf("this box has no operator identity at %s (run `freehold login` to materialize it): %v", oplogin.Dir(), err)
	}
	return client.New(client.ConnectURL(cfg.AgentToolsURL), auth, cfg.AgentToolsPubkey)
}

// configPath resolves the config path for the world verb (the CLI's default).
func configPath() string {
	return config.DefaultPath()
}