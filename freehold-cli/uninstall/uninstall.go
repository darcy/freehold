// Package uninstall implements `freehold uninstall` — removing the control
// plane ITSELF (the counterpart to `teardown`, which keeps it).
package uninstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/box"
	"freehold/providers/proxmox"
	worldteardown "freehold/providers/proxmox/teardown"
)

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the control plane + world + THIS box's doors; data kept by default. --remove-data also drops the durable plane",
	RunE: func(cmd *cobra.Command, args []string) error {
		if name, _ := cmd.Flags().GetString("name"); name != "" {
			p := config.Resolve(name)
			if p == nil {
				return fmt.Errorf("no profile %q — `freehold profiles` lists them", name)
			}
			config.SetCurrent(p)
		} else {
			ok, err := common.NegotiateProfile(cmd, "uninstall")
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
			}
		}
		configPath := common.ProfileConfigPath(cmd)
		yes, _ := cmd.Flags().GetBool("non-interactive")
		removeData, _ := cmd.Flags().GetBool("remove-data")
		hostFlag, _ := cmd.Flags().GetString("host")

		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if cfg == nil {
			return fmt.Errorf("no config at %s", configPath)
		}
		host, verr := resolveUninstall(cfg, hostFlag)
		if verr != nil {
			return verr
		}
		if !yes {
			extra := ""
			if removeData {
				extra = " + the durable plane (--remove-data)"
			}
			cpLxc := "?"
			if cfg.Lxc.Cp.Vmid != nil {
				cpLxc = fmt.Sprintf("%d", *cfg.Lxc.Cp.Vmid)
			}
			fmt.Printf("uninstall profile %q (host %s):\n  removes: control plane LXC %s + the world + this box's door + the runner key%s\n  keeps:   nothing local (config + state are wiped)\n",
				cfg.Name, displayHost(host, cfg.Runner.Target), cpLxc, extra)
			if err := common.ConfirmDestructive("uninstall"); err != nil {
				return err
			}
		}
		if cfg.Runner.Addr == "" || cfg.Runner.Pubkey == "" {
			if err := runUninstallTransient(cfg, host, removeData); err != nil {
				return err
			}
			wipeLocalProfile(configPath, config.Current())
			return nil
		}
		if err := runUninstall(cfg, configPath, host, removeData); err != nil {
			return err
		}
		wipeLocalProfile(configPath, config.Current())
		return nil
	},
}

func init() {
	common.AddCommonFlags(uninstallCmd, nil)
	uninstallCmd.Flags().String("config", common.DefaultConfigPath(), "Config path (default: ~/.config/freehold/profile config)")
	uninstallCmd.Flags().String("name", "", "Profile name (resolves --host from profiles/<name>/config.toml when --host is omitted)")
	uninstallCmd.Flags().String("host", "", "The environment address freehold reached (defaults to the profile's recorded host; informational in this path — the remote work rides the local runner)")
	uninstallCmd.Flags().Bool("remove-data", false, "ALSO remove the durable plane (datasets + the freehold-created thin pool). Without it the plane survives so a later install re-adopts the runner identity")
	uninstallCmd.Flags().Bool("non-interactive", false, "Skip the confirmation prompt (scripting/CI only)")
}

// resolveUninstall checks the preconditions and resolves the host for DISPLAY.
func resolveUninstall(cfg *config.Config, hostFlag string) (string, error) {
	host := cfg.Host
	if hostFlag != "" {
		host = hostFlag
	}
	if cfg.Runner.Addr != "" && cfg.Runner.Pubkey != "" {
		if cfg.Lxc.Cp.Vmid == nil {
			return "", fmt.Errorf("uninstall needs the recorded CP LXC vmid (the profile has none — the CP may already be gone; re-install or run `teardown` from the build box)")
		}
		return host, nil
	}
	if host == "" {
		return "", fmt.Errorf("uninstall needs --host (no local runner and no recorded host to reach the host directly)")
	}
	return host, nil
}

func runUninstall(cfg *config.Config, configPath, host string, removeData bool) error {
	fmt.Printf("uninstalling %q (host %s)…\n", cfg.Name, displayHost(host, cfg.Runner.Target))
	self, err := os.Executable()
	if err != nil {
		return err
	}
	agentDir := filepath.Join(common.FreeholdHome(), "control-plane", "agent-ops")
	runner := &worldteardown.ExecRunner{
		OrchestratorBin: self,
		Addr:            cfg.Runner.Addr,
		AgentDir:        agentDir,
		Runner:          cfg.Runner.Target,
		Domain:          cfg.TenantSlug(),
		ConfigPath:      configPath,
	}

	out, err := common.ExecDirect(cfg.Runner.Addr, agentDir, cfg.Runner.Pubkey, cfg.Runner.Target, "echo freehold-door-ok", []string{cfg.Runner.Target}, 30)
	if err != nil {
		return fmt.Errorf("uninstall won't touch the host: the door can't be verified — fix/start the runner first: %w", err)
	}
	if !strings.Contains(out.Stdout, "freehold-door-ok") {
		return fmt.Errorf("uninstall won't touch the host: the door probe did not answer (got %q)", strings.TrimSpace(out.Stdout))
	}

	if err := common.DoorAction("revoke"); err != nil {
		fmt.Printf("  (warning: door revoke skipped — %v)\n", err)
	}

	pool := "rpool"
	if cfg.Plane.Backend != nil && *cfg.Plane.Backend != "" {
		pool = *cfg.Plane.Backend
	}
	kind := ""
	if cfg.Plane.BackendKind != nil {
		kind = *cfg.Plane.BackendKind
	}
	tcfg := &worldteardown.Cfg{
		Domain:        cfg.TenantSlug(),
		RunNTarget:    cfg.Runner.Target,
		RunnerKeyRefs: common.RunnerKeyRefs(cfg, runner),
		Managed:       []string{"relay", "cp", "k3s"},
		WorldHome:     common.FreeholdHome(),
		ConfigPath:    configPath,
		Pool:          pool,
		BackendKind:   kind,
		Data:          removeData,
		Vmid: map[string]*uint32{
			"relay": cfg.Lxc.Relay.Vmid,
			"cp":    cfg.Lxc.Cp.Vmid,
			"k3s":   cfg.Lxc.K3s.Vmid,
		},
		ThinPool: common.ThinPoolOf(cfg),
	}
	tcfg.Live = func(line string) { fmt.Println("  " + line) }
	if _, err := worldteardown.Run(runner, tcfg, worldteardown.ScopeWholeWorld, true); err != nil {
		return err
	}
	if !removeData {
		if err := removeRunnerKeys(runner, cfg); err != nil {
			return err
		}
	}
	common.WarnOtherDoors(runner)
	return nil
}

