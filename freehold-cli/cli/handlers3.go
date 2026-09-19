package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/freehold-cli/cli/flows"
	"freehold/freehold-cli/cli/login"
	"freehold/platform/services/certificates/letsencrypt"
	"freehold/platform/services/externaldns/cloudflare"
	"freehold/providers/proxmox/teardown"
	"github.com/spf13/cobra"
)

// thinPoolOf returns the freehold-CREATED thin pool recorded in the config
// ("" when the plane reused a stock pool — nothing recorded).
func thinPoolOf(cfg *config.Config) string {
	if cfg.Plane.ThinPool != nil {
		return *cfg.Plane.ThinPool
	}
	return ""
}

// --- teardown ---

var teardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Tear the WORLD down (the inverse of `build`), CP-preserving: relay/k3s LXCs + the CP-side agent-tools process go, and the internal DNS records clear. The control plane, its co-located runner, the durable plane, the cert mirror, and Cloudflare records all STAY; your data is kept. `uninstall` drops the control plane itself; `uninstall --remove-data` drops the plane",
	RunE: func(cmd *cobra.Command, args []string) error {
		ok, err := negotiateProfile(cmd, "teardown")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no tenant profiles — run `freehold login` to add the world's profile first")
		}
		configPath := profileConfigPath(cmd)
		yes, _ := cmd.Flags().GetBool("yes")
		tenant, _ := cmd.Flags().GetString("tenant")
		data, _ := cmd.Flags().GetBool("data")
		removeDNS, _ := cmd.Flags().GetBool("remove-dns")
		scope := teardown.ScopeFor(optOf(tenant), data)

		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if cfg == nil {
			fmt.Println("nothing to tear down — no config")
			return nil
		}

		// Whole-world teardown is CP-PRESERVING and CP-DRIVEN (the inverse of
		// build): the CP's co-located runner removes relay/k3s and stops the
		// CP-side agent-tools process, and the console clears the world's
		// internal DNS. The CP, its runner, the durable plane, and the cert
		// mirror all stay. Data removal is `uninstall --remove-data`'s job.
		if tenant == "" {
			if data {
				return fmt.Errorf("teardown keeps your data (and the control plane):\n  `freehold uninstall --remove-data` also drops the durable plane")
			}
			return runWholeWorldTeardown(cfg, configPath, removeDNS, yes)
		}

		// Per-tenant teardown still needs the box that built the world (a local
		// provisioning runner + the door); the CP-driven path above has no
		// tenant scope.

		// The teardown engine shells `freehold exec` — resolve the
		// CLI binary as OURSELF (we are it).
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

		// The door must work before anything remote: a signed exec probe.
		// The ssh target requires its own secret in `secrets` (same rule the
		// exec CLI applies when no --secret refs are given).
		out, err := flows.Exec(cfg.Runner.Addr, agentDir, cfg.Runner.Pubkey, cfg.Runner.Target, "echo freehold-door-ok", []string{cfg.Runner.Target}, 30)
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
		tcfg := &teardown.Cfg{
			Domain:        cfg.TenantSlug(),
			RunNTarget:    cfg.Runner.Target,
			RunnerKeyRefs: runnerKeyRefs(cfg, runner),
			Managed:       cfg.Managed,
			WorldHome:     freeholdHome(),
			ConfigPath:    configPath,
			Pool:          pool,
			BackendKind:   kind,
			TenantRole:    teardown.TenantLxcRole(tenant),
			Data:          data,
			Vmid: map[string]*uint32{
				"relay": cfg.Lxc.Relay.Vmid,
				"cp":    cfg.Lxc.Cp.Vmid,
				"k3s":   cfg.Lxc.K3s.Vmid,
			},
			ThinPool: thinPoolOf(cfg),
			// No prune: the recorded LXC coordinates (vmid + ip) are
			// operator-owned facts — rebuild reuses them for a deterministic
			// re-boot of the SAME world.
		}

		// Confirmation gate: --yes skips the prompt (scripting/CI).
		if !yes {
			fmt.Printf("teardown scope: %s (config %s)\n", scope, configPath)
			fmt.Println("keeps: config · world home · door key · plane locations · DNS creds")
			if err := confirmDestructive("teardown"); err != nil {
				return err
			}
		}
		// --remove-dns: delete the world's freehold-managed A records; default
		// leaves them. Runs BEFORE the physical teardown so the sealed relay
		// credential (and the in-memory cfg hosts) are still readable even on a
		// --data wipe that removes the world home/config.
		if removeDNS {
			if err := removeManagedDNS(cfg); err != nil {
				return err
			}
		}
		// Stream every line as it lands (--yes runs have no operator to
		// page through; the TUI subprocess stream shows the same bytes).
		tcfg.Live = func(line string) { fmt.Println("  " + line) }
		report, err := teardown.Run(runner, tcfg, scope, true)
		if err != nil {
			return err
		}
		// The report re-prints every line already streamed via Live, so on
		// the streaming path emit only the completion summary — the
		// duplicated body was pure noise on the CLI and in the TUI stream.
		if idx := strings.IndexByte(report, '\n'); idx >= 0 {
			fmt.Println(report[:idx])
		} else {
			fmt.Println(report)
		}
		// Per-tenant teardown keeps the recorded coords + dataset mapping for
		// reattach; the whole-world path cleared them in runWholeWorldTeardown.
		return nil
	},
}

