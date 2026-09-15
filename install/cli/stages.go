// The bootstrap engine's self-staged commands: freehold-install re-invokes
// ITSELF for the low-level stage commands (exec / provision / storage /
// deploy-cp) exactly as the shared engine selfStages them. Each talks to the
// host/siblings through a provisioning runner.
package cli

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/install/cpdeploy"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/box"
	"freehold/platform/provisioning/drive"
	"freehold/platform/provisioning/planebase"
)

func mustStr(cmd *cobra.Command, name string) string { v, _ := cmd.Flags().GetString(name); return v }
func mustU32(cmd *cobra.Command, name string) uint32 { v, _ := cmd.Flags().GetUint32(name); return v }
func mustU64(cmd *cobra.Command, name string) uint64 { v, _ := cmd.Flags().GetUint64(name); return v }
func mustBool(cmd *cobra.Command, name string) bool  { v, _ := cmd.Flags().GetBool(name); return v }
func mustArr(cmd *cobra.Command, name string) []string {
	v, _ := cmd.Flags().GetStringArray(name)
	return v
}

// agentAuth builds the signed runner client auth from an identity dir (the
// ops/agent identity the install stages sign as).
func agentAuth(dir string) (*client.AgentAuth, error) {
	if err := box.EnsureIdentity(dir); err != nil {
		return nil, err
	}
	id, err := box.LoadIdentity(dir)
	if err != nil {
		return nil, fmt.Errorf("bad agent identity dir %s: %v", dir, err)
	}
	secret, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil {
		return nil, fmt.Errorf("bad agent identity dir %s: %v", dir, err)
	}
	pub, err := crypto.PubkeyFromSecret(secret)
	if err != nil {
		return nil, err
	}
	auth := &client.AgentAuth{}
	copy(auth.Secret[:], secret)
	auth.Pubkey = pub
	return auth, nil
}

// installConnect builds an McpClient to the provisioning runner at addr, using
// the identity in agentDir, authenticating to the runner's pubkey read from
// the local runner package.
func installConnect(addr, agentDir, target string) (*client.McpClient, error) {
	auth, err := agentAuth(agentDir)
	if err != nil {
		return nil, err
	}
	runnerPK, err := box.LoadPubkey(filepath.Join(config.StateDir(), "runner", target))
	if err != nil {
		return nil, fmt.Errorf("runner %q not found at %s (provision it first): %v",
			target, filepath.Join(config.StateDir(), "runner", target), err)
	}
	return client.New(client.ConnectURL(addr), auth, runnerPK)
}

var execCmd = &cobra.Command{
	Use:   "exec <TARGET> <CMD>",
	Short: "Signed exec against a running runner (self-staged)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 {
			return fmt.Errorf("exec needs <TARGET> <CMD>")
		}
		addr, _ := cmd.Flags().GetString("addr")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		timeoutS, _ := cmd.Flags().GetUint64("timeout")
		c, err := installConnect(addr, agentDir, args[0])
		if err != nil {
			return err
		}
		refs := []string{args[0]}
		if s, _ := cmd.Flags().GetStringSlice("secret"); len(s) > 0 {
			refs = s
		}
		out, err := c.Exec(args[0], args[1], refs, timeoutS)
		if err != nil {
			return err
		}
		if out.Stdout != "" {
			fmt.Print(out.Stdout)
		}
		if out.Stderr != "" {
			fmt.Fprint(os.Stderr, out.Stderr)
		}
		if out.TimedOut {
			return fmt.Errorf("exec timed out")
		}
		return nil
	},
}

func registerSelfFlags(cmd *cobra.Command) {
	cmd.Flags().String("addr", "127.0.0.1:8787", "Runner MCP address (loopback)")
	cmd.Flags().String("agent-dir", box.OpsDir(), "Agent identity dir (signing)")
	cmd.Flags().String("target", "proxmox-box", "Target runner")
}

// --- provision (self-staged: stageBootstrap) -------------------------------------------------

var provisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "Bootstrap a target (proxmox-lxc) through a provisioning runner",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		target := mustStr(cmd, "target")
		kind := mustStr(cmd, "kind")
		c, err := installConnect(addr, agentDir, target)
		if err != nil {
			return err
		}
		switch kind {
		case "proxmox-lxc":
			return provisionProxmoxLxc(c, cmd, target)
		default:
			return fmt.Errorf("unknown kind %q (proxmox-lxc supported)", kind)
		}
	},
}

