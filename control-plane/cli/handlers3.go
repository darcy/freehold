package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/control-plane/cli/flows"
	"freehold/control-plane/cli/login"
	"freehold/control-plane/cli/teardown"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/planebase"
	"freehold/platform/services/certificates/letsencrypt"
	"freehold/platform/services/externaldns/cloudflare"
	relaydeploy "freehold/platform/services/relay/buzz"
	"freehold/providers/proxmox"
	"freehold/providers/proxmox/drive"
	"github.com/spf13/cobra"
)

// --- deploy-relay ---

var deployRelayCmd = &cobra.Command{
	Use:   "deploy-relay",
	Short: "C2/B: deploy the Buzz relay onto the target through a provisioning runner",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := resolveExecProfile(cmd); err != nil {
			return err
		}
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
		spec := &relaydeploy.RelayDeploySpec{
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
		res, err := relaydeploy.DeployRelay(proxmox.GuestExecFunc(c, target), spec)
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
	deployRelayCmd.Flags().String("buzz-ref", relaydeploy.DefaultBufRef, "block/buzz ref to fetch (tag or SHA; pinned SHA by default)")
	deployRelayCmd.Flags().String("lxc", "", "Deploy INTO this LXC on the target (the target is the PVE host; the LXC is where docker lives after `freehold bootstrap proxmox-lxc`). Omitted = deploy directly on the target host")
	deployRelayCmd.Flags().String("owner-pubkey", "", "Relay OWNER Nostr pubkey (64-hex) — written to RELAY_OWNER_PUBKEY; the bundle's run.sh refuses to start with CHANGE_ME placeholders")
	deployRelayCmd.Flags().String("relay-url", "", "The relay's OWN resolvable URL (http://host:port) — written into BUZZ_DOMAIN/RELAY_URL/media so the relay binds the REAL community (the example.com placeholders are not literal CHANGE_ME)")
	deployRelayCmd.Flags().String("operator-pubkey", "", "The OPERATOR's Nostr pubkey (64-hex) — invite the human operator to the relay once it comes up (fail-closed: required)")
	deployRelayCmd.Flags().String("domain", "", "The forced identity DOMAIN (never an IP): writes BUZZ_DOMAIN/RELAY_URL = the domain (wss) AND provisions the TLS local-CA posture (own openssl CA + server cert; import ca.crt on your devices)")
}

// --- deploy-cp ---

// --- bootstrap ---

var provisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "C2/A2: bootstrap-provision a target through a provisioning runner",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := resolveExecProfile(cmd); err != nil {
			return err
		}
		common := readCommonFlags(cmd)
		kind, _ := cmd.Flags().GetString("kind")
		target, _ := cmd.Flags().GetString("target")
		operatorPub, _ := cmd.Flags().GetString("operator-pubkey")
		domain, _ := cmd.Flags().GetString("domain")
		role, _ := cmd.Flags().GetString("role")
		if kind == "" || operatorPub == "" || domain == "" {
			return fmt.Errorf("bootstrap needs --kind --operator-pubkey --domain")
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		// fail-closed gate: --operator-pubkey must be a real pubkey (npub or
		// hex); the value itself is consumed by deploy-relay.
		if _, err := crypto.ParsePubkeyInput(operatorPub); err != nil {
			return err
		}
		hostname, err := bootstrap.DomainLXCName(domain, role)
		if err != nil {
			return err
		}
		var res *proxmox.BootstrapResult
		switch kind {
		case "proxmox-lxc":
			spec := &proxmox.ProxmoxLxcSpec{
				Hostname: hostname,
				Storage:  mustStr(cmd, "storage"),
				RootfsGB: mustU32(cmd, "rootfs-gb"),
				MemoryMB: mustU32(cmd, "memory-mb"),
				Bridge:   mustStr(cmd, "bridge"),
			}
			if v, _ := cmd.Flags().GetUint32("vmid"); v != 0 {
				spec.VMID = &v
			}
			if t, _ := cmd.Flags().GetString("template"); t != "" {
				spec.Template = &t
			}
			if ip, _ := cmd.Flags().GetString("lxc-ip"); ip != "" {
				gw, _ := cmd.Flags().GetString("lxc-gw")
				spec.NetIP, spec.NetGW = &ip, &gw
			}
			for _, m := range mustArr(cmd, "mount") {
				ms, err := bootstrap.ParseMount(m)
				if err != nil {
					return err
				}
				spec.Mounts = append(spec.Mounts, ms)
			}
			res, err = proxmox.BootstrapProxmoxLxc(proxmox.ClientExec(c, target), spec)
		case "vultr-vps":
			res, err = proxmox.BootstrapVultrVps(proxmox.ClientExec(c, target), target, &proxmox.VultrVpsSpec{
				Label:        hostname,
				Region:       mustStr(cmd, "region"),
				Plan:         mustStr(cmd, "plan"),
				OsID:         mustU32(cmd, "os-id"),
				DestroyAfter: mustBool(cmd, "destroy"),
			})
		case "hetzner-vps":
			res, err = proxmox.BootstrapHetznerVps(proxmox.ClientExec(c, target), target, &proxmox.HetznerVpsSpec{
				Label:        hostname,
				Location:     mustStr(cmd, "location"),
				ServerType:   mustStr(cmd, "server-type"),
				Image:        mustStr(cmd, "image"),
				DestroyAfter: mustBool(cmd, "destroy"),
			})
		default:
			return fmt.Errorf("unknown --kind %q (proxmox-lxc | vultr-vps | hetzner-vps)", kind)
		}
		if err != nil {
			return err
		}
		if res.IP == "" {
			return fmt.Errorf("the %s driver did not report a target IP", kind)
		}
		// No A4 DNS gate: everything resolves internally behind the proxy, so
		// we don't require the domain to map to this IP before continuing.
		fmt.Printf("target is up at %s (no DNS gate — internal resolution is enough for install)\n", res.IP)
		fmt.Printf("BOOTSTRAPPED %s (%s): %s\n", res.Name, res.Kind, res.Detail)
		return nil
	},
}

