// Package config reproduces the freehold installer's connection profile
// (~/.config/freehold/config.toml): the desired state of the world.
// Present + converged -> running mode; present + not -> configure; absent ->
// bootstrap. Ported from installer/src/config.rs.
package config

import (
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Config is the connection profile / desired world state.
//
// There is NO world/base domain anymore: the relay and control‑plane hosts are
// separate literal hostnames (RelayURL/CPURL), and everything sits behind a
// single static proxy IP (Proxy.Ip — the Caddy edge on the k3s node), which
// both hosts resolve to. Relay/CP LXCs are DHCP, behind the proxy.
type Config struct {
	RelayURL         string      `toml:"relay_url"`
	RelayWsURL       string      `toml:"relay_ws_url,omitempty"`
	RelayPubkey      *string     `toml:"relay_pubkey,omitempty"`
	CPURL            string      `toml:"cp_url"`
	OperatorPubkey   string      `toml:"operator_pubkey"`
	OperatorIdentity *string     `toml:"operator_identity,omitempty"`
	Runner           RunnerRef   `toml:"runner"`
	Proxy            ProxySpec   `toml:"proxy"`
	Lxc              LxcSpec     `toml:"lxc"`
	Plane            PlaneSpec   `toml:"plane,omitempty"`
	Dns              DnsSpec     `toml:"dns,omitempty"`
	Litellm          LitellmSpec `toml:"litellm,omitempty"`
	Caddy            CaddySpec   `toml:"caddy,omitempty"`
	CPAName          string      `toml:"cpa_name,omitempty"`
	Managed          []string    `toml:"managed"`
}

// ProxySpec is the single static address in the world: the proxy (Caddy) node,
// which the relay + CP hosts resolve to and which Caddy binds for TLS. CIDR form.
type ProxySpec struct {
	Ip *string `toml:"ip,omitempty"`
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

// PlaneSpec is the durable volume plane (Phase 0.12).
type PlaneSpec struct {
	Backend     *string `toml:"backend,omitempty"`
	BackendKind *string `toml:"backend_kind,omitempty"`
	// ThinPool names a thin pool FREEHOLD CREATED (LVM-thin backend). Set
	// only by the carve branch of the rebuild placement gate; a REUSED
	// stock pool (pve/data) is never recorded here. Teardown --data removes
	// exactly this pool and nothing else.
	ThinPool *string                 `toml:"thin_pool,omitempty"`
	Mounts   map[string][]PlaneMount `toml:"mounts,omitempty"`
}

// PlaneMount is one resolved durable-plane mount (HOST source + guest path).
type PlaneMount struct {
	Source    string `toml:"source"`
	GuestPath string `toml:"guest_path"`
}

// RunnerRef identifies the provisioning runner.
type RunnerRef struct {
	Addr   string `toml:"addr"`
	Pubkey string `toml:"pubkey"`
	Target string `toml:"target"`
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
type LxcSpec struct {
	Relay LxcGuest `toml:"relay"`
	Cp    LxcGuest `toml:"cp"`
	K3s   LxcGuest `toml:"k3s,omitempty"`
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
