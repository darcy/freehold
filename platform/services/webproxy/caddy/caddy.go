package deploy

import (
	"fmt"
	"sort"
	"strings"
)

// Caddyfile renders Caddy's runtime config for freehold's TLS fronting proxy.
// It fronts BOTH the relay and CP hosts (each a per-slot wildcard cert),
// reverse-proxying to the plain-HTTP LXC upstreams on the LAN. Caddy neither
// runs its own ACME client nor auto-HTTPS here — freehold issues the per-host
// (possibly wildcard) certs through embedded lego and presents them from the
// durable volume (/data/tls/{fullchain,key}.pem on the Caddy data PVC; Caddy
// reloads on change), so nothing fights for 80/443 or reaches for ACME itself.
// Indentation is SPACES (not tabs) so it embeds cleanly in the ConfigMap YAML.
//
// relayHost is the relay's own public host (its Buzz origin) — NEVER derived,
// freehold asks for it (it may sit at the world domain or anywhere the
// operator chooses). Caddy fronts exactly that host and reverse-proxies to the
// relay LXC's plain-HTTP upstream. A CP vhost is added from the same render.
//
// pairUpstream is the NIP-AB device-pairing sidecar (pair-relay, a hostNetwork
// pod on the same node binding 5000): the relay vhost routes /pair* to it so
// Buzz's mobile pairing handshake works through the edge. Empty (no k3s node
// IP resolved) omits the route — the pairing surface needs the edge.
func RenderCaddyfile(relayHost, relayUpstream, pairUpstream, cpHost, cpUpstream, cpMcpUpstream string) string {
	relayHandle := fmt.Sprintf("  handle /pair* {\n    reverse_proxy %s\n  }\n", pairUpstream)
	if pairUpstream == "" {
		relayHandle = ""
	}
	return fmt.Sprintf(`{
  auto_https off
}

%s {
  tls /data/tls/relay/fullchain.pem /data/tls/relay/key.pem
%s  handle {
    reverse_proxy %s
  }
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
`, relayHost, relayHandle, relayUpstream, cpHost, cpMcpUpstream, cpUpstream)
}

// AppVhost is one agent-exposed service's site block: its own hostname, the
// internal upstream it proxies to, the cert slot that must ALREADY be
// installed (a vhost referencing a missing cert crash-loops the whole edge),
// and the console upstream its member gate forwards through ("" = ungated —
// an operator-only choice).
type AppVhost struct {
	FQDN     string
	Upstream string
	Slot     string
	Gate     string
}

// RenderAppVhosts renders the apps' site blocks appended after the base
// Caddyfile. Deterministic order (sorted by FQDN) so the build's terraform
// render and the expose verb's kubectl patch produce IDENTICAL config — the
// two appliers converge, never fight.
func RenderAppVhosts(apps []AppVhost) string {
	sorted := append([]AppVhost(nil), apps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].FQDN < sorted[j].FQDN })
	var b strings.Builder
	for _, a := range sorted {
		b.WriteString("\n" + a.FQDN + " {\n")
		b.WriteString("  tls /data/tls/" + a.Slot + "/fullchain.pem /data/tls/" + a.Slot + "/key.pem\n")
		if a.Gate != "" {
			b.WriteString("  forward_auth " + a.Gate + " {\n    uri /auth/verify\n  }\n")
		}
		b.WriteString("  reverse_proxy " + a.Upstream + "\n")
		b.WriteString("}\n")
	}
	return b.String()
}

// RenderCaddyfileApps is the full edge config: the base vhosts plus the apps'
// site blocks (RenderAppVhosts) — the one renderer both appliers share.
func RenderCaddyfileApps(relayHost, relayUpstream, pairUpstream, cpHost, cpUpstream, cpMcpUpstream string, apps []AppVhost) string {
	return RenderCaddyfile(relayHost, relayUpstream, pairUpstream, cpHost, cpUpstream, cpMcpUpstream) +
		RenderAppVhosts(apps)
}