func init() {
	addCommonFlags(provisionCmd, nil)
	provisionCmd.Flags().String("target", "proxmox-box", "Target to drive provisioning through (a runner targeting the PVE host for proxmox-lxc, the vultr runner for vultr-vps)")
	provisionCmd.Flags().String("role", "relay", "Role of this target: 'relay' or 'cp' — the LXC name is derived from the domain: <normalized-domain>-relay / -cp (--name is gone)")
	provisionCmd.Flags().Uint32("vmid", 0, "LXC vmid (proxmox-lxc; must be >= 100 when given; omitted = the driver picks the lowest free id via `pct list`)")
	provisionCmd.Flags().Uint32("rootfs-gb", 16, "LXC rootfs size in GB (proxmox-lxc)")
	provisionCmd.Flags().Uint32("memory-mb", 2048, "LXC memory in MB (proxmox-lxc)")
	provisionCmd.Flags().String("template", "", "LXC template name in storage 'local'; auto-detect when omitted")
	provisionCmd.Flags().String("storage", "local-lvm", "LXC storage (proxmox-lxc)")
	provisionCmd.Flags().String("bridge", "vmbr0", "LXC network bridge (proxmox-lxc)")
	provisionCmd.Flags().String("lxc-ip", "", "STATIC guest IP (CIDR) + gateway for Proxmox-on-Cloud-Compute hosts")
	provisionCmd.Flags().String("lxc-gw", "", "")
	provisionCmd.Flags().StringArray("mount", nil, "Durable-plane dataset mount baked into `pct create` (repeatable), shape `<dataset>:<guest-path>` — the \"born on the plane\" reference")
	provisionCmd.Flags().String("region", "atl", "Vultr region (vultr-vps)")
	provisionCmd.Flags().String("plan", "vc2-1c-1gb", "Vultr plan (vultr-vps)")
	provisionCmd.Flags().Uint32("os-id", 1743, "Vultr OS id (vultr-vps; Debian 12 = 1743)")
	provisionCmd.Flags().String("location", "fsn1", "Hetzner location (hetzner-vps)")
	provisionCmd.Flags().String("server-type", "cx22", "Hetzner server type (hetzner-vps)")
	provisionCmd.Flags().String("image", "ubuntu-22.04", "Hetzner OS image (hetzner-vps)")
	provisionCmd.Flags().Bool("destroy", false, "Destroy the VPS after verifying (vultr-vps/hetzner-vps; for tests/cleanup)")
	provisionCmd.Flags().String("operator-pubkey", "", "The OPERATOR's Nostr pubkey (64-hex) — relay invite (create-new) / attach auth (attach-existing) + console admin seed (fail-closed: required at bootstrap)")
	provisionCmd.Flags().String("domain", "", "The relay's identity DOMAIN (never an IP): the install BLOCKS (A4) until it resolves to the provisioned target's IP")
	provisionCmd.Flags().Uint64("domain-wait-secs", 300, "Seconds to wait for the domain to resolve to the target IP (A4)")
	provisionCmd.Flags().String("kind", "", "Target kind: proxmox-lxc | vultr-vps | hetzner-vps")
}