func init() {
	addCommonFlags(teardownCmd, nil)
	teardownCmd.Flags().String("config", defaultConfigPath(), "Config path (default: ~/.config/freehold/config.toml)")
	teardownCmd.Flags().Bool("yes", false, "Skip the confirmation prompt (scripting/CI only)")
	teardownCmd.Flags().String("tenant", "", "Per-tenant scoped teardown: only this tenant's LXC (and, with --data, its dataset) is destroyed. relay | cp | k3s-volumes. Omitted = whole-world teardown")
	teardownCmd.Flags().Bool("data", false, "With --tenant: ALSO destroy that tenant's dataset (data+compute). Without --tenant: REFUSED — teardown keeps your data; `freehold uninstall --remove-data` drops the durable plane")
	teardownCmd.Flags().Bool("remove-dns", false, "ALSO delete the freehold-managed RELAY A record on the DNS provider recorded in config (Dns.Manager, created by `build --manage-dns`). The CP's record is kept (the CP survives teardown). Default leaves them")
}

// removeManagedDNS deletes the world's freehold-managed RELAY A record on the
// provider recorded in config.Dns.Manager (teardown --remove-dns). The CP's
// record is deliberately KEPT: teardown is CP-preserving, so deleting it would
// break the surviving control plane's public DNS. Resolves the sealed
// relay-slot credential (the same one --manage-dns used for the records + the
// LE cert) to drive the manager.
func removeManagedDNS(cfg *config.Config) error {
	m := cfg.Dns.Manager
	if m == nil || !m.Managed {
		fmt.Println("  (no freehold-managed DNS recorded — nothing to remove)")
		return nil
	}
	secretHex, err := encSecretFromDir(filepath.Join(freeholdHome(), "control-plane", "agent-ops"))
	if err != nil {
		return fmt.Errorf("no ops identity for DNS removal: %w", err)
	}
	secret, err := hexBytes(secretHex)
	if err != nil {
		return fmt.Errorf("ops identity enc secret: %w", err)
	}
	open := func(secret, aad, blob []byte) ([]byte, error) { return crypto.Open(secret, aad, blob) }
	provider, env, err := cert.LoadCreds(filepath.Join(freeholdHome(), "control-plane", "dns-provider-relay.json"), open, secret)
	if err != nil {
		return fmt.Errorf("load relay DNS credential for removal: %w", err)
	}
	if provider != "cloudflare" {
		return fmt.Errorf("recorded DNS manager %q cannot remove records yet", provider)
	}
	man, err := dnsman.For(provider, env)
	if err != nil {
		return err
	}
	// Only the relay record goes: the CP survives teardown, so its A record
	// must stay for the control plane to remain reachable.
	if h := cfg.RelayHost(); h != "" {
		if err := man.DeleteA(h); err != nil {
			return fmt.Errorf("remove DNS record %s: %w", h, err)
		}
	}
	return nil
}

func defaultConfigPath() string {
	return config.ConfigPath()
}

// freeholdHome mirrors installer::freehold_home — the active profile's scoped
// state dir, or the legacy ~/.freehold (FREEHOLD_HOME override) when no profile
// is negotiated.
func freeholdHome() string {
	return config.StateDir()
}

// runWholeWorldTeardown is the CP-preserving whole-world teardown: it asks the
// CP (console /api/world-teardown) to remove the WORLD — relay/k3s, the
// CP-side freehold-agent-tools process, and the internal DNS records — through
// its co-located runner. The CP, its runner, the durable plane, and the cert
// mirror all stay, so the next build (or a re-install after a wipe) re-uses
// them. Works from any box (a thin one has no local runner).
func runWholeWorldTeardown(cfg *config.Config, configPath string, removeDNS, yes bool) error {
	if !yes {
		fmt.Println("teardown scope: whole-world (CP-preserving — the CP, its runner, and the plane stay)")
		fmt.Println("keeps: control plane · co-located runner · durable plane · cert mirror · Cloudflare records")
		if err := confirmDestructive("teardown"); err != nil {
			return err
		}
	}
	// Optional Cloudflare A-record removal; default keeps them (build upserts).
	if removeDNS {
		if err := removeManagedDNS(cfg); err != nil {
			fmt.Printf("  (warning: DNS removal skipped — %v)\n", err)
		}
	}
	secretHex, err := oplogin.SecretHex()
	if err != nil {
		return fmt.Errorf("no operator session on this box (%v) — run `freehold login` first", err)
	}
	secret, err := oplogin.NsecToSecret(secretHex)
	if err != nil {
		return err
	}
	c, err := oplogin.Login(cfg.CPURL, secret)
	if err != nil {
		return fmt.Errorf("login to %s failed: %w", cfg.CPURL, err)
	}
	fmt.Printf("tearing down world %s through the CP (the CP + runner are preserved)…\n", cfg.TenantSlug())
	res, err := c.WorldTeardown()
	if err != nil {
		return fmt.Errorf("world-teardown (console /api/world-teardown): %w", err)
	}
	if res.Report != "" {
		fmt.Println(res.Report)
	}
	fmt.Printf("  world teardown: %d internal DNS record(s) cleared; the CP + runner are preserved\n", res.DnsRemoved)
	// Forget the WORLD's recorded coords so the next build re-creates relay/k3s.
	// The CP's coords are KEPT — the CP LXC survives and its recorded IP is what
	// the console/agent-tools URLs resolve against.
	cfg.Lxc.Relay.Vmid, cfg.Lxc.Relay.Ip = nil, nil
	cfg.Lxc.K3s.Vmid, cfg.Lxc.K3s.Ip = nil, nil
	if err := cfg.Save(configPath); err != nil {
		return err
	}
	fmt.Println("cleared recorded relay/k3s coordinates (vmid + ip) — the next build re-creates them")
	return nil
}

var _ = strconv.Itoa
var _ = strings.TrimSpace
