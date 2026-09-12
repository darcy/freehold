// Package cpbuild is the CP-owned world bring-up engine: it drives the CP's
// co-located runner through the world-build stages (plane, relay, agent-tools,
// k3s, DNS, litellm, caddy, cert). Shared by freehold-agent-tools (the /mcp
// world_build tool) and freehold-console (the CP executor) so the two run the
// SAME stages; the console is what a thin login box triggers and does NOT
// depend on the relay roster to authorize the build.
package cpbuild

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/delegate"
	"freehold/contract/relay"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/cli/flows"
	"freehold/platform/agents"
	"freehold/platform/migrations"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/drive"
	"freehold/platform/provisioning/planebase"
	"freehold/platform/provisioning/stages"
	"freehold/platform/services/certificates/letsencrypt"
	relaydeploy "freehold/platform/services/relay/buzz"
	caddydeploy "freehold/platform/services/webproxy/caddy"
)

const relayFreeholdChannel = "00000000-0000-4000-8000-00000000f0ef"

type Spec struct {
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
		if gw := stages.ParsePctGateway(out); gw != "" {
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
		if err := s.run(stages.DnsAddCmd(s.CpLxc, binDir, stateDir, r.Name, r.IP, r.Source, searchBase), 120); err != nil {
			return fmt.Errorf("world-build dns register %s: %w", r.Name, err)
		}
	}
	// The resolver WILDCARD: all *.searchBase -> the proxy (Caddy) edge, so the
	// dotted public hosts (relay.<base>, cp.<base>) resolve to TLS - never to a
	// guest LXC (dnsmasq's bare `relay`/`cp` records would otherwise leak the
	// guest IP into the FQDN answer and make the edge unreachable from the CP).
	if searchBase != "" && s.ProxyIP != "" {
		if err := s.run(stages.DnsApexCmd(s.CpLxc, binDir, stateDir, searchBase, config.StripCIDR(s.ProxyIP)), 120); err != nil {
			return fmt.Errorf("world-build dns apex: %w", err)
		}
	}
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
		pctSet, resolvConf := stages.DnsPointCmd(role.vmid, s.CpIP, r, searchBase)
		if err := s.run(pctSet, 60); err != nil {
			return fmt.Errorf("world-build dns point %s: %w", role.name, err)
		}
		if err := s.run(resolvConf, 60); err != nil {
			return fmt.Errorf("world-build dns point %s resolv.conf: %w", role.name, err)
		}
	}
	for _, q := range []struct{ name, want string }{
		{"relay", s.RelayIP}, {"litellm", s.LitellmIP},
	} {
		if q.want == "" {
			continue
		}
		if err := s.run(stages.DnsVerifyCmd(s.CpLxc, q.name, q.want), 30); err != nil {
			return fmt.Errorf("world-build dns verify %s: %w", q.name, err)
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
		action, err := bootstrap.ResolveProxmox(mc, s.RunnerTarget, false, nil)
		if err != nil {
			return nil, fmt.Errorf("storage resolve: %w", err)
		}
		if action.Kind != "Reuse" {
			return nil, fmt.Errorf("no storage backend to ensure onto: %s", action.Message)
		}
		if *action.Detected == planebase.ExistingZfs {
			kind = planebase.KindZfs
		} else {
			kind = planebase.KindLvmThin
		}
	}
	mounts := map[planebase.Tenant][]planebase.MountSpec{}
	for _, tenant := range []planebase.Tenant{planebase.TenantRelay, planebase.TenantCp, planebase.TenantK3sVolumes} {
		var ms []planebase.MountSpec
		switch kind {
		case planebase.KindZfs:
			ms, err = drive.ResolveTenantMounts(mc, s.RunnerTarget, s.PlanePool, s.RelayHost, tenant)
		case planebase.KindLvmThin:
			ms, err = drive.ResolveLvmMounts(mc, s.RunnerTarget, s.PlanePool, s.RelayHost, tenant, s.SizeGB, s.PoolSizeGB, s.ThinPool)
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
	hostname, err := bootstrap.DomainLXCName(s.RelayHost, role)
	if err != nil {
		return 0, err
	}
	spec := &bootstrap.ProxmoxLxcSpec{
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
	if role == "k3s" && s.ProxyIP != "" {
		// pct net0 wants CIDR (host/prefix); the serve flag carries the bare
		// proxy IP (the DNS/caddy consumers expect bare), so rebuild the CIDR
		// — the recorded proxy world is a /24 home LAN (default relay-gw).
		ip := s.ProxyIP
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
	res, err := bootstrap.BootstrapProxmoxLxc(mc, s.RunnerTarget, spec)
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
// (DomainLXCName) on the host, so the DNS/caddy/litellm/cert steps address the
// REAL vmids world_build just booted.
func (s *Spec) resolveGuestVmids() {
	for _, r := range []struct {
		role string
		vmid *uint32
	}{
		{"relay", &s.RelayLxc}, {"cp", &s.CpLxc}, {"k3s", &s.K3sVmid},
	} {
		if *r.vmid != 0 {
			continue
		}
		name, err := bootstrap.DomainLXCName(s.RelayHost, r.role)
		if err != nil {
			continue
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
	if _, err := relaydeploy.DeployRelay(mc, s.RunnerTarget, &relaydeploy.RelayDeploySpec{
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
	if relayIP == "" && s.RelayLxc != 0 {
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
	// Seed the server's channel + the operator + this console into its roster
	seedFlags := fmt.Sprintf("%s seed --state-dir %s --relay-url %s --granted %s,%s --name agent-tools",
		bin, atState, relayDial, s.OwnerPub, s.Audience)
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
	serveFlags := fmt.Sprintf(
		"--state-dir %s --addr 0.0.0.0:8089 --relay-url %s --relay-pubkey %s --relay-lxc %d --relay-compose %s --k3s-vmid %d --runner-addr %s --runner-pubkey %s --runner-target %s --cpa-name %s --owner-pubkey %s --self-url %s",
		atState, relayDial, s.RelayPK, s.RelayLxc, s.RelayCompose, s.K3sVmid,
		s.RunnerAddr, s.RunnerPK, s.RunnerTarget, s.CpaName, s.OwnerPub, s.SelfURL)
	if s.RelayAuthURL != "" {
		serveFlags += " --relay-auth-url " + s.RelayAuthURL
	} else {
		serveFlags += " --relay-auth-url " + s.RelayURL
	}
	if s.RelayWS != "" {
		serveFlags += " --relay-ws " + s.RelayWS
	}
	if s.LitellmBaseURL != "" {
		serveFlags += " --litellm-base " + s.LitellmBaseURL
	}
	if s.PlanePool != "" {
		serveFlags += " --plane-pool " + s.PlanePool
	}
	if s.PlaneKind != "" {
		serveFlags += " --plane-kind " + s.PlaneKind
	}
	if s.ThinPool != "" {
		serveFlags += " --thin-pool " + s.ThinPool
	}
	if s.SizeGB != 0 {
		serveFlags += " --size-gb " + strconv.FormatUint(s.SizeGB, 10)
	}
	if s.PoolSizeGB != 0 {
		serveFlags += " --pool-size-gb " + strconv.FormatUint(s.PoolSizeGB, 10)
	}
	if s.RootfsGB != 0 {
		serveFlags += " --rootfs-gb " + strconv.FormatUint(uint64(s.RootfsGB), 10)
	}
	if s.MemoryMB != 0 {
		serveFlags += " --memory-mb " + strconv.FormatUint(uint64(s.MemoryMB), 10)
	}
	if s.StorageName != "" {
		serveFlags += " --storage " + s.StorageName
	}
	if s.RelayGW != "" {
		serveFlags += " --relay-gw " + s.RelayGW
	}
	if s.Bridge != "" {
		serveFlags += " --bridge " + s.Bridge
	}
	if s.SelfURL != "" {
		serveFlags += " --self-url " + s.SelfURL
	}
	if s.CpLxc != 0 {
		serveFlags += " --cp-lxc " + strconv.FormatUint(uint64(s.CpLxc), 10)
	}
	if s.ProxyIP != "" {
		serveFlags += " --proxy-ip " + s.ProxyIP
	}
	if s.LitellmIP != "" {
		serveFlags += " --litellm-ip " + s.LitellmIP
	}
	if s.RelayHost != "" {
		serveFlags += " --relay-host " + s.RelayHost
	}
	if s.RelayIP != "" {
		serveFlags += " --relay-ip " + s.RelayIP
	}
	if s.CpHost != "" {
		serveFlags += " --cp-host " + s.CpHost
	}
	if s.CpIP != "" {
		serveFlags += " --cp-ip " + s.CpIP
	}
	start := fmt.Sprintf(
		"pct exec %d -- sh -c 'setsid nohup %s serve %s >> %s/serve.log 2>&1 < /dev/null & echo $! | tee %s/serve.pid'",
		s.CpLxc, bin, serveFlags, atState, atState)
	if err := s.run(start, 30); err != nil {
		return fmt.Errorf("start agent-tools: %w", err)
	}
	// Poll until the serve endpoint answers (any HTTP code proves the listener
	// is up; "000" means not yet bound).
	up := false
	for i := 0; i < 15; i++ {
		code, err := s.runOut(fmt.Sprintf("pct exec %d -- curl -s -m 3 -o /dev/null -w %%{http_code} http://127.0.0.1:8089/mcp", s.CpLxc), 15)
		if err == nil && strings.TrimSpace(code) != "000" {
			up = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !up {
		return fmt.Errorf("agent-tools serve did not answer within the poll window — check %s/serve.log", atState)
	}
	return nil
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

// issueCert runs the resumable DNS-01 issuance IN-PROCESS for one slot's host
// (lego via internal/cert; the sealed DNS cred opened in memory), so a re-run
// after a timeout RESUMES the same order instead of re-challenging.
func (s *Spec) issueCert(slot, host, provider string, env map[string]string) (*cert.Issued, error) {
	secret, err := s.consoleEncSecret()
	if err != nil {
		return nil, err
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return nil, err
	}
	dp, err := cert.NewDNSProvider(provider, env)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if !ok {
		po, err = resume.Begin()
		if err != nil {
			return nil, err
		}
	}
	issued, err := resume.Resolve(po)
	if err != nil {
		// Only the terminal "authorization invalid" state justifies discarding
		// the pending resumable order: resuming it can never succeed. A transient
		// failure (a polling timeout, a flaky network read) must KEEP the order so
		// the next run resumes the same order + challenge instead of minting a new
		// ACME order (and risking rate limits) each time.
		if errors.Is(err, cert.ErrAuthInvalid) {
			_ = os.Remove(statePath)
		}
		return nil, err
	}
	return issued, nil
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
// DNS-01 issue, then install the FRESH pair through the co-located runner with
// the key file-transited (never a stale runner-package key).
func (s *Spec) worldCert() error {
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
		issued, err := s.issueCert(sl.slot, sl.host, provider, env)
		if err != nil {
			return fmt.Errorf("cert %s issue: %w", sl.slot, err)
		}
		if err := s.installCaddyCertFile(s.K3sVmid, sl.slot, issued.Fullchain, issued.Key); err != nil {
			return fmt.Errorf("cert %s install: %w", sl.slot, err)
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
		spec.resolveGuestVmids()
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
		spec.resolveGuestVmids()
		spec.refreshGuestIPs()
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
		// 4. The CP-owned resolver: register the split-horizon names (bare
		// guests + the dotted public hosts via the proxy) and point every guest
		// at the CP as its nameserver, then verify the resolver actually ANSWERS
		// (dnsmasq served the records, not merely tcp/53 open). No secrets.
		if spec.CpLxc != 0 && spec.CpIP != "" {
			if err := spec.worldDNS(); err != nil {
				return "", err
			}
			report = append(report, "dns register/point applied")
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
		// 7.5. Record the world-service health coords (k3s/litellm/caddy) so any
		// management box renders the live world through /api/world.
		if spec.CpLxc != 0 && spec.CpIP != "" {
			if err := spec.worldServices(); err != nil {
				return "", err
			}
			report = append(report, "world-service coords recorded")
		}
		if len(report) == 0 {
			return "", fmt.Errorf("world-build: no world coords recorded (k3s vmid / relay lxc)")
		}
		return strings.Join(report, "\n"), nil
	}
}

// BuildCreateAgentFn returns the create-agent deploy: mint a durable identity
// on the CP, add it as a relay member, seat it in #freehold, apply its pod
// through the co-located runner, and hand the minted pubkey to Tools.CreateAgent
// BuildMigrator wires the CP's verify-gated migration runner (Step 7): a
// durable ledger at <stateDir>/migrations.json (backed up with the CP plane),
// running the versioned migration SCRIPTS (platform/migrations/files/<epoch>.sh
// + <epoch>.verify.sh, OMARCY-style: one timestamped .sh per migration).
// Each pending migration runs in ascending epoch order through bash on the CP
// (where the data it operates on lives); done only when its verify gate passes.
// The scripts receive the durable-plane paths + the freehold-agent-tools binary
// via env (FREEHOLD_AGENT_TOOLS / REGISTRY / CONSOLE_STATE) — never argv, so no
// credential crosses the audit.
func BuildMigrator(spec *Spec, consoleStateDir string) agent.Migrator {
	return func() ([]migrations.Result, error) {
		st, err := migrations.Open(filepath.Join(spec.StateDir, "migrations.json"))
		if err != nil {
			return nil, err
		}
		scripts, err := migrations.Scripts()
		if err != nil {
			return nil, fmt.Errorf("enumerate migration scripts: %w", err)
		}
		binDir, _ := spec.cpGuestDirs()
		runEnv := append(os.Environ(),
			"FREEHOLD_AGENT_TOOLS="+filepath.Join(binDir, "freehold-agent-tools"),
			"REGISTRY="+filepath.Join(spec.StateDir, "registry.json"),
			"CONSOLE_STATE="+consoleStateDir,
		)
		run := func(epoch, body string) error {
			dir := filepath.Join(spec.StateDir, "migrations", "files")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			path := filepath.Join(dir, epoch+".sh")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				return err
			}
			cmd := exec.Command("bash", path)
			cmd.Env = runEnv
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w: %s", path, err, strings.TrimSpace(string(out)))
			}
			return nil
		}
		all := make([]migrations.Migration, 0, len(scripts))
		for _, s := range scripts {
			all = append(all, s.Migration(func(body string) error { return run(s.Epoch, body) }))
		}
		return st.Run(all)
	}
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

// (which registers the registry row). Branches to the CPA manifest/prompt when
// the name is the CPA's, so stageCpa's dogfooded create_agent produces the CPA.
func BuildCreateAgentFn(spec *Spec) agent.CreateAgentFn {
	return func(name, purpose string) (string, error) {
		if name == "" {
			return "", fmt.Errorf("create-agent needs a non-empty name")
		}
		// The k3s vmid (where the pod manifests apply) may be 0 for the server
		// deployed BEFORE k3s was booted (the console world_build booted it);
		// resolve it by hostname so apply targets the real guest.
		if spec.K3sVmid == 0 {
			spec.resolveGuestVmids()
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
		dir := filepath.Join(spec.StateDir, "agents", sanitizeDir(name))
		if _, err := agent.EnsureIdentity(dir); err != nil {
			return "", fmt.Errorf("mint %s identity: %w", name, err)
		}
		id, err := flows.LoadIdentity(dir)
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
			spec.resolveGuestVmids()
			relayLxc = spec.RelayLxc
		}
		cmdLine := fmt.Sprintf("cd %s && docker compose exec -T relay buzz-admin add-member --pubkey %s", spec.RelayCompose, pub)
		full := fmt.Sprintf("pct exec %d -- sh -c '%s'", relayLxc, cmdLine)
		if err := spec.run(full, 120); err != nil {
			return "", fmt.Errorf("add relay member %s: %w", pub, err)
		}

		// Profile + #freehold channel + join, signed by the agent (NIP-98 against the
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
		if err := delegate.EnsureChannelAuth(spec.RelayURL, authURL, nSec, relayFreeholdChannel, "#freehold"); err != nil {
			return "", fmt.Errorf("ensure #freehold channel: %w", err)
		}
		if err := relay.JoinChannelAuth(spec.RelayURL, authURL, nSec, relayFreeholdChannel); err != nil {
			return "", fmt.Errorf("join #freehold channel: %w", err)
		}

		if err := spec.run(agent.AgentIdentityScript(spec.K3sVmid, id.NostrSecretHex, spec.OwnerPub, name), 120); err != nil {
			return "", fmt.Errorf("%s identity secret: %w", name, err)
		}
		var manifest string
		if name == spec.CpaName {
			manifest = agent.CPAManifestScript(spec.K3sVmid, spec.RelayWS, agents.CPASystemPrompt, name, spec.LitellmBaseURL, "", spec.SelfURL, spec.Audience)
		} else {
			manifest = agent.AgentManifestScript(spec.K3sVmid, spec.RelayWS, agents.AgentSystemPrompt(name, purpose), spec.LitellmBaseURL, agent.CpaLiteLLMModel, name, agent.KeySecretFor(spec.CpaName), spec.SelfURL, spec.Audience)
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
			if err := relay.PutUserAuth(spec.RelayURL, authURL, spec.Sec, spec.Audience, pub); err != nil {
				return "", fmt.Errorf("member CPA into the agent-tools roster: %w", err)
			}
		}
		return pub, nil
	}
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
