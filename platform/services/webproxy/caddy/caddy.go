package deploy

import (
	"fmt"
)

// Caddyfile renders Caddy's runtime config for freehold's TLS fronting proxy.
//
// Caddy neither runs its own ACME client nor auto-HTTPS here: freehold issues
// the wildcard certificate through embedded lego (DNS-01) and presents it from
// the durable volume, so the pod's relay/CP vhosts serve the cert BY HOST and
// reverse-proxy to the plain-HTTP LXCs on the LAN.
//
// The certs live at /data/tls/{fullchain,key}.pem on the Caddy data PVC (the
// lego stage F3 writes them there; Caddy reloads on change). auto_https is off
// so Caddy never fights the operator's external nginx for 80/443 or reaches for
// ACME itself.
//
// relayHost is the relay's own public host (its Buzz origin) — NEVER derived,
// freehold asks for it (it may sit at the world domain or anywhere the
// operator chooses). Caddy fronts exactly that host and reverse-proxies to the
// relay LXC's plain-HTTP upstream. A CP vhost is added from the same Render
// RenderCaddyfile builds Caddy's runtime config for freehold's TLS fronting
// proxy. It fronts BOTH the relay and CP hosts (each a per-slot wildcard cert),
// reverse-proxying to the plain-HTTP LXC upstreams on the LAN. Caddy neither
// runs its own ACME client nor auto-HTTPS here — freehold issues the per-host
// (possibly wildcard) certs through embedded lego and presents them from the
// durable volume, so nothing fights for 80/443 or reaches for ACME itself.
// Indentation is SPACES (not tabs) so it embeds cleanly in the ConfigMap YAML.
func RenderCaddyfile(relayHost, relayUpstream, cpHost, cpUpstream, cpMcpUpstream string) string {
	return fmt.Sprintf(`{
  auto_https off
}

%s {
  tls /data/tls/relay/fullchain.pem /data/tls/relay/key.pem
  reverse_proxy %s
}

%s {
  tls /data/tls/cp/fullchain.pem /data/tls/cp/key.pem
  handle /mcp {
    reverse_proxy %s
  }
  handle {
    reverse_proxy %s
  }
}
`, relayHost, relayUpstream, cpHost, cpMcpUpstream, cpUpstream)
}
