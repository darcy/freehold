// Package config reproduces the freehold installer's connection profile
// (~/.config/freehold/config.toml): the desired state of the world.
// Present + converged -> running mode; present + not -> configure; absent ->
// bootstrap. Ported from installer/src/config.rs.
package config

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"net/netip"
)

// Config is the connection profile / desired world state.
//
// There is NO world/base domain anymore: the relay and control‑plane hosts are
// separate literal hostnames (RelayURL/CPURL), and everything sits behind a
// single static proxy IP (Proxy.Ip — the Caddy edge on the k3s node), which
// both hosts resolve to. Relay/CP LXCs are DHCP, behind the proxy.
type Config struct {
	RelayURL    string  `toml:"relay_url"`
	RelayWsURL  string  `toml:"relay_ws_url,omitempty"`
	RelayPubkey *string `toml:"relay_pubkey,omitempty"`
	CPURL       string  `toml:"cp_url"`
	// Name is the world/profile name this config was installed under — the
	// profile dir name. It prefixes LXC hostnames (<name>-<role>) for worlds
	// installed with one; empty falls back to the domain-derived names.
	Name string `toml:"name,omitempty"`
	// Host is the environment address install reached the substrate at (e.g.
	// root@192.168.30.224). Persisted so `uninstall --name` resolves the host
	// without a --host flag, and so a re-install after a local wipe can name
	// the substrate it cannot read without host access.
	Host string `toml:"host,omitempty"`
	// AccessMode is the install access strategy that reached Host —
	// "ssh-root-proxmox" today; provider-API modes (api-vultr) later. The
	// installers differ only here; they all end at the same normalized CP.
	AccessMode string `toml:"access_mode,omitempty"`
	// CpPubkey is the control plane's own Nostr pubkey — the box's trust anchor
	// for a CP it has never met. Recorded by `freehold login` (adopted from the
	// CP's own `/api/world` report — the operator never supplies it) and by build
	// once the CP is minted; it identifies the CP itself, never who logs in.
	CpPubkey         string      `toml:"cp_pubkey,omitempty"`
	OperatorPubkey   string      `toml:"operator_pubkey"`
	OperatorIdentity *string     `toml:"operator_identity,omitempty"`
	Runner           RunnerRef   `toml:"runner"`
	Proxy            ProxySpec   `toml:"proxy"`
	Lxc              LxcSpec     `toml:"lxc"`
	Plane            PlaneSpec   `toml:"plane,omitempty"`
	Dns              DnsSpec     `toml:"dns,omitempty"`
	Litellm          LitellmSpec `toml:"litellm,omitempty"`
	Caddy            CaddySpec   `toml:"caddy,omitempty"`
	Gateway          GatewaySpec `toml:"gateway,omitempty"`
	HostProvider     HostSpec    `toml:"host_provider,omitempty"`
	CPAName          string      `toml:"cpa_name,omitempty"`
	// OperatorName is the operator's display name in Buzz (asked at install);
	// published as the operator's kind:0 profile at build.
	OperatorName     string      `toml:"operator_name,omitempty"`
	Managed          []string    `toml:"managed"`
	// AgentTools is the CP's freehold-agent-tools MCP server (create/grant/
	// manage-agent), recorded once deployed so the build (stageCpa + reconcile)
	// and later the CPA call it over the shared signed-header MCP surface.
	AgentToolsURL    string `toml:"agent_tools_url,omitempty"`
	AgentToolsPubkey string `toml:"agent_tools_pubkey,omitempty"`
}

// ProxySpec is the single static address in the world: the EDGE every public
// host resolves to. With a gateway (GatewaySpec set) that is the gateway
// guest's LAN address; without one it is the proxy (Caddy) node itself.
// CIDR form.
type ProxySpec struct {
	Ip *string `toml:"ip,omitempty"`
}

// GatewaySpec is the freehold-subnet gateway (docs/NETWORK.md): the
// one guest the surrounding network sees. Set (non-nil Cidr), the world's
// other guests are born on an internal subnet behind it — static IPs off
// InternalIPFor, default route through GatewayInternalIP, outbound NAT and
// 80/443/6443 forwards on the gateway. Cidr is the internal subnet (e.g.
// 10.77.0.0/24); Vlan is the tag on the PVE bridge (stays in-host — the
// router never sees it; 0/absent = untagged bridge).
type GatewaySpec struct {
	Cidr *string `toml:"cidr,omitempty"`
	Vlan *int    `toml:"vlan,omitempty"`
}

