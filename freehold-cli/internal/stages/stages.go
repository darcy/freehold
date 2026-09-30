// The install surface's self-staged commands: the freehold binary re-invokes
// ITSELF for the low-level stage commands (exec / provision / storage /
// deploy-cp) exactly as the shared engine selfStages them. Each talks to the
// host/siblings through a provisioning runner.
package stages

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/version"
	"freehold/freehold-cli/internal/cpdeploy"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/box"
	"freehold/platform/provisioning/planebase"
	"freehold/providers/proxmox"
	"freehold/providers/proxmox/drive"
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
// the local runner package. The profile state ROOT is derived from agentDir
// (OpsDir lives at <state-root>/control-plane/agent-ops): a self-staged child
// is a fresh process with no in-process config.Current, so this is how it finds
// the profile-scoped runner package the parent wrote. A non-conventional
// agent-dir falls back to the default state root.
func installConnect(addr, agentDir, target string) (*client.McpClient, error) {
	auth, err := agentAuth(agentDir)
	if err != nil {
		return nil, err
	}
	runnerDir := filepath.Join(stateRootFor(agentDir), "runner", target)
	runnerPK, err := box.LoadPubkey(runnerDir)
	if err != nil {
		return nil, fmt.Errorf("runner %q not found at %s (provision it first): %v",
			target, runnerDir, err)
	}
	return client.New(client.ConnectURL(addr), auth, runnerPK)
}

// stateRootFor derives the profile state root from a conventional
// <root>/control-plane/agent-ops agent-dir; anything else uses the default
// state root (config.StateDir()).
func stateRootFor(agentDir string) string {
	if filepath.Base(agentDir) == "agent-ops" && filepath.Base(filepath.Dir(agentDir)) == "control-plane" {
		return filepath.Dir(filepath.Dir(agentDir))
	}
	return config.StateDir()
}

func registerSelfFlags(cmd *cobra.Command) {
	cmd.Flags().String("addr", "127.0.0.1:8787", "Runner MCP address (loopback)")
	cmd.Flags().String("agent-dir", box.OpsDir(), "Agent identity dir (signing)")
	cmd.Flags().String("target", box.RunnerTarget, "Target runner")
	cmd.Flags().Bool("transient", false, "reach the host by direct root SSH (no served runner)")
	cmd.Flags().String("host", "", "host to SSH into for --transient")
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
		exec, cleanup, err := stageExec(addr, agentDir, target, mustStr(cmd, "host"), mustBool(cmd, "transient"))
		if err != nil {
			return err
		}
		defer cleanup()
		switch kind {
		case "proxmox-lxc":
			return provisionProxmoxLxc(exec, cmd)
		default:
			return fmt.Errorf("unknown kind %q (proxmox-lxc supported)", kind)
		}
	},
}

