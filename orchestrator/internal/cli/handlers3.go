package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"freehold/orchestrator/internal/bootstrap"
	"freehold/orchestrator/internal/client"
	"freehold/orchestrator/internal/deploy"
	"freehold/orchestrator/internal/planebase"
	"freehold/orchestrator/internal/teardown"
	"github.com/spf13/cobra"
)

// --- deploy-relay ---

var deployRelayCmd = &cobra.Command{
	Use:   "deploy-relay",
	Short: "C2/B: deploy the Buzz relay onto the target through a provisioning runner",
	RunE: func(cmd *cobra.Command, args []string) error {
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		name, _ := cmd.Flags().GetString("name")
		deployDir, _ := cmd.Flags().GetString("deploy-dir")
		httpPort, _ := cmd.Flags().GetUint16("http-port")
		buzRef, _ := cmd.Flags().GetString("buzz-ref")
		lxcStr, _ := cmd.Flags().GetString("lxc")
		ownerPub, _ := cmd.Flags().GetString("owner-pubkey")
		relayURL, _ := cmd.Flags().GetString("relay-url")
		operatorPub, _ := cmd.Flags().GetString("operator-pubkey")
		domain, _ := cmd.Flags().GetString("domain")
		if ownerPub == "" || relayURL == "" || operatorPub == "" {
			return fmt.Errorf("deploy-relay needs --owner-pubkey --relay-url --operator-pubkey")
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		spec := &deploy.RelayDeploySpec{
			RelayName:      name,
			DeployDir:      deployDir,
			HTTPPort:       httpPort,
			BuzzRef:        buzRef,
			OwnerPubkey:    ownerPub,
			RelayURL:       relayURL,
			OperatorPubkey: operatorPub,
		}
		if domain != "" {
			spec.Domain = &domain
		}
		if lxcStr != "" {
			var v uint32
			fmt.Sscanf(lxcStr, "%d", &v)
			spec.LXc = &v
		}
		res, err := deploy.DeployRelay(c, target, spec)
		if err != nil {
			return err
		}
		fmt.Println(res.RelayURL)
		fmt.Println(res.Detail)
		return nil
	},
}

func init() {
	addCommonFlags(deployRelayCmd, nil)
	deployRelayCmd.Flags().String("target", "proxmox-box", "Target to deploy through (the runner holding the SSH credential to the PVE host / relay LXC)")
	deployRelayCmd.Flags().String("name", "", "Relay hostname (reported; defaults to <normalized-domain>-relay when --domain is given)")
	deployRelayCmd.Flags().String("deploy-dir", "/srv/data/relay", "Where the official compose bundle lands on the target")
	deployRelayCmd.Flags().Uint16("http-port", 3000, "Relay HTTP port (WRITTEN into the compose .env BUZZ_HTTP_PORT)")
	deployRelayCmd.Flags().String("buzz-ref", deploy.DefaultBufRef, "block/buzz ref to fetch (tag or SHA; pinned SHA by default)")
	deployRelayCmd.Flags().String("lxc", "", "Deploy INTO this LXC on the target (the target is the PVE host; the LXC is where docker lives after `freehold bootstrap proxmox-lxc`). Omitted = deploy directly on the target host")
	deployRelayCmd.Flags().String("owner-pubkey", "", "Relay OWNER Nostr pubkey (64-hex) — written to RELAY_OWNER_PUBKEY; the bundle's run.sh refuses to start with CHANGE_ME placeholders")
	deployRelayCmd.Flags().String("relay-url", "", "The relay's OWN resolvable URL (http://host:port) — written into BUZZ_DOMAIN/RELAY_URL/media so the relay binds the REAL community (the example.com placeholders are not literal CHANGE_ME)")
	deployRelayCmd.Flags().String("operator-pubkey", "", "The OPERATOR's Nostr pubkey (64-hex) — invite the human operator to the relay once it comes up (fail-closed: required)")
	deployRelayCmd.Flags().String("domain", "", "The forced identity DOMAIN (never an IP): writes BUZZ_DOMAIN/RELAY_URL = the domain (wss) AND provisions the TLS local-CA posture (own openssl CA + server cert; import ca.crt on your devices)")
}

// --- deploy-cp ---

var deployCpCmd = &cobra.Command{
	Use:   "deploy-cp",
	Short: "C1: deploy the control plane onto the target box (OPERATE mode)",
	RunE: func(cmd *cobra.Command, args []string) error {
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		stateDir, _ := cmd.Flags().GetString("state-dir")
		binDir, _ := cmd.Flags().GetString("bin-dir")
		bind, _ := cmd.Flags().GetString("bind")
		binary, _ := cmd.Flags().GetString("binary")
		relayURL, _ := cmd.Flags().GetString("relay-url")
		relayPubkey, _ := cmd.Flags().GetString("relay-pubkey")
		relayHostIP, _ := cmd.Flags().GetString("relay-host-ip")
		lxcStr, _ := cmd.Flags().GetString("lxc")
		runnerBinary, _ := cmd.Flags().GetString("runner-binary")
		runnerPackage, _ := cmd.Flags().GetString("runner-package")
		operatorPub, _ := cmd.Flags().GetString("operator-pubkey")
		publicOrigin, _ := cmd.Flags().GetString("public-origin")
		if binary == "" || relayURL == "" {
			return fmt.Errorf("deploy-cp needs --binary --relay-url")
		}
		authn := operatorPub != ""
		bindAddr := deploy.ResolveCpBind(optOf(bind), authn)
		var adminKeys []string
		if operatorPub != "" {
			adminKeys = []string{operatorPub}
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		spec := &deploy.DeployCpSpec{
			StateDir: stateDir, BinDir: binDir, BindAddr: bindAddr,
			BinaryPath: binary, RelayURL: relayURL, AdminPubkeys: adminKeys,
		}
		if relayPubkey != "" {
			spec.RelayPubkey = optOf(relayPubkey)
		}
		if relayHostIP != "" {
			spec.RelayHostIP = optOf(relayHostIP)
		}
		if publicOrigin != "" {
			spec.PublicOrigin = optOf(publicOrigin)
		}
		if lxcStr != "" {
			var v uint32
			fmt.Sscanf(lxcStr, "%d", &v)
			spec.LXc = &v
		}
		if runnerBinary != "" {
			spec.RunnerBinary = optOf(runnerBinary)
		}
		if runnerPackage != "" {
			spec.RunnerPackage = optOf(runnerPackage)
		}
		res, err := deploy.DeployCp(c, target, spec)
		if err != nil {
			return err
		}
		fmt.Println(res.Detail)
		fmt.Println("console pubkey:", res.Pubkey)
		return nil
	},
}

func optOf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func init() {
	addCommonFlags(deployCpCmd, nil)
	deployCpCmd.Flags().String("target", "proxmox-box", "Target runner (the box where the relay lives)")
	deployCpCmd.Flags().String("state-dir", deploy.DefaultCPStateDir(), "Remote state dir on the box (also holds the seeded console identity)")
	deployCpCmd.Flags().String("bin-dir", deploy.DefaultCPBinDir(), "Remote dir for the shipped binary")
	deployCpCmd.Flags().String("bind", "", "Loopback bind for the console (C3: non-loopback is refused). An EXPLICIT value is always honored. (Option so the default flip under --operator-pubkey can't swallow a deliberate --bind 127.0.0.1:8080.)")
	deployCpCmd.Flags().String("binary", "", "LOCAL path of the built control-plane binary")
	deployCpCmd.Flags().String("relay-url", "", "The relay this CP helps serve (the ONE scope; C4 posture record)")
	deployCpCmd.Flags().String("relay-pubkey", "", "The RELAY's signing pubkey (the 39002 roster trust anchor). When omitted, the deploy tries NIP-11 discovery (best-effort — Buzz often advertises none; pass it when known)")
	deployCpCmd.Flags().String("relay-host-ip", "", "The relay LXC's LAN IP — pinned into the CP guest's /etc/hosts so the console can RESOLVE the relay domain (the operator's DNS may not reach inside the guests: tailnet etc.)")
	deployCpCmd.Flags().String("lxc", "", "Deploy INTO this LXC on the target — the CP lives in its OWN guest, a different LXC than the relay's by default (omitted = the target host)")
	deployCpCmd.Flags().String("runner-binary", "", "LOCAL path of the built freehold-runner binary (co-locates the CP's own runner: ship + systemd unit + adopt + self-grant)")
	deployCpCmd.Flags().String("runner-package", "", "LOCAL dir of an EXISTING runner package to co-locate + adopt")
	deployCpCmd.Flags().String("operator-pubkey", "", "The OPERATOR's Nostr pubkey (64-hex) — seeds the console's NIP-98 admin whitelist (C3.5) and relaxes the loopback-only bind guard")
	deployCpCmd.Flags().String("public-origin", "", "The console's PUBLIC origin behind the operator's proxy")
}

// --- bootstrap ---

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "C2/A2: bootstrap-provision a target through a provisioning runner",
	RunE: func(cmd *cobra.Command, args []string) error {
		common := readCommonFlags(cmd)
		kind, _ := cmd.Flags().GetString("kind")
		target, _ := cmd.Flags().GetString("target")
		operatorPub, _ := cmd.Flags().GetString("operator-pubkey")
		domain, _ := cmd.Flags().GetString("domain")
		if kind == "" || operatorPub == "" || domain == "" {
			return fmt.Errorf("bootstrap needs --kind --operator-pubkey --domain")
		}
		_ = common
		_ = target
		return fmt.Errorf("bootstrap: the full proxmox/vultr/hetzner driver is not yet ported — use the Rust binary on main (bootstrap resolution/ensure live under `storage`)")
	},
}

func init() {
	addCommonFlags(bootstrapCmd, nil)
	bootstrapCmd.Flags().String("target", "proxmox-box", "Target to drive provisioning through (a runner targeting the PVE host for proxmox-lxc, the vultr runner for vultr-vps)")
	bootstrapCmd.Flags().String("role", "relay", "Role of this target: 'relay' or 'cp' — the LXC name is derived from the domain: <normalized-domain>-relay / -cp (--name is gone)")
	bootstrapCmd.Flags().Uint32("vmid", 0, "LXC vmid (proxmox-lxc; must be >= 100 when given; omitted = the driver picks the lowest free id via `pct list`)")
	bootstrapCmd.Flags().Uint32("rootfs-gb", 16, "LXC rootfs size in GB (proxmox-lxc)")
	bootstrapCmd.Flags().Uint32("memory-mb", 2048, "LXC memory in MB (proxmox-lxc)")
	bootstrapCmd.Flags().String("template", "", "LXC template name in storage 'local'; auto-detect when omitted")
	bootstrapCmd.Flags().String("storage", "local-lvm", "LXC storage (proxmox-lxc)")
	bootstrapCmd.Flags().String("bridge", "vmbr0", "LXC network bridge (proxmox-lxc)")
	bootstrapCmd.Flags().String("lxc-ip", "", "STATIC guest IP (CIDR) + gateway for Proxmox-on-Cloud-Compute hosts")
	bootstrapCmd.Flags().String("lxc-gw", "", "")
	bootstrapCmd.Flags().StringArray("mount", nil, "Durable-plane dataset mount baked into `pct create` (repeatable), shape `<dataset>:<guest-path>` — the \"born on the plane\" reference")
	bootstrapCmd.Flags().String("region", "atl", "Vultr region (vultr-vps)")
	bootstrapCmd.Flags().String("plan", "vc2-1c-1gb", "Vultr plan (vultr-vps)")
	bootstrapCmd.Flags().Uint32("os-id", 1743, "Vultr OS id (vultr-vps; Debian 12 = 1743)")
	bootstrapCmd.Flags().String("location", "fsn1", "Hetzner location (hetzner-vps)")
	bootstrapCmd.Flags().String("server-type", "cx22", "Hetzner server type (hetzner-vps)")
	bootstrapCmd.Flags().String("image", "ubuntu-22.04", "Hetzner OS image (hetzner-vps)")
	bootstrapCmd.Flags().Bool("destroy", false, "Destroy the VPS after verifying (vultr-vps; for tests/cleanup)")
	bootstrapCmd.Flags().String("operator-pubkey", "", "The OPERATOR's Nostr pubkey (64-hex) — relay invite (create-new) / attach auth (attach-existing) + console admin seed (fail-closed: required at bootstrap)")
	bootstrapCmd.Flags().String("domain", "", "The relay's identity DOMAIN (never an IP): the install BLOCKS (A4) until it resolves to the provisioned target's IP")
	bootstrapCmd.Flags().Uint64("domain-wait-secs", 300, "Seconds to wait for the domain to resolve to the target IP (A4)")
	bootstrapCmd.Flags().String("kind", "", "Target kind: proxmox-lxc | vultr-vps | hetzner-vps")
}

// --- storage ---

var storageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Phase 0.12: resolve/ensure/destroy the durable volume plane",
}