// GatewayInternalIP returns the gateway guest's INTERNAL address: the first
// usable host of the internal CIDR (x.x.x.1).
func GatewayInternalIP(cidr string) string { return nthIP(cidr, 1) }

// InternalIPFor returns the deterministic internal address for a guest role
// on the internal CIDR: relay .11, cp .12, k3s .13.
func InternalIPFor(cidr, role string) string {
	switch role {
	case "relay":
		return nthIP(cidr, 11)
	case "cp":
		return nthIP(cidr, 12)
	case "k3s":
		return nthIP(cidr, 13)
	}
	return ""
}

// nthIP returns the nth usable host address of a CIDR (n=1 = the address
// right after the network address). "" when the CIDR does not parse.
func nthIP(cidr string, n int) string {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() {
		return ""
	}
	network, perr := netip.AddrFrom4(p.Addr().As4()).Prefix(p.Bits())
	if perr != nil {
		return ""
	}
	b := network.Addr().As4()
	sum := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	sum += uint32(n)
	return netip.AddrFrom4([4]byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)}).String()
}

// GatewayNftConf renders the gateway guest's nftables ruleset: masquerade for
// the internal subnet, 80/443 (tcp+udp) DNAT to the Caddy edge, 6443 to the
// kube-apiserver, 8080 to the CP console (the box's pre-Caddy build path —
// the console is NIP-98-gated; the flat-LAN world exposed it identically on
// the CP's LAN IP), 3000 to the relay (its LAN-dial port — the CP's and the
// pods' http://<relay-host>:3000 dials resolve through the resolver's
// bare-record shadowing in unreliable order, so BOTH answers must work).
// The DNATs match EITHER ingress (the internal guests hairpin through the
// gateway — a k3s pod's wss:// to the public FQDN arrives on eth1) but ONLY
// traffic ADDRESSED TO THE GATEWAY (edgeIP): an unconstrained dport DNAT
// hijacks every transit :443 — the guests' own egress to the internet
// included — and serves them the edge's TLS. The masquerade drops the
// egress-interface pin for the same reason (the hairpin reply must NAT back
// to the gateway, or the guest talks to itself).
// Shared by the box boot stage and the CP build's re-assert — one renderer,
// never two diverging rule sets. wanIf is the gateway's public-side
// interface (eth0).
func GatewayNftConf(cidr, edgeIP, k3sIP, cpIP, relayIP, wanIf string) string {
	return fmt.Sprintf(`flush ruleset
table ip freehold {
	chain forward {
		type filter hook forward priority 0; accept;
	}
	chain postrouting {
		type nat hook postrouting priority srcnat;
		ip saddr %s masquerade
	}
	chain prerouting {
		type nat hook prerouting priority dstnat;
		ip daddr %s tcp dport { 80, 443 } dnat to %s
		ip daddr %s udp dport { 80, 443 } dnat to %s
		ip daddr %s tcp dport 6443 dnat to %s:6443
		ip daddr %s tcp dport 8080 dnat to %s:8080
		ip daddr %s tcp dport 3000 dnat to %s:3000
	}
}
`, cidr, edgeIP, k3sIP, edgeIP, k3sIP, edgeIP, k3sIP, edgeIP, cpIP, edgeIP, relayIP)
}

// GatewayDnsmasqConf renders the gateway's resolver config: the internal
// subnet's forwarder (the guests' resolv.conf points at the gateway; without
// a listener the CP's own upstream lookups — Cloudflare API, image pulls —
// die on a refused 10.77.0.1:53). upstream is the LAN router; a public
// fallback rides behind it.
func GatewayDnsmasqConf(upstream string) string {
	return fmt.Sprintf(`interface=eth1
bind-interfaces
no-resolv
server=%s
server=1.1.1.1
`, upstream)
}

// TenantSlug is a stable filesystem/LXC slug for the world, derived from the
// RELAY host (never derived from a base domain — there is none).
func (c *Config) TenantSlug() string { return slugFromHost(c.RelayHost(), "") }

// Slug is the per-role slug (relay/cp/k3s), each built from the RELAY host plus
// the role suffix, so mounts/LXC names survive compute-only rebuilds.
func (c *Config) Slug(role string) string { return slugFromHost(c.RelayHost(), role) }