func removeRunnerKeys(runner *worldteardown.ExecRunner, cfg *config.Config) error {
	return worldteardown.RemoveRunnerSubstrate(runner, common.RunnerKeyRefs(cfg, runner), cfg.Runner.Target)
}

func displayHost(host, runnerTarget string) string {
	if host != "" {
		return host
	}
	return runnerTarget
}

func wipeLocalProfile(configPath string, p *config.Profile) {
	// Only remove a PROFILE-SCOPED config dir, and only when configPath IS that
	// profile's config — never the parent of an arbitrary --config path (which
	// could be the shared freehold home or a custom directory holding others).
	if p != nil && p.ConfigPath != "" && filepath.Clean(configPath) == filepath.Clean(p.ConfigPath) {
		if dir := filepath.Dir(p.ConfigPath); dir != "" && dir != "/" {
			if err := os.RemoveAll(dir); err != nil {
				fmt.Printf("  (warning: could not remove %s: %v)\n", dir, err)
			} else {
				fmt.Printf("wiped profile config %s\n", dir)
			}
		}
	} else if configPath != "" {
		_ = os.Remove(configPath)
	}
	if p != nil && p.StateDir != "" && p.StateDir != "/" {
		if err := os.RemoveAll(p.StateDir); err != nil {
			fmt.Printf("  (warning: could not remove %s: %v)\n", p.StateDir, err)
		} else {
			fmt.Printf("wiped profile state %s\n", p.StateDir)
		}
	}
}

// runUninstallTransient removes the CP + world from a thin box by direct root
// SSH. --remove-data is refused.
func runUninstallTransient(cfg *config.Config, host string, removeData bool) error {
	if removeData {
		return fmt.Errorf("--remove-data needs the build box (a thin box cannot reach the durable plane's storage); re-run without it, or uninstall from the build box")
	}
	fmt.Printf("uninstalling %q (host %s, transient access)…\n", cfg.Name, host)
	pem, err := box.DoorKeyPEM()
	if err != nil {
		return fmt.Errorf("no DOOR_SPEC key to reach the host (run `freehold login` first): %w", err)
	}
	keyPath, cleanup, err := proxmox.WriteTempKey(pem)
	if err != nil {
		return err
	}
	defer cleanup()
	prov := proxmox.New(proxmox.SSHExec(strings.TrimPrefix(host, "root@"), keyPath))

	guests, err := prov.ListGuests()
	if err != nil {
		return fmt.Errorf("uninstall won't touch the host: the door can't be verified (is this box's DOOR_SPEC key authorized?): %w", err)
	}
	byName := map[string]string{}
	for _, g := range guests {
		byName[g.Name] = g.ID
	}
	for _, role := range []string{"relay", "k3s", "cp"} {
		name, nerr := bootstrap.LXCName(cfg.Name, cfg.TenantSlug(), role)
		if nerr != nil {
			return nerr
		}
		id, ok := byName[name]
		if !ok {
			fmt.Printf("  · %s (%s) absent — skipping\n", role, name)
			continue
		}
		if err := prov.DestroyGuest(id); err != nil {
			return err
		}
		fmt.Printf("  ✓ destroyed %s (%s)\n", role, name)
	}
	adapter := common.GuestExecAdapter{ExecFn: func(cmd string, timeoutS uint64) (bool, string) {
		out, err := prov.GuestExec("", cmd, timeoutS)
		if err != nil || out == nil {
			return false, ""
		}
		ok := out.ExitCode == nil || *out.ExitCode == 0
		return ok, out.Stdout
	}}
	if err := worldteardown.RemoveRunnerSubstrate(adapter, common.RunnerKeyRefs(cfg, adapter), cfg.Runner.Target); err != nil {
		return err
	}
	if door, derr := common.DoorPubkey(); derr == nil {
		if err := worldteardown.RemoveAuthorizedKey(adapter, door); err != nil {
			return err
		}
	}
	common.WarnOtherDoors(adapter)
	return nil
}

// Command returns the uninstall command for root registration.
func Command() *cobra.Command { return uninstallCmd }