func provisionProxmoxLxc(c *client.McpClient, cmd *cobra.Command, target string) error {
	role := mustStr(cmd, "role")
	hostname := mustStr(cmd, "hostname")
	if hostname == "" {
		domain := mustStr(cmd, "domain")
		hostname = strings.ReplaceAll(domain, ".", "-") + "-" + role
	}
	var vmid *uint32
	if v := mustStr(cmd, "vmid"); v != "" {
		var n uint32
		fmt.Sscanf(v, "%d", &n)
		vmid = &n
	}
	var ip, gw *string
	if v := mustStr(cmd, "lxc-ip"); v != "" {
		ip = &v
	}
	if v := mustStr(cmd, "lxc-gw"); v != "" {
		gw = &v
	}
	mounts := []planebase.MountSpec{}
	for _, m := range mustArr(cmd, "mount") {
		ms, err := bootstrap.ParseMount(m)
		if err != nil {
			return err
		}
		mounts = append(mounts, ms)
	}
	spec := &bootstrap.ProxmoxLxcSpec{
		Hostname: hostname, VMID: vmid,
		Storage: mustStr(cmd, "storage"), RootfsGB: mustU32(cmd, "rootfs-gb"),
		MemoryMB: mustU32(cmd, "memory-mb"), Bridge: mustStr(cmd, "bridge"),
		NetIP: ip, NetGW: gw, Mounts: mounts,
	}
	res, err := bootstrap.BootstrapProxmoxLxc(c, target, spec)
	if err != nil {
		return err
	}
	fmt.Println(res.Detail)
	return nil
}

// --- storage (self-staged: resolve/ensure) -------------------------------------------------

var storageCmd = &cobra.Command{Use: "storage", Short: "durable volume plane"}

var storageResolveCmd = &cobra.Command{
	Use:   "resolve",
	Short: "Resolve the durable backend (or create it with consent)",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		target := mustStr(cmd, "target")
		confirm := mustBool(cmd, "confirm-storage")
		c, err := installConnect(addr, agentDir, target)
		if err != nil {
			return err
		}
		action, err := bootstrap.ResolveProxmox(c, target, confirm, optOf(mustStr(cmd, "device")))
		if err != nil {
			return err
		}
		switch action.Kind {
		case "Reuse":
			label := "ZFS zpool"
			if *action.Detected == planebase.ExistingLvmThin {
				label = "LVM VG/thin-pool"
			}
			fmt.Printf("STORAGE: reusing existing backend (%s %s) — nothing created\n", label, action.Pool)
			fmt.Printf("STORAGE-POOL: %s\n", action.Pool)
			if *action.Detected == planebase.ExistingLvmThin {
				pools, err := bootstrap.ThinPools(c, target, action.Pool)
				if err != nil {
					return err
				}
				thin := "-"
				if len(pools) > 0 {
					thin = strings.Join(pools, ",")
				}
				fmt.Printf("STORAGE-THINPOOL: %s\n", thin)
			}
		case "Create":
			fmt.Printf("STORAGE: creating new backend (pool %s) with consent…\n", action.Pool)
			fmt.Printf("STORAGE-POOL: %s\n", action.Pool)
			if *action.Backend == planebase.BackendZfs {
				if err := bootstrap.EnsureZpool(c, target, action.Pool, optOf(mustStr(cmd, "device"))); err != nil {
					return err
				}
			} else {
				fmt.Println("STORAGE: LVM-thin — per-tenant ensure will create the pool+LV")
			}
		case "Bail":
			return fmt.Errorf("%s", action.Message)
		}
		return nil
	},
}