var storageResolveCmd = &cobra.Command{
	Use:   "resolve",
	Short: "Resolve the durable backend (ZFS → LVM-thin → bail for the Proxmox branch); with consent, create the backend",
	RunE: func(cmd *cobra.Command, args []string) error {
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		device, _ := cmd.Flags().GetString("device")
		confirm, _ := cmd.Flags().GetBool("confirm-storage")
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		action, err := bootstrap.ResolveProxmox(c, target, confirm, optOf(device))
		if err != nil {
			return err
		}
		switch action.Kind {
		case "Reuse":
			fmt.Printf("reuse existing backend (%s, pool %s)\n", action.Detected.String(), action.Pool)
		case "Create":
			fmt.Printf("create backend (%s, pool %s)\n", action.Backend.String(), action.Pool)
		case "Bail":
			return fmt.Errorf("%s", action.Message)
		}
		return nil
	},
}

var storageEnsureCmd = &cobra.Command{
	Use:   "ensure",
	Short: "Ensure a tenant's dataset/volume exists (idempotent) + is guest-writable",
	RunE: func(cmd *cobra.Command, args []string) error {
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		tenant, _ := cmd.Flags().GetString("tenant")
		domain, _ := cmd.Flags().GetString("domain")
		pool, _ := cmd.Flags().GetString("pool")
		if tenant == "" || domain == "" || pool == "" {
			return fmt.Errorf("storage ensure needs --tenant --domain --pool")
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		t, err := tenantFor(tenant)
		if err != nil {
			return err
		}
		dataset, err := planebase.DatasetPath(pool, domain, t)
		if err != nil {
			return err
		}
		if err := bootstrap.EnsureDataset(c, target, dataset); err != nil {
			return err
		}
		fmt.Printf("ensured dataset %s\n", dataset)
		return nil
	},
}

var storageDestroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Destroy a tenant's dataset subtree (data+compute teardown half)",
	RunE: func(cmd *cobra.Command, args []string) error {
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		tenant, _ := cmd.Flags().GetString("tenant")
		domain, _ := cmd.Flags().GetString("domain")
		pool, _ := cmd.Flags().GetString("pool")
		if tenant == "" || domain == "" || pool == "" {
			return fmt.Errorf("storage destroy needs --tenant --domain --pool")
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		t, err := tenantFor(tenant)
		if err != nil {
			return err
		}
		dataset, err := planebase.DatasetPath(pool, domain, t)
		if err != nil {
			return err
		}
		destroyed, err := destroyDatasetVia(c, target, pool, dataset)
		if err != nil {
			return err
		}
		fmt.Printf("STORAGE-DESTROYED: %v\n", destroyed)
		if !destroyed {
			fmt.Println("no dataset to destroy (absent) — nothing destroyed")
		}
		return nil
	},
}

var storageInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Report the live durable-plane snapshot: host capacity + per-mount size/used + guest bind-mount liveness (read-only, DATA-tab source)",
	RunE: func(cmd *cobra.Command, args []string) error {
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		pool, _ := cmd.Flags().GetString("pool")
		if pool == "" {
			return fmt.Errorf("storage info needs --pool")
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		out, err := bootstrap.ExecToOK(c, target, "zfs list -H -o name,used,avail "+pool+" 2>/dev/null || true", "info", 60)
		if err != nil {
			return err
		}
		fmt.Print(out.Stdout)
		return nil
	},
}

func tenantFor(tenant string) (planebase.Tenant, error) {
	switch tenant {
	case "relay":
		return planebase.TenantRelay, nil
	case "cp":
		return planebase.TenantCp, nil
	case "k3s-volumes", "k3s":
		return planebase.TenantK3sVolumes, nil
	}
	return 0, fmt.Errorf("unknown tenant %q (relay | cp | k3s-volumes)", tenant)
}

func destroyDatasetVia(c *client.McpClient, target, pool, dataset string) (bool, error) {
	// zfs destroy is destructive; report absent vs destroyed.
	// A nonzero `zfs list` exit means the dataset is ABSENT (safe no-op); a
	// transport/signing error (wedged runner) is NOT absent — surface it so the
	// operator isn't told "nothing destroyed" when the truth is "couldn't ask"
	// (DEFER-destroyDatasetVia).
	out, err := bootstrap.Exec(c, target, "zfs list -H -o name "+dataset+" >/dev/null 2>&1", 30)
	if err != nil {
		return false, err
	}
	if out.ExitCode == nil || *out.ExitCode != 0 {
		return false, nil // absent -> not destroyed
	}
	if _, err := bootstrap.ExecToOK(c, target, "zfs destroy -r "+dataset, "zfs destroy", 120); err != nil {
		return false, err
	}
	return true, nil
}

// --- teardown ---

var teardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Tear the managed world down: destroy the LXCs, remove the runner's key from the host LAST (after verification), then local cleanup",
	RunE: func(cmd *cobra.Command, args []string) error {
		configPath, _ := cmd.Flags().GetString("config")
		yes, _ := cmd.Flags().GetBool("yes")
		tenant, _ := cmd.Flags().GetString("tenant")
		data, _ := cmd.Flags().GetBool("data")
		if !yes {
			return fmt.Errorf("teardown aborted (not confirmed); pass --yes")
		}
		scope := teardown.ScopeFor(optOf(tenant), data)
		_ = configPath
		_ = scope
		return fmt.Errorf("teardown: the installer-config-driven driver is not yet ported — use the Rust binary on main")
	},
}

