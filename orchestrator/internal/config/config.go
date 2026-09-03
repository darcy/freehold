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
type Config struct {
	Domain           string      `toml:"domain"`
	RelayURL         string      `toml:"relay_url"`
	RelayWsURL       string      `toml:"relay_ws_url,omitempty"`
	RelayPubkey      *string     `toml:"relay_pubkey,omitempty"`
	CPURL            string      `toml:"cp_url"`
	OperatorPubkey   string      `toml:"operator_pubkey"`
	OperatorIdentity *string     `toml:"operator_identity,omitempty"`
	Runner           RunnerRef   `toml:"runner"`
	Lxc              LxcSpec     `toml:"lxc"`
	Plane            PlaneSpec   `toml:"plane,omitempty"`
	Dns              DnsSpec     `toml:"dns,omitempty"`
	Litellm          LitellmSpec `toml:"litellm,omitempty"`
	CPAName          string      `toml:"cpa_name,omitempty"`
	Managed          []string    `toml:"managed"`
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

// LxcSpec holds the managed LXC coordinates.
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
}

// LitellmSpec is the C0 gateway's coords: the kube NodePort URL the agents
// call + which kube node hosts it (for teardown/readiness).
type LitellmSpec struct {
	URL  string `toml:"url,omitempty"`
	Host string `toml:"host,omitempty"` // the k3s guest name
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

// K3sLive: any HTTP answer from the kube-apiserver on the RECORDED k3s ip
// (auth-gated 401 counts). Mirrors Rust k3s_live: the recorded ip is CIDR —
// stripping the prefix keeps the URL from parsing as host:443 with the
// "/24:6443/healthz" tail as a path.
func K3sLive(cfg *Config) bool {
	if cfg.Lxc.K3s.Ip == nil {
		return false
	}
	return HTTPAny("https://" + StripCIDR(*cfg.Lxc.K3s.Ip) + ":6443/healthz")
}