func mustStr(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

func mustU32(cmd *cobra.Command, name string) uint32 {
	v, _ := cmd.Flags().GetUint32(name)
	return v
}

func mustBool(cmd *cobra.Command, name string) bool {
	v, _ := cmd.Flags().GetBool(name)
	return v
}

func mustArr(cmd *cobra.Command, name string) []string {
	v, _ := cmd.Flags().GetStringArray(name)
	return v
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
		// Tenant context, like exec: resolve the ACTIVE profile so the runner
		// pubkey resolves from the profile's state dir (not the legacy default),
		// else a multi-world box signs for the wrong runner audience.
		if err := resolveExecProfile(cmd); err != nil {
			return err
		}
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		device, _ := cmd.Flags().GetString("device")
		confirm, _ := cmd.Flags().GetBool("confirm-storage")
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		action, err := drive.ResolveProxmox(drive.ClientExec(c, target), confirm, optOf(device))
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
			// Machine-parseable for the pipeline to thread the REAL backend
			// identity into the ensure + config steps.
			fmt.Printf("STORAGE-POOL: %s\n", action.Pool)
			if *action.Detected == planebase.ExistingLvmThin {
				// The placement gate's probe: ALL thin pools the VG holds
				// RIGHT NOW. The membership list — not just the first pool
				// — is what lets the gate decide adopt-vs-carve honestly for
				// a NAMED pool (--thin-pool): a two-pool VG where the named
				// pool is the SECOND one must adopt it (created=false), and
				// a name that matches none must carve (created=true).
				pools, err := drive.ThinPools(drive.ClientExec(c, target), action.Pool)
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
			label := "ZFS zpool"
			if *action.Backend == planebase.BackendLvmThin {
				label = "LVM-thin pool"
			}
			fmt.Printf("STORAGE: creating new backend (%s, pool %s) with consent…\n", label, action.Pool)
			fmt.Printf("STORAGE-POOL: %s\n", action.Pool)
			if *action.Backend == planebase.BackendZfs {
				if err := proxmox.EnsureZpool(proxmox.ClientExec(c, target), action.Pool, optOf(device)); err != nil {
					return err
				}
			} else {
				// consent + no zpool + no VG: drive's per-tenant ensure
				// creates the thin pool in a NEW VG when needed; there is no
				// VG to name here yet.
				fmt.Println("STORAGE: LVM-thin — per-tenant ensure will create the pool+LV")
			}
		case "Bail":
			return fmt.Errorf("%s", action.Message)
		}
		return nil
	},
}

// parseKind resolves the backend KIND to drive with. The plane records it
// (`plane.backend_kind`) and the pipeline passes it as `--kind`; honoring the
// recorded kind beats re-detecting, because a host with BOTH a zpool and a VG
// would otherwise always resolve to ZFS and drive an LVM-backed tenant the
// wrong way. Absent flag => detect via resolve_proxmox(consent=false): a host
// with a zpool drives ZFS; a host with only an LVM VG (stock PVE: VG `pve`,
// no zpool) drives LVM-thin. A non-nil action in the return means "no
// existing backend" — the caller decides its tolerated-no-op shape.
func parseKind(c *client.McpClient, target, kindFlag string) (planebase.BackendKind, *drive.ResolveAction, error) {
	switch kindFlag {
	case "":
	case "zfs":
		return planebase.KindZfs, nil, nil
	case "lvmth":
		return planebase.KindLvmThin, nil, nil
	default:
		return "", nil, fmt.Errorf("unknown storage backend kind: %s (expected zfs|lvmth)", kindFlag)
	}
	action, err := drive.ResolveProxmox(drive.ClientExec(c, target), false, nil)
	if err != nil {
		return "", nil, err
	}
	if action.Kind == "Reuse" {
		// a USABLE backend exists → the caller drives it; the non-nil
		// return is reserved for "no existing backend" (Create/Bail).
		if *action.Detected == planebase.ExistingZfs {
			return planebase.KindZfs, nil, nil
		}
		return planebase.KindLvmThin, nil, nil
	}
	return "", action, nil
}

