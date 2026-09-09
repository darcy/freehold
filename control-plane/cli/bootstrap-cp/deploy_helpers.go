package deploy

import (
	"fmt"
	"strings"
)

// ValidateLoopbackBind replicates control_plane::validate_loopback_bind: the
// whole string must be `host:port` with a parseable port, and the host must
// be loopback (when authn is absent).
func ValidateLoopbackBind(addr string) error {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return fmt.Errorf("%q is not a host:port address", addr)
	}
	port := addr[i+1:]
	if len(port) == 0 {
		return fmt.Errorf("%q has a non-numeric or absent port", addr)
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return fmt.Errorf("%q has a non-numeric or absent port", addr)
		}
	}
	if _, err := parseUint16(port); err != nil {
		return fmt.Errorf("%q has a non-numeric or absent port", addr)
	}
	host := strings.TrimPrefix(addr[:i], "[")
	host = strings.TrimSuffix(host, "]")
	if host == "localhost" || host == "::1" || isLoopbackIP(host) {
		return nil
	}
	return fmt.Errorf("refusing to bind the console to %q: loopback-only (no authn/TLS on the HTTP surface); reach it from elsewhere with an SSH tunnel (ssh -L 8080:127.0.0.1:8080 <box>)", addr)
}

// ResolveCpBind: explicit wins; under authn the default relaxes to the LAN.
func ResolveCpBind(explicit *string, authn bool) string {
	if explicit != nil && *explicit != "" {
		return *explicit
	}
	if authn {
		return "0.0.0.0:8080"
	}
	return "127.0.0.1:8080"
}

// DefaultCPStateDir is the remote CP state dir default (deploy_cp.rs).
func DefaultCPStateDir() string { return "/srv/data/cp/control-plane" }

// DefaultCPBinDir is the remote CP bin dir default (deploy_cp.rs).
func DefaultCPBinDir() string { return "/srv/data/cp/bin" }

func parseUint16(s string) (uint16, error) {
	var n uint16
	if len(s) == 0 || len(s) > 5 {
		return 0, fmt.Errorf("not a port")
	}
	for _, c := range s {
		d := int(c - '0')
		if d < 0 || d > 9 {
			return 0, fmt.Errorf("not a port")
		}
		n = n*10 + uint16(d)
	}
	return n, nil
}

func isLoopbackIP(host string) bool {
	// 127.0.0.0/8 and ::1.
	if strings.HasPrefix(host, "127.") {
		// verify dotted quad numeric
		parts := strings.Split(host, ".")
		if len(parts) != 4 {
			return false
		}
		for _, p := range parts {
			if p == "" || len(p) > 3 {
				return false
			}
			for _, c := range p {
				if c < '0' || c > '9' {
					return false
				}
			}
		}
		return true
	}
	return host == "::1"
}

func parseUint16Port(addr string) uint16 {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 8080
	}
	p, err := parseUint16(addr[i+1:])
	if err != nil {
		return 8080
	}
	return p
}

func isHexPubkey(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}