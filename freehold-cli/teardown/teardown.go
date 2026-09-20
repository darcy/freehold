// Package teardown implements `freehold teardown` — the CP-preserving inverse
// of `build`: relay/k3s + the CP-side agent-tools process go, internal DNS
// clears, and the CP, its runner, the durable plane, and the cert mirror stay.
package teardown

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/freehold-cli/internal/common"
	worldteardown "freehold/providers/proxmox/teardown"
)

var teardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Tear the WORLD down (the inverse of `build`), CP-preserving: relay/k3s LXCs + the CP-side agent-tools process go, and the internal DNS records clear. The control plane, its co-located runner, the durable plane, the cert mirror, and Cloudflare records all STAY; your data is kept. `uninstall` drops the control plane itself; `uninstall --remove-data` drops the plane",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := common.NegotiateProfile(cmd, "teardown")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
		}
		configPath := common.ProfileConfigPath(cmd)
		yes, _ := cmd.Flags().GetBool("yes")
		tenant, _ := cmd.Flags().GetString("tenant")
		data, _ := cmd.Flags().GetBool("data")
		removeDNS, _ := cmd.Flags().GetBool("remove-dns")
		scope := worldteardown.ScopeFor(common.OptOf(tenant), data)

		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if cfg == nil {
			fmt.Println("nothing to tear down — no config")
			return nil
		}

		if tenant == "" {
			if data {
				return fmt.Errorf("teardown keeps your data (and the control plane):\n  `freehold uninstall --remove-data` also drops the durable plane")
			}
			return common.RunWholeWorldTeardown(cfg, configPath, removeDNS, yes)
		}

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
			return fmt.Errorf("teardown won't touch the host: the door can't be verified — fix/start the runner first: %w", err)
		}
		if !strings.Contains(out.Stdout, "freehold-door-ok") {
			return fmt.Errorf("teardown won't touch the host: the door probe did not answer (got %q)", strings.TrimSpace(out.Stdout))
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
			Managed:       cfg.Managed,
			WorldHome:     common.FreeholdHome(),
			ConfigPath:    configPath,
			Pool:          pool,
			BackendKind:   kind,
			TenantRole:    worldteardown.TenantLxcRole(tenant),
			Data:          data,
			Vmid: map[string]*uint32{
				"relay": cfg.Lxc.Relay.Vmid,
				"cp":    cfg.Lxc.Cp.Vmid,
				"k3s":   cfg.Lxc.K3s.Vmid,
			},
			ThinPool: common.ThinPoolOf(cfg),
		}

		if !yes {
			fmt.Printf("teardown scope: %s (config %s)\n", scope, configPath)
			fmt.Println("keeps: config · world home · door key · plane locations · DNS creds")
			if err := common.ConfirmDestructive("teardown"); err != nil {
				return err
			}
		}
		if removeDNS {
			if err := common.RemoveManagedDNS(cfg); err != nil {
				return err
			}
		}
		tcfg.Live = func(line string) { fmt.Println("  " + line) }
		report, err := worldteardown.Run(runner, tcfg, scope, true)
		if err != nil {
			return err
		}
		if idx := strings.IndexByte(report, '\n'); idx >= 0 {
			fmt.Println(report[:idx])
		} else {
			fmt.Println(report)
		}
		return nil
	},
}

func init() {
	common.AddCommonFlags(teardownCmd, nil)
	teardownCmd.Flags().String("config", common.DefaultConfigPath(), "Config path (default: ~/.config/freehold/config.toml)")
	teardownCmd.Flags().Bool("yes", false, "Skip the confirmation prompt (scripting/CI only)")
	teardownCmd.Flags().String("tenant", "", "Per-tenant scoped teardown: only this tenant's LXC (and, with --data, its dataset) is destroyed. relay | cp | k3s-volumes. Omitted = whole-world teardown")
	teardownCmd.Flags().Bool("data", false, "With --tenant: ALSO destroy that tenant's dataset (data+compute). Without --tenant: REFUSED — teardown keeps your data; `freehold uninstall --remove-data` drops the durable plane")
	teardownCmd.Flags().Bool("remove-dns", false, "ALSO delete the freehold-managed RELAY A record on the DNS provider recorded in config (Dns.Manager, created by `build --manage-dns`). The CP's record is kept (the CP survives teardown). Default leaves them")
}

// Command returns the teardown command for root registration.
func Command() *cobra.Command { return teardownCmd }