var storageEnsureCmd = &cobra.Command{
	Use:   "ensure",
	Short: "Ensure a tenant's dataset/volume exists (idempotent) + guest-writable",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		target := mustStr(cmd, "target")
		tenant := mustStr(cmd, "tenant")
		domain := mustStr(cmd, "domain")
		pool := mustStr(cmd, "pool")
		kindStr := mustStr(cmd, "kind")
		thinPool := mustStr(cmd, "thin-pool")
		if tenant == "" || domain == "" || pool == "" {
			return fmt.Errorf("storage ensure needs --tenant --domain --pool")
		}
		c, err := installConnect(addr, agentDir, target)
		if err != nil {
			return err
		}
		t, err := tenantFor(tenant)
		if err != nil {
			return err
		}
		kind, action, err := parseKind(c, target, kindStr)
		if err != nil {
			return err
		}
		if action != nil {
			return fmt.Errorf("no storage backend to ensure onto: %s — run `storage resolve` first", action.Message)
		}
		var mounts []planebase.MountSpec
		switch kind {
		case planebase.KindZfs:
			mounts, err = drive.ResolveTenantMounts(c, target, pool, domain, t)
		case planebase.KindLvmThin:
			mounts, err = drive.ResolveLvmMounts(c, target, pool, domain, t,
				mustU64(cmd, "size-gb"), mustU64(cmd, "pool-size-gb"), thinPool)
		}
		if err != nil {
			return err
		}
		kindOut := "zfs"
		if kind == planebase.KindLvmThin {
			kindOut = "lvmth"
		}
		fmt.Printf("STORAGE-BACKEND: %s %s\n", kindOut, pool)
		for _, m := range mounts {
			fmt.Printf("STORAGE-MOUNT %s:%s\n", m.Source, m.GuestPath)
		}
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
	return 0, fmt.Errorf("unknown tenant %q (relay|cp|k3s-volumes)", tenant)
}

func parseKind(c *client.McpClient, target, kindFlag string) (planebase.BackendKind, *bootstrap.ResolveAction, error) {
	switch kindFlag {
	case "":
	case "zfs":
		return planebase.KindZfs, nil, nil
	case "lvmth":
		return planebase.KindLvmThin, nil, nil
	default:
		return "", nil, fmt.Errorf("unknown storage backend kind: %s (expected zfs|lvmth)", kindFlag)
	}
	action, err := bootstrap.ResolveProxmox(c, target, false, nil)
	if err != nil {
		return "", nil, err
	}
	if action.Kind == "Reuse" {
		if *action.Detected == planebase.ExistingZfs {
			return planebase.KindZfs, nil, nil
		}
		return planebase.KindLvmThin, nil, nil
	}
	return "", action, nil
}

// --- deploy-cp (self-staged: stageDeployCp) -------------------------------------------------

var deployCpCmd = &cobra.Command{
	Use:   "deploy-cp",
	Short: "Deploy the control plane into the cp LXC (OPERATE mode)",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		target := mustStr(cmd, "target")
		c, err := installConnect(addr, agentDir, target)
		if err != nil {
			return err
		}
		binary := mustStr(cmd, "binary")
		if binary == "" || mustStr(cmd, "relay-url") == "" {
			return fmt.Errorf("deploy-cp needs --binary --relay-url")
		}
		stateDir, binDir := mustStr(cmd, "state-dir"), mustStr(cmd, "bin-dir")
		if stateDir == "" {
			stateDir = cpdeploy.DefaultCPStateDir()
		}
		if binDir == "" {
			binDir = cpdeploy.DefaultCPBinDir()
		}
		operatorPub := mustStr(cmd, "operator-pubkey")
		bind := cpdeploy.ResolveCpBind(optOf(mustStr(cmd, "bind")), operatorPub != "")
		spec := &cpdeploy.DeployCpSpec{
			StateDir: stateDir, BinDir: binDir, BindAddr: bind,
			BinaryPath: binary, RelayURL: mustStr(cmd, "relay-url"),
		}
		if operatorPub != "" {
			spec.AdminPubkeys = []string{operatorPub}
		}
		opt := func(f string) *string {
			if v := mustStr(cmd, f); v != "" {
				return &v
			}
			return nil
		}
		spec.RelayPubkey = opt("relay-pubkey")
		spec.RelayHostIP = opt("relay-host-ip")
		spec.AgentToolsURL = opt("agent-tools-url")
		spec.AgentToolsPubkey = opt("agent-tools-pubkey")
		spec.PublicOrigin = opt("public-origin")
		spec.WorldConfig = opt("world-config")
		spec.AgentToolsBinary = opt("agent-tools-binary")
		spec.RunnerBinary = opt("runner-binary")
		spec.RunnerPackage = opt("runner-package")
		if v := mustStr(cmd, "lxc"); v != "" {
			var n uint32
			fmt.Sscanf(v, "%d", &n)
			spec.LXc = &n
		}
		res, err := cpdeploy.DeployCp(c, target, spec)
		if err != nil {
			return err
		}
		fmt.Println(res.Detail)
		fmt.Println("console pubkey:", res.Pubkey)
		return nil
	},
}

