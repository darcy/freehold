package cli

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/control-plane/cli/flows"
)

// doorCmd is the box's DOOR_SPEC surface: authorize/revoke its OWN public door
// key on the host door. The box derives the door key deterministically from its
// agent-ops identity seed (its enc_secret — the private half never leaves the
// box; only the public line is presented), and the CP appends/removes it
// through its co-located runner (world_authorize_door / world_revoke_door).
var doorCmd = &cobra.Command{
	Use:   "door <authorize|revoke>",
	Short: "authorize/revoke this box's door key on the host (DOOR_SPEC)",
	Long: "door authorizes or revokes THIS box's public door key on the host door,\n" +
		"through the CP's co-located runner. The box derives the door key from its\n" +
		"agent-ops identity seed (the private half never leaves the box; only the\n" +
		"public line is presented). authorize = a fresh box can run CP-lifecycle\n" +
		"verbs (bootstrap-cp/teardown-cp); revoke removes it.",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("door needs a subcommand: authorize|revoke")
		}
		return doorAction(args[0])
	},
}

func init() {
	rootCmd.AddCommand(doorCmd)
}

// doorPubkey derives the box's deterministic SSH door public line from its
// agent-ops identity NOSTR secret seed — the SAME key that signs its world API
// calls (DOOR_SPEC §3), so the box's door is the box's signing identity. The
// private half never leaves the box; only the public line is presented.
func doorPubkey() (string, error) {
	id, err := flows.LoadIdentity(rbOpsDir())
	if err != nil {
		return "", fmt.Errorf("this box has no ops identity at %s (run `freehold login` to materialize it): %v", rbOpsDir(), err)
	}
	seed, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil || len(seed) != 32 {
		return "", fmt.Errorf("agent-ops nostr_secret is not a 32-byte seed")
	}
	host, _ := os.Hostname()
	return crypto.SSHPublicKeyFromSeed(seed, "freehold-door-"+host)
}

func doorAction(action string) error {
	if action != "authorize" && action != "revoke" {
		return fmt.Errorf("unknown door action %q (authorize|revoke)", action)
	}
	pubkey, err := doorPubkey()
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	mc, err := worldMcp(cfg)
	if err != nil {
		return err
	}
	tool := "world_revoke_door"
	verb := "revoked"
	if action == "authorize" {
		tool = "world_authorize_door"
		verb = "authorized"
	}
	if _, err := callAgentToolsText(mc, tool, map[string]interface{}{"pubkey": pubkey}); err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	fmt.Printf("door key %s on the host door (%s)\n", verb, pubkey)
	return nil
}