func init() {
	addCommonFlags(teardownCmd, nil)
	teardownCmd.Flags().String("config", defaultConfigPath(), "Config path (default: ~/.config/freehold/config.toml)")
	teardownCmd.Flags().Bool("yes", false, "Skip the confirmation prompt (scripting/CI only)")
	teardownCmd.Flags().String("tenant", "", "Per-tenant scoped teardown: only this tenant's LXC (and, with --data, its dataset) is destroyed. relay | cp | k3s-volumes. Omitted = whole-world teardown (compute + config + local home)")
	teardownCmd.Flags().Bool("data", false, "With --tenant: ALSO destroy the tenant's dataset (data+compute). Without --tenant: whole-world teardown also destroys all datasets")
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "freehold", "config.toml")
}

func init() {
	storageCmd.AddCommand(storageResolveCmd, storageEnsureCmd, storageInfoCmd, storageDestroyCmd)
	addCommonFlags(storageResolveCmd, nil)
	addCommonFlags(storageEnsureCmd, nil)
	addCommonFlags(storageInfoCmd, nil)
	addCommonFlags(storageDestroyCmd, nil)
	for _, sc := range []*cobra.Command{storageResolveCmd, storageEnsureCmd, storageInfoCmd, storageDestroyCmd} {
		sc.Flags().String("target", "proxmox-box", "Target to drive storage through (the runner holding the host ssh key)")
	}
	storageEnsureCmd.Flags().String("tenant", "", "Tenant: relay | cp | k3s-volumes")
	storageEnsureCmd.Flags().String("domain", "", "The relay's identity domain (for the dataset naming)")
	storageEnsureCmd.Flags().String("pool", "", "Storage pool (zpool name / VG name)")
	storageEnsureCmd.Flags().String("kind", "", "Backend kind recorded by the plane (`zfs` | `lvmth`). Honored when present; absent => detect")
	storageDestroyCmd.Flags().String("tenant", "", "Tenant: relay | cp | k3s-volumes")
	storageDestroyCmd.Flags().String("domain", "", "The relay's identity domain (for the dataset naming)")
	storageDestroyCmd.Flags().String("pool", "", "Storage pool (zpool name / VG name)")
	storageDestroyCmd.Flags().String("kind", "", "Backend kind recorded by the plane (`zfs` | `lvmth`). Honored when present; absent => detect")
	storageInfoCmd.Flags().String("pool", "", "Storage pool (zpool name / VG name)")
	storageInfoCmd.Flags().String("kind", "", "Backend kind recorded by the plane (`zfs` | `lvmth`). Honored when present; absent => detect")
	storageResolveCmd.Flags().String("device", "", "Physical device for a NEW zpool (e.g. /dev/sdb) — required only on the consent-gated create path, when no existing backend is detected")
	storageResolveCmd.Flags().Bool("confirm-storage", false, "Operator consent to CREATE a backend (zpool OR LVM-thin) when none is detected. Absent + no backend = actionable bail")
}

var _ = strconv.Itoa
var _ = strings.TrimSpace