// optOf returns nil for an empty string (optional flag).
func optOf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func init() {
	registerSelfFlags(execCmd)
	execCmd.Flags().StringSliceP("secret", "s", nil, "Secret names to request")
	execCmd.Flags().Uint64("timeout", 60, "Runner-side watchdog in seconds")

	registerSelfFlags(provisionCmd)
	provisionCmd.Flags().String("kind", "proxmox-lxc", "proxmox-lxc")
	provisionCmd.Flags().String("role", "", "relay|cp|k3s")
	provisionCmd.Flags().String("hostname", "", "Guest hostname (default <slug>-<role>)")
	provisionCmd.Flags().String("domain", "", "Relay identity domain")
	provisionCmd.Flags().String("storage", "local-lvm", "PVE storage")
	provisionCmd.Flags().Uint32("rootfs-gb", 16, "rootfs size GB")
	provisionCmd.Flags().Uint32("memory-mb", 2048, "RAM MB")
	provisionCmd.Flags().String("bridge", "vmbr0", "network bridge")
	provisionCmd.Flags().String("lxc-ip", "", "static IPv4 (CIDR)")
	provisionCmd.Flags().String("lxc-gw", "", "gateway for static IP")
	provisionCmd.Flags().String("vmid", "", "VMID (auto when empty)")
	provisionCmd.Flags().StringArray("mount", nil, "durable mount <source>:<guest>")

	storageCmd.AddCommand(storageResolveCmd, storageEnsureCmd)
	for _, sc := range []*cobra.Command{storageResolveCmd, storageEnsureCmd} {
		registerSelfFlags(sc)
	}
	storageResolveCmd.Flags().Bool("confirm-storage", false, "consent to CREATE a backend")
	storageResolveCmd.Flags().String("device", "", "physical device for a NEW zpool")
	storageEnsureCmd.Flags().String("tenant", "", "relay|cp|k3s-volumes")
	storageEnsureCmd.Flags().String("domain", "", "relay identity domain")
	storageEnsureCmd.Flags().String("pool", "", "storage pool")
	storageEnsureCmd.Flags().String("kind", "", "backend kind (zfs|lvmth)")
	storageEnsureCmd.Flags().Uint64("size-gb", drive.TenantLVSizeGB, "per-tenant LV size GiB")
	storageEnsureCmd.Flags().Uint64("pool-size-gb", drive.FreshPoolSizeGB, "thin pool size GiB")
	storageEnsureCmd.Flags().String("thin-pool", "", "thin pool name")

	registerSelfFlags(deployCpCmd)
	deployCpCmd.Flags().String("state-dir", cpdeploy.DefaultCPStateDir(), "remote state dir")
	deployCpCmd.Flags().String("bin-dir", cpdeploy.DefaultCPBinDir(), "remote bin dir")
	deployCpCmd.Flags().String("bind", "", "loopback bind")
	deployCpCmd.Flags().String("binary", "", "LOCAL freehold-console binary")
	deployCpCmd.Flags().String("relay-url", "", "relay URL")
	deployCpCmd.Flags().String("relay-pubkey", "", "relay signing pubkey")
	deployCpCmd.Flags().String("relay-host-ip", "", "relay LXC LAN IP")
	deployCpCmd.Flags().String("lxc", "", "cp LXC vmid")
	deployCpCmd.Flags().String("runner-binary", "", "LOCAL runner binary")
	deployCpCmd.Flags().String("runner-package", "", "LOCAL runner package dir")
	deployCpCmd.Flags().String("operator-pubkey", "", "operator Nostr pubkey")
	deployCpCmd.Flags().String("public-origin", "", "console public origin")
	deployCpCmd.Flags().String("agent-tools-url", "", "agent-tools MCP URL")
	deployCpCmd.Flags().String("agent-tools-pubkey", "", "agent-tools pubkey")
	deployCpCmd.Flags().String("agent-tools-binary", "", "LOCAL freehold-agent-tools binary")
	deployCpCmd.Flags().String("world-config", "", "cpbuild.Coords JSON (the console's build-executor coords)")
}
