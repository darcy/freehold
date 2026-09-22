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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"freehold/agents"
	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/delegate"
	"freehold/contract/identity"
	"freehold/contract/relay"
	"freehold/contract/wire"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
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
	RunnerAddr     string
	RunnerPK       string
	RunnerTarget   string
	CpaName        string
	OwnerPub       string
	LitellmBaseURL string
	Sec            []byte
	Audience       string
	SelfURL        string
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
	for _, r := range stages.DnsRecords(s.RelayHost, s.RelayIP, s.CpHost, s.CpIP, s.ProxyIP, s.LitellmIP) {
		if err := s.run(proxmox.DnsAddCmd(s.CpLxc, binDir, stateDir, r.Name, r.IP, r.Source, searchBase), 120); err != nil {
			return fmt.Errorf("world-build dns register %s: %w", r.Name, err)
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
	for _, q := range []struct{ name, want string }{
		{"relay", s.RelayIP}, {"litellm", s.LitellmIP},
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
	if vmid != 0 {
		spec.VMID = &vmid
	}
	roleIP := ""
	switch role {
	case "k3s":
		roleIP = s.ProxyIP
	case "relay":
		roleIP = s.RelayIP
	case "cp":
		roleIP = s.CpIP
	}
	if roleIP != "" {
		// pct net0 wants CIDR (host/prefix); the serve/recorded values carry the
		// bare IP (the DNS/caddy consumers expect bare), so rebuild the CIDR —
		// the LAN defaults to /24 home-labs. Assigning relay/CP static addresses
		// (via --relay-ip/--cp-ip) runs them OFF DHCP, which avoids exhausting a
		// small LAN DHCP pool across repeated teardown/build cycles.
		ip := roleIP
		if !strings.Contains(ip, "/") {
			ip += "/24"
		}
		gw := s.RelayGW
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
func (s *Spec) resolveGuestVmids() error {
	for _, r := range []struct {
		role string
		vmid *uint32
	}{
		{"relay", &s.RelayLxc}, {"cp", &s.CpLxc}, {"k3s", &s.K3sVmid},
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
		{s.K3sVmid, &s.ProxyIP},
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
	// Resolve the relay's current IP (bootstrap did not boot the relay; this
	// world_build just did).
	relayIP := s.RelayIP
	if s.RelayLxc != 0 {
		// Always re-read the relay's CURRENT DHCP lease: a prior cycle's recorded
		// IP can go stale (the relay LXC can come back on a different .30.x lease
		// after a teardown+rebuild), and pinning the seed against a dead IP makes
		// the agent-tools roster seed fail with "no route to host". Fall back to
		// the recorded value only if the live read yields nothing.
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
		pin := fmt.Sprintf("pct exec %d -- sh -c \"grep -Fq '%s' /etc/hosts 2>/dev/null || echo '%s %s' >> /etc/hosts\"", s.CpLxc, s.RelayHost, relayIP, s.RelayHost)
		if err := s.run(pin, 30); err != nil {
			return fmt.Errorf("pin relay host into cp: %w", err)
		}
	}
	relayDial := "http://" + s.RelayHost + ":3000"
	// Seed the server's channel + the operator into its roster. The console's
	// driving identity (s.Audience) is deliberately NOT seeded: the console
	// never calls this MCP (it reads the registry/facts files directly), so
	// membering it only put an un-nameable identity in the roster. The CPA is
	// membered separately, by this server's own identity (BuildCreateAgentFn).
	seedFlags := fmt.Sprintf("%s seed --state-dir %s --relay-url %s --granted %s --name agent-tools",
		bin, atState, relayDial, s.OwnerPub)
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
	file := filepath.Join(s.StateDir, "console", "identity.json")
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
				caddyfile := caddydeploy.RenderCaddyfile(spec.RelayHost, relayUpstream, spec.CpHost, cpUpstream, cpMcpUpstream)
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
				// own write lock and re-reads the file when it is done: neither half
				// is sufficient alone. See Registry.WithRegistryLocked.
				if err := spec.AgentRegistry.WithRegistryLocked(func() error {
					report = spec.appendMigrations(report)
					return nil
				}); err != nil {
					report = append(report, "WARN: migrations re-read: "+err.Error())
				}
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
				if err := spec.startAgentTools(); err != nil {
					return "", fmt.Errorf("world-build agent-tools reload: %w", err)
				}
			}
		}
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
		er := &teardown.ExecRunner{}
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

// migrationRunner returns the queue closure: every pending script under root,
// ascending, each with the durable-plane paths + relay coords in its env.
func (s *Spec) migrationRunner(root, consoleStateDir string) func() ([]migrations.Result, error) {
	return func() ([]migrations.Result, error) {
		binDir, _ := s.cpGuestDirs()
		relayAuthURL := s.RelayAuthURL
		if relayAuthURL == "" {
			relayAuthURL = s.RelayURL
		}
		runEnv := append(os.Environ(),
			"FREEHOLD_AGENT_TOOLS="+filepath.Join(binDir, "freehold-agent-tools"),
			"REGISTRY="+filepath.Join(s.StateDir, "registry.json"),
			"CONSOLE_STATE="+consoleStateDir,
			"STATE_DIR="+s.StateDir,
			"FREEHOLD_RELAY_URL="+s.RelayURL,
			"FREEHOLD_RELAY_AUTH_URL="+relayAuthURL,
			"FREEHOLD_CPA_NAME="+s.cpaNameOrDefault(),
		)
		return migrations.Run(root, func(_, path string) error {
			return s.runMigrationScript(path, runEnv, migrationScriptTimeout, migrationWaitDelay)
		})
	}
}

// runMigrationScript runs one shipped script and returns its failure with output
// attached. Both bounds exist because the world_build path invokes the queue WHILE
// HOLDING the registry's write lock (Registry.WithRegistryLocked): an unbounded script
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
	if strings.TrimSpace(channel) == "" {
		if err := delegate.EnsurePrivateChannelAuth(s.RelayURL, authURL, nSec, relayFreeholdChannel, "#freehold"); err != nil {
			return "", "", false, fmt.Errorf("ensure #freehold channel: %w", err)
		}
		return relayFreeholdChannel, "#freehold", false, nil
	}
	name := "#" + strings.TrimPrefix(strings.TrimSpace(channel), "#")
	// The shared channel has a FIXED id and is always private: never recreate it
	// under the name-derived id, and never as open, even when a create passes
	// private=false.
	if strings.EqualFold(name, "#freehold") {
		if err := delegate.EnsurePrivateChannelAuth(s.RelayURL, authURL, nSec, relayFreeholdChannel, "#freehold"); err != nil {
			return "", "", false, fmt.Errorf("ensure #freehold channel: %w", err)
		}
		return relayFreeholdChannel, "#freehold", false, nil
	}
	// A relay read error must NOT be mistaken for "absent" (that would create a
	// duplicate of an existing channel) — fail the create instead. The lookup
	// signs as the CREATING agent (nSec), the identity that owns/joins the
	// channel: a private channel the console identity is not a member of must
	// still be found, or a rebuild would create a duplicate.
	existingID, existingName, ok, err := relay.FindChannelAuth(s.RelayURL, authURL, nSec, channel)
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
	if err := create(s.RelayURL, authURL, nSec, id, name); err != nil {
		return "", "", false, fmt.Errorf("create channel %s: %w", name, err)
	}
	return id, name, true, nil
}

// BuildCreateAgentFn returns the create-agent deploy: mint a durable identity on
// the CP, add it as a relay member, publish its profile, ensure each named channel
// (private when asked), apply its pod through the co-located runner, and hand the
// minted pubkey to Tools.CreateAgent (which registers the registry row). Branches
// to the CPA manifest/prompt when the name is the CPA's, so stageCpa's dogfooded
// create_agent produces the CPA.
func BuildCreateAgentFn(spec *Spec) agent.CreateAgentFn {
	return func(name, purpose string, channels []string, private bool) (string, error) {
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
		if err := relay.PublishProfileAuth(spec.RelayURL, authURL, nSec, name, "freehold agent"); err != nil {
			return "", fmt.Errorf("publish %s profile: %w", name, err)
		}
		// Each named channel is resolved (created, owned by this agent, when
		// absent) and joined; the operator is added to each. Empty list = the
		// default freehold channel. After the channels exist the CPA is added to
		// each so the system's main touchpoint sees every department
		// (agents.DepartmentChannels gives a department #freehold + #freehold-<name>).
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
			if channelID == relayFreeholdChannel && name != spec.CpaName {
				cpaSec := spec.cpaSecret()
				if len(cpaSec) != 32 {
					return "", fmt.Errorf("add %s to #freehold: the CPA identity is unavailable to sign the membership (a private #freehold admits members only through its owner)", name)
				}
				if err := relay.PutUserChannelAuth(spec.RelayURL, authURL, cpaSec, channelID, pub); err != nil {
					return "", fmt.Errorf("add %s to #freehold: %w", name, err)
				}
				if spec.OwnerPub != "" {
					if err := relay.PutUserChannelAuth(spec.RelayURL, authURL, cpaSec, channelID, spec.OwnerPub); err != nil {
						return "", fmt.Errorf("add operator to #freehold: %w", err)
					}
				}
				joined = append(joined, channelRef{channelID, channelName})
				continue
			}
			// JOIN it (open channels allow free joins; a private one may refuse —
			// best-effort), then try to ADD the operator (the agent owns a channel
			// it created; it may not own a pre-existing one).
			_ = relay.JoinChannelAuth(spec.RelayURL, authURL, nSec, channelID)
			if spec.OwnerPub != "" {
				perr := relay.PutUserChannelAuth(spec.RelayURL, authURL, nSec, channelID, spec.OwnerPub)
				if perr != nil && created {
					return "", fmt.Errorf("add operator to %s: %w", channelName, perr)
				}
			}
			joined = append(joined, channelRef{channelID, channelName})
		}
		if name != spec.CpaName {
			if cpaPub := spec.cpaPubkey(); cpaPub != "" {
				for _, ref := range joined {
					// Best-effort: the created agent signs, so it lands on a
					// channel it owns (its own #freehold-<name>); the CPA is
					// already the owner/member of #freehold — skip.
					if ref.id == relayFreeholdChannel {
						continue
					}
					_ = relay.PutUserChannelAuth(spec.RelayURL, authURL, nSec, ref.id, cpaPub)
				}
			}
		}

		if err := spec.run(agent.AgentIdentityScript(spec.K3sVmid, id.NostrSecretHex, spec.OwnerPub, name), 120); err != nil {
			return "", fmt.Errorf("%s identity secret: %w", name, err)
		}
		var manifest string
		if name == spec.CpaName {
			manifest = agent.CPAManifestScript(spec.K3sVmid, spec.RelayWS, agents.CPASystemPrompt(spec.RepoURL), name, spec.LitellmBaseURL, "", spec.SelfURL, spec.Audience)
		} else {
			// A reserved department name selects that department's embedded
			// prompt; any other name renders the custom template (agents.SystemPrompt).
			manifest = agent.AgentManifestScript(spec.K3sVmid, spec.RelayWS, agents.SystemPrompt(name, purpose, spec.RepoURL), spec.LitellmBaseURL, agent.CpaLiteLLMModel, name, agent.KeySecretFor(spec.CpaName), spec.SelfURL, spec.Audience)
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
			if err := relay.PutUserAuth(spec.RelayURL, authURL, sec, self, pub); err != nil {
				return "", fmt.Errorf("member CPA into the agent-tools roster: %w", err)
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
	name := s.CpaName
	if name == "" {
		name = agent.DefaultCPAName
	}
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
