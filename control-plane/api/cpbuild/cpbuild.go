// Package cpbuild is the CP-owned world bring-up engine: it drives the CP's
// co-located runner through the world-build stages (plane, relay, agent-tools,
// k3s, DNS, litellm, caddy, cert). Shared by freehold-agent-tools (the /mcp
// world_build tool) and freehold-console (the CP executor) so the two run the
// SAME stages; the console is what a thin login box triggers and does NOT
// depend on the relay roster to authorize the build.
package cpbuild

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"freehold/agents"
	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/delegate"
	"freehold/contract/identity"
	"freehold/contract/nipoa"
	"freehold/contract/relay"
	"freehold/contract/wire"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/api/cpstate"
	"freehold/control-plane/secret-management"
	"freehold/control-plane/state"
	"freehold/platform/migrations"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/planebase"
	"freehold/platform/provisioning/stages"
	"freehold/platform/services/certificates/letsencrypt"
	relaydeploy "freehold/platform/services/relay/buzz"
	caddydeploy "freehold/platform/services/webproxy/caddy"
	"freehold/providers/proxmox"
	"freehold/providers/proxmox/drive"
	"freehold/providers/proxmox/teardown"
)

const relayFreeholdChannel = "00000000-0000-4000-8000-00000000f0ef"

// The shipped migration scripts are bounded because the world_build path runs the
// queue while holding the registry's write lock: one wedged script would otherwise
// block every roster read and write in the serve for as long as it hung. Each script
// is a handful of relay curls and file edits, so minutes is generous; the wait delay
// is how long to give a killed script's orphaned descendants to drop the output pipe
// before the pipes are closed and the hold is released regardless.
const (
	migrationScriptTimeout = 5 * time.Minute
	migrationWaitDelay     = 5 * time.Second
)

// AgentToolsPort is the CP's freehold-agent-tools MCP bind port. Any URL the
// CPA pod bootstraps its stdio bridge from (the agent-tools `--self-url`, and
// the CPA/agent manifests' bridge URL) MUST use this port — the pod curls
// <url>/freehold-agent-tools-binary off the agent-tools server itself, so
// pointing it at the console's port makes the fetch 404 and silently falls
// back to plain buzz-dev-mcp (no create_agent).
const AgentToolsPort = config.AgentToolsPort

type Spec struct {
	Name           string
	StateDir       string
	RelayURL       string
	RelayAuthURL   string
	RelayWS        string
	RelayPK        string
	RelayHost      string
	RelayIP        string
	CpHost         string
	CpIP           string
	CpLxc          uint32
	ProxyIP        string
	LitellmIP      string
	PlanePool      string
	PlaneKind      string
	ThinPool       string
	SizeGB         uint64
	PoolSizeGB     uint64
	RootfsGB       uint32
	MemoryMB       uint32
	StorageName    string
	RelayGW        string
	Bridge         string
	RelayLxc       uint32
	RelayCompose   string
	K3sVmid        uint32
	// The freehold-subnet gateway (docs/NETWORK.md). GatewayCIDR is
	// the internal subnet ("" = no gateway — guests ride the LAN bridge as
	// before); GatewayVlan the in-host bridge tag; GatewayLxc the gateway
	// guest's vmid. K3sIP is the k3s node's address — with a gateway the
	// INTERNAL one (ProxyIP is then the gateway's LAN address, the edge);
	// without one it mirrors ProxyIP.
	GatewayCIDR string
	GatewayVlan int
	GatewayLxc  uint32
	// HostedGateway: the HOST is the gateway (api-vultr) — no guest, the
	// nftables assert runs on the host itself.
	HostedGateway bool
	K3sIP         string
	RunnerAddr     string
	RunnerPK       string
	RunnerTarget   string
	CpaName        string
	OwnerPub       string
	LitellmBaseURL string
	Sec            []byte
	Audience       string
	SelfURL        string
	// OperatorName is the operator's display name in Buzz (the kind:0 the
	// build publishes for them — what makes the desktop app skip its
	// first-run onboarding). Empty renders "Operator".
	OperatorName string
	// RepoURL is the source repository the shared system-orientation block
	// points agents at. Empty = the agents package's upstream default.
	RepoURL string

	// AgentRegistry/FactsStore are the running agent-tools server's in-process
	// durable stores, set only when the build runs INSIDE that server (its own
	// world_build tool). nil for the console executor, which opens its own copy
	// of each and restarts the serve process to reload them.
	AgentRegistry *agenttools.Registry
	FactsStore    *agenttools.FactsStore

	// AgentIdentityDir is where agent identity dirs live (the durable
	// agent-tools state dir, `<root>/agent-tools`), so the console executor and
	// the agent-tools server mint into the SAME dir and NEVER re-mint a
	// surviving identity. Empty = StateDir (the agent-tools server sets both to
	// its own state dir).
	AgentIdentityDir string

	// DepartmentRunners maps a department name → the capability runners its
	// pod may exec through (one per capability its role holds — the grant
	// unit is the runner), populated by stageDepartmentRunners each build.
	// Read by BuildCreateAgentFn to wire the pod's FREEHOLD_RUNNER_* env, and
	// by the agent reconcile to grant the department's pubkey onto each.
	DepartmentRunners map[string][]agent.RunnerCoords

	// OwnerSecret is the OWNER key's Nostr secret — the key whose pubkey is
	// OwnerPub (the relay's `owner`-role member: the operator identity the
	// world was installed under). It attests each agent's memory plane,
	// minting its BUZZ_AUTH_TAG, and it must derive OwnerPub (checked in
	// ownerKey) because the tag's owner and the pods' BUZZ_ACP_AGENT_OWNER are
	// one addressing scheme. Set by the composition root that holds that
	// identity; absent, the build's own read resolves it, and an
	// unresolvable key fails the create loudly — never a pod with a harness
	// and silently no writable memory.
	OwnerSecret []byte
}

// agentIdentityDir returns the agent-identity root (AgentIdentityDir or, when
// unset, StateDir).
func (s *Spec) agentIdentityDir() string {
	if s.AgentIdentityDir != "" {
		return s.AgentIdentityDir
	}
	return s.StateDir
}

func (s *Spec) client() (*client.McpClient, error) {
	auth := &client.AgentAuth{}
	copy(auth.Secret[:], s.Sec)
	auth.Pubkey = s.Audience
	return client.New(client.ConnectURL(s.RunnerAddr), auth, s.RunnerPK)
}

// execOut runs cmd through the co-located runner (target = the box) and
// returns its stdout. Secrets requested: the runner requires the SSH target's
// OWN credential among the requested secrets (it does not default to it then),
// so the target name is always first; extraSecrets add requested secrets by
// name (the runner injects each as an env var + redacts it).
func (s *Spec) execOut(cmd string, timeoutS uint64, extraSecrets ...string) (string, error) {
	mc, err := s.client()
	if err != nil {
		return "", err
	}
	secrets := append([]string{s.RunnerTarget}, extraSecrets...)
	out, err := mc.Exec(s.RunnerTarget, cmd, secrets, timeoutS)
	if err != nil {
		return "", err
	}
	if out.TimedOut || out.ExitCode == nil || *out.ExitCode != 0 {
		ec := -1
		if out.ExitCode != nil {
			ec = *out.ExitCode
		}
		return "", fmt.Errorf("runner exec failed (timed=%v exit=%d): %s %s", out.TimedOut, ec, strings.TrimSpace(out.Stdout), strings.TrimSpace(out.Stderr))
	}
	return out.Stdout, nil
}

func (s *Spec) runOut(cmd string, timeoutS uint64) (string, error) {
	return s.execOut(cmd, timeoutS)
}

func (s *Spec) run(cmd string, timeoutS uint64) error {
	_, err := s.execOut(cmd, timeoutS)
	return err
}

// runSecrets runs cmd through the co-located runner requesting extra secret
// names by name (e.g. the litellm master + provider key the register curl
// reads from $LITELLM / $PROVIDER_KEY).
func (s *Spec) runSecrets(cmd string, timeoutS uint64, extra ...string) error {
	_, err := s.execOut(cmd, timeoutS, extra...)
	return err
}

// cpGuestDirs derives the deployed control-plane's bin + state dirs inside the
// cp LXC from the agent-tools state dir (<root>/agent-tools -> <root>/bin +
// <root>/control-plane, the cpGuestDirs layout deploy-cp uses).
func (s *Spec) cpGuestDirs() (binDir, stateDir string) {
	root := filepath.Dir(s.StateDir)
	return filepath.Join(root, "bin"), filepath.Join(root, "control-plane")
}

// guestSearchBase reads the `search` line from the CP LXC's resolv.conf (PVE
// writes the same search domain to every guest it manages). Empty when absent.
func (s *Spec) guestSearchBase() string {
	out, err := s.runOut(fmt.Sprintf("pct exec %d -- sh -c \"grep '^search' /etc/resolv.conf | head -1 | cut -d' ' -f2-\"", s.CpLxc), 30)
	if err != nil {
		return ""
	}
	base := strings.TrimSpace(out)
	if base == "" || strings.ContainsAny(base, " \"'`$;(){}") || !strings.Contains(base, ".") {
		return ""
	}
	return base
}

// guestNameserver returns the CP LXC's dnsmasq UPSTREAM (the router), tried in
// order: the PVE-owned net0 gw= (static guests only), then the default route,
// then the resolv.conf nameserver that isn't the resolver's own IP.
func (s *Spec) guestNameserver() string {
	if out, err := s.runOut(fmt.Sprintf("pct config %d", s.CpLxc), 30); err == nil {
		if gw := proxmox.ParsePctGateway(out); gw != "" {
			return gw
		}
	}
	if out, err := s.runOut(fmt.Sprintf("pct exec %d -- sh -c \"ip route show default | head -1 | cut -d' ' -f3\"", s.CpLxc), 30); err == nil {
		if ns := strings.TrimSpace(out); ns != "" && strings.ContainsAny(ns, "0123456789") {
			return ns
		}
	}
	if out, err := s.runOut(fmt.Sprintf("pct exec %d -- sh -c \"grep '^nameserver' /etc/resolv.conf | cut -d' ' -f2\"", s.CpLxc), 30); err == nil {
		for _, l := range strings.Split(out, "\n") {
			ns := strings.TrimSpace(l)
			if ns != "" && ns != s.CpIP && strings.ContainsAny(ns, "0123456789") {
				return ns
			}
		}
	}
	return ""
}

// worldDNS registers the CP resolver's explicit records and points every guest
// at it (the stageDnsRegister + stageDnsPoint pair, CP-side): upsert the
// split-horizon names via `control-plane dns add` inside the CP, pct-set the
// guests' nameserver, rewrite their resolv.conf now (pct regenerates it only at
// the next boot), and prove the resolver ANSWERS a record from its own loopback.
func (s *Spec) worldDNS() error {
	binDir, stateDir := s.cpGuestDirs()
	searchBase := s.guestSearchBase()
	// Behind the gateway the BARE guest records (relay/cp -> their internal
	// IPs) must NOT exist: dnsmasq's bare-record match shadows the FQDN's
	// edge answer (the wildcard), so every wss/https dial to the public hosts
	// lands on the guest's :443 — where nothing listens — and the pods die on
	// "connection refused". The LAN dials ride the gateway's forwards instead
	// (3000/8080 DNAT), which serve BOTH resolver answers.
	relayIP, cpIP := s.RelayIP, s.CpIP
	if s.GatewayCIDR != "" {
		relayIP, cpIP = "", ""
	}
	for _, r := range stages.DnsRecords(s.RelayHost, relayIP, s.CpHost, cpIP, s.ProxyIP, s.LitellmIP) {
		if err := s.run(proxmox.DnsAddCmd(s.CpLxc, binDir, stateDir, r.Name, r.IP, r.Source, searchBase), 120); err != nil {
			return fmt.Errorf("world-build dns register %s: %w", r.Name, err)
		}
	}
	if s.GatewayCIDR != "" {
		// The prior world's bare records are already in the resolver's store —
		// the add upsert never removes. Drop them explicitly.
		for _, name := range []string{"relay", "cp"} {
			if err := s.run(proxmox.DnsRemoveCmd(s.CpLxc, binDir, stateDir, name), 120); err != nil {
				return fmt.Errorf("world-build dns drop bare %s: %w", name, err)
			}
		}
	}
	// The resolver WILDCARD: all *.apex -> the proxy (Caddy) edge, so the
	// dotted public hosts (relay.<apex>, cp.<apex>) resolve to TLS - never to a
	// guest LXC (dnsmasq's bare `relay`/`cp` records would otherwise leak the
	// guest IP into the FQDN answer). The apex is the guest search base when
	// present, else derived from the relay host (strip its leading label).
	apex := searchBase
	if apex == "" {
		if i := strings.Index(s.RelayHost, "."); i > 0 && i < len(s.RelayHost)-1 {
			apex = s.RelayHost[i+1:]
		}
	}
	if apex != "" && s.ProxyIP != "" {
		if err := s.run(proxmox.DnsApexCmd(s.CpLxc, binDir, stateDir, apex, config.StripCIDR(s.ProxyIP)), 120); err != nil {
			return fmt.Errorf("world-build dns apex: %w", err)
		}
	}
	if err := s.pointGuestsAtResolver(); err != nil {
		return err
	}
	// Behind the gateway the bare relay record is DROPPED (it shadows the
	// FQDN's edge answer) — the verify then checks the FQDN -> the EDGE; the
	// bare-name check stands for flat-LAN worlds.
	relayWant := s.RelayIP
	relayName := "relay"
	if s.GatewayCIDR != "" {
		relayName, relayWant = s.RelayHost, s.ProxyIP
	}
	for _, q := range []struct{ name, want string }{
		{relayName, relayWant}, {"litellm", s.LitellmIP},
	} {
		if q.want == "" {
			continue
		}
		if err := s.run(proxmox.DnsVerifyCmd(s.CpLxc, q.name, q.want), 30); err != nil {
			return fmt.Errorf("world-build dns verify %s: %w", q.name, err)
		}
	}
	return nil
}

// pointGuestsAtResolver pct-sets each guest's nameserver to the CP resolver and
// rewrites its resolv.conf now (pct only regenerates it at the next boot). It is
// called by worldDNS, which the build runs EARLY — before the terraform services
// phase — because the litellm/caddy image pulls need working DNS, and a freshly
// booted guest otherwise sits on DHCP/public resolvers that intermittently fail
// containerd's lookups (EAI_AGAIN). The resolver must already be installed by the
// time this points guests at it (worldDNS registers the records first, which
// installs and reloads dnsmasq).
func (s *Spec) pointGuestsAtResolver() error {
	searchBase := s.guestSearchBase()
	router := s.guestNameserver()
	for _, role := range []struct {
		name string
		vmid uint32
	}{
		{"relay", s.RelayLxc}, {"cp", s.CpLxc}, {"k3s", s.K3sVmid},
	} {
		if role.vmid == 0 {
			continue
		}
		r := ""
		if role.name == "cp" {
			r = router
		}
		pctSet, resolvConf := proxmox.DnsPointCmd(role.vmid, s.CpIP, r, searchBase)
		if err := s.run(pctSet, 60); err != nil {
			return fmt.Errorf("dns point %s: %w", role.name, err)
		}
		if err := s.run(resolvConf, 60); err != nil {
			return fmt.Errorf("dns point %s resolv.conf: %w", role.name, err)
		}
	}
	return nil
}

