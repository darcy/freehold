// Package uninstall implements `freehold uninstall` — removing the control
// plane ITSELF (the counterpart to `teardown`, which keeps it).
package uninstall

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
	"freehold/platform/provisioning"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/box"
	"freehold/providers/proxmox"
	worldteardown "freehold/providers/proxmox/teardown"
	"freehold/providers/registry"
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
		destroyHost, _ := cmd.Flags().GetBool("destroy-host")
		hostFlag, _ := cmd.Flags().GetString("host")

		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if cfg == nil {
			return fmt.Errorf("no config at %s", configPath)
		}
		host, verr := resolveUninstall(cfg, hostFlag, destroyHost)
		if verr != nil {
			return verr
		}
		// --destroy-host on a created-host world: the host dies with
		// everything on it, so the world teardown is best-effort. The
		// gates BELOW assume a fully-built world (a recorded CP vmid, a
		// RUNNING serve) — a half-installed world has neither, and
		// stopping the bill needs the API, never host access. So: the
		// teardown rides ONLY when the serve actually answers; otherwise
		// the destroy runs straight.
		if destroyHost && cfg.HostProvider.ID != "" {
			serveAlive := cfg.Runner.Addr != "" && cfg.Runner.Pubkey != "" && serveAlive(cfg)
			if serveAlive {
				if err := runUninstall(cfg, configPath, host, removeData, false); err != nil {
					fmt.Printf("  (the world teardown failed — destroying the host anyway: %v)\n", err)
				}
			} else {
				fmt.Println("  no world to tear down (a half-installed host, or the runner is not serving) — destroying the instance directly; the API needs no host access")
			}
			running, derr := destroyHostViaProvider(cfg, yes, true)
			if derr != nil {
				return derr
			}
			if !running {
				wipeLocalProfile(configPath, config.Current())
			} else {
				fmt.Println("  profile kept: it holds the created host instance handle (the instance is still running)")
			}
			return nil
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
			keeps := "nothing local (config + state are wiped)"
			if cfg.HostProvider.ID != "" && !destroyHost {
				keeps = "the " + cfg.HostProvider.Provider + " instance (STILL RUNNING AND BILLING) + its profile handle"
			}
			fmt.Printf("uninstall profile %q (host %s):\n  removes: control plane LXC %s + the world + this box's door + the runner key%s\n  keeps:   %s\n",
				cfg.Name, displayHost(host, cfg.Runner.Target), cpLxc, extra, keeps)
			if err := common.ConfirmDestructive("uninstall"); err != nil {
				return err
			}
		}
		// A default uninstall on a created-host world KEEPS the profile as
		// the instance's handle — so the box's door + substrate keys stay
		// authorized: the kept profile's whole value is that a re-adopt or
		// a --destroy-host re-entry still works.
		keepHostAccess := cfg.HostProvider.ID != ""
		if cfg.Runner.Addr == "" || cfg.Runner.Pubkey == "" {
			if err := runUninstallTransient(cfg, host, removeData, keepHostAccess); err != nil {
				return err
			}
			instanceRunning, derr := destroyHostViaProvider(cfg, yes, destroyHost)
			if derr != nil {
				return derr
			}
			if instanceRunning {
				// The profile is the ONLY handle to a still-billing instance —
				// wiping it makes the printed recovery command a dead end.
				fmt.Println("  profile kept: it holds the created host instance handle (the instance is still running)")
			} else {
				wipeLocalProfile(configPath, config.Current())
			}
			return nil
		}
		if err := runUninstall(cfg, configPath, host, removeData, keepHostAccess); err != nil {
			return err
		}
		instanceRunning, derr := destroyHostViaProvider(cfg, yes, destroyHost)
		if derr != nil {
			return derr
		}
		if instanceRunning {
			fmt.Println("  profile kept: it holds the created host instance handle (the instance is still running)")
		} else {
			wipeLocalProfile(configPath, config.Current())
		}
		return nil
	},
}

