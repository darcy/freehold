package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/control-plane/cli/flows"
	"freehold/control-plane/cli/teardown"
)

// uninstallCmd removes the control plane ITSELF — the counterpart to `teardown`
// (which keeps it). It removes the CP + world, this box's door and the runner
// substrate key, and wipes the local profile config/state. Data is KEPT by
// default (the durable plane survives, so a later install re-adopts the
// runner identity); --remove-data also drops the plane.
//
// PR2 drives this from a box with a LOCAL provisioning runner (the host side is
// reached through it). The thin-box / dead-CP path needs the transient Access
// seam and lands in PR3.
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
			ok, err := negotiateProfile(cmd, "uninstall")
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
			}
		}
		configPath := profileConfigPath(cmd)
		yes, _ := cmd.Flags().GetBool("yes")
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
			fmt.Printf("uninstall profile %q (host %s):\n  removes: control plane LXC %d + the world + this box's door + the runner key%s\n  keeps:   nothing local (config + state are wiped)\nproceed? [type yes] ",
				cfg.Name, host, *cfg.Lxc.Cp.Vmid, extra)
			var a string
			if _, err := fmt.Scanln(&a); err != nil || a != "yes" {
				return fmt.Errorf("uninstall aborted (not confirmed)")
			}
		}
		if err := runUninstall(cfg, configPath, host, removeData); err != nil {
			return err
		}
		// Everything remote is gone; now wipe THIS box's profile config + state.
		// The durable plane (and, on --remove-data, nothing) is all that remains.
		wipeLocalProfile(configPath, config.Current())
		return nil
	},
}

func init() {
	addCommonFlags(uninstallCmd, nil)
	uninstallCmd.Flags().String("config", defaultConfigPath(), "Config path (default: ~/.config/freehold/profile config)")
	uninstallCmd.Flags().String("name", "", "Profile name (resolves --host from profiles/<name>/config.toml when --host is omitted)")
	uninstallCmd.Flags().String("host", "", "The environment address freehold reached (defaults to the profile's recorded host)")
	uninstallCmd.Flags().Bool("remove-data", false, "ALSO remove the durable plane (datasets + the freehold-created thin pool). Without it the plane survives so a later install re-adopts the runner identity")
	uninstallCmd.Flags().Bool("yes", false, "Skip the confirmation prompt (scripting/CI only)")
}

// resolveUninstall checks the preconditions and resolves the host (--host wins,
// else the profile's recorded host). A thin box — no local provisioning runner
// — is refused until the transient Access seam lands (PR3), and a profile with
// no recorded CP vmid cannot dispatch the CP destroy.
func resolveUninstall(cfg *config.Config, hostFlag string) (string, error) {
	host := cfg.Host
	if hostFlag != "" {
		host = hostFlag
	}
	if host == "" {
		return "", fmt.Errorf("uninstall needs --host (the profile records no host) — re-run with `--host root@<box>`")
	}
	if cfg.Runner.Addr == "" || cfg.Runner.Pubkey == "" {
		return "", fmt.Errorf("uninstall needs a local provisioning runner — run it from the box that built the world (the thin-box path lands with the transient Access seam)")
	}
	if cfg.Lxc.Cp.Vmid == nil {
		return "", fmt.Errorf("uninstall needs the recorded CP LXC vmid (the profile has none — the CP may already be gone; re-install or run `teardown` from the build box)")
	}
	return host, nil
}