func provisionProxmoxLxc(exec proxmox.ExecFunc, cmd *cobra.Command) error {
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
	spec := &proxmox.ProxmoxLxcSpec{
		Hostname: hostname, VMID: vmid,
		Storage: mustStr(cmd, "storage"), RootfsGB: mustU32(cmd, "rootfs-gb"),
		MemoryMB: mustU32(cmd, "memory-mb"), Bridge: mustStr(cmd, "bridge"),
		NetIP: ip, NetGW: gw, Mounts: mounts,
	}
	res, err := proxmox.BootstrapProxmoxLxc(exec, spec)
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
		relayDomain := mustStr(cmd, "relay-domain")
		confirm := mustBool(cmd, "confirm-storage")
		exec, cleanup, err := stageExec(addr, agentDir, target, mustStr(cmd, "host"), mustBool(cmd, "transient"))
		if err != nil {
			return err
		}
		defer cleanup()
		// One read-only pass enumerates every backend + its data. The engine
		// consumes the JSON inventory line and drives the (plain-language)
		// selection; direct CLI use also prints the recommended backend. The
		// domain scopes "this world's plane" so another world's freehold data
		// is ordinary reuse, never a reconnect.
		inv, err := proxmox.StorageInventory(exec)
		if err != nil {
			return err
		}
		if line := planebase.EncodeInventory(inv); line != "" {
			fmt.Println(line)
		}
		opts := planebase.BuildOptions(inv, relayDomain)
		if sel := mustStr(cmd, "plane-pool"); sel != "" {
			opt, ok := planebase.FindOption(opts, sel)
			if !ok {
				return fmt.Errorf("no storage backend named %q found on this host", sel)
			}
			if opt.Kind == planebase.KindBlocked {
				return fmt.Errorf("storage %q cannot be used: %s", sel, opt.Reason)
			}
			if opt.Kind == planebase.KindCreateDevice {
				return fmt.Errorf("preparing a new storage backend on %s is a later phase — choose an existing backend", opt.Device)
			}
			printStorageSelection(opt)
			return nil
		}
		if !inv.Empty() {
			if i := planebase.Recommend(opts); i >= 0 {
				printStorageSelection(opts[i])
			}
			// A real choice / cautions / blocked-only: emit nothing selected;
			// the engine prompts from the inventory.
			return nil
		}
		// A truly bare host: the legacy create/bail branch.
		action, err := drive.ResolveProxmox(exec, confirm, optOf(mustStr(cmd, "device")))
		if err != nil {
			return err
		}
		switch action.Kind {
		case "Reuse":
			fmt.Printf("STORAGE-POOL: %s\n", action.Pool)
		case "Create":
			fmt.Printf("STORAGE: creating new backend (pool %s) with consent…\n", action.Pool)
			fmt.Printf("STORAGE-POOL: %s\n", action.Pool)
			if *action.Backend == planebase.BackendZfs {
				if err := proxmox.EnsureZpool(exec, action.Pool, optOf(mustStr(cmd, "device"))); err != nil {
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

// printStorageSelection emits the legacy selection contract lines for a chosen
// option, so `storage ensure` and direct CLI users see the same backend the
// engine selected.
func printStorageSelection(opt planebase.Option) {
	switch opt.Kind {
	case planebase.KindReuseZpool:
		fmt.Printf("STORAGE: reusing existing backend (ZFS zpool %s) — nothing created\n", opt.Backend)
		fmt.Printf("STORAGE-POOL: %s\n", opt.Backend)
		fmt.Printf("STORAGE-BACKEND: zfs %s\n", opt.Backend)
	case planebase.KindReuseVG:
		fmt.Printf("STORAGE: reusing existing backend (LVM VG %s) — nothing created\n", opt.Backend)
		fmt.Printf("STORAGE-POOL: %s\n", opt.Backend)
		thin := "-"
		if names := planebase.PoolNames(opt); len(names) > 0 {
			thin = strings.Join(names, ",")
		}
		fmt.Printf("STORAGE-THINPOOL: %s\n", thin)
		fmt.Printf("STORAGE-BACKEND: lvmth %s\n", opt.Backend)
	}
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
		exec, cleanup, err := stageExec(addr, agentDir, target, mustStr(cmd, "host"), mustBool(cmd, "transient"))
		if err != nil {
			return err
		}
		defer cleanup()
		t, err := tenantFor(tenant)
		if err != nil {
			return err
		}
		kind, action, err := parseKind(exec, kindStr)
		if err != nil {
			return err
		}
		if action != nil {
			return fmt.Errorf("no storage backend to ensure onto: %s — run `storage resolve` first", action.Message)
		}
		var mounts []planebase.MountSpec
		switch kind {
		case planebase.KindZfs:
			mounts, err = drive.ResolveTenantMounts(exec, pool, domain, t)
		case planebase.KindLvmThin:
			mounts, err = drive.ResolveLvmMounts(exec, pool, domain, t,
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

// storageDestroyCmd destroys a tenant's dataset subtree on the selected
// backend — the erase half of reconnecting to (or replacing) a previous
// freehold plane. Only freehold-namespaced entries are touched.
var storageDestroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Destroy a tenant's dataset subtree (erase a previous freehold plane)",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		target := mustStr(cmd, "target")
		tenant := mustStr(cmd, "tenant")
		domain := mustStr(cmd, "domain")
		pool := mustStr(cmd, "pool")
		kindStr := mustStr(cmd, "kind")
		if tenant == "" || domain == "" || pool == "" {
			return fmt.Errorf("storage destroy needs --tenant --domain --pool")
		}
		exec, cleanup, err := stageExec(addr, agentDir, target, mustStr(cmd, "host"), mustBool(cmd, "transient"))
		if err != nil {
			return err
		}
		defer cleanup()
		t, err := tenantFor(tenant)
		if err != nil {
			return err
		}
		kind, action, err := parseKind(exec, kindStr)
		if err != nil {
			return err
		}
		if action != nil {
			// No backend to destroy on => nothing was ever created: a
			// tolerated no-op, not a hard error.
			fmt.Println("STORAGE-DESTROYED: false")
			return nil
		}
		destroyed, err := drive.DestroyTenantBackend(exec, kind, pool, domain, t)
		if err != nil {
			return err
		}
		fmt.Printf("STORAGE-DESTROYED: %v\n", destroyed)
		return nil
	},
}

// storageDestroyPoolCmd is driven by the teardown engine (whole-world --data):
// remove a freehold-CREATED thin pool after its tenant LVs are gone.
var storageDestroyPoolCmd = &cobra.Command{
	Use:   "destroy-pool",
	Short: "Remove a freehold-CREATED thin pool from its VG (full teardown --data half)",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		target := mustStr(cmd, "target")
		pool := mustStr(cmd, "pool")
		thinPool := mustStr(cmd, "thin-pool")
		if pool == "" || thinPool == "" {
			return fmt.Errorf("storage destroy-pool needs --pool (the VG) and --thin-pool")
		}
		exec, cleanup, err := stageExec(addr, agentDir, target, mustStr(cmd, "host"), mustBool(cmd, "transient"))
		if err != nil {
			return err
		}
		defer cleanup()
		if err := drive.RemoveThinPool(exec, pool, thinPool); err != nil {
			return err
		}
		fmt.Println("STORAGE-POOL-DESTROYED: true")
		return nil
	},
}

// storageInfoCmd reports the live durable-plane snapshot (read-only; the DATA
// tab's source).
var storageInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Report the live durable-plane snapshot: host capacity + per-mount size/used + guest bind-mount liveness",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		agentDir, _ := cmd.Flags().GetString("agent-dir")
		target := mustStr(cmd, "target")
		pool := mustStr(cmd, "pool")
		kindStr := mustStr(cmd, "kind")
		mountSpecs := mustArr(cmd, "mount")
		if pool == "" {
			return fmt.Errorf("storage info needs --pool")
		}
		exec, cleanup, err := stageExec(addr, agentDir, target, mustStr(cmd, "host"), mustBool(cmd, "transient"))
		if err != nil {
			return err
		}
		defer cleanup()
		kind, action, err := parseKind(exec, kindStr)
		if err != nil {
			return err
		}
		if action != nil {
			fmt.Println("STORAGE-CAPACITY: -")
			return nil
		}
		var mounts []drive.MountArg
		for _, m := range mountSpecs {
			ma, err := parseInfoMount(m)
			if err != nil {
				return err
			}
			mounts = append(mounts, ma)
		}
		info, err := drive.ProbeStorage(exec, kind, pool, mounts)
		if err != nil {
			return err
		}
		fmt.Printf("STORAGE-CAPACITY: %s\n", info.Capacity)
		for _, m := range info.Mounts {
			mounted := "-"
			if m.GuestMounted != nil {
				if *m.GuestMounted {
					mounted = "mounted"
				} else {
					mounted = "absent"
				}
			}
			size, used := "-", "-"
			if m.Size != nil {
				size = strconv.FormatUint(*m.Size, 10)
			}
			if m.Used != nil {
				used = strconv.FormatUint(*m.Used, 10)
			}
			fmt.Printf("STORAGE-INFO %s:%s:%s:%s:%s:%s\n", m.Role, m.Source, m.Guest, size, used, mounted)
		}
		return nil
	},
}

// parseInfoMount parses a `storage info --mount` spec:
// `<role>:<source>:<guest>:<vmid|->`.
func parseInfoMount(s string) (drive.MountArg, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return drive.MountArg{}, fmt.Errorf("--mount must be <role>:<source>:<guest>:<vmid|-> (got %q)", s)
	}
	var vmid *uint32
	if parts[3] != "-" {
		n, err := strconv.ParseUint(parts[3], 10, 32)
		if err != nil {
			return drive.MountArg{}, fmt.Errorf("vmid must be a number or '-' (got %q)", s)
		}
		v := uint32(n)
		vmid = &v
	}
	return drive.MountArg{Role: parts[0], Source: parts[1], Guest: parts[2], VMID: vmid}, nil
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

func parseKind(exec proxmox.ExecFunc, kindFlag string) (planebase.BackendKind, *drive.ResolveAction, error) {
	switch kindFlag {
	case "":
	case "zfs":
		return planebase.KindZfs, nil, nil
	case "lvmth":
		return planebase.KindLvmThin, nil, nil
	default:
		return "", nil, fmt.Errorf("unknown storage backend kind: %s (expected zfs|lvmth)", kindFlag)
	}
	action, err := drive.ResolveProxmox(exec, false, nil)
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
		transport, cleanup, err := buildTransport(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
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
		spec.KeyComment = mustStr(cmd, "key-comment")
		// The version pin: install stamps it, build/teardown don't (their
		// deploy-cp invocations pass no --version).
		if v := mustStr(cmd, "version"); v != "" {
			ch := mustStr(cmd, "channel")
			if ch == "" {
				ch = version.Channel(v)
			}
			spec.Pin = &version.Pin{Version: v, Channel: ch, Commit: mustStr(cmd, "commit")}
		}
		if d := mustStr(cmd, "migrations-dir"); d != "" {
			spec.MigrationsDir = &d
		}
		if v := mustStr(cmd, "lxc"); v != "" {
			var n uint32
			fmt.Sscanf(v, "%d", &n)
			spec.LXc = &n
		}
		if mustBool(cmd, "redeploy") {
			// update's lighter path: replace binaries + copy scripts, restart,
			// no adoption/rotation/promotion.
			return cpdeploy.Redeploy(transport, spec)
		}
		res, err := cpdeploy.DeployCp(transport, spec)
		if err != nil {
			return err
		}
		fmt.Println(res.Detail)
		fmt.Println("console pubkey:", res.Pubkey)
		return nil
	},
}

// buildTransport builds the host transport shared by the self-staged CP verbs:
// the served runner (default) or a transient root-SSH door (--transient). The
// returned cleanup releases any transient key material.
func buildTransport(cmd *cobra.Command) (cpdeploy.Transport, func(), error) {
	addr, _ := cmd.Flags().GetString("addr")
	agentDir, _ := cmd.Flags().GetString("agent-dir")
	target := mustStr(cmd, "target")
	if mustBool(cmd, "transient") {
		keyPath, cleanup, err := transientKey(agentDir, target)
		if err != nil {
			return nil, nil, err
		}
		return sshTransport{host: strings.TrimPrefix(mustStr(cmd, "host"), "root@"), key: keyPath}, cleanup, nil
	}
	c, err := installConnect(addr, agentDir, target)
	if err != nil {
		return nil, nil, err
	}
	return cpdeploy.ClientTransport{C: c, Target: target}, func() {}, nil
}

// --- stamp-version (self-staged: the update pin step) --------------------------------------

var stampVersionCmd = &cobra.Command{
	Use:   "stamp-version",
	Short: "Write the CP's <state-dir>/version.json pin (install/update only)",
	RunE: func(cmd *cobra.Command, args []string) error {
		transport, cleanup, err := buildTransport(cmd)
		if err != nil {
			return err
		}
		defer cleanup()
		stateDir := mustStr(cmd, "state-dir")
		if stateDir == "" {
			stateDir = cpdeploy.DefaultCPStateDir()
		}
		v := mustStr(cmd, "version")
		if v == "" {
			return fmt.Errorf("stamp-version needs --version")
		}
		ch := mustStr(cmd, "channel")
		if ch == "" {
			ch = version.Channel(v)
		}
		spec := &cpdeploy.DeployCpSpec{StateDir: stateDir, BinDir: cpdeploy.DefaultCPBinDir()}
		if lxc := mustStr(cmd, "lxc"); lxc != "" {
			var n uint32
			fmt.Sscanf(lxc, "%d", &n)
			spec.LXc = &n
		}
		return cpdeploy.StampPin(transport, spec, version.Pin{Version: v, Channel: ch, Commit: mustStr(cmd, "commit")})
	},
}

// StampVersionCommand returns the version-pin command for root registration.
func StampVersionCommand() *cobra.Command { return stampVersionCmd }

// optOf returns nil for an empty string (optional flag).
func optOf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func init() {
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
	provisionCmd.Flags().String("operator-pubkey", "", "Operator Nostr pubkey (accepted for symmetry; unused by proxmox-lxc)")

	storageCmd.AddCommand(storageResolveCmd, storageEnsureCmd, storageDestroyCmd, storageDestroyPoolCmd, storageInfoCmd)
	for _, sc := range []*cobra.Command{storageResolveCmd, storageEnsureCmd, storageDestroyCmd, storageDestroyPoolCmd, storageInfoCmd} {
		registerSelfFlags(sc)
	}
	storageResolveCmd.Flags().Bool("confirm-storage", false, "consent to CREATE a backend")
	storageResolveCmd.Flags().String("device", "", "physical device for a NEW zpool")
	storageResolveCmd.Flags().String("plane-pool", "", "select the backend to use by name (VG or zpool)")
	storageResolveCmd.Flags().String("relay-domain", "", "this world's relay domain (scopes reconnect vs. ordinary reuse)")
	storageEnsureCmd.Flags().String("tenant", "", "relay|cp|k3s-volumes")
	storageEnsureCmd.Flags().String("domain", "", "relay identity domain")
	storageEnsureCmd.Flags().String("pool", "", "storage pool")
	storageEnsureCmd.Flags().String("kind", "", "backend kind (zfs|lvmth)")
	storageEnsureCmd.Flags().Uint64("size-gb", drive.TenantLVSizeGB, "per-tenant LV size GiB")
	storageEnsureCmd.Flags().Uint64("pool-size-gb", drive.FreshPoolSizeGB, "thin pool size GiB")
	storageEnsureCmd.Flags().String("thin-pool", "", "thin pool name")
	storageDestroyCmd.Flags().String("tenant", "", "relay|cp|k3s-volumes")
	storageDestroyCmd.Flags().String("domain", "", "relay identity domain")
	storageDestroyCmd.Flags().String("pool", "", "storage pool")
	storageDestroyCmd.Flags().String("kind", "", "backend kind (zfs|lvmth)")
	storageDestroyPoolCmd.Flags().String("pool", "", "the VG the thin pool lives in")
	storageDestroyPoolCmd.Flags().String("thin-pool", "", "the thin pool to remove (freehold-created only)")
	storageInfoCmd.Flags().String("pool", "", "storage pool")
	storageInfoCmd.Flags().String("kind", "", "backend kind (zfs|lvmth)")
	storageInfoCmd.Flags().StringArray("mount", nil, "mount ref <role>:<source>:<guest>:<vmid|-> (repeatable)")

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
	deployCpCmd.Flags().String("key-comment", "", "authorized_keys comment for a rotated substrate key (default: the runner name)")
	deployCpCmd.Flags().String("version", "", "version to stamp (version.json); absent = don't promote")
	deployCpCmd.Flags().String("channel", "", "release channel to stamp (default: derived from --version)")
	deployCpCmd.Flags().String("commit", "", "commit sha to stamp")
	deployCpCmd.Flags().String("migrations-dir", "", "LOCAL dir of <epoch>.sh migration scripts (shipped unmarked; the queue runs at the end of world bring-up)")
	deployCpCmd.Flags().Bool("redeploy", false, "replace binaries in an EXISTING plane (update): no adoption/rotation/promotion")

	registerSelfFlags(stampVersionCmd)
	stampVersionCmd.Flags().String("state-dir", cpdeploy.DefaultCPStateDir(), "remote state dir")
	stampVersionCmd.Flags().String("lxc", "", "cp LXC vmid")
	stampVersionCmd.Flags().String("version", "", "version to stamp")
	stampVersionCmd.Flags().String("channel", "", "channel to stamp (default: derived from --version)")
	stampVersionCmd.Flags().String("commit", "", "commit sha to stamp")
}