var storageEnsureCmd = &cobra.Command{
	Use:   "ensure",
	Short: "Ensure a tenant's dataset/volume exists (idempotent) + is guest-writable",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := resolveExecProfile(cmd); err != nil {
			return err
		}
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		tenant, _ := cmd.Flags().GetString("tenant")
		domain, _ := cmd.Flags().GetString("domain")
		pool, _ := cmd.Flags().GetString("pool")
		kindStr, _ := cmd.Flags().GetString("kind")
		sizeGB, _ := cmd.Flags().GetUint64("size-gb")
		poolSizeGB, _ := cmd.Flags().GetUint64("pool-size-gb")
		thinPool, _ := cmd.Flags().GetString("thin-pool")
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
		// DETECT the backend when --kind is absent; consent=false here —
		// ensure never CREATES a backend, it only mounts tenants onto one
		// that exists.
		kind, action, err := parseKind(c, target, kindStr)
		if err != nil {
			return err
		}
		if action != nil {
			if action.Kind == "Create" {
				return fmt.Errorf("storage ensure cannot run on a backend that needs creating — run `storage resolve --confirm-storage` first")
			}
			return fmt.Errorf("no storage backend to ensure onto: %s — run `storage resolve` first", action.Message)
		}
		// Resolve + chown the tenant's born-at-create mounts, then print one
		// `<source>:<guest>` line per mount for the pipeline to record.
		var mounts []planebase.MountSpec
		switch kind {
		case planebase.KindZfs:
			mounts, err = drive.ResolveTenantMounts(drive.ClientExec(c, target), pool, domain, t)
		case planebase.KindLvmThin:
			mounts, err = drive.ResolveLvmMounts(drive.ClientExec(c, target), pool, domain, t, sizeGB, poolSizeGB, thinPool)
		}
		if err != nil {
			return err
		}
		// Always emit the discoverable backend kind so the pipeline records
		// it (dispatch of later destroy/re-resolve steps).
		kindOut := "zfs"
		if kind == planebase.KindLvmThin {
			kindOut = "lvmth"
		}
		fmt.Printf("STORAGE-BACKEND: %s %s\n", kindOut, pool)
		for _, m := range mounts {
			fmt.Printf("STORAGE-MOUNT %s:%s\n", m.Source, m.GuestPath)
		}
		fmt.Printf("STORAGE: %s datasets ensured + guest-writable\n", tenant)
		return nil
	},
}

var storageDestroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Destroy a tenant's dataset subtree (data+compute teardown half)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := resolveExecProfile(cmd); err != nil {
			return err
		}
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		tenant, _ := cmd.Flags().GetString("tenant")
		domain, _ := cmd.Flags().GetString("domain")
		pool, _ := cmd.Flags().GetString("pool")
		kindStr, _ := cmd.Flags().GetString("kind")
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
		kind, action, err := parseKind(c, target, kindStr)
		if err != nil {
			return err
		}
		if action != nil {
			// No backend to destroy on => NOTHING was ever created: a
			// tolerated no-op (the pre-plane / VPS-downgraded / unprovisioned
			// teardown case), not a hard error after the LXCs are already
			// gone.
			fmt.Println("STORAGE-DESTROYED: false")
			return nil
		}
		// The bool distinguishes ABSENT (nothing to destroy — a no-op for the
		// caller) from DESTROYED (the dataset subtree went away).
		destroyed, err := drive.DestroyTenantBackend(drive.ClientExec(c, target), kind, pool, domain, t)
		if err != nil {
			return err
		}
		fmt.Printf("STORAGE-DESTROYED: %v\n", destroyed)
		return nil
	},
}

var storageDestroyPoolCmd = &cobra.Command{
	Use:   "destroy-pool",
	Short: "Remove a freehold-CREATED thin pool from its VG (full teardown --data half)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := resolveExecProfile(cmd); err != nil {
			return err
		}
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		pool, _ := cmd.Flags().GetString("pool")
		thinPool, _ := cmd.Flags().GetString("thin-pool")
		if pool == "" || thinPool == "" {
			return fmt.Errorf("storage destroy-pool needs --pool (the VG) and --thin-pool")
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		// The CLI is the ONLY caller; the pool passed here comes from the
		// config's plane.thin_pool, recorded only when freehold carved it.
		// drive.RemoveThinPool refuses while tenant LVs still ride it.
		if err := drive.RemoveThinPool(drive.ClientExec(c, target), pool, thinPool); err != nil {
			return err
		}
		fmt.Println("STORAGE-POOL-DESTROYED: true")
		return nil
	},
}

var storageInfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Report the live durable-plane snapshot: host capacity + per-mount size/used + guest bind-mount liveness (read-only, DATA-tab source)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := resolveExecProfile(cmd); err != nil {
			return err
		}
		common := readCommonFlags(cmd)
		target, _ := cmd.Flags().GetString("target")
		pool, _ := cmd.Flags().GetString("pool")
		kindStr, _ := cmd.Flags().GetString("kind")
		mountSpecs, _ := cmd.Flags().GetStringArray("mount")
		if pool == "" {
			return fmt.Errorf("storage info needs --pool")
		}
		c, err := connect(common, target)
		if err != nil {
			return err
		}
		kind, action, err := parseKind(c, target, kindStr)
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
		info, err := drive.ProbeStorage(drive.ClientExec(c, target), kind, pool, mounts)
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

// thinPoolOf returns the freehold-CREATED thin pool recorded in the config
// ("" when the plane reused a stock pool — nothing recorded).
func thinPoolOf(cfg *config.Config) string {
	if cfg.Plane.ThinPool != nil {
		return *cfg.Plane.ThinPool
	}
	return ""
}

// parseInfoMount parses a `storage info --mount` spec:
// `<role>:<source>:<guest>:<vmid|->`. Neither a host path/dataset nor a guest
// path carries `:`, so a plain 4-field split is unambiguous.
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

func init() {
	storageCmd.AddCommand(storageResolveCmd, storageEnsureCmd, storageInfoCmd, storageDestroyCmd, storageDestroyPoolCmd)
	addCommonFlags(storageResolveCmd, nil)
	addCommonFlags(storageEnsureCmd, nil)
	addCommonFlags(storageInfoCmd, nil)
	addCommonFlags(storageDestroyCmd, nil)
	addCommonFlags(storageDestroyPoolCmd, nil)
	for _, sc := range []*cobra.Command{storageResolveCmd, storageEnsureCmd, storageInfoCmd, storageDestroyCmd, storageDestroyPoolCmd} {
		sc.Flags().String("target", "proxmox-box", "Target to drive storage through (the runner holding the host ssh key)")
	}
	storageEnsureCmd.Flags().String("tenant", "", "Tenant: relay | cp | k3s-volumes")
	storageEnsureCmd.Flags().String("domain", "", "The relay's identity domain (for the dataset naming)")
	storageEnsureCmd.Flags().String("pool", "", "Storage pool (zpool name / VG name)")
	storageEnsureCmd.Flags().String("kind", "", "Backend kind recorded by the plane (`zfs` | `lvmth`). Honored when present; absent => detect")
	storageEnsureCmd.Flags().Uint64("size-gb", drive.TenantLVSizeGB, "Per-tenant thin LV size in GiB (LVM-thin backend; operator-prompted at rebuild)")
	storageEnsureCmd.Flags().Uint64("pool-size-gb", drive.FreshPoolSizeGB, "Thin-pool size in GiB when a NEW pool is carved (only when the VG has none)")
	storageEnsureCmd.Flags().String("thin-pool", "", "Thin pool the tenant LVs land in (placement gate's choice): named = adopt-or-carve that pool; empty = reuse the VG's pool / carve the default when none")
	storageDestroyPoolCmd.Flags().String("pool", "", "The VG the thin pool lives in")
	storageDestroyPoolCmd.Flags().String("thin-pool", "", "The thin pool to remove (recorded plane.thin_pool — freehold-created only)")
	storageDestroyCmd.Flags().String("tenant", "", "Tenant: relay | cp | k3s-volumes")
	storageDestroyCmd.Flags().String("domain", "", "The relay's identity domain (for the dataset naming)")
	storageDestroyCmd.Flags().String("pool", "", "Storage pool (zpool name / VG name)")
	storageDestroyCmd.Flags().String("kind", "", "Backend kind recorded by the plane (`zfs` | `lvmth`). Honored when present; absent => detect")
	storageInfoCmd.Flags().String("pool", "", "Storage pool (zpool name / VG name)")
	storageInfoCmd.Flags().String("kind", "", "Backend kind recorded by the plane (`zfs` | `lvmth`). Honored when present; absent => detect")
	storageInfoCmd.Flags().StringArray("mount", nil, "Mount ref <role>:<source>:<guest>:<vmid|-> (repeatable; vmid '-' skips the guest probe)")
	storageResolveCmd.Flags().String("device", "", "Physical device for a NEW zpool (e.g. /dev/sdb) — required only on the consent-gated create path, when no existing backend is detected")
	storageResolveCmd.Flags().Bool("confirm-storage", false, "Operator consent to CREATE a backend (zpool OR LVM-thin) when none is detected. Absent + no backend = actionable bail")
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