// worldServices records the deployed world's health-monitored service coords
// (k3s / litellm / caddy) into the CP's services registry, via the Go console
// subcommand (directly into state.json, mirroring how `dns add` writes DNS).
// The console then probes them co-located and serves them on /api/world so a
// logging-in management box renders the live world. Idempotent upsert.
func (s *Spec) worldServices() error {
	binDir, stateDir := s.cpGuestDirs()
	type svc struct{ kind, url string }
	var svcs []svc
	if s.ProxyIP != "" {
		svcs = append(svcs, svc{"k3s", fmt.Sprintf("https://%s:6443", s.ProxyIP)})
	}
	if s.LitellmIP != "" {
		svcs = append(svcs, svc{"litellm", fmt.Sprintf("http://%s:31400/health/liveliness", s.LitellmIP)})
	}
	if s.CpHost != "" {
		svcs = append(svcs, svc{"caddy", fmt.Sprintf("https://%s", s.CpHost)})
	}
	for _, v := range svcs {
		cmd := fmt.Sprintf("pct exec %d -- %s/freehold-console services --state-dir %s --kind %s --url %s",
			s.CpLxc, binDir, stateDir, v.kind, v.url)
		if err := s.run(cmd, 60); err != nil {
			return fmt.Errorf("world-build services register %s: %w", v.kind, err)
		}
	}
	return nil
}

// worldStorage re-ensures the durable volume plane CP-side (the box's
// stagePlacement + stageStorage ensure half): resolve the backend kind
// (recorded --plane-kind, else detect like the box's parseKind), then ensure
// each tenant's dataset/LV onto the recorded pool and chown it guest-writable
// — idempotent, no box-side secret (the storage ops run on the PVE host
// through the co-located runner, exactly as the box drives them). Returns the
// ensured mounts per tenant (the born-at-create mounts the LXC boots bake).
func (s *Spec) worldStorage() (map[planebase.Tenant][]planebase.MountSpec, error) {
	mc, err := s.client()
	if err != nil {
		return nil, err
	}
	kind := planebase.BackendKind(s.PlaneKind)
	if kind == "" {
		// No recorded kind: classify the host's storage read-only and take
		// the recommended SAFE backend (or the recorded PlanePool). This
		// replaces the old blind `vgs[0]` guess, which on a multi-VG host
		// could land the plane on a busy pool.
		inv, err := proxmox.StorageInventory(proxmox.ClientExec(mc, s.RunnerTarget))
		if err != nil {
			return nil, fmt.Errorf("storage inventory: %w", err)
		}
		opts := planebase.BuildOptions(inv, s.RelayHost)
		var chosen planebase.Option
		ok := false
		if s.PlanePool != "" {
			chosen, ok = planebase.FindOption(opts, s.PlanePool)
			if !ok || chosen.Kind == planebase.KindBlocked {
				return nil, fmt.Errorf("recorded plane pool %q on %s is not usable — clear it or pick another", s.PlanePool, s.RunnerTarget)
			}
		} else if i := planebase.Recommend(opts); i >= 0 {
			chosen, ok = opts[i], true
		}
		if !ok {
			return nil, fmt.Errorf("no safe storage backend found on %s — record a plane pool explicitly", s.RunnerTarget)
		}
		// This non-interactive path cannot collect the typed share consent the
		// box-side engine requires, so it refuses a shared (Caution) backend
		// rather than silently selecting one. BuildOptions is domain-scoped, so
		// another world's freehold data is ordinary reuse (Caution when it
		// shares a thin pool), never a mis-detected reconnect.
		if chosen.Safety == planebase.Caution {
			return nil, fmt.Errorf("storage %q on %s already shares capacity with live volumes — this automatic path will not select it; record the plane pool explicitly after confirming", chosen.Backend, s.RunnerTarget)
		}
		if s.PlanePool == "" {
			s.PlanePool = chosen.Backend
		}
		switch chosen.Kind {
		case planebase.KindReuseZpool:
			kind = planebase.KindZfs
		case planebase.KindReuseVG:
			kind = planebase.KindLvmThin
		default:
			return nil, fmt.Errorf("recorded plane option %q is not a ready backend (creating one is a later phase)", s.PlanePool)
		}
	}
	mounts := map[planebase.Tenant][]planebase.MountSpec{}
	for _, tenant := range []planebase.Tenant{planebase.TenantRelay, planebase.TenantCp, planebase.TenantK3sVolumes} {
		var ms []planebase.MountSpec
		switch kind {
		case planebase.KindZfs:
			ms, err = drive.ResolveTenantMounts(drive.ClientExec(mc, s.RunnerTarget), s.PlanePool, s.RelayHost, tenant)
		case planebase.KindLvmThin:
			ms, err = drive.ResolveLvmMounts(drive.ClientExec(mc, s.RunnerTarget), s.PlanePool, s.RelayHost, tenant, s.SizeGB, s.PoolSizeGB, s.ThinPool)
		default:
			return nil, fmt.Errorf("unknown storage backend kind %q (zfs|lvmth)", kind)
		}
		if err != nil {
			return nil, fmt.Errorf("storage ensure %s: %w", tenant, err)
		}
		mounts[tenant] = ms
	}
	return mounts, nil
}

// bootLxc boots (or reuses) a role's LXC via the shared bootstrap driver
// through the co-located runner, baking the durable-plane mounts at create.
// role's static address (k3s = the proxy IP; relay/cp are DHCP behind the
// proxy) rides the spec.
func (s *Spec) bootLxc(role string, vmid uint32, mounts []planebase.MountSpec) (uint32, error) {
	hostname, err := bootstrap.LXCName(s.Name, s.RelayHost, role)
	if err != nil {
		return 0, err
	}
	spec := &proxmox.ProxmoxLxcSpec{
		Hostname: hostname,
		Storage:  s.StorageName,
		RootfsGB: s.RootfsGB,
		MemoryMB: s.MemoryMB,
		Bridge:   s.Bridge,
		Mounts:   mounts,
	}
	// Fail LOUD on blank size/placement: a world-config without them (a
	// world installed before persistence shipped) would otherwise reach the
	// substrate tool as "memory 0"/"bridge=" and fail with a cryptic usage
	// error. The operator's values live in the world's config; name the fix.
	var missing []string
	if spec.MemoryMB < 16 {
		missing = append(missing, "memory_mb")
	}
	if spec.RootfsGB < 4 {
		missing = append(missing, "rootfs_gb")
	}
	if spec.Bridge == "" {
		missing = append(missing, "bridge")
	}
	if spec.Storage == "" {
		missing = append(missing, "storage")
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("the world's config carries no %s — guest creation cannot proceed; add them under [plane] in the world's config and re-run the build (worlds installed after persistence ship them)", strings.Join(missing, "/"))
	}
	if vmid != 0 {
		spec.VMID = &vmid
	}
	roleIP := ""
	switch role {
	case "k3s":
		roleIP = s.k3sIP()
	case "relay":
		roleIP = s.RelayIP
	case "cp":
		roleIP = s.CpIP
	}
	if roleIP != "" {
		// pct net0 wants CIDR (host/prefix); the serve/recorded values carry the
		// bare IP (the DNS/caddy consumers expect bare), so rebuild the CIDR —
		// the LAN defaults to /24 home-labs, the internal subnet to its OWN
		// mask. Assigning relay/CP static addresses (via --relay-ip/--cp-ip)
		// runs them OFF DHCP, which avoids exhausting a small LAN DHCP pool
		// across repeated teardown/build cycles.
		ip := roleIP
		gw := s.RelayGW
		if s.GatewayCIDR != "" {
			// Behind the gateway the guests' default route is the gateway's
			// INTERNAL address, and eth0 rides the internal subnet's tag —
			// the "born on the freehold-subnet" rule (docs/NETWORK.md).
			gw = config.GatewayInternalIP(s.GatewayCIDR)
			if s.GatewayVlan > 0 {
				spec.Tag = &s.GatewayVlan
			}
			if !strings.Contains(ip, "/") {
				ip += "/" + strconv.Itoa(maskBits(s.GatewayCIDR))
			}
		} else if !strings.Contains(ip, "/") {
			ip += "/24"
		}
		spec.NetIP = &ip
		spec.NetGW = &gw
	}
	mc, err := s.client()
	if err != nil {
		return 0, err
	}
	res, err := proxmox.BootstrapProxmoxLxc(proxmox.ClientExec(mc, s.RunnerTarget), spec)
	if err != nil {
		return 0, fmt.Errorf("boot %s LXC: %w", role, err)
	}
	if res.ID != "" {
		if v, perr := strconv.ParseUint(res.ID, 10, 32); perr == nil {
			return uint32(v), nil
		}
	}
	if vmid != 0 {
		return vmid, nil
	}
	return 0, fmt.Errorf("boot %s LXC: no vmid resolved", role)
}

// resolveGuestVmids fills any UNKNOWN guest vmid (0 — a fresh world whose
// coords were cleared at teardown) by looking up the deterministic hostname
// (LXCName) on the host, so the DNS/caddy/litellm/cert steps address the REAL
// vmids world_build just booted. An invalid world name is a config error and
// fails the build; a transient `pct list` failure stays best-effort (the
// recorded vmid, if any, is used).
// k3sIP is the k3s node's address: the recorded internal one behind a
// gateway, else the proxy IP (the legacy flat-LAN world, where the k3s node
// IS the proxy).
func (s *Spec) k3sIP() string {
	if s.K3sIP != "" {
		return s.K3sIP
	}
	return s.ProxyIP
}

// k3sGW is the k3s node's default route: the gateway's internal address
// behind a gateway, else the LAN gateway the world was installed with.
func (s *Spec) k3sGW() string {
	if s.GatewayCIDR != "" {
		return config.GatewayInternalIP(s.GatewayCIDR)
	}
	return s.RelayGW
}

// maskBits returns the prefix length of a CIDR (24 for 10.77.0.0/24), 0 when
// it does not parse.
func maskBits(cidr string) int {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return 0
	}
	return p.Bits()
}

func (s *Spec) resolveGuestVmids() error {
	for _, r := range []struct {
		role string
		vmid *uint32
	}{
		{"relay", &s.RelayLxc}, {"cp", &s.CpLxc}, {"k3s", &s.K3sVmid}, {"gateway", &s.GatewayLxc},
	} {
		if *r.vmid != 0 {
			continue
		}
		name, err := bootstrap.LXCName(s.Name, s.RelayHost, r.role)
		if err != nil {
			return err
		}
		out, err := s.runOut("pct list", 30)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(out, "\n")[1:] {
			cols := strings.Fields(l)
			if len(cols) >= 2 && cols[len(cols)-1] == name {
				if v, perr := strconv.ParseUint(cols[0], 10, 32); perr == nil {
					*r.vmid = uint32(v)
				}
				break
			}
		}
	}
	return nil
}

// refreshGuestIPs re-reads the relay/cp/k3s guests' CURRENT IPv4 after a boot
// (a DHCP re-lease can change an address the recorded coords no longer match),
// updating the fields the DNS/caddy/litellm/cert steps consume. Best-effort.
func (s *Spec) refreshGuestIPs() {
	type role struct {
		vmid uint32
		ip   *string
	}
	for _, r := range []role{
		{s.RelayLxc, &s.RelayIP},
		{s.CpLxc, &s.CpIP},
		// The k3s node address lands in K3sIP: behind a gateway that is the
		// INTERNAL address (ProxyIP is the gateway edge; clobbering it would
		// repoint the DNS records at a guest).
		{s.K3sVmid, &s.K3sIP},
	} {
		if r.vmid == 0 {
			continue
		}
		out, err := s.runOut(fmt.Sprintf("pct exec %d -- ip -4 -o addr show eth0", r.vmid), 30)
		if err != nil {
			continue
		}
		for _, t := range strings.Fields(out) {
			if strings.Contains(t, "/") && t != "127.0.0.1/8" {
				// ip -4 -o addr reports CIDR; the downstream consumers
				// (DNS records, Caddy/litellm upstreams) expect a bare IP.
				*r.ip = config.StripCIDR(t)
				break
			}
		}
	}
	// litellm is a k3s NodePort served on the proxy (k3s node) IP. A fresh
	// world's console spec has no litellm_ip/base baked (litellm did not exist
	// at deploy-cp time), so derive both — otherwise the litellm step (and the
	// CPA pod's litellm-key Secret) is skipped, and the agent pods get an empty
	// OPENAI_COMPAT_BASE_URL.
	s.FillEdgeURLs()
}

// worldBootRelay boots the relay LXC (if missing) + deploys the Buzz stack
// into it via the shared deploy driver (idempotent compose bring-up).
func (s *Spec) worldBootRelay(mounts []planebase.MountSpec) error {
	vmid, err := s.bootLxc("relay", s.RelayLxc, mounts)
	if err != nil {
		return err
	}
	s.RelayLxc = vmid
	deployDir := ""
	for _, m := range mounts {
		if m.GuestPath != "/var/lib/docker" {
			deployDir = m.GuestPath
			break
		}
	}
	if deployDir == "" {
		deployDir = "/srv/data/relay"
	}
	mc, err := s.client()
	if err != nil {
		return err
	}
	host := s.RelayHost
	relayURL := "https://" + host
	if _, err := relaydeploy.DeployRelay(proxmox.GuestExecFunc(mc, s.RunnerTarget), &relaydeploy.RelayDeploySpec{
		RelayName:      "relay",
		DeployDir:      deployDir,
		HTTPPort:       3000,
		BuzzRef:        relaydeploy.DefaultBufRef,
		LXc:            &s.RelayLxc,
		OwnerPubkey:    s.OwnerPub,
		RelayURL:       relayURL,
		OperatorPubkey: s.OwnerPub,
		Domain:         &host,
	}); err != nil {
		return fmt.Errorf("deploy relay: %w", err)
	}
	return nil
}

// worldBootK3s boots the k3s LXC (if missing — the driver picks a free vmid on
// a fresh world) so spec.K3sVmid is recorded BEFORE the terraform substrate
// phase ADOPTS the LXC (lxc.sh is adopt-if-missing; a 0 vmid would `pct create
// 0` and fail). The k3s INSTALL + the durable local-path carve-out are OWNED by
// the terraform module's k3s-bringup.sh.
func (s *Spec) worldBootK3s(mounts []planebase.MountSpec) error {
	vmid, err := s.bootLxc("k3s", s.K3sVmid, mounts)
	if err != nil {
		return err
	}
	s.K3sVmid = vmid
	return nil
}