// slugFromHost lowercases the host and maps every character outside [a-z0-9-]
// (dots, etc.) to '-', collapsing runs, then appends "-"+role when non-empty.
func slugFromHost(host, role string) string {
	var b []byte
	lastDash := false
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z':
			b = append(b, c)
			lastDash = false
		case c >= 'A' && c <= 'Z':
			b = append(b, c+('a'-'A'))
			lastDash = false
		case c >= '0' && c <= '9' || c == '-':
			b = append(b, c)
			lastDash = false
		default:
			if !lastDash {
				b = append(b, '-')
				lastDash = true
			}
		}
	}
	s := strings.Trim(string(b), "-")
	if role != "" {
		s = s + "-" + role
	}
	return s
}

// SlugHost lowercases/consolidates an arbitrary host into a stable profile
// dir / LXC-safe slug (the exported form of slugFromHost).
func SlugHost(host string) string { return slugFromHost(host, "") }

// PlaneSpec is the durable volume plane (Phase 0.12).
type PlaneSpec struct {
	Backend     *string `toml:"backend,omitempty"`
	BackendKind *string `toml:"backend_kind,omitempty"`
	// ThinPool names a thin pool FREEHOLD CREATED (LVM-thin backend). Set
	// only by the carve branch of the rebuild placement gate; a REUSED
	// stock pool (pve/data) is never recorded here. Teardown --data removes
	// exactly this pool and nothing else.
	ThinPool *string `toml:"thin_pool,omitempty"`
	Mounts   map[string][]PlaneMount `toml:"mounts,omitempty"`
	// Guest size/placement as the operator set them at install — persisted so
	// a later world-config render (an UPDATE's) carries them: the update's
	// own flags never do, and a blank spec made the next guest-create fail
	// the substrate tool's parameter validation.
	SizeGB     uint32 `toml:"size_gb,omitempty"`
	PoolSizeGB uint32 `toml:"pool_size_gb,omitempty"`
	RootfsGB   uint32 `toml:"rootfs_gb,omitempty"`
	MemoryMB   uint32 `toml:"memory_mb,omitempty"`
	Storage    string `toml:"storage,omitempty"`
	Bridge     string `toml:"bridge,omitempty"`
	RelayGW    string `toml:"relay_gw,omitempty"`
}

// PlaneMount is one resolved durable-plane mount (HOST source + guest path).
type PlaneMount struct {
	Source    string `toml:"source"`
	GuestPath string `toml:"guest_path"`
}

// CoLocatedRunnerMCPAddr is the loopback MCP address the CP deploy (bootstrap-cp)
// boots its co-located runner on INSIDE the CP LXC guest, and the address the
// console's world-build must dial from that same guest netns. The deploy adopts
// the runner with --mcp-addr 127.0.0.1:8787 and the runner (no explicit serve
// --addr) defaults to the same loopback; box-side runners ride a different host
// loopback, so the console must target THIS guest address, not cfg.Runner.Addr.
const CoLocatedRunnerMCPAddr = "127.0.0.1:8787"

// RunnerRef identifies the provisioning runner.
type RunnerRef struct {
	Addr   string `toml:"addr"`
	Pubkey string `toml:"pubkey"`
	Target string `toml:"target"`
}

// RelayLanDial renders the relay's LAN dial URL (pre-Caddy): the relay's own
// hostname on the LAN HTTP port. A host already carrying a port is used
// verbatim (it names its own dial port). Empty host => empty URL. Buzz keys
// the community to the HOST header, so the dial must be the relay's hostname,
// never a raw IP.
func RelayLanDial(host string) string {
	host = strings.TrimSuffix(strings.TrimSpace(host), "/")
	if host == "" {
		return ""
	}
	if strings.Contains(host, ":") {
		return "http://" + host
	}
	return "http://" + host + ":3000"
}

// RelayHost returns the relay's own public host (its Buzz origin) from
// RelayURL — never derived; it is whatever the operator chose.
func (c *Config) RelayHost() string {
	return urlHost(c.RelayURL)
}

// CPHost returns the control plane's own public host from CPURL.
func (c *Config) CPHost() string {
	return urlHost(c.CPURL)
}

// urlHost strips scheme+path, returning the bare host from a base URL.
func urlHost(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(u, "/:"); i >= 0 {
		u = u[:i]
	}
	return u
}