// runUninstall does the remote work through the LOCAL provisioning runner: it
// destroys the CP + world (relay/cp/k3s), and with removeData the datasets + the
// freehold-created thin pool. The runner is LOCAL — on the box, not inside the
// CP — so the CP LXC is destroyed synchronously in the same pass (the datasets
// then drop safely, the CP's mount gone). The box's operator door is revoked
// through the CP first (best-effort — the CP goes in this pass).
func runUninstall(cfg *config.Config, configPath, host string, removeData bool) error {
	fmt.Printf("uninstalling %q (host %s)…\n", cfg.Name, host)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	agentDir := filepath.Join(freeholdHome(), "control-plane", "agent-ops")
	runner := &teardown.ExecRunner{
		OrchestratorBin: self,
		Addr:            cfg.Runner.Addr,
		AgentDir:        agentDir,
		Runner:          cfg.Runner.Target,
		Domain:          cfg.TenantSlug(),
		ConfigPath:      configPath,
	}

	// The door must work before anything remote (same guard `teardown` uses).
	out, err := flows.Exec(cfg.Runner.Addr, agentDir, cfg.Runner.Pubkey, cfg.Runner.Target, "echo freehold-door-ok", []string{cfg.Runner.Target}, 30)
	if err != nil {
		return fmt.Errorf("uninstall won't touch the host: the door can't be verified — fix/start the runner first: %w", err)
	}
	if !strings.Contains(out.Stdout, "freehold-door-ok") {
		return fmt.Errorf("uninstall won't touch the host: the door probe did not answer (got %q)", strings.TrimSpace(out.Stdout))
	}

	// Revoke this box's operator door through the CP while it is still up.
	// Best-effort: the CP is removed in this pass and the door dies with it; a
	// failure here only means the door line lingers in authorized_keys.
	if err := doorAction("revoke"); err != nil {
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
	// The full world: relay + cp + k3s. With removeData teardown.Run also
	// destroys the tenant datasets + the freehold-created thin pool and removes
	// the runner's authorized_keys line (its Data half); without it we drop that
	// line explicitly afterwards (uninstall always removes the runner key).
	tcfg := &teardown.Cfg{
		Domain:        cfg.TenantSlug(),
		RunNTarget:    cfg.Runner.Target,
		RunnerComment: cfg.Runner.Pubkey,
		// The FULL world is removed here (not cfg.Managed, which can omit k3s
		// and litellm in a partial world) — uninstall drops the CP + every guest.
		Managed:     []string{"relay", "cp", "k3s"},
		WorldHome:   freeholdHome(),
		ConfigPath:  configPath,
		Pool:        pool,
		BackendKind: kind,
		Data:        removeData,
		Vmid: map[string]*uint32{
			"relay": cfg.Lxc.Relay.Vmid,
			"cp":    cfg.Lxc.Cp.Vmid,
			"k3s":   cfg.Lxc.K3s.Vmid,
		},
		ThinPool: thinPoolOf(cfg),
	}
	tcfg.Live = func(line string) { fmt.Println("  " + line) }
	if _, err := teardown.Run(runner, tcfg, teardown.ScopeWholeWorld, true); err != nil {
		return err
	}
	if !removeData {
		if err := removeRunnerDoor(runner, cfg.Runner.Pubkey); err != nil {
			return err
		}
	}
	return nil
}

// removeRunnerDoor deletes the runner's authorized_keys line on the host (the
// runner substrate key), verified. The comment is the runner's pubkey.
func removeRunnerDoor(runner *teardown.ExecRunner, comment string) error {
	// '|' as the sed delimiter: the comment can contain '/' (base64), which
	// would terminate a '/'-delimited pattern early.
	sed := fmt.Sprintf("sed -i '\\|ssh-ed25519 [A-Za-z0-9+/=]* %s$|d' /root/.ssh/authorized_keys", comment)
	if ok, out := runner.Exec(sed); !ok {
		return fmt.Errorf("runner key removal failed on the host: %s", out)
	}
	ok, out := runner.Exec(fmt.Sprintf("grep -c 'ssh-ed25519 .* %s' /root/.ssh/authorized_keys || true", comment))
	if !ok {
		return fmt.Errorf("runner key verification failed: %s", out)
	}
	if strings.TrimSpace(out) != "0" {
		return fmt.Errorf("the runner's key is STILL in authorized_keys (%s line(s))", strings.TrimSpace(out))
	}
	fmt.Println("runner substrate key removed from the host (verified)")
	return nil
}

// wipeLocalProfile removes the box's profile config dir + scoped state dir.
// Nothing local survives — the durable plane (if kept) is the only remnant.
func wipeLocalProfile(configPath string, p *config.Profile) {
	if dir := filepath.Dir(configPath); dir != "" && dir != "/" {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Printf("  (warning: could not remove %s: %v)\n", dir, err)
		} else {
			fmt.Printf("wiped profile config %s\n", dir)
		}
	}
	if p != nil && p.StateDir != "" && p.StateDir != "/" {
		if err := os.RemoveAll(p.StateDir); err != nil {
			fmt.Printf("  (warning: could not remove %s: %v)\n", p.StateDir, err)
		} else {
			fmt.Printf("wiped profile state %s\n", p.StateDir)
		}
	}
}