// worldGateway (re)writes the gateway's nftables ruleset — a pure
// function of the recorded coords, so every build re-asserts it (a rebuilt
// k3s with a new internal IP must not leave a stale DNAT). The gateway is
// created by the BOX install (before the CP, whose default route it
// becomes); the CP build only owns the config. No-op without one. On a
// hosted gateway (api-vultr) the assert rides the host itself — same script,
// no pct wrapper.
func (s *Spec) worldGateway() error {
	if s.GatewayCIDR == "" || (s.GatewayLxc == 0 && !s.HostedGateway) {
		return nil
	}
	kip := s.k3sIP()
	if kip == "" {
		return nil
	}
	// Forwards: 80/443 (tcp+udp — Caddy serves h3) and 6443 (kubectl from the
	// LAN) DNAT to the k3s node's INTERNAL address; 8080 to the CP console
	// (the box's pre-Caddy build path — NIP-98-gated). NOT the gateway's own
	// LAN IP (self-DNAT: the gateway's INPUT chain listens on nothing, and
	// the world's public surface dies). Masquerade carries the subnet out.
	// The ruleset is rewritten whole — never merged.
	bridge := s.Bridge
	if bridge == "" {
		bridge = "vmbr0"
	}
	var conf, dns string
	if s.HostedGateway {
		conf = config.HostGatewayNftConf(s.GatewayCIDR, s.ProxyIP, kip, s.CpIP, s.RelayIP, bridge)
		dns = config.HostGatewayDnsmasqConf(bridge)
	} else {
		conf = config.GatewayNftConf(s.GatewayCIDR, s.ProxyIP, kip, s.CpIP, s.RelayIP, "eth0")
		dns = config.GatewayDnsmasqConf(s.RelayGW)
	}
	// No single quotes in the script (the heredoc delimiter is unquoted; the
	// ruleset has none) — it rides `sh -c '...'` through the runner verbatim.
	script := fmt.Sprintf(`set -e
apt-get install -y -qq nftables dnsmasq >/dev/null 2>&1 || true
sysctl net.ipv4.ip_forward=1 >/dev/null
cat > /etc/nftables.conf <<NFT
%sNFT
cat > /etc/dnsmasq.d/freehold.conf <<DNS
%sDNS
systemctl enable nftables dnsmasq >/dev/null 2>&1 || true
systemctl restart nftables dnsmasq >/dev/null 2>&1 || nft -f /etc/nftables.conf
`, conf, dns)
	var cmd string
	if s.HostedGateway {
		cmd = script
	} else {
		cmd = fmt.Sprintf("pct exec %d -- sh -c '%s'", s.GatewayLxc, script)
	}
	if err := s.run(cmd, 300); err != nil {
		return fmt.Errorf("gateway nftables: %w", err)
	}
	return nil
}

// deployAgentTools ships + seeds + launches the CP's freehold-agent-tools
// server IN the cp guest, so the console's world_build can bring up the
// operator toolset itself (the robin-relay comes up first; the roster seed
// needs it). The binary is already in the guest (bootstrap's deploy-cp ships
// it); the server mints a durable identity, seeds its roster channel with the
// operator + this console identity, is granted on the co-located runner, and
// serve is launched. Idempotent (reused across reconciles).
func (s *Spec) deployAgentTools() error {
	binDir, stateDir := s.cpGuestDirs()
	root := filepath.Dir(s.StateDir)
	atState := filepath.Join(root, "agent-tools")
	bin := binDir + "/freehold-agent-tools"

	if err := s.run(fmt.Sprintf("pct exec %d -- sh -c 'mkdir -p %s && chmod 700 %s'", s.CpLxc, atState, atState), 30); err != nil {
		return fmt.Errorf("agent-tools mkdir state: %w", err)
	}
	if err := s.run(fmt.Sprintf("pct exec %d -- sh -c 'p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" >/dev/null 2>&1; rm -f %s/serve.pid; true'", s.CpLxc, atState, atState), 30); err != nil {
		return fmt.Errorf("agent-tools stop prior: %w", err)
	}
	out, err := s.runOut(fmt.Sprintf("pct exec %d -- %s identity --state-dir %s", s.CpLxc, bin, atState), 30)
	if err != nil {
		return fmt.Errorf("agent-tools identity: %w", err)
	}
	pubkey := strings.TrimSpace(out)
	if len(pubkey) != 64 {
		return fmt.Errorf("agent-tools identity readback not 64-hex: %q", pubkey)
	}
	if s.RelayLxc != 0 {
		cmdLine := fmt.Sprintf("cd %s && docker compose exec -T relay buzz-admin add-member --pubkey %s", s.RelayCompose, pubkey)
		if err := s.run(fmt.Sprintf("pct exec %d -- sh -c '%s'", s.RelayLxc, cmdLine), 120); err != nil {
			return fmt.Errorf("relay member agent-tools: %w", err)
		}
	}
	// The relay host must DIAL via the LAN URL (http://<relayHost>:3000, pinned
	// into the cp guest's /etc/hosts so it reaches the just-booted relay before
	// the Caddy edge exists) while the NIP-98 signature uses the PUBLIC URL.
	if err := s.pinRelayHost(); err != nil {
		return err
	}
	relayDial := config.RelayLanDial(s.RelayHost)
	// Seed the server's channel. The roster is the AGENT surface: the CPA is
	// membered by this server's own identity (BuildCreateAgentFn). The
	// console's driving identity (s.Audience) was deliberately never seeded —
	// the console never calls this MCP (it reads the registry/facts files
	// directly, and its world_migrate trigger is a signed local peer). The
	// OPERATOR is likewise a peer (--owner-pubkey, full operator scope) and is
	// REVOKED from the channel here: its CLI world verbs live on console
	// routes, and its signature authenticates via the peer rule regardless of
	// membership — a stale CLI's migration sweep must survive this flip.
	seedFlags := fmt.Sprintf("%s seed --state-dir %s --relay-url %s --name agent-tools",
		bin, atState, relayDial)
	// Heal any prior membership (put-user is additive; only --revoke drops).
	if s.OwnerPub != "" {
		seedFlags += " --revoke " + s.OwnerPub
	}
	// Revoke the console's driving identity if a PRIOR seed membered it: put-user
	// is additive, so dropping it from --granted alone doesn't heal a world that
	// already has it. The console never calls this MCP.
	if s.Audience != "" {
		seedFlags += " --revoke " + s.Audience
	}
	if s.RelayAuthURL != "" {
		seedFlags += " --relay-auth-url " + s.RelayAuthURL
	} else {
		seedFlags += " --relay-auth-url " + s.RelayURL
	}
	if err := s.run(fmt.Sprintf("pct exec %d -- %s", s.CpLxc, seedFlags), 60); err != nil {
		return fmt.Errorf("seed agent-tools roster: %w", err)
	}
	// Grant the server on the co-located runner (it applies agent pods).
	if s.RunnerTarget != "" {
		grant := fmt.Sprintf("%s/freehold-console grant %s --state-dir %s --pubkey %s",
			binDir, s.RunnerTarget, stateDir, pubkey)
		if err := s.run(fmt.Sprintf("pct exec %d -- %s", s.CpLxc, grant), 60); err != nil {
			return fmt.Errorf("grant agent-tools on runner: %w", err)
		}
	}
	return s.startAgentTools()
}

// ---- the edge cert on the CP (the box's F3 start/await pair, CP-side) ------
//
// Durable-reuse gate FIRST (read-only on the node, no LE order when a valid
// cert survives the durable mirror at /srv/data/k8s-volumes/caddy-edge/<slot>):
// a teardown+rebuild comes back on the same cert with no challenge and no
// rate-limit exposure — the PVC is seeded from the mirror. Only when no valid
// cert exists does issuance run: lego IN-PROCESS (the DNS-01 provider cred is
// sealed to the agent-tools identity under <stateDir>/world-secrets, opened in
// memory, resumable), the private key is sealed into the co-located runner
// package (which the box's own issuance already ships), and the install runs
// through the runner with the key requested BY NAME.

// durableFullchain reads a slot's fullchain from the durable-plane mirror
// (/srv/data/k8s-volumes/caddy-edge/<slot>) — a plain node file, readable
// BEFORE the Caddy edge exists (the cold-rebuild recovery gate).
func (s *Spec) durableFullchain(k3sVmid uint32, slot string) ([]byte, error) {
	out, err := s.runOut(fmt.Sprintf("pct exec %d -- bash -c 'test -s %s/fullchain.pem 2>/dev/null && base64 -w0 < %s/fullchain.pem'",
		k3sVmid, stages.CaddyEdgeDurableDir(slot), stages.CaddyEdgeDurableDir(slot)), 60)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, fmt.Errorf("no durable mirror for %s", slot)
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(out))
}

// durableKeyPresent reports whether the durable mirror also holds a non-empty
// key.pem (a fullchain alone is not enough to serve TLS).
func (s *Spec) durableKeyPresent(k3sVmid uint32, slot string) bool {
	return s.run(fmt.Sprintf("pct exec %d -- bash -c 'test -s %s/key.pem'", k3sVmid, stages.CaddyEdgeDurableDir(slot)), 60) == nil
}

// seedCaddyCertFromDurable seeds a slot's Caddy PVC backing dir from the
// durable-plane mirror and restarts the edge. No private key transits a
// command or the audit — both files come from the node's own durable volume.
func (s *Spec) seedCaddyCertFromDurable(k3sVmid uint32, slot string) error {
	cmd := fmt.Sprintf(`set -e
pct exec %d -- bash -c '
set -e
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
PV=$($K get pvc caddy-data -n caddy -o jsonpath={.spec.volumeName})
PDIR=$($K get pv $PV -o jsonpath={.spec.local.path})
DIR=$PDIR/tls/%s
SRC=%s
mkdir -p "$DIR"
cp "$SRC/fullchain.pem" "$DIR/fullchain.pem"
cp "$SRC/key.pem" "$DIR/key.pem"
chmod 600 "$DIR/key.pem"
$K -n caddy rollout restart deploy/caddy >/dev/null 2>&1 || true
echo DURABLE_SEED_OK
'`, k3sVmid, slot, stages.CaddyEdgeDurableDir(slot))
	return s.run(cmd, 120)
}

// dnsCredFromStore opens the operator's DNS-01 provider credential for a slot
// from the CP's durable sealed store (<stateDir>/world-secrets/dns-<slot>.json)
// via the established cert.LoadCreds record (sealed to the agent-tools
// identity — written by the box build's hand-off). Errors loudly when no copy
// has been handed off yet (the ISSUE path needs it; the durable-reuse path
// does not).
func (s *Spec) dnsCredFromStore(slot string) (string, map[string]string, error) {
	path := filepath.Join(s.StateDir, "world-secrets", "dns-"+slot+".json")
	if !cert.CredExists(path) {
		return "", nil, fmt.Errorf("no DNS provider credential on the CP at %s — run `freehold build` to hand it off (or the durable-reuse path serves an existing cert)", path)
	}
	secret, err := s.consoleEncSecret()
	if err != nil {
		return "", nil, err
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	return cert.LoadCreds(path, open, secret)
}

// certSeedFromCache installs a slot's edge cert from the box-shipped seed
// (world-secrets/cert-seed-<slot>.json — the box cert cache, sealed to the
// console identity like the DNS creds). It is the FRESH lifecycle's escape
// from the LE order loop: the full-destroy uninstall wipes the plane (and the
// durable mirror with it), so the operator box carries the issued cert across
// worlds instead. Absent/stale/wrong-host seeds report (false, nil) — the
// caller falls through to the normal issue path; only a failed INSTALL errors
// (the same failure the issue path would hit).
func (s *Spec) certSeedFromCache(k3sVmid uint32, slot, host string) (bool, error) {
	path := filepath.Join(s.StateDir, "world-secrets", "cert-seed-"+slot+".json")
	if !cert.CredExists(path) {
		return false, nil
	}
	secret, err := s.consoleEncSecret()
	if err != nil {
		return false, err
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	fullchain, key, err := cert.LoadSeed(path, open, secret, host, time.Now(), 30*24*time.Hour)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cert %s: seed cache unusable (%v) — issuing\n", slot, err)
		return false, nil
	}
	if err := s.installCaddyCertFile(k3sVmid, slot, fullchain, key); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stderr, "cert %s: seeded from the box cert cache (no LE order)\n", slot)
	return true, nil
}

// runnerLitellmSecrets maps the CP store's litellm env keys to the secret NAMES
// the world-build requests from the co-located runner.
var runnerLitellmSecrets = []struct{ name, env string }{
	{"litellm", "master"},
	{"postgres-pw", "pg"},
	{"provider-key", "provider"},
}