// LxcSpec holds the managed LXC coordinates. Relay/Cp are DHCP (their Ip is
// the DISCOVERED address, populated after boot — the static is Proxy.Ip); K3s
// carries no Ip because the k3s node's address IS Proxy.Ip (the one static).
// With a gateway (GatewaySpec set) all four are STATIC on the internal subnet
// instead, and Gateway records the gateway guest (its Vmid for teardown; its
// LAN address IS Proxy.Ip, its internal address is GatewayInternalIP).
type LxcSpec struct {
	Relay   LxcGuest `toml:"relay"`
	Cp      LxcGuest `toml:"cp"`
	K3s     LxcGuest `toml:"k3s,omitempty"`
	Gateway LxcGuest `toml:"gateway,omitempty"`
}

// DnsSpec is the CP-owned resolver's explicit records (name -> IP), the
// same table the CP state holds — mirrored here for the TUI's read-only
// panel + the services-at-a-glance render (the CP is authoritative).
type DnsSpec struct {
	Records map[string]string `toml:"records,omitempty"`
	// Manager records that freehold MANAGES this world's relay/cp DNS
	// records (opt-in --manage-dns). Provider is the dnsman provider that
	// owns the A records; Managed=true means freehold (re)creates
	// relay.<domain>/cp.<domain> -> Proxy.Ip on build; IP is the address they
	// point at. Teardown leaves them by default; --remove-dns deletes them.
	Manager *DnsManager `toml:"manager,omitempty"`
}

// DnsManager is freehold's DNS-manager assertion (see DnsSpec.Manager).
type DnsManager struct {
	Provider string `toml:"provider,omitempty"`
	Managed  bool   `toml:"managed"`
	IP       string `toml:"ip,omitempty"`
}

// LitellmSpec is the C0 gateway's coords: the kube NodePort URL the agents
// call + which kube node hosts it (for teardown/readiness).
type LitellmSpec struct {
	URL  string `toml:"url,omitempty"`
	Host string `toml:"host,omitempty"` // the k3s guest name
}

// CaddySpec records the core TLS fronting proxy's coords. URL is the public
// relay URL Caddy fronts; Host is the proxy's host (k3s). Per-host cert expiry
// (RelayCert/CPCert, RFC3339) feeds the Certs tab + the reuse gate; CertIssuer
// is the DNS provider used. RelayLegoDomain/CPLegoDomain are the Let's Encrypt
// cert-domain per host (default = the respective host; set "*.base" to issue a
// wildcard that covers the host — e.g. *.freehold-test.darcydev.net — so the
// DNS-01 challenge targets the apex the user's zone serves).
type CaddySpec struct {
	URL             string `toml:"url,omitempty"`
	Host            string `toml:"host,omitempty"`
	RelayCert       string `toml:"relay_cert_expiry,omitempty"`
	CPCert          string `toml:"cp_cert_expiry,omitempty"`
	CertIssuer      string `toml:"cert_issuer,omitempty"`
	RelayLegoDomain string `toml:"relay_lego_domain,omitempty"`
	CPLegoDomain    string `toml:"cp_lego_domain,omitempty"`
}

// LxcGuest is a managed LXC's connect/status coordinates.
type LxcGuest struct {
	Vmid *uint32 `toml:"vmid,omitempty"`
	Ip   *string `toml:"ip,omitempty"`
}

// DefaultPath returns the config path (~/.config/freehold/config.toml).
func DefaultPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "freehold", "config.toml")
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return filepath.Join(home, ".config", "freehold", "config.toml")
}