// destroyHostViaProvider destroys the world's CREATED host when the
// operator opted in (--destroy-host). DEFAULT OFF on purpose: a created
// host bills by the hour, so a default uninstall leaves it RUNNING and says
// so loudly — the operator decides when the bill stops, exactly like the
// durable plane survives a default uninstall. Returns whether an instance
// is still running (the caller keeps the profile — it is the only handle).
// The provider's secret needs ride the env (never persisted).
func destroyHostViaProvider(cfg *config.Config, yes, destroy bool) (bool, error) {
	if cfg.HostProvider.Provider == "" || cfg.HostProvider.ID == "" {
		return false, nil
	}
	prov, err := registry.ByName(cfg.HostProvider.Provider)
	if err != nil {
		return false, err
	}
	if !destroy {
		fmt.Printf("  NOTE: the %s instance %s is still RUNNING and billing — destroy it with:\n    %s freehold uninstall --destroy-host\n",
			prov.Name(), cfg.HostProvider.ID, secretEnvHint(prov))
		return true, nil
	}
	if !yes {
		if err := common.ConfirmDestructive("destroy the " + prov.Name() + " instance (its data dies with it)"); err != nil {
			fmt.Println("  instance left running — destroy it later with --destroy-host")
			return true, nil
		}
	}
	answers := map[string]string{}
	for k, v := range cfg.HostProvider.Answers {
		answers[k] = v
	}
	for _, need := range prov.Needs() {
		if !need.Secret {
			continue
		}
		if v := strings.TrimSpace(os.Getenv(need.Name)); v != "" {
			answers[need.Name] = v
		}
	}
	session := &provisioning.HostSession{Answers: answers}
	fmt.Printf("  destroying the %s instance %s…\n", prov.Name(), cfg.HostProvider.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := prov.Destroy(ctx, session, cfg.HostProvider.ID); err != nil {
		return true, err
	}
	fmt.Println("  ✓ instance destroyed")
	return false, nil
}

// secretEnvHint names the env vars the provider's secrets ride.
func secretEnvHint(prov provisioning.HostProvider) string {
	var names []string
	for _, n := range prov.Needs() {
		if n.Secret {
			names = append(names, n.Name+"=...")
		}
	}
	return strings.Join(names, " ")
}

func init() {
	common.AddCommonFlags(uninstallCmd, nil)
	uninstallCmd.Flags().String("config", common.DefaultConfigPath(), "Config path (default: ~/.config/freehold/profile config)")
	uninstallCmd.Flags().String("name", "", "Profile name (resolves --host from profiles/<name>/config.toml when --host is omitted)")
	uninstallCmd.Flags().String("host", "", "The environment address freehold reached (defaults to the profile's recorded host; informational in this path — the remote work rides the local runner)")
	uninstallCmd.Flags().Bool("remove-data", false, "ALSO remove the durable plane (datasets + the freehold-created thin pool). Without it the plane survives so a later install re-adopts the runner identity")
	uninstallCmd.Flags().Bool("destroy-host", false, "ALSO destroy a CREATED host (e.g. a vultr world's instance; its secret needs ride the env). Without it the instance keeps running and billing")
	uninstallCmd.Flags().Bool("non-interactive", false, "Skip the confirmation prompt (scripting/CI only)")
}

// resolveUninstall checks the preconditions and resolves the host for DISPLAY.
// The recorded CP vmid matters for the teardown path (destroying guests by
// id); --destroy-host does not need it — the instance's destruction removes
// the guests wholesale.
func resolveUninstall(cfg *config.Config, hostFlag string, destroyHost bool) (string, error) {
	host := cfg.Host
	if hostFlag != "" {
		host = hostFlag
	}
	if cfg.Runner.Addr != "" && cfg.Runner.Pubkey != "" {
		if cfg.Lxc.Cp.Vmid == nil && !destroyHost {
			return "", fmt.Errorf("uninstall needs the recorded CP LXC vmid (the profile has none — the CP may already be gone; re-install or run `teardown` from the build box)")
		}
		return host, nil
	}
	if host == "" {
		return "", fmt.Errorf("uninstall needs --host (no local runner and no recorded host to reach the host directly)")
	}
	return host, nil
}

// serveAlive probes the runner's serve quietly — the served-runner path the
// world teardown drives. A half-installed world has no serve; the teardown
// cannot run and the destroy goes straight.
func serveAlive(cfg *config.Config) bool {
	agentDir := filepath.Join(common.FreeholdHome(), "control-plane", "agent-ops")
	out, err := common.ExecDirect(cfg.Runner.Addr, agentDir, cfg.Runner.Pubkey, cfg.Runner.Target, "echo freehold-door-ok", []string{cfg.Runner.Target}, 30)
	return err == nil && out != nil && strings.Contains(out.Stdout, "freehold-door-ok")
}

// doorAlive quietly probes the box's access to the host (the same probe
// runUninstall gates on) — a dead door means a prior uninstall kept the
// profile as the instance's handle.
func doorAlive(cfg *config.Config, host string) bool {
	agentDir := filepath.Join(common.FreeholdHome(), "control-plane", "agent-ops")
	out, err := common.ExecDirect(cfg.Runner.Addr, agentDir, cfg.Runner.Pubkey, cfg.Runner.Target, "echo freehold-door-ok", []string{cfg.Runner.Target}, 30)
	if err == nil && strings.Contains(out.Stdout, "freehold-door-ok") {
		return true
	}
	// The served-runner path may be dead where the transient door is not.
	keyPath, cleanup, derr := proxmox.WriteTempKey(mustDoorPEM())
	if derr != nil {
		return false
	}
	defer cleanup()
	tout, terr := proxmox.SSHExec(strings.TrimPrefix(host, "root@"), keyPath)("echo freehold-door-ok", 30)
	return terr == nil && tout.ExitCode != nil && *tout.ExitCode == 0 && strings.Contains(tout.Stdout, "freehold-door-ok")
}

func mustDoorPEM() []byte {
	pem, _ := box.DoorKeyPEM()
	return pem
}

func runUninstall(cfg *config.Config, configPath, host string, removeData, keepHostAccess bool) error {
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

	if keepHostAccess {
		// The profile is the created host's handle — the box's keys stay
		// authorized so the handle is worth something (a re-adopt, or a
		// --destroy-host re-entry without host access).
	} else if err := common.DoorAction(nil, "revoke"); err != nil {
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
	managed := []string{"relay", "cp", "k3s"}
	vmid := map[string]*uint32{
		"relay": cfg.Lxc.Relay.Vmid,
		"cp":    cfg.Lxc.Cp.Vmid,
		"k3s":   cfg.Lxc.K3s.Vmid,
	}
	if gw := gatewayVmid(cfg, func() (string, error) {
		out, err := common.ExecDirect(cfg.Runner.Addr, agentDir, cfg.Runner.Pubkey, cfg.Runner.Target, "pct list", []string{cfg.Runner.Target}, 30)
		if err != nil {
			return "", err
		}
		return out.Stdout, nil
	}); gw != nil {
		// The gateway is destroyed LAST (after the guests it routes for) —
		// the role iteration is ordered, so it rides at the end.
		managed = append(managed, "gateway")
		vmid["gateway"] = gw
	}
	tcfg := &worldteardown.Cfg{
		Domain:        cfg.TenantSlug(),
		RunNTarget:    cfg.Runner.Target,
		RunnerKeyRefs: common.RunnerKeyRefs(cfg, runner),
		Managed:       managed,
		WorldHome:     common.FreeholdHome(),
		ConfigPath:    configPath,
		Pool:          pool,
		BackendKind:   kind,
		Data:          removeData,
		Vmid:          vmid,
		ThinPool:      common.ThinPoolOf(cfg),
	}
	tcfg.Live = func(line string) { fmt.Println("  " + line) }
	if _, err := worldteardown.Run(runner, tcfg, worldteardown.ScopeWholeWorld, true); err != nil {
		return err
	}
	if !removeData && !keepHostAccess {
		if err := removeRunnerKeys(runner, cfg); err != nil {
			return err
		}
	}
	common.WarnOtherDoors(runner)
	return nil
}

// gatewayVmid returns the gateway's vmid for teardown: the recorded one, else
// adopt-by-name — a profile whose record predates the gateway (an old
// world-config, or one rendered on another box/home) still destroys it by
// matching the bootstrap-named <name>-gateway in pct list. Nil (skip the
// gateway) when there is no record, the host has no such guest (flat-LAN
// world), or the probe fails — never a hard failure; the probe rides the door
// already verified by the caller.
func gatewayVmid(cfg *config.Config, probe func() (string, error)) *uint32 {
	if cfg.Lxc.Gateway.Vmid != nil {
		return cfg.Lxc.Gateway.Vmid
	}
	name, err := bootstrap.LXCName(cfg.Name, cfg.RelayHost(), "gateway")
	if err != nil {
		return nil
	}
	out, err := probe()
	if err != nil {
		fmt.Printf("  (warning: could not probe for an unrecorded gateway LXC — it may be left behind: %v)\n", err)
		return nil
	}
	for _, l := range strings.Split(out, "\n")[1:] {
		cols := strings.Fields(l)
		if len(cols) >= 2 && cols[len(cols)-1] == name {
			if v, perr := strconv.ParseUint(cols[0], 10, 32); perr == nil {
				vmid := uint32(v)
				return &vmid
			}
		}
	}
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
func runUninstallTransient(cfg *config.Config, host string, removeData, keepHostAccess bool) error {
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
	for _, role := range []string{"relay", "k3s", "cp", "gateway"} {
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
	if !keepHostAccess {
		if err := worldteardown.RemoveRunnerSubstrate(adapter, common.RunnerKeyRefs(cfg, adapter), cfg.Runner.Target); err != nil {
			return err
		}
		if door, derr := common.DoorPubkey(); derr == nil {
			if err := worldteardown.RemoveAuthorizedKey(adapter, door); err != nil {
				return err
			}
		}
	}
	common.WarnOtherDoors(adapter)
	return nil
}

// Command returns the uninstall command for root registration.
func Command() *cobra.Command { return uninstallCmd }