// reseedCoLocatedRunner re-provisions the CP's co-located runner from the CP's
// own durable litellm store. deploy-cp re-ships the box runner package on every
// install, so a package created before the litellm secrets (or wiped by a prior
// deploy) lacks them; the box cannot re-derive them (the CP is the durable
// owner) and the runner reads its package only at boot. Re-seal from the store
// and restart. A no-op when the runner already holds every name, or when the
// store has no litellm secret yet (the first build seeds store + runner
// together, box-side).
func (s *Spec) reseedCoLocatedRunner() error {
	store, err := state.Open(s.StateDir)
	if err != nil {
		return fmt.Errorf("open CP state: %w", err)
	}
	rec, ok := store.GetRunner(s.RunnerTarget)
	if !ok {
		return fmt.Errorf("co-located runner %q is not adopted", s.RunnerTarget)
	}
	pkg, err := wire.Load(rec.PackageDir)
	if err != nil {
		return fmt.Errorf("load runner package: %w", err)
	}
	need := false
	for _, m := range runnerLitellmSecrets {
		if _, ok := pkg.Secrets[m.name]; !ok {
			need = true
		}
	}
	if !need {
		return nil
	}
	path := filepath.Join(s.StateDir, "world-secrets", "litellm.json")
	if !cert.CredExists(path) {
		return nil // first build: the box seeds the store + runner together
	}
	secret, err := s.consoleEncSecret()
	if err != nil {
		return err
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	_, env, err := cert.LoadCreds(path, open, secret)
	if err != nil {
		return fmt.Errorf("open CP litellm store: %w", err)
	}
	for _, m := range runnerLitellmSecrets {
		v := env[m.env]
		if v == "" {
			return fmt.Errorf("CP litellm store is missing the %q value", m.env)
		}
		if _, err := provisioner.AddSecret(store, s.RunnerTarget, m.name, []byte(v)); err != nil {
			return fmt.Errorf("re-seed co-located runner %s: %w", m.name, err)
		}
	}
	// The runner loads its package at boot (in-memory keyring), so restart it.
	// systemctl is LOCAL to the console's own CP guest — the runner cannot
	// restart itself through the exec channel it is serving.
	if out, err := exec.Command("systemctl", "restart", "freehold-runner").CombinedOutput(); err != nil {
		return fmt.Errorf("restart co-located runner: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// `systemctl restart` returns before the runner has re-bound its port; wait
	// so the next exec doesn't race a refused connection.
	addr := s.RunnerAddr
	if addr == "" {
		addr = config.CoLocatedRunnerMCPAddr
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
		if derr == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("co-located runner did not re-listen on %s after restart: %v", addr, derr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// consoleEncSecret returns the CP's encryption secret — the CONSOLE identity
// (nested at <StateDir>/console/identity.json), the executor's own keypair, to
// which the box seals the DNS/world secrets (handoffDNS) it opens in-memory for
// cert issuance. The console's world_build owns this path (not agent-tools).
func (s *Spec) consoleEncSecret() ([]byte, error) {
	return s.consoleEncSecretAt(s.StateDir)
}

// consoleEncSecretAt is the consoleEncSecret read at an explicit dir — the
// CONSOLE-ROOTED form the owner-key path needs: the sealed operator record
// lives at the console root too, and on the agent-tools serve (StateDir = the
// agent-tools root) no console identity exists to unseal it with.
func (s *Spec) consoleEncSecretAt(dir string) ([]byte, error) {
	file := filepath.Join(dir, "console", "identity.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var id struct {
		EncSecretHex string `json:"enc_secret_hex"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, err
	}
	return hex.DecodeString(id.EncSecretHex)
}

// openCertOrder opens (reusing or freshly placing) one slot's resumable DNS-01
// order, PLACING its challenge TXT but NOT waiting for propagation — it returns
// once the record is on the wire. worldCert opens EVERY slot's order this way
// first, so the usually-slow DNS-01 propagation of all hosts progresses in
// parallel, then waits + resolves them all.
func (s *Spec) openCertOrder(slot, host, provider string, env map[string]string) (*cert.Resume, *cert.PendingOrder, string, error) {
	secret, err := s.consoleEncSecret()
	if err != nil {
		return nil, nil, "", err
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return nil, nil, "", err
	}
	dp, err := cert.NewDNSProvider(provider, env)
	if err != nil {
		return nil, nil, "", err
	}
	statePath := filepath.Join(s.StateDir, "world-secrets", "cert-pending-"+slot+".json")
	resume := &cert.Resume{
		Domain:   host,
		Provider: dp,
		Seal:     func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) },
		Open:     func(secret, aad, blob []byte) ([]byte, error) { return crypto.Open(secret, aad, blob) },
		SealPub:  pub,
		OpenSec:  secret,
		Path:     statePath,
	}
	po, ok, err := resume.TryLoad()
	if err != nil {
		return nil, nil, "", err
	}
	if !ok {
		// No resumable order: we're about to create a NEW ACME order + place a
		// fresh challenge TXT. Purge any leftover _acme-challenge records for
		// this host first (API-based, so it works even if the record hasn't
		// propagated yet) — otherwise we STACK another value onto the same name
		// every issuance, which is the redundant-record → ACME order/rate-limit
		// bloat surfaced on librem / relay.migrate.
		if n, perr := cert.PurgeChallengeRecords(host, provider, env); perr != nil {
			return nil, nil, "", fmt.Errorf("cert %s purge challenge: %w", slot, perr)
		} else if n > 0 {
			fmt.Fprintf(os.Stderr, "cert %s: purged %d stale challenge record(s) before issue\n", slot, n)
		}
		po, err = resume.Begin()
		if err != nil {
			return nil, nil, "", err
		}
	}
	return resume, po, statePath, nil
}

// installCaddyCertFile writes a slot's FRESH issued fullchain + key into the
// caddy-data PVC /data/tls/<slot> AND the durable mirror, then rolls caddy.
// The key is file-transited (sftp upload + pct push) — NEVER the runner's
// sealed cert-key-<slot>, which would be a STALE key mismatching the fresh
// fullchain (and the serving co-located runner cannot be restarted mid-call to
// reload a fresh seal). No credential crosses argv/audit.
func (s *Spec) installCaddyCertFile(k3sVmid uint32, slot string, fullchain, key []byte) error {
	mc, err := s.client()
	if err != nil {
		return err
	}
	fcTmp, err := os.CreateTemp("", "fh-fc-*")
	if err != nil {
		return err
	}
	keyTmp, err := os.CreateTemp("", "fh-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(fcTmp.Name())
	defer os.Remove(keyTmp.Name())
	if err := fcTmp.Close(); err != nil {
		return err
	}
	if err := keyTmp.Close(); err != nil {
		return err
	}
	if err := os.WriteFile(fcTmp.Name(), fullchain, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(keyTmp.Name(), key, 0o600); err != nil {
		return err
	}
	if _, err := mc.Upload(s.RunnerTarget, fcTmp.Name(), "/tmp/fh-fc-"+slot+".pem", 60); err != nil {
		return err
	}
	if _, err := mc.Upload(s.RunnerTarget, keyTmp.Name(), "/tmp/fh-key-"+slot+".pem", 60); err != nil {
		return err
	}
	cmd := fmt.Sprintf(`set -e
pct push %d /tmp/fh-fc-%s.pem /tmp/fc-%s.pem
pct push %d /tmp/fh-key-%s.pem /tmp/key-%s.pem
pct exec %d -- sh -c '
set -e
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
PV=$($K get pvc caddy-data -n caddy -o jsonpath={.spec.volumeName})
PDIR=$($K get pv $PV -o jsonpath={.spec.local.path})
DIR=$PDIR/tls/%s
DUR=%s
mkdir -p "$DIR" "$DUR"
cp /tmp/fc-%s.pem "$DIR/fullchain.pem"
cp /tmp/key-%s.pem "$DIR/key.pem"
chmod 600 "$DIR/key.pem"
cp /tmp/fc-%s.pem "$DUR/fullchain.pem"
cp /tmp/key-%s.pem "$DUR/key.pem"
chmod 600 "$DUR/key.pem"
$K -n caddy rollout restart deploy/caddy >/dev/null 2>&1 || true
rm -f /tmp/fc-%s.pem /tmp/key-%s.pem
'
rm -f /tmp/fh-fc-%s.pem /tmp/fh-key-%s.pem
`,
		k3sVmid, slot, slot, k3sVmid, slot, slot, k3sVmid, // 1-7
		slot, stages.CaddyEdgeDurableDir(slot), // 8-9 (DIR tls/slot, DUR)
		slot, slot, slot, slot, slot, slot, slot, slot) // 10-17
	return s.run(cmd, 180)
}

// worldCert resolves each edge slot's cert CP-side: the durable-reuse gate
// (valid mirror => seed the PVC, no LE order) else an in-process resumable
// DNS-01 issue. It PLACES every slot's challenge FIRST (record on the wire,
// no wait), then waits for all of them to propagate, then resolves + installs
// each — so a slow DNS-01 propagation for a fresh apex overlaps across hosts
// instead of serializing (relay then cp), and a relay propagation stall no
// longer prevents cp's challenge from even being created.
func (s *Spec) worldCert() error {
	type pendSlot struct {
		slot     string
		host     string
		resume   *cert.Resume
		po       *cert.PendingOrder
		path     string
		provider string
		env      map[string]string
	}
	var pending []pendSlot

	// Phase A — durable-reuse seed, or open + PLACE each slot's order (no pause).
	for _, sl := range []struct{ slot, host string }{
		{"relay", s.RelayHost}, {"cp", s.CpHost},
	} {
		if sl.host == "" {
			continue
		}
		// Durable-reuse gate: a valid cert on the durable mirror (>= 30d left)
		// means no LE order, no challenge, no rate-limit — seed the PVC from it.
		if fc, err := s.durableFullchain(s.K3sVmid, sl.slot); err == nil && s.durableKeyPresent(s.K3sVmid, sl.slot) {
			if _, ok := cert.ReuseIfValidBytes(fc, time.Now(), 30*24*time.Hour); ok {
				if err := s.seedCaddyCertFromDurable(s.K3sVmid, sl.slot); err != nil {
					return fmt.Errorf("cert %s durable seed: %w", sl.slot, err)
				}
				continue
			}
		}
		// Box-shipped seed: the operator box caches each slot's issued cert
		// across builds, so a world whose plane was destroyed (the FRESH
		// lifecycle's full-destroy uninstall) pre-seeds from it — no new LE
		// order, no challenge, no rate-limit exposure. Absent/stale seeds fall
		// through to the issue path.
		seeded, serr := s.certSeedFromCache(s.K3sVmid, sl.slot, sl.host)
		if serr != nil {
			return fmt.Errorf("cert %s seed: %w", sl.slot, serr)
		}
		if seeded {
			continue
		}
		// Issue path: the sealed DNS cred must be on the CP (the box build's
		// hand-off ships it). The fresh fullchain+key pair is installed by
		// file-transit — no restart of the serving co-located runner.
		provider, env, err := s.dnsCredFromStore(sl.slot)
		if err != nil {
			return fmt.Errorf("cert %s: %w", sl.slot, err)
		}
		resume, po, statePath, err := s.openCertOrder(sl.slot, sl.host, provider, env)
		if err != nil {
			return fmt.Errorf("cert %s issue: %w", sl.slot, err)
		}
		pending = append(pending, pendSlot{slot: sl.slot, host: sl.host, resume: resume, po: po, path: statePath, provider: provider, env: env})
	}

	// Phase B — wait for every placed challenge to be served at the authoritative
	// zone (all records are already on the wire, so their propagation overlaps).
	for _, p := range pending {
		if err := p.resume.PropagationWait(p.po); err != nil {
			// A propagation timeout is transient: KEEP the order so the next run
			// resumes the same order + challenge instead of minting a new one.
			return fmt.Errorf("cert %s issue: %w", p.slot, err)
		}
	}

	// Phase C — accept + finalize + download each, install through the runner,
	// and clean up the placed challenge TXT from the zone.
	for _, p := range pending {
		issued, err := p.resume.Resolve(p.po)
		if err != nil {
			// Only the terminal "authorization invalid" state justifies discarding
			// the pending resumable order: resuming it can never succeed. A
			// transient failure (a polling timeout, a flaky network read) must
			// KEEP the order so the next run resumes instead of re-challenging.
			if errors.Is(err, cert.ErrAuthInvalid) {
				_ = os.Remove(p.path)
			}
			return fmt.Errorf("cert %s issue: %w", p.slot, err)
		}
		if err := s.installCaddyCertFile(s.K3sVmid, p.slot, issued.Fullchain, issued.Key); err != nil {
			return fmt.Errorf("cert %s install: %w", p.slot, err)
		}
		// Issue succeeded: clean up the placed challenge TXT so it doesn't linger
		// in the zone (lego's Present never removes it; Resume only discards state).
		if n, perr := cert.PurgeChallengeRecords(p.host, p.provider, p.env); perr == nil && n > 0 {
			fmt.Fprintf(os.Stderr, "cert %s: cleaned %d challenge record(s) after issue\n", p.slot, n)
		}
	}
	return nil
}

// worldLiteLLM seeds the CPA pod's litellm key CP-side. The kube workloads
// (postgres + gateway manifests) and model registration are OWNED by the
// terraform module now (worldTerraform "apply" → kube-apply.sh); this leg only
// mints the CPA's gateway master-key Secret first-run-wins from the injected
// env. Runs after the terraform step so the gateway is already reachable.
func (s *Spec) worldLiteLLM() error {
	if err := s.runSecrets(agent.AgentLiteLLMKeyScript(s.K3sVmid, s.CpaName), 60, "litellm"); err != nil {
		return fmt.Errorf("seed CPA litellm key: %w", err)
	}
	return nil
}

// BuildWorldApply returns the CP's world-build/reconcile driver: it runs the
// shared stage commands (internal/stages) through the co-located runner, so the
// box can "login + trigger" the CP to (re)assert the world. Each step is
// idempotent. Substrate (plane + cp/relay/k3s LXCs) is ensured by the Go
// staircases, then ADOPTED + the kube workloads OWNED by the embedded terraform
// module (worldTerraform → kube-apply.sh); the overlay (agent-tools, DNS, the
// CPA litellm key, caddy, cert) stays scripted but ordered here.
func BuildWorldApply(spec *Spec) agent.WorldApply {
	return func() (string, error) {
		var report []string
		// 0.5. Public A records (relay/cp -> proxy) on the CP's stored DNS
		// credential. The CP owns the cred and does DNS-01, so record
		// management joins the CP build (it was box-side pre-split). No-op
		// without an edge/proxy or a stored credential.
		if err := spec.manageDomainDNS(); err != nil {
			return "", fmt.Errorf("world-build manage DNS: %w", err)
		}
		// 1. The durable volume plane: re-ensure each tenant's dataset/LV onto
		// the recorded pool (idempotent, guest-writable) and capture the
		// born-at-create mounts. Runs FIRST — the LXC boots bake the mounts.
		var mounts map[planebase.Tenant][]planebase.MountSpec
		if spec.PlanePool != "" && spec.RelayHost != "" {
			var err error
			mounts, err = spec.worldStorage()
			if err != nil {
				return "", fmt.Errorf("world-build storage: %w", err)
			}
			report = append(report, "durable plane ensured")
		}
		// 2. The relay LXC: boot if missing (baking the durable mounts at
		// create) + deploy the Buzz stack (idempotent compose bring-up).
		// Resolve any EXISTING guest vmids by hostname FIRST so a boot reuses
		// an already-created LXC (a fresh/partial world with 0 recorded vmids;
		// PickFreeVMID refuses a name that already exists).
		if err := spec.resolveGuestVmids(); err != nil {
			return "", fmt.Errorf("world-build resolve guests: %w", err)
		}
		if spec.RelayHost != "" {
			if err := spec.worldBootRelay(mounts[planebase.TenantRelay]); err != nil {
				return "", fmt.Errorf("world-build relay: %w", err)
			}
			report = append(report, "relay booted + stack deployed")
		}
		// 2.4. Re-assert the CONSOLE identity's relay membership on EVERY build.
		// A relay rebuild/reseed can drop it, and then every console-signed
		// publish (the runner channels, agent rosters, agent-tools seeding) 403s
		// relay_membership_required — the console is not re-membered anywhere else
		// (install did it once, a later reconcile didn't). buzz-admin add-member
		// is idempotent.
		if spec.Audience != "" && spec.CpLxc != 0 {
			if err := spec.addRelayCommunityMember(spec.Audience); err != nil {
				return "", fmt.Errorf("world-build console relay membership: %w", err)
			}
		}
		// 2.5. Deploy the operator toolset (freehold-agent-tools) once the relay
		// it seeds its roster against is up — the console's world_build brings
		// up agent-tools itself (no box-one / deploy-cp dependency).
		if spec.CpLxc != 0 {
			if err := spec.deployAgentTools(); err != nil {
				return "", fmt.Errorf("world-build agent-tools: %w", err)
			}
			report = append(report, "agent-tools live")
		}
		// 3. Terraform PHASE 1 — the SUBSTRATE (plane + cp/relay/k3s LXCs + k3s
		// bring-up), restricted via -target. This BRINGS UP k3s so the
		// kubernetes-provider resources in phase 2 have an API to connect to: a
		// full plan now would fail, because the kubeconfig doesn't exist yet.
		if spec.RelayHost != "" && spec.PlanePool != "" {
			// Boot the k3s LXC in Go first so its vmid is allocated + recorded
			// (bootLxc picks a free vmid on a fresh world); terraform ADOPTS it.
			if err := spec.worldBootK3s(mounts[planebase.TenantK3sVolumes]); err != nil {
				return "", fmt.Errorf("world-build k3s boot: %w", err)
			}
			substrate := []string{
				"null_resource.plane",
				"null_resource.lxc_cp",
				"null_resource.lxc_relay",
				"null_resource.lxc_k3s",
				"null_resource.k3s_bringup",
			}
			if err := spec.tfRun("apply", substrate, nil, false); err != nil {
				return "", fmt.Errorf("world-build terraform substrate: %w", err)
			}
			report = append(report, "terraform substrate applied (plane + LXCs + k3s)")
		}
		// 3.5. Re-read the guests' CURRENT vmids + IPs (a fresh world whose coords
		// were cleared at teardown has 0 vmids; the boot steps just picked
		// them). Consumed by DNS/caddy/litellm/cert below.
		if err := spec.resolveGuestVmids(); err != nil {
			return "", fmt.Errorf("world-build resolve guests: %w", err)
		}
		spec.refreshGuestIPs()
		// 3.5a-pre. The CP resolver step runs BEFORE the services phase: it
		// installs dnsmasq, registers the split-horizon records, points every
		// guest at the CP, and verifies the resolver answers. The litellm/caddy
		// image pulls need working DNS, and a freshly booted guest otherwise sits
		// on DHCP/public resolvers that intermittently fail containerd's lookups.
		// Pointing guests at a CP whose dnsmasq is not yet installed would leave
		// them with no resolver at all, so the point and the install ship as one
		// step.
		// 3.5a0. The gateway's nftables (re)assert — after the k3s boot (its
		// DNAT target is the recorded internal IP), before anything dials the
		// edge through it. No-op without a gateway.
		if err := spec.worldGateway(); err != nil {
			return "", fmt.Errorf("world-build gateway: %w", err)
		}
		if spec.GatewayCIDR != "" && (spec.GatewayLxc != 0 || spec.HostedGateway) {
			report = append(report, "gateway nftables asserted")
		}
		if spec.CpLxc != 0 && spec.CpIP != "" {
			if err := spec.worldDNS(); err != nil {
				return "", fmt.Errorf("world-build dns: %w", err)
			}
			report = append(report, "dns register/point applied")
		}
		// 3.5a. Re-provision the CP's co-located runner from the CP's own
		// durable litellm store if a re-deploy wiped its package — the services
		// phase below requests these BY NAME. No-op when it already holds them.
		if err := spec.reseedCoLocatedRunner(); err != nil {
			return "", fmt.Errorf("world-build reseed co-located runner: %w", err)
		}
		// 3.5b. Terraform PHASE 2 — the SERVICE definitions (postgres.tf /
		// litellm.tf / caddy.tf) as kubernetes-provider resources. The kubeconfig
		// is staged from the now-up k3s (server rewritten to the node IP) and the
		// rendered Caddyfile rides -var caddyfile_b64 (plain, not secret).
		if spec.K3sVmid != 0 && spec.RelayHost != "" && spec.PlanePool != "" {
			if err := spec.stageKubeconfig(); err != nil {
				return "", fmt.Errorf("world-build tf kubeconfig: %w", err)
			}
			var extra []string
			if spec.CpHost != "" && spec.RelayIP != "" {
				relayUpstream := fmt.Sprintf("%s:3000", spec.RelayIP)
				cpUpstream, cpMcpUpstream := "", ""
				if spec.CpIP != "" {
					cpUpstream = fmt.Sprintf("%s:8080", spec.CpIP)
					cpMcpUpstream = fmt.Sprintf("%s:8089", spec.CpIP)
				}
				// pairUpstream: the pair-relay pod binds 5000 on the k3s node
				// itself (hostNetwork, same node as the caddy edge).
				pairUpstream := ""
				if kip := spec.k3sIP(); kip != "" {
					pairUpstream = fmt.Sprintf("%s:5000", kip)
				}
				caddyfile := caddydeploy.RenderCaddyfile(spec.RelayHost, relayUpstream, pairUpstream, spec.CpHost, cpUpstream, cpMcpUpstream)
				extra = append(extra, "-var",
					"caddyfile_b64="+base64.StdEncoding.EncodeToString([]byte(caddyfile)))
			}
			if err := spec.tfRun("apply", nil, extra, true); err != nil {
				return "", fmt.Errorf("world-build terraform services: %w", err)
			}
			report = append(report, "terraform services applied (postgres/litellm/caddy)")
		}
		// 5. The litellm gateway — the CPA pod's litellm key seed (the kube
		// workloads + model registration are owned by the terraform services
		// phase above); the operator's provider key rides the runner, never argv.
		if spec.K3sVmid != 0 && spec.LitellmIP != "" {
			if err := spec.worldLiteLLM(); err != nil {
				return "", fmt.Errorf("world-build litellm: %w", err)
			}
			report = append(report, "litellm gateway live")
		}
		// 7. The edge certs: durable-reuse gate (no LE order when the durable
		// mirror has a valid cert) else an in-process resumable DNS-01 issue,
		// then install into the Caddy PVC through the co-located runner.
		if spec.K3sVmid != 0 && (spec.RelayHost != "" || spec.CpHost != "") {
			if err := spec.worldCert(); err != nil {
				return "", fmt.Errorf("world-build cert: %w", err)
			}
			report = append(report, "cert issued/installed (or already present)")
		}
		// 7.25. Drop the CP's /etc/hosts public-host pins. The agent-tools seed
		// pinned relay.<apex>/cp.<apex> -> the guest LXC IP so it could dial the
		// relay's LAN events endpoint before Caddy existed. With the edge up those
		// pins must go - they make the CP resolve the public hosts to the guest
		// (no TLS there), so /api/world reports the relay/cp edge down. The
		// resolver's apex wildcard now fronts them through Caddy.
		if spec.CpLxc != 0 {
			hosts := []string{spec.RelayHost}
			if spec.CpHost != "" {
				hosts = append(hosts, spec.CpHost)
			}
			for _, h := range hosts {
				if h == "" {
					continue
				}
				esc := strings.ReplaceAll(h, ".", `\.`)
				if err := spec.run(fmt.Sprintf("pct exec %d -- sed -i '/%s/d' /etc/hosts", spec.CpLxc, esc), 30); err != nil {
					return "", fmt.Errorf("world-build drop host pin %s: %w", h, err)
				}
			}
			report = append(report, "edge host pins dropped (public hosts resolve to the proxy)")
		}
		// 7.5. Record the world-service health coords (k3s/litellm/caddy) so any
		// management box renders the live world through /api/world.
		if spec.CpLxc != 0 && spec.CpIP != "" {
			if err := spec.worldServices(); err != nil {
				return "", err
			}
			report = append(report, "world-service coords recorded")
		}
		// 7.75. Department capability runners: stand up each department's
		// dedicated runner (the raw capability grant lives with the department
		// identity) and record the pod-facing coords BEFORE the pods are
		// (re)created below, so each department pod gets its FREEHOLD_RUNNER_*
		// env. Idempotent + rebuild-safe.
		if spec.K3sVmid != 0 && spec.CpLxc != 0 {
			if err := spec.stageDepartmentRunners(); err != nil {
				return "", fmt.Errorf("world-build department runners: %w", err)
			}
			report = append(report, "department capability runners reconciled")
		}
		// 7.9. LiteLLM default aliases: ensure the gateway carries the alias set
		// the pods request (Code/General/Freehold/ExtraThinking) BEFORE the pods
		// (re)apply pointing at theirs — an unregistered alias is a 400ing
		// agent. Idempotent; a failure fails the build loudly.
		if spec.K3sVmid != 0 && spec.LitellmIP != "" {
			if err := spec.stageLitellmAliases(); err != nil {
				return "", fmt.Errorf("world-build litellm aliases: %w", err)
			}
			report = append(report, "litellm default aliases ensured")
		}
		// 8. The agent org + world facts are CP-owned now: create the CPA +
		// departments, reconcile every registered agent, and register the world
		// facts in-process, then restart agent-tools so its in-memory registry/
		// facts reload from what we just wrote. A thin login box no longer needs
		// a local runner or operator identity for any of it.
		if spec.K3sVmid != 0 {
			if spec.AgentRegistry != nil {
				// Running inside the agent-tools server: write its OWN in-process
				// stores directly (no second file handle, no restart).
				if err := spec.reconcileAgentsInto(spec.AgentRegistry); err != nil {
					return "", fmt.Errorf("world-build agents: %w", err)
				}
				report = append(report, "CPA + departments + agents reconciled")
				if spec.FactsStore != nil {
					if err := spec.registerWorldFactsInto(spec.FactsStore, mounts); err != nil {
						report = append(report, "WARN: world facts not registered: "+err.Error())
					} else {
						report = append(report, "world facts registered")
					}
				}
				// The scripts edit registry.json OUT-OF-BAND (a separate
				// freehold-agent-tools process), so the queue runs under the serve's
				// own write lock and re-reads the file when it is done — neither
				// half is sufficient alone. That guarantee lives in migrationRunner,
				// not here, so the world_migrate tool gets it too.
				report = spec.appendMigrations(report)
				report = spec.appendMemoryPlane(report)
				report = spec.appendAgentToolsAudience(report)
			} else {
				// Console executor: write the files, then reload the serve process.
				// The serve is deliberately left RUNNING across this whole block: the
				// pods reconcileAgents applies curl their stdio bridge binary off this
				// server's /freehold-agent-tools-binary, and a failed fetch silently
				// degrades the pod to plain buzz-dev-mcp with no create_agent. It is
				// restarted (killing any prior serve) by startAgentTools below, so it
				// loads these writes as its starting state.
				if err := spec.reconcileAgents(); err != nil {
					return "", fmt.Errorf("world-build agents: %w", err)
				}
				report = append(report, "CPA + departments + agents reconciled")
				if err := spec.registerWorldFactsServer(mounts); err != nil {
					// Bookkeeping only — never fatal (mirrors the old box-side warn).
					report = append(report, "WARN: world facts not registered: "+err.Error())
				} else {
					report = append(report, "world facts registered")
				}
				// The scripts read the registry file and talk to the relay directly,
				// so they need NO running serve — they run BEFORE the restart, and the
				// process that comes up loads their result as its starting state.
				report = spec.appendMigrations(report)
				report = spec.appendMemoryPlane(report)
				report = spec.appendAgentToolsAudience(report)
				if err := spec.startAgentTools(); err != nil {
					return "", fmt.Errorf("world-build agent-tools reload: %w", err)
				}
			}
		}
		// 8.5. The operator's Buzz profile (kind 0) — the event that makes the
		// desktop app skip its first-run onboarding (starter channels, private
		// Welcome, built-in welcome-team agents). Reads first; never overwrites.
		report = spec.stageOperatorProfile(report)
		if len(report) == 0 {
			return "", fmt.Errorf("world-build: no world coords recorded (k3s vmid / relay lxc)")
		}
		return strings.Join(report, "\n"), nil
	}
}

// vmidPtr returns a pointer to a recorded vmid, or nil when the role was never
// created (0 = unrecorded — the teardown treats nil as "already gone").
func vmidPtr(v uint32) *uint32 {
	if v == 0 {
		return nil
	}
	return &v
}

// cpTeardownRunner drives teardown.Run through the CP's co-located runner: the
// low-level pct/terraform commands ride the runner (the embedded ExecRunner
// with an injected exec), while the durable-plane destroys reuse the shared
// drive helpers against the same runner client.
type cpTeardownRunner struct {
	*teardown.ExecRunner
	c      *client.McpClient
	target string
}

func (r *cpTeardownRunner) DestroyDataset(tenant, domain, pool, kind, dataset string) (bool, error) {
	t, err := tenantFromName(tenant)
	if err != nil {
		return false, err
	}
	return drive.DestroyTenantBackend(drive.ClientExec(r.c, r.target), planebase.BackendKind(kind), pool, domain, t)
}

func (r *cpTeardownRunner) DestroyPool(vg, pool string) error {
	return drive.RemoveThinPool(drive.ClientExec(r.c, r.target), vg, pool)
}

func tenantFromName(name string) (planebase.Tenant, error) {
	switch name {
	case "relay":
		return planebase.TenantRelay, nil
	case "cp":
		return planebase.TenantCp, nil
	case "k3s-volumes":
		return planebase.TenantK3sVolumes, nil
	}
	return 0, fmt.Errorf("unknown tenant %q", name)
}

// BuildWorldTeardownApply returns the CP-owned world-teardown driver — the
// BuildWorldApply mirror. It runs the shared teardown engine through the
// co-located runner: terraform destroy (the kube layer — the LXCs carry no
// destroy provisioner), then pct stop/destroy of relay + k3s, and stops the
// CP-side freehold-agent-tools process. The CP and its co-located runner
// SURVIVE — `teardown` is the inverse of `build`, not of `uninstall`. The
// agent-tools durable state (identity, roster seed, runner grant) stays on the
// CP plane; build step 2.5 re-launches it. The internal DNS records are cleared
// by the console AFTER the runner-driven work (clearWorldDNS). Compute-only.
func BuildWorldTeardownApply(spec *Spec) agent.WorldApply {
	return func() (string, error) {
		if spec.RunnerTarget == "" {
			return "", fmt.Errorf("world-teardown: no runner target recorded")
		}
		mc, err := spec.client()
		if err != nil {
			return "", err
		}
		// Domain keys the world's per-world tf root (TerraformDestroy resolves
		// /srv/data/freehold-tf-<dashed-domain>, falling back to the shared dir
		// only for a world with no per-world root) — without it the destroy
		// half looks at the wrong dir and skips destroying the world's state.
		er := &teardown.ExecRunner{Domain: spec.RelayHost}
		er.SetExec(func(cmd string) (bool, string) {
			out, err := spec.execOut(cmd, 600)
			if err != nil {
				return false, out
			}
			return true, out
		})
		runner := &cpTeardownRunner{ExecRunner: er, c: mc, target: spec.RunnerTarget}
		cfg := &teardown.Cfg{
			Domain:      spec.RelayHost,
			RunNTarget:  spec.RunnerTarget,
			Managed:     []string{"relay", "k3s"}, // the CP + its co-located runner stay
			Pool:        spec.PlanePool,
			BackendKind: spec.PlaneKind,
			Vmid: map[string]*uint32{
				"relay": vmidPtr(spec.RelayLxc),
				"k3s":   vmidPtr(spec.K3sVmid),
			},
		}
		// Compute-only: cfg.Data stays false, so no dataset destroys run (the
		// plane survives teardown; `uninstall --remove-data` drops it).
		// The 4th arg is CONFIRM (Run's signature), not data.
		report, err := teardown.Run(runner, cfg, teardown.ScopeWholeWorld, true /* confirm */)
		if err != nil {
			return "", err
		}
		// Stop the CP-side agent-tools process: its world work (dialing the
		// relay, seeding membership) is dead with the relay, but its durable
		// state stays so build step 2.5 re-launches the same identity.
		if err := spec.stopAgentTools(); err != nil {
			return report, err
		}
		report += "\nCP + co-located runner preserved (uninstall drops them)"
		return report, nil
	}
}

// stopAgentTools stops the CP-side freehold-agent-tools serve process in the cp
// guest. Its durable state dir stays on the CP plane; build step 2.5 re-launches
// it (killing any prior serve first) with the same identity.
func (s *Spec) stopAgentTools() error {
	if s.CpLxc == 0 {
		return nil
	}
	atState := filepath.Join(filepath.Dir(s.StateDir), "agent-tools")
	cmd := fmt.Sprintf("pct exec %d -- sh -c 'p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" >/dev/null 2>&1; rm -f %s/serve.pid; true'",
		s.CpLxc, atState, atState)
	if err := s.run(cmd, 30); err != nil {
		return fmt.Errorf("world-teardown stop agent-tools: %w", err)
	}
	return nil
}

// cpDestroyDetached is the host command that stops + destroys the CP LXC
// without killing its own caller: setsid + a short sleep so the runner's exec
// returns before the container (which hosts the runner) goes away.
func cpDestroyDetached(vmid uint32) string {
	return fmt.Sprintf("setsid sh -c 'sleep 5; pct stop %d --skiplock; pct destroy %d --skiplock' >/dev/null 2>&1 </dev/null &", vmid, vmid)
}

// BuildMigrator wires the CP's migration runner: the Omarchy-style scripts that
// install/update shipped into <consoleStateDir>/migrations/scripts/<epoch>.sh,
// with completion markers at <consoleStateDir>/migrations/<epoch>.sh — the
// CONSOLE's durable state dir, which is where ShipMigrations writes them and
// where the console's /api/world pending count reads them (spec.StateDir is the
// agent-tools dir, a sibling, and never holds the scripts). Each pending script
// runs in ascending epoch order with `bash -euo pipefail` on the CP (where the
// data it operates on lives); success marks it done, failure stops the queue
// unmarked. The scripts receive the durable-plane paths + the agent-tools binary
// via env (FREEHOLD_AGENT_TOOLS / REGISTRY / CONSOLE_STATE / STATE_DIR) plus the
// relay coords a channel edit needs (FREEHOLD_RELAY_URL / FREEHOLD_RELAY_AUTH_URL
// / FREEHOLD_CPA_NAME) — never argv, so no credential crosses the audit.
//
// Every world runs every shipped script exactly once, a fresh install included:
// the explicit consoleStateDir keeps THIS caller (the agent-tools serve, passing
// its own --console-state-dir) and the world bring-up (deriving it via
// consoleStateRoot) from silently diverging on where the scripts live.
func BuildMigrator(spec *Spec, consoleStateDir string) agent.Migrator {
	root := filepath.Join(consoleStateDir, "migrations")
	return spec.migrationRunner(root, consoleStateDir)
}

// consoleStateRoot is the CP's console durable state dir, derived from the
// agent-tools state dir the Spec is anchored on (`<root>/agent-tools` ->
// `<root>/control-plane`), which is also the serve's --console-state-dir default.
// Deriving it here rather than passing StateDir is the point: the scripts and
// their markers live under the CONSOLE root.
func (s *Spec) consoleStateRoot() string {
	_, stateDir := s.cpGuestDirs()
	return stateDir
}

// sealedOwnerPath is the sealed operator identity's ONE home: the CONSOLE's
// world-secrets (the build's PutSecret lands there), never the Spec's own
// StateDir. The two differ on the agent-tools server (its StateDir is its own
// state root — under which nothing stages world-secrets), and a StateDir-keyed
// lookup there silently misses the record and falls back to the console
// identity — refusing every create with a derives-mismatch (seen live: a
// CPA-created agent blocked on "the readable owner secret derives <console>,
// not the recorded owner").
func (s *Spec) sealedOwnerPath() string {
	return filepath.Join(s.consoleStateRoot(), "world-secrets", "operator.json")
}

// migrationRunner returns the queue closure: every pending script under root,
// ascending, each with the durable-plane paths + relay coords in its env.
//
// The run takes the registry's write lock when the Spec carries one, and re-reads the
// file in the same critical section. This is the ONLY place that guarantee is placed,
// deliberately: the queue is reachable from two entry points — the tail of world_build
// and the world_migrate tool (which `freehold update` drives) — and both funnel here,
// so neither can forget it and neither can wrap it a second time (the mutex is not
// reentrant). The scripts edit registry.json as a SEPARATE process with its own file
// handle, so a roster write landing inside that window would save this process's stale
// rows over what a script just wrote; a re-read alone then loads that clobbered state
// and reports it converged. See Registry.WithRegistryLocked.
func (s *Spec) migrationRunner(root, consoleStateDir string) func() ([]migrations.Result, error) {
	return func() ([]migrations.Result, error) {
		binDir, _ := s.cpGuestDirs()
		relayAuthURL := s.RelayAuthURL
		if relayAuthURL == "" {
			relayAuthURL = s.RelayURL
		}
		// The script's DIAL is the relay's LAN form (the CP's dnsmasq pins the
		// relay host to the relay LXC, where nothing listens on 443 — TLS is
		// the edge's); the NIP-98 signature still covers the canonical https.
		// The same dial-LAN / sign-public split every other relay client uses.
		runEnv := append(os.Environ(),
			"FREEHOLD_AGENT_TOOLS="+filepath.Join(binDir, "freehold-agent-tools"),
			"REGISTRY="+filepath.Join(s.StateDir, "registry.json"),
			"CONSOLE_STATE="+consoleStateDir,
			"STATE_DIR="+s.StateDir,
			"FREEHOLD_RELAY_URL="+s.relayDial(),
			"FREEHOLD_RELAY_AUTH_URL="+relayAuthURL,
			"FREEHOLD_CPA_NAME="+s.cpaNameOrDefault(),
		)
		run := func() ([]migrations.Result, error) {
			return migrations.Run(root, func(_, path string) error {
				return s.runMigrationScript(path, runEnv, migrationScriptTimeout, migrationWaitDelay)
			})
		}
		if s.AgentRegistry == nil {
			// No in-process registry to keep aligned (the console-executor build path,
			// where the serve is a different process restarted after the queue).
			return run()
		}
		var res []migrations.Result
		err := s.AgentRegistry.WithRegistryLocked(func() error {
			var rerr error
			res, rerr = run()
			return rerr
		})
		return res, err
	}
}

// runMigrationScript runs one shipped script and returns its failure with output
// attached. Both bounds exist because the queue runs WHILE HOLDING the registry's write
// lock (migrationRunner, via Registry.WithRegistryLocked): an unbounded script
// would block every roster read and write in the serve for as long as it hung, and a
// relay curl stuck on TCP setup is enough to produce one. A timeout is an ordinary
// queue failure — the script stays unmarked and is retried on the next bring-up.
//
// The context kills the script's shell, but a descendant it left running can still hold
// the output pipe's write end open, which would keep CombinedOutput blocked past the
// deadline and defeat the very bound this is here for; WaitDelay closes the pipes and
// releases the hold once it elapses after the kill. The two durations are parameters so
// a test can prove both bounds at test speed.
func (s *Spec) runMigrationScript(path string, env []string, timeout, waitDelay time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", path)
	cmd.Env = env
	cmd.WaitDelay = waitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// appendMigrations runs the shipped migration queue against the agent org that
// just came up and appends one report line. Never fatal: a failed script stays
// unmarked and is retried on the NEXT bring-up, so the line is WARN-prefixed
// rather than aborting a build that has otherwise converged. The line always
// names the pending count, so a queue that found nothing can never read as a
// successful run.
func (s *Spec) appendMigrations(report []string) []string {
	root := filepath.Join(s.consoleStateRoot(), "migrations")
	pending, err := migrations.Pending(root)
	if err != nil {
		return append(report, "WARN: migrations: "+err.Error())
	}
	if len(pending) == 0 {
		return append(report, "migrations: 0 pending")
	}
	res, err := s.migrationRunner(root, s.consoleStateRoot())()
	// Run stops at the first failure and returns the results it HAS, so the
	// per-script cells are reported even alongside the error: the scripts before
	// the failure really ran and really got marked, and that is exactly what an
	// operator needs to tell a partial converge from none.
	cells := make([]string, 0, len(res))
	failed := err != nil
	for _, r := range res {
		if r.OK {
			cells = append(cells, r.Name+" ok")
			continue
		}
		failed = true
		cells = append(cells, fmt.Sprintf("%s FAILED: %s", r.Name, r.Err))
	}
	if len(cells) == 0 {
		cells = []string{"nothing reported"}
	}
	line := fmt.Sprintf("migrations: %d pending -> %s", len(pending), strings.Join(cells, ", "))
	if failed {
		line = "WARN: " + line
	}
	return append(report, line)
}

// appendAgentToolsAudience appends the audience-drift check line. The live
// agent-tools identity (agentToolsAudience — what every pod's signed call
// must verify against) is compared against the pubkey the console state
// recorded from the box's profile at deploy. A mismatch means the durable
// agent-tools identity was re-minted under the fleet (e.g. a serve boot
// against an unmounted durable plane): every EXISTING pod still signs the
// dead audience, so every CP tool call fails signature verify while the
// world otherwise looks healthy. Detection only — the repair (restore the
// durable agent-tools state from backup, or a deliberate re-point + pod
// re-create) is an operator decision, never an auto-write.
func (s *Spec) appendAgentToolsAudience(report []string) []string {
	audience, aerr := s.agentToolsAudience()
	if aerr != nil {
		// The durable identity is unreadable — the very failure the line
		// exists to surface. Say so; never misattribute another identity as
		// the "live" audience.
		return append(report, "WARN: agent-tools: "+aerr.Error()+" — pods' bridge audience is undeterminable; restore the durable agent-tools state from backup")
	}
	if audience == "" {
		return report
	}
	if _, err := os.Stat(filepath.Join(s.consoleStateRoot(), state.StateFile)); os.IsNotExist(err) {
		return append(report, "agent-tools: audience "+agenttools.ShortHex(audience)+" (no console state — nothing recorded to compare)")
	} else if err != nil {
		return append(report, "WARN: agent-tools: console state unreadable: "+err.Error())
	}
	store, err := state.Open(s.consoleStateRoot())
	if err != nil {
		return append(report, "WARN: agent-tools: console state unreadable: "+err.Error())
	}
	rec := store.AgentToolsPubkey()
	switch {
	case rec == nil || *rec == "":
		return append(report, "agent-tools: audience "+agenttools.ShortHex(audience)+" (console state records none — pre-recording world)")
	case *rec == audience:
		return append(report, "agent-tools: audience "+agenttools.ShortHex(audience)+" matches the console record")
	default:
		return append(report, "WARN: agent-tools: AUDIENCE DRIFT — the live identity is "+agenttools.ShortHex(audience)+
			" but the console/box profile records "+agenttools.ShortHex(*rec)+
			": every existing agent pod signs the stale pubkey and every CP tool call fails signature verify. "+
			"Restore the durable agent-tools state from backup (do not hand-patch), or deliberately re-point the profile and re-create the pods.")
	}
}

// ownerKey resolves the OWNER key that attests the agents' memory plane: the
// operator identity the world was installed under (OwnerPub — the relay's
// `owner`-role member, and the same key the pods declare as
// BUZZ_ACP_AGENT_OWNER). Resolution order: a composition root that already
// holds the key sets OwnerSecret; else the sealed `operator` world-secret the
// box's build hands off (the operator nsec lives ONLY on the box — the CP is
// its durable owner the same way the DNS creds are); else, for a world where
// the operator and console identities coincide, the console identity on disk.
//
// Whatever resolves MUST derive the recorded OwnerPub, checked here rather
// than per-caller: the tag's owner field and BUZZ_ACP_AGENT_OWNER are two
// halves of one addressing scheme, so a wrong key restored from a backup would
// have every agent write into a store it can never read back.
func (s *Spec) ownerKey() ([]byte, error) {
	if s.OwnerPub == "" {
		return nil, fmt.Errorf("no owner pubkey is recorded on the build spec")
	}
	if len(s.OwnerSecret) == 32 {
		return s.validatedOwnerKey(s.OwnerSecret)
	}
	sealed := s.sealedOwnerPath()
	if !cert.CredExists(sealed) {
		disk, err := cpstate.ConsoleSecret(s.consoleStateRoot())
		if err != nil {
			return nil, fmt.Errorf("no owner key: the operator identity is not sealed on the CP (run `freehold build` from the operator box) and the console identity under %s/console is unreadable: %w", s.consoleStateRoot(), err)
		}
		return s.validatedOwnerKey(disk)
	}
	encSec, serr := s.consoleEncSecretAt(s.consoleStateRoot())
	if serr != nil {
		return nil, fmt.Errorf("the sealed operator identity at %s needs the console enc key: %w", sealed, serr)
	}
	open := func(recSecret, aad, blob []byte) ([]byte, error) { return crypto.Open(recSecret, aad, blob) }
	_, env, err := cert.LoadCreds(sealed, open, encSec)
	if err != nil {
		return nil, s.staleSealedOwner(sealed, fmt.Errorf("unreadable (%w)", err))
	}
	if hexRaw := env["nostr"]; hexRaw != "" {
		sec, derr := hex.DecodeString(hexRaw)
		if derr == nil && len(sec) == 32 {
			if _, verr := s.validatedOwnerKey(sec); verr == nil {
				return sec, nil
			}
		}
	}
	return nil, s.staleSealedOwner(sealed, fmt.Errorf("usable for a different owner"))
}

// staleSealedOwner reports a sealed operator record that no longer serves —
// unreadable, or sealed for a key that is not the recorded owner. The record is
// NEVER deleted here (a CP-side delete of the only copy is a destructive act
// the operator owns); the message names the one file to remove so the next
// `freehold build` re-seeds it from the box's ledger.
func (s *Spec) staleSealedOwner(path string, cause error) error {
	return fmt.Errorf("the sealed operator identity at %s is %s — remove that file and re-run `freehold build` from the operator box to re-seed it", path, cause)
}

// validatedOwnerKey checks a candidate secret against the recorded OwnerPub —
// the one invariant that keeps the attestation addressing the store the pods
// actually read from.
func (s *Spec) validatedOwnerKey(sec []byte) ([]byte, error) {
	pk, err := crypto.PubkeyFromSecret(sec)
	if err != nil {
		return nil, fmt.Errorf("the owner secret is unusable: %w", err)
	}
	if pk != s.OwnerPub {
		return nil, fmt.Errorf("the readable owner secret derives %s, not the recorded owner %s — attestations would address the wrong store; re-run `freehold build` from the operator box (or check %s/console/identity.json)",
			agenttools.ShortHex(pk), agenttools.ShortHex(s.OwnerPub), s.consoleStateRoot())
	}
	return sec, nil
}

// mintAuthTag renders the NIP-OA attestation an agent's pod needs as
// BUZZ_AUTH_TAG. `buzz mem` takes its owner from the tag, so without it the
// agent boots with a working harness and NO writable long-term memory — failing
// in a way the pod log does not show, which is precisely how this stayed broken
// across releases. So this fails the create rather than return an empty tag: the
// key must resolve, and the minted tag must verify against the very agent key
// the pod boots with.
//
// The attesting key is the OWNER key (OwnerPub — the operator identity the
// world was installed under, resolved by ownerKey), never the agent's own: the
// CLI rejects self-attestation outright, and an owner-signed tag keeps every
// agent store in the same owner namespace the respond gate already uses. It is
// normally NOT the console identity — see ownerKey.
func (s *Spec) mintAuthTag(agentPk, who string) (string, error) {
	ownerSec, err := s.ownerKey()
	if err != nil {
		return "", fmt.Errorf("%s: no owner key to attest the agent's memory plane: %w", who, err)
	}
	tag, err := nipoa.MintEngram(agentPk, ownerSec)
	if err != nil {
		return "", fmt.Errorf("%s: minting the memory-plane attestation: %w", who, err)
	}
	if err := nipoa.Verify(agentPk, tag); err != nil {
		return "", fmt.Errorf("%s: the minted attestation does not verify for its own agent key: %w", who, err)
	}
	return tag.JSON(), nil
}

// appendMemoryPlane appends the report line that makes the agent memory plane's
// health visible at bring-up. It names the attesting identity rather than a bare
// ok because the failure it covers is a silent one: a world whose agents answer
// conversations yet persist nothing looks healthy from every other angle. Never
// fatal — a create with an unresolvable key already fails loudly at
// mintAuthTag — this is what keeps a converged world distinguishable from a
// silently unattested one.
func (s *Spec) appendMemoryPlane(report []string) []string {
	if _, err := s.ownerKey(); err != nil {
		return append(report, "WARN: memory plane: "+err.Error()+" — agent pods come up with NO writable `buzz mem`")
	}
	return append(report, "memory plane: owner "+agenttools.ShortHex(s.OwnerPub)+" attests each agent pod (kind "+strconv.Itoa(nipoa.AgentEngramKind)+")")
}

// freeholdWelcomeMarker is the #t tag on the CPA's one-time #freehold welcome
// message; its presence on the relay is the whole idempotence state (the relay
// DB is durable across rebuild/adopt, so the marker read is enough — no
// fresh-vs-adopt flag).
const freeholdWelcomeMarker = "fh-welcome"

// operatorDisplayName resolves the operator's display name (OperatorName or
// "Operator").
func (s *Spec) operatorDisplayName() string {
	if name := strings.TrimSpace(s.OperatorName); name != "" {
		return name
	}
	return "Operator"
}

// operatorNameArg encodes the operator display name for a serve argv hop
// (base64: one token, any unicode, no shell/flag metacharacters).
func operatorNameArg(name string) string {
	return base64.StdEncoding.EncodeToString([]byte(name))
}

// OperatorNameFromArg decodes what operatorNameArg encoded. Exported for the
// serve (the argv consumer).
func OperatorNameFromArg(arg string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(arg))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// stageOperatorProfile publishes the operator's kind:0 profile on the relay
// (name = OperatorName) — the event that makes the Buzz desktop app SKIP its
// first-run onboarding (its starter channels, its private Welcome channel, and
// its built-in welcome-team agents) and land straight in the workspace, where
// #freehold + #general are the first-run surface. Guarded by a read: an
// existing profile — the operator's own edits included — is never overwritten.
// WARN-only: a missed publish degrades to the stock onboarding, never a failed
// build.
func (s *Spec) stageOperatorProfile(report []string) []string {
	if s.OwnerPub == "" || s.RelayHost == "" {
		return report
	}
	degraded := " — the Buzz desktop app runs its stock first-run onboarding"
	sec, err := s.ownerKey()
	if err != nil {
		return append(report, "WARN: operator profile: "+err.Error()+degraded)
	}
	authURL := s.RelayAuthURL
	if authURL == "" {
		authURL = s.RelayURL
	}
	evs, err := relay.QueryEventsAuth(s.relayDial(), authURL, sec, []interface{}{map[string]interface{}{
		"kinds":   []interface{}{0},
		"authors": []interface{}{s.OwnerPub},
		"limit":   1,
	}})
	if err != nil {
		return append(report, "WARN: operator profile: relay read failed: "+err.Error()+degraded)
	}
	if len(evs) > 0 {
		if name := operatorProfileName(evs[0]); name != "" {
			return append(report, "operator profile already present ("+name+")")
		}
		return append(report, "operator profile already present")
	}
	name := s.operatorDisplayName()
	if err := relay.PublishProfileAuth(s.relayDial(), authURL, sec, name, "freehold operator"); err != nil {
		return append(report, "WARN: operator profile publish failed: "+err.Error()+degraded)
	}
	return append(report, "operator profile published ("+name+") — the Buzz desktop app skips its first-run onboarding")
}

// operatorProfileName pulls the display name out of a kind:0 profile event —
// the name the operator chose on the Buzz side, the one mentions render
// against. Empty when the event is missing or malformed.
func operatorProfileName(ev map[string]interface{}) string {
	content, _ := ev["content"].(string)
	var p struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(content), &p) == nil {
		return strings.TrimSpace(p.Name)
	}
	return ""
}

// postFreeholdWelcome posts the CPA's one-time welcome message in #freehold,
// mentioning the operator (so it files into their Inbox on a first connect).
// Marker-guarded: a #freehold message carrying freeholdWelcomeMarker means it
// already ran — a rebuild/re-adopt never re-posts. Best-effort: the caller
// warns on error (the next build's marker read self-heals a transient miss).
func (s *Spec) postFreeholdWelcome(nSec []byte) error {
	if s.OwnerPub == "" {
		return nil
	}
	authURL := s.RelayAuthURL
	if authURL == "" {
		authURL = s.RelayURL
	}
	// Marker first — the welcome is one-time. Two single-filter reads, the
	// pattern every relay caller uses (the bridge honors body[0] only; a
	// multi-filter request would silently drop the second).
	evs, err := relay.QueryEventsAuth(s.relayDial(), authURL, nSec, []interface{}{map[string]interface{}{
		"kinds": []interface{}{delegate.StreamMsgKind},
		"#h":    []interface{}{relayFreeholdChannel},
		"#t":    []interface{}{freeholdWelcomeMarker},
		"limit": 1,
	}})
	if err != nil {
		return err
	}
	if len(evs) > 0 {
		return nil
	}
	// The mention carries the name the operator is known by on the relay —
	// their own kind:0 (an updated world has no recorded name; the profile
	// is where it lives). A failed read errors out rather than posting the
	// default: the marker is still absent, so the next build re-runs this
	// path — nothing is ever posted wrong.
	pevs, err := relay.QueryEventsAuth(s.relayDial(), authURL, nSec, []interface{}{map[string]interface{}{
		"kinds":   []interface{}{0},
		"authors": []interface{}{s.OwnerPub},
		"limit":   1,
	}})
	if err != nil {
		return err
	}
	name := s.operatorDisplayName()
	if len(pevs) > 0 {
		if n := operatorProfileName(pevs[0]); n != "" {
			name = n
		}
	}
	content := "Welcome to your freehold, @" + name +
		" — I'm @freehold, your main touchpoint. Ask here and I'll bring in network, data, compute, or ai when their hands are needed. This channel is where the core agents coordinate; #general is open for anything."
	return delegate.PostTaggedMessageAuth(s.relayDial(), authURL, nSec, relayFreeholdChannel, s.OwnerPub, [][]string{{"t", freeholdWelcomeMarker}}, content)
}

// pinRelayHost idempotently pins the relay's LAN IP to its hostname in the CP
// guest's /etc/hosts. Always re-reads the relay's CURRENT DHCP lease: a prior
// cycle's recorded IP can go stale (the relay LXC can come back on a different
// .30.x lease after a teardown+rebuild), and pinning against a dead IP makes
// the relay dial fail with "no route to host". The recorded value is the
// fallback when the live read yields nothing. Every consumer that dials the
// relay by HOSTNAME depends on this pin (the console's channel sync, the
// agent-tools serve's roster queries, the seed) — the edge DNS record for the
// same name points at the PROXY, so an unpinned resolve reaches the wrong box.
// hostsLineRe is the strict gate for /etc/hosts pin values: a hostname
// (dot-separated alphanumerics/hyphens) and a bare IP. Both reach a
// root-executed sh -c single-quoted into the CP guest, so anything outside
// this charset (a quote, a dollar, whitespace) is refused BEFORE
// interpolation — the same posture as the doorKeyRe gate.
var hostsLineRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

func (s *Spec) pinRelayHost() error {
	relayIP := s.RelayIP
	if s.RelayLxc != 0 {
		if out, err := s.runOut(fmt.Sprintf("pct exec %d -- ip -4 -o addr show eth0", s.RelayLxc), 30); err == nil {
			for _, t := range strings.Fields(out) {
				if strings.Contains(t, "/") && t != "127.0.0.1/8" {
					relayIP = config.StripCIDR(t)
					break
				}
			}
		}
	}
	if relayIP != "" && s.RelayHost != "" {
		if !hostsLineRe.MatchString(s.RelayHost) || !hostsLineRe.MatchString(relayIP) {
			return fmt.Errorf("pin relay host into cp: refusing unsafe values (host %q / ip %q must be a bare hostname and IP)", s.RelayHost, relayIP)
		}
		// REPLACE any existing line for this host, never merely append-if-absent:
		// a stale entry from a prior cycle (the relay came back on a new lease)
		// would otherwise satisfy a grep and keep resolving to the dead IP.
		pin := fmt.Sprintf("pct exec %d -- sh -c \"sed -i '/[[:space:]]%s$/d' /etc/hosts; echo '%s %s' >> /etc/hosts\"", s.CpLxc, s.RelayHost, relayIP, s.RelayHost)
		if err := s.run(pin, 30); err != nil {
			return fmt.Errorf("pin relay host into cp: %w", err)
		}
	}
	return nil
}

// doorKeyRe is the DOOR_SPEC §2.5 strict authorized_keys-line gate: key type +
// base64 body + an optional safe comment, with NO whitespace runs, quotes,
// backticks, $, (, ;, &, |, or newlines. A string passing this is a key line,
// not a shell payload — safe to single-quote into the append/remove command.
var doorKeyRe = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp256) [A-Za-z0-9+/]+=? ?[A-Za-z0-9._@-]*$`)

// BuildWorldDoor builds the DOOR_SPEC authorize/revoke drivers: the CP appends
// an operator box's public door key to (or removes it from) the host door
// through its co-located runner — the same runner that already holds the host
// door and drives world_build. The pubkey is validated against doorKeyRe at
// the API boundary BEFORE it ever reaches the shell.
func BuildWorldDoor(spec *Spec) (agent.DoorAuthorizeAppend, agent.DoorRevoke) {
	authorize := func(pubkey string) error {
		if !doorKeyRe.MatchString(pubkey) {
			return fmt.Errorf("world-authorize-door: pubkey must match a strict authorized_keys line (key type + base64 body + optional safe comment, no shell metacharacters)")
		}
		cmd := "mkdir -p ~/.ssh && chmod 700 ~/.ssh && grep -Fqx '" + pubkey + "' ~/.ssh/authorized_keys || (touch ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys && echo '" + pubkey + "' >> ~/.ssh/authorized_keys)"
		if _, err := spec.execOut(cmd, 30); err != nil {
			return fmt.Errorf("world-authorize-door: %w", err)
		}
		return nil
	}
	revoke := func(pubkey string) error {
		if !doorKeyRe.MatchString(pubkey) {
			return fmt.Errorf("world-revoke-door: pubkey must match a strict authorized_keys line (key type + base64 body + optional safe comment, no shell metacharacters)")
		}
		// Exact-line removal with a real error on a real failure (NO `|| true`
		// masking — a false "revoked" for the lost/compromised-box lever is a
		// silent security lie). Run only when the file exists (absent = nothing
		// to revoke, not an error). The `.` in the comment is the ONLY regex
		// special the doorKeyRe charset allows — escape it so the sed address
		// is a literal line, matching authorize's grep -Fqx exact-line check.
		escaped := strings.ReplaceAll(pubkey, ".", `\.`)
		cmd := "if [ -f ~/.ssh/authorized_keys ]; then sed -i '\\|" + escaped + "|d' ~/.ssh/authorized_keys; fi"
		if _, err := spec.execOut(cmd, 30); err != nil {
			return fmt.Errorf("world-revoke-door: %w", err)
		}
		return nil
	}
	return authorize, revoke
}

// BuildWorldStatus builds the single-inventory world_status payload: the
// registry agents + the console's runners/DNS read underneath (the console's
// state.json on the box — what /api/overview + /api/dns serve).
// BuildWorldExec runs a command through the CP's co-located runner — the
// drive-through-CP exec surface a thin login box uses (world_exec), so it has
// the build box's full operational surface without hosting a runner. Operator-
// scoped (dispatch gates it). A target that isn't the CP's own runner target
// is rejected, so a box never silently execs on a host it didn't name.
func BuildWorldExec(spec *Spec) agent.ExecFn {
	return func(target, cmd string, timeoutS uint64, secrets ...string) (string, error) {
		if target != "" && target != spec.RunnerTarget {
			return "", fmt.Errorf("world-exec: the CP's runner is bound to target %q, not %q — use %q (or omitting target)", spec.RunnerTarget, target, spec.RunnerTarget)
		}
		if timeoutS == 0 {
			timeoutS = 30
		}
		return spec.execOut(cmd, timeoutS, secrets...)
	}
}

func BuildWorldStatus(spec *Spec, reg *agenttools.Registry, consoleStateDir string, facts *agenttools.FactsStore) agent.WorldStatusFunc { // The /mcp world_status surface shares the SAME single-inventory assembly the
	// console's /api/world route serves (agenttools.WorldStatus) — one
	// implementation, both surfaces, never divergent.
	return func() (map[string]interface{}, error) {
		return agenttools.WorldStatus(reg, facts, consoleStateDir)
	}
}

// ensureAgentChannel resolves the named channel (kind 39000 group meta, by
// display name) or creates it — owned by the creating agent — when absent,
// returning its id + display name. An empty name is the default freehold
// channel, which is always PRIVATE and ENSURED because the first agent created
// may be the one that brings it into existence. An explicit channel is created
// private when private is set (the per-department channels), open otherwise.
func (s *Spec) ensureAgentChannel(nSec []byte, channel string, private bool) (id, displayName string, created bool, err error) {
	authURL := s.RelayAuthURL
	if authURL == "" {
		authURL = s.RelayURL
	}
	// The DIAL is the LAN form (relayDial — the relay guest's own :3000, which
	// the CP reaches on-subnet; behind a gateway the public https URL would
	// resolve through the bare-record shadowing to the relay guest's :443,
	// where nothing listens). The NIP-98 signature covers the CANONICAL
	// public origin — the dial-LAN / sign-public split.
	dial := s.relayDial()
	if strings.TrimSpace(channel) == "" {
		if err := delegate.EnsurePrivateChannelAuth(dial, authURL, nSec, relayFreeholdChannel, "#freehold"); err != nil {
			return "", "", false, fmt.Errorf("ensure #freehold channel: %w", err)
		}
		return relayFreeholdChannel, "#freehold", false, nil
	}
	name := "#" + strings.TrimPrefix(strings.TrimSpace(channel), "#")
	// The shared channel has a FIXED id and is always private: never recreate it
	// under the name-derived id, and never as open, even when a create passes
	// private=false.
	if strings.EqualFold(name, "#freehold") {
		if err := delegate.EnsurePrivateChannelAuth(dial, authURL, nSec, relayFreeholdChannel, "#freehold"); err != nil {
			return "", "", false, fmt.Errorf("ensure #freehold channel: %w", err)
		}
		return relayFreeholdChannel, "#freehold", false, nil
	}
	// A relay read error must NOT be mistaken for "absent" (that would create a
	// duplicate of an existing channel) — fail the create instead. The lookup
	// signs as the CREATING agent (nSec), the identity that owns/joins the
	// channel: a private channel the console identity is not a member of must
	// still be found, or a rebuild would create a duplicate.
	existingID, existingName, ok, err := relay.FindChannelAuth(dial, authURL, nSec, channel)
	if err != nil {
		return "", "", false, fmt.Errorf("look up channel %q: %w", channel, err)
	}
	if ok {
		return existingID, existingName, false, nil
	}
	id = relay.ChannelIDFromName(channel)
	create := delegate.EnsureChannelAuth
	if private {
		create = delegate.EnsurePrivateChannelAuth
	}
	if err := create(dial, authURL, nSec, id, name); err != nil {
		return "", "", false, fmt.Errorf("create channel %s: %w", name, err)
	}
	return id, name, true, nil
}

// litellmModelFor resolves a pod's OPENAI_COMPAT_MODEL: the core identities
// (the CPA + departments) are pinned to the core alias; a custom agent runs
// its persisted choice (one of agent.CustomLiteLLMModels), defaulting to the
// General alias.
func litellmModelFor(cpaName, name, model string) string {
	if name == cpaName {
		return agent.CoreLiteLLMModel
	}
	if _, ok := agents.DepartmentPrompt(name); ok {
		return agent.CoreLiteLLMModel
	}
	if slices.Contains(agent.CustomLiteLLMModels, model) {
		return model
	}
	return agent.DefaultAgentLiteLLMModel
}

// BuildCreateAgentFn returns the create-agent deploy: mint a durable identity on
// the CP, add it as a relay member, publish its profile, ensure each named channel
// (private when asked), apply its pod through the co-located runner, and hand the
// minted pubkey to Tools.CreateAgent (which registers the registry row). Branches
// to the CPA manifest/prompt when the name is the CPA's, so stageCpa's dogfooded
// create_agent produces the CPA.
func BuildCreateAgentFn(spec *Spec) agent.CreateAgentFn {
	return func(name, purpose string, channels []string, private bool, model string) (string, error) {
		model = litellmModelFor(spec.CpaName, name, model)
		if name == "" {
			return "", fmt.Errorf("create-agent needs a non-empty name")
		}
		// The k3s vmid (where the pod manifests apply) may be 0 for the server
		// deployed BEFORE k3s was booted (the console world_build booted it);
		// resolve it by hostname so apply targets the real guest.
		if spec.K3sVmid == 0 {
			if err := spec.resolveGuestVmids(); err != nil {
				return "", fmt.Errorf("create-agent %q: %w", name, err)
			}
		}
		if spec.K3sVmid == 0 {
			return "", fmt.Errorf("create-agent %q: no k3s vmid recorded/resolvable to apply the pod", name)
		}
		// A DIFFERENT name that sanitizes to the CPA's pod would delete+reapply
		// the CPA's pod; the CPA's own name is allowed (stageCpa dogfoods
		// creating it). CIDR-less, pod-name collision guard only for others.
		if name != spec.CpaName && agent.PodName(name) == agent.PodName(spec.CpaName) {
			return "", fmt.Errorf("create-agent %q: the sanitized pod name collides with the control plane agent", name)
		}
		dir := filepath.Join(spec.agentIdentityDir(), "agents", sanitizeDir(name))
		if _, err := agent.EnsureIdentity(dir); err != nil {
			return "", fmt.Errorf("mint %s identity: %w", name, err)
		}
		id, err := identity.Load(dir)
		if err != nil {
			return "", fmt.Errorf("%s identity unreadable: %w", name, err)
		}
		pub, err := id.NostrPubkeyHex()
		if err != nil {
			return "", err
		}

		// The memory-plane attestation the pod boots with. Minted HERE, before
		// anything is mutated on the relay: an agent that cannot be attested is
		// an agent whose memory would silently never persist, and adding it as a
		// relay member first would leave a half-created agent behind on the
		// failure. The tag is bound to this exact pubkey — the same identity the
		// identity Secret ships as BUZZ_PRIVATE_KEY — so it is minted from `pub`
		// and never re-derived.
		authTag, terr := spec.mintAuthTag(pub, fmt.Sprintf("create-agent %q", name))
		if terr != nil {
			return "", terr
		}

		// Relay membership (relay-administered; the CP cannot self-add — runs
		// buzz-admin through the co-located runner into the relay LXC). The
		// relay vmid may be unknown (fresh world) — resolve it by hostname.
		relayLxc := spec.RelayLxc
		if relayLxc == 0 {
			if err := spec.resolveGuestVmids(); err != nil {
				return "", fmt.Errorf("add relay member %s: %w", pub, err)
			}
			relayLxc = spec.RelayLxc
		}
		cmdLine := fmt.Sprintf("cd %s && docker compose exec -T relay buzz-admin add-member --pubkey %s", spec.RelayCompose, pub)
		full := fmt.Sprintf("pct exec %d -- sh -c '%s'", relayLxc, cmdLine)
		if err := spec.run(full, 120); err != nil {
			return "", fmt.Errorf("add relay member %s: %w", pub, err)
		}

		// Profile + the target channel, signed by the agent (NIP-98 against the
		// CANONICAL relay URL; the dial may be the LAN form pre-Caddy).
		nSec, err := hex.DecodeString(id.NostrSecretHex)
		if err != nil {
			return "", err
		}
		authURL := spec.RelayAuthURL
		if authURL == "" {
			authURL = spec.RelayURL
		}
		if err := relay.PublishProfileAuth(spec.relayDial(), authURL, nSec, name, "freehold agent"); err != nil {
			return "", fmt.Errorf("publish %s profile: %w", name, err)
		}
		// Each named channel is resolved (created, owned by this agent, when
		// absent) and joined; the operator is added to each. Empty list = the
		// default freehold channel. After the channels exist the CPA is added to
		// each so the system's main touchpoint sees every department
		// (agents.DepartmentChannels gives a department #freehold only — the
		// departments hold their conversations there).
		type channelRef struct{ id, name string }
		var joined []channelRef
		for _, ch := range channelNames(channels) {
			channelID, channelName, created, err := spec.ensureAgentChannel(nSec, ch, private)
			if err != nil {
				return "", err
			}
			// #freehold is PRIVATE and owned by the CPA: only its owner can add
			// members, so the new agent + operator are added signed by the CPA
			// (a self-join would be refused). Every other channel here is owned
			// by the created agent, so it self-joins and writes the memberships.
			// Every write is guarded by IsMemberAuth: membership is
			// state-idempotent but NOT event-idempotent (buzz renders each
			// put-user as a fresh "you were added"), so the reconcile must
			// re-assert nothing on a converged world. A relay read error fails
			// OPEN to the write attempt (the add lands or fails as before).
			if channelID == relayFreeholdChannel && name != spec.CpaName {
				cpaSec := spec.cpaSecret()
				if len(cpaSec) != 32 {
					return "", fmt.Errorf("add %s to #freehold: the CPA identity is unavailable to sign the membership (a private #freehold admits members only through its owner)", name)
				}
				if member, merr := relay.IsMemberAuth(spec.relayDial(), authURL, cpaSec, channelID, pub); merr != nil || !member {
					if err := relay.PutUserChannelAuth(spec.relayDial(), authURL, cpaSec, channelID, pub); err != nil {
						return "", fmt.Errorf("add %s to #freehold: %w", name, err)
					}
				}
				if spec.OwnerPub != "" {
					if member, merr := relay.IsMemberAuth(spec.relayDial(), authURL, cpaSec, channelID, spec.OwnerPub); merr != nil || !member {
						if err := relay.PutUserChannelAuth(spec.relayDial(), authURL, cpaSec, channelID, spec.OwnerPub); err != nil {
							return "", fmt.Errorf("add operator to #freehold: %w", err)
						}
					}
				}
				joined = append(joined, channelRef{channelID, channelName})
				continue
			}
			// JOIN it (open channels allow free joins; a private one may refuse —
			// best-effort), then try to ADD the operator (the agent owns a channel
			// it created; it may not own a pre-existing one).
			if member, merr := relay.IsMemberAuth(spec.relayDial(), authURL, nSec, channelID, pub); merr != nil || !member {
				_ = relay.JoinChannelAuth(spec.relayDial(), authURL, nSec, channelID)
			}
			if spec.OwnerPub != "" {
				if member, merr := relay.IsMemberAuth(spec.relayDial(), authURL, nSec, channelID, spec.OwnerPub); merr != nil || !member {
					perr := relay.PutUserChannelAuth(spec.relayDial(), authURL, nSec, channelID, spec.OwnerPub)
					if perr != nil && created {
						return "", fmt.Errorf("add operator to %s: %w", channelName, perr)
					}
				}
			}
			joined = append(joined, channelRef{channelID, channelName})
		}
		if name != spec.CpaName {
			if cpaPub := spec.cpaPubkey(); cpaPub != "" {
				for _, ref := range joined {
				// Best-effort: the created agent signs, so it lands on a
				// channel it owns (a custom channel it created); the CPA is
				// already the owner/member of #freehold — skip.
					if ref.id == relayFreeholdChannel {
						continue
					}
					if member, merr := relay.IsMemberAuth(spec.relayDial(), authURL, nSec, ref.id, cpaPub); merr == nil && member {
						continue
					}
					_ = relay.PutUserChannelAuth(spec.relayDial(), authURL, nSec, ref.id, cpaPub)
				}
			}
		}

		if err := spec.run(agent.AgentIdentityScript(spec.K3sVmid, id.NostrSecretHex, spec.OwnerPub, name), 120); err != nil {
			return "", fmt.Errorf("%s identity secret: %w", name, err)
		}
		// The bridge audience is the AGENT-TOOLS server's pubkey (resolved from
		// the durable identity), not spec.Audience — for the console executor
		// that is the console's own runner-signing identity, and a pod stamped
		// with it signs a dead audience forever. An unreadable durable identity
		// fails the create loudly rather than stamping a wrong key.
		audience, aerr := spec.agentToolsAudience()
		if aerr != nil {
			return "", fmt.Errorf("create-agent %q: %w", name, aerr)
		}
		var manifest string
		if name == spec.CpaName {
			manifest = agent.CPAManifestScript(spec.K3sVmid, spec.RelayWS, agents.CPASystemPrompt(spec.RepoURL, spec.operatorTZ()), name, spec.LitellmBaseURL, "", spec.SelfURL, audience, authTag, spec.operatorTZ())
		} else {
			// A reserved department name selects that department's embedded
			// prompt; any other name renders the custom template (agents.SystemPrompt).
			// A department with capability runners gets their FREEHOLD_RUNNER_* env
			// (the CPA and custom agents get none, so their bridge has no exec).
			// The inbound author gate: a reserved department wakes for the
			// operator + every core identity; a custom agent wakes for its
			// asker (today the operator — the CP cannot see chat threads) +
			// the CPA. The CPA itself runs "anyone" (CPAManifestScript).
			runner := spec.DepartmentRunners[name]
			manifest = agent.AgentManifestScript(spec.K3sVmid, spec.RelayWS, agents.SystemPrompt(name, purpose, spec.RepoURL, spec.operatorTZ()), spec.LitellmBaseURL, model, name, agent.KeySecretFor(spec.CpaName), spec.SelfURL, audience, "allowlist", spec.respondAllowlist(name), authTag, spec.operatorTZ(), runner...)
		}
		if err := spec.run(manifest, 420); err != nil {
			return "", fmt.Errorf("%s pod apply: %w", name, err)
		}
		// The CPA is the server's first-class caller: member it into this
		// server's own roster (the channel owner is this server, signing the
		// put-user with its secret) so its harness (via the mcp stdio bridge)
		// is authorized to call create/grant/manage — the same audited path the
		// build dogfoods. Idempotent on re-deploy.
		if name == spec.CpaName {
			authURL := spec.RelayAuthURL
			if authURL == "" {
				authURL = spec.RelayURL
			}
			// The agent-tools roster channel is OWNED by the agent-tools server,
			// so its put-user must be signed by THAT identity, not the console's
			// (the relay rejects a non-owner with "not a channel member"). The
			// identity lives on the same CP plane, so the console executor reads
			// it and signs; inside the agent-tools process it is the same key.
			sec, self := spec.Sec, spec.Audience
			if id, err := identity.Load(spec.agentToolsRoot()); err == nil {
				if s2, derr := hex.DecodeString(id.NostrSecretHex); derr == nil {
					if pk, perr := id.NostrPubkeyHex(); perr == nil {
						sec, self = s2, pk
					}
				}
			}
			if err := relay.PutUserAuth(spec.relayDial(), authURL, sec, self, pub); err != nil {
				return "", fmt.Errorf("member CPA into the agent-tools roster: %w", err)
			}
		}
		// The one-time #freehold welcome — the operator's first-run surface now
		// that the desktop app's own onboarding is skipped (stageOperatorProfile).
		// Best-effort: a transient miss self-heals on the next build's marker read.
		if name == spec.CpaName {
			if err := spec.postFreeholdWelcome(nSec); err != nil {
				fmt.Fprintln(os.Stderr, "WARN: the #freehold welcome message was not posted:", err)
			}
		}
		return pub, nil
	}
}

// channelNames normalizes a requested channel list: blank entries dropped,
// duplicates collapsed, and an empty list becomes the default freehold channel.
func channelNames(channels []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range channels {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return []string{"#freehold"}
	}
	return out
}

// cpaPubkey returns the CPA's Nostr pubkey from its durable identity dir, or ""
// when the CPA has not been created yet (a fresh world where stageCpa has not
// run). Used to add the CPA to every department channel.
func (s *Spec) cpaPubkey() string {
	return s.identityPubkey(s.cpaNameOrDefault())
}

// identityPubkey returns the Nostr pubkey of the agent named name from its
// durable identity dir, or "" when absent/unreadable.
func (s *Spec) identityPubkey(name string) string {
	id, err := identity.Load(filepath.Join(s.agentIdentityDir(), "agents", sanitizeDir(name)))
	if err != nil {
		return ""
	}
	pk, err := id.NostrPubkeyHex()
	if err != nil {
		return ""
	}
	return pk
}

// respondAllowlist renders the comma-separated pubkeys an agent pod's inbound
// author gate accepts (the manifest's BUZZ_ACP_RESPOND_TO_ALLOWLIST). A
// reserved department wakes for the operator + every core identity (the CPA +
// the four departments — itself included; buzz-acp ignores self-events); a
// custom agent wakes for its asker + the CPA. The asker is the operator today:
// create_agent is called by the CPA's harness and the CP cannot see chat
// threads. A missing core identity is skipped — reconcileAgentsInto pre-mints
// them, so a fresh first build still has them all.
func (s *Spec) respondAllowlist(name string) string {
	isDepartment := false
	for _, dep := range agents.DepartmentNames() {
		if dep == name {
			isDepartment = true
			break
		}
	}
	pubkeys := []string{s.OwnerPub, s.cpaPubkey()}
	if isDepartment {
		for _, dep := range agents.DepartmentNames() {
			pubkeys = append(pubkeys, s.identityPubkey(dep))
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, pk := range pubkeys {
		if pk == "" || seen[pk] {
			continue
		}
		seen[pk] = true
		out = append(out, pk)
	}
	return strings.Join(out, ",")
}

// cpaSecret returns the CPA's Nostr secret (32 bytes), or nil when its identity
// is absent/unreadable. #freehold is private and owned by the CPA, so its
// membership writes must be signed by this key, not the joining agent's.
func (s *Spec) cpaSecret() []byte {
	name := s.CpaName
	if name == "" {
		name = agent.DefaultCPAName
	}
	id, err := identity.Load(filepath.Join(s.agentIdentityDir(), "agents", sanitizeDir(name)))
	if err != nil {
		return nil
	}
	sec, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil {
		return nil
	}
	return sec
}

// AgentIdentityPath returns the durable identity dir for an agent named name
// under an agent-identity root (the layout BuildCreateAgentFn mints into).
// Exported for the CLI surfaces (registry/channel migrations) that must load
// the same identity the create path wrote.
func AgentIdentityPath(root, name string) string {
	return filepath.Join(root, "agents", sanitizeDir(name))
}

// sanitizeDir turns an agent name into a filesystem-safe identity dir name.
// sanitizeDir turns an agent name into a filesystem-safe identity dir name.
func sanitizeDir(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