// Load reads the config; returns (nil, nil) when the file is absent.
func Load(path string) (*Config, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Save writes the config (pretty TOML), creating parent dirs.
func (c *Config) Save(path string) error {
	if parent := filepath.Dir(path); parent != "" {
		os.MkdirAll(parent, 0o755)
	}
	raw, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// SplitURL splits a URL into (host, port).
func SplitURL(url string) (string, uint16) {
	u := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	u = strings.TrimSuffix(u, "/")
	host, portStr := u, "443"
	if strings.HasPrefix(url, "http://") {
		portStr = "80"
	}
	if i := strings.LastIndex(u, ":"); i >= 0 {
		host, portStr = u[:i], u[i+1:]
	}
	p := (uint16)(0)
	for _, c := range portStr {
		d := c - '0'
		if d < 0 || d > 9 {
			p = 0
			break
		}
		p = p*10 + uint16(d)
	}
	if p == 0 {
		if strings.HasPrefix(url, "https://") {
			p = 443
		} else {
			p = 80
		}
	}
	return host, p
}

func u16String(p uint16) string {
	if p == 0 {
		return "0"
	}
	var b [5]byte
	i := len(b)
	for p > 0 {
		i--
		b[i] = byte('0' + p%10)
		p /= 10
	}
	return string(b[i:])
}

// URLReachable does a TCP connect to the URL's host:port (liveness, no TLS
// handshake).
func URLReachable(url string) bool {
	host, port := SplitURL(url)
	addr := net.JoinHostPort(strings.Trim(host, "[]"), u16String(port))
	conn, err := net.DialTimeout("tcp", addr, 6*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// probeClient builds the liveness HTTP client: TLS validation OFF, mirroring
// the Rust http_any/http_ok (danger_accept_invalid_certs) — these probes
// check LIVENESS, not CA pinning, and must accept the local-CA / self-signed
// posture. 6s timeout.
func probeClient() *http.Client {
	return &http.Client{
		Timeout:   6 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
}

// HTTPAny GETs the URL accepting ANY HTTP response (including the k3s 401 —
// only a transport failure means down).
func HTTPAny(url string) bool {
	resp, err := probeClient().Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// HTTPOK GETs the URL requiring a 2xx/3xx response.
func HTTPOK(url string) bool {
	resp, err := probeClient().Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// RelayLive polls the relay's own /_liveness — the relay's OWN answer (2xx),
// not "some proxy on that host answers". Mirrors Rust relay_live (http_ok).
func RelayLive(cfg *Config) bool {
	return HTTPOK(strings.TrimSuffix(cfg.RelayURL, "/") + "/_liveness")
}

// LitellmLive: any HTTP answer from the recorded litellm gateway URL (the
// kube NodePort — /health/liveliness 200s when litellm is up).
func LitellmLive(cfg *Config) bool {
	if cfg.Litellm.URL == "" {
		return false
	}
	return HTTPAny(strings.TrimSuffix(cfg.Litellm.URL, "/") + "/health/liveliness")
}

// StripCIDR drops the /prefix from a recorded CIDR ip ("1.2.3.4/24" ->
// "1.2.3.4"); the config records ips in CIDR form.
func StripCIDR(ip string) string {
	if i := strings.Index(ip, "/"); i >= 0 {
		return ip[:i]
	}
	return ip
}

// LxcIP returns the bare IP (CIDR stripped) recorded for a guest, or "" when
// none is recorded yet.
func LxcIP(g LxcGuest) string {
	if g.Ip == nil {
		return ""
	}
	return StripCIDR(*g.Ip)
}

// ResolveTarget returns the bare LAN IP an install/build/teardown step should
// connect to for a service BEFORE the world's public DNS resolves (or the
// Caddy edge exists). role is one of "relay", "cp", "proxy". "" = no recorded
// IP — the caller falls back to the hostname/URL. With a gateway the world's
// guests are internal — the dialable address is the gateway (the edge), whose
// DNAT fronts everything the domain would.
func (c *Config) ResolveTarget(role string) string {
	if c.Gateway.Cidr != nil && *c.Gateway.Cidr != "" {
		if c.Proxy.Ip != nil {
			return StripCIDR(*c.Proxy.Ip)
		}
		return ""
	}
	switch role {
	case "relay":
		return LxcIP(c.Lxc.Relay)
	case "cp":
		return LxcIP(c.Lxc.Cp)
	case "proxy":
		if c.Proxy.Ip == nil {
			return ""
		}
		return StripCIDR(*c.Proxy.Ip)
	}
	return ""
}

// K3sLive: any HTTP answer from the kube-apiserver on the PROXY static ip
// (the k3s node = Proxy.Ip; auth-gated 401 counts). Mirrors Rust k3s_live: the
// recorded ip is CIDR — stripping the prefix keeps the URL from parsing as
// host:443 with the "/24:6443/healthz" tail as a path.
func K3sLive(cfg *Config) bool {
	if cfg.Proxy.Ip == nil {
		return false
	}
	return HTTPAny("https://" + StripCIDR(*cfg.Proxy.Ip) + ":6443/healthz")
}
