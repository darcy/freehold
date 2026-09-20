// Package channel implements `freehold channel` — read or set the CP's recorded
// release channel (the channel half of the world's version pin). The version
// half is owned by install/update; `channel set` rewrites only the channel.
package channel

import (
	"bufio"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/version"
	"freehold/freehold-cli/internal/common"
	"freehold/freehold-cli/internal/stages"
	oplogin "freehold/freehold-cli/login"
	"freehold/platform/provisioning/box"
	"freehold/providers/proxmox"
)

var channelCmd = &cobra.Command{
	Use:   "channel",
	Short: "Show the world's release channel",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadProfile(cmd)
		if err != nil {
			return err
		}
		pin, err := readPin(cfg)
		if err != nil {
			return err
		}
		fmt.Printf("channel: %s\nversion: %s\n", orDash(pin.Channel), orDash(pin.Version))
		return nil
	},
}

var setCmd = &cobra.Command{
	Use:   "set <stable|rc|dev>",
	Short: "Set the world's release channel (recorded on the CP)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ch := args[0]
		switch ch {
		case "stable", "rc", "dev":
		default:
			return fmt.Errorf("unknown channel %q (stable|rc|dev)", ch)
		}
		cfg, err := loadProfile(cmd)
		if err != nil {
			return err
		}
		pin, err := readPin(cfg)
		if err != nil {
			return err
		}
		if pin.Version == "" {
			return fmt.Errorf("the CP has no version stamp yet — run `freehold install` or `freehold update` first")
		}
		// Only the running CLI is needed for the self-staged pin write; a
		// release-only box has no local sibling set.
		self, err := os.Executable()
		if err != nil {
			return err
		}
		eng, err := box.NewEngine(box.FlagsFromConfig(cfg), box.Bins{Self: self})
		if err != nil {
			return err
		}
		eng.Provider = proxmox.New(eng.HostExecFunc())
		eng.ProviderFactory = stages.TransientFactory(eng)
		eng.Out = os.Stdout
		eng.Stdin = bufio.NewReader(os.Stdin)
		// Preserve the version + commit: channel set changes ONLY the channel.
		if err := eng.StampVersionPin(version.Pin{Version: pin.Version, Channel: ch, Commit: pin.Commit}); err != nil {
			return err
		}
		fmt.Printf("✓ channel set to %s\n", ch)
		return nil
	},
}

func init() {
	channelCmd.AddCommand(setCmd)
	channelCmd.Flags().String("config", common.DefaultConfigPath(), "Tenant config path to drive the world against")
	setCmd.Flags().String("config", common.DefaultConfigPath(), "Tenant config path to drive the world against")
}

func loadProfile(cmd *cobra.Command) (*config.Config, error) {
	ok, err := common.NegotiateProfile(cmd, "channel")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
	}
	return config.Load(common.ConfigPath())
}

// readPin reads the CP's stamped version pin via the console login.
func readPin(cfg *config.Config) (version.Pin, error) {
	sec, err := oplogin.SecretHex()
	if err != nil {
		return version.Pin{}, fmt.Errorf("no operator identity (run `freehold login`): %v", err)
	}
	key, err := oplogin.NsecToSecret(sec)
	if err != nil {
		return version.Pin{}, err
	}
	c, err := oplogin.Login(cfg.CPURL, key)
	if err != nil {
		return version.Pin{}, fmt.Errorf("console login: %v", err)
	}
	w, err := c.World()
	if err != nil {
		return version.Pin{}, err
	}
	return w.Version, nil
}

func orDash(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Command returns the channel command for root registration.
func Command() *cobra.Command { return channelCmd }
