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
// portalUpstream shares the CP vhost's upstream: the portal (the world root —
// the member face) and the operator console are two front doors over ONE
// backend.
func RenderCaddyfile(relayHost, relayUpstream, pairUpstream, cpHost, cpUpstream, cpMcpUpstream, portalHost string) string {
	relayHandle := fmt.Sprintf("  handle /pair* {\n    reverse_proxy %s\n  }\n", pairUpstream)
	if pairUpstream == "" {
		relayHandle = ""
	}
	// The portal vhost rides only when the console has moved OFF the world
	// root (admin.<root>): the root then fronts the same backend with the
	// portal slot's cert. While the console IS the root, its own vhost serves
	// that name and no portal vhost exists.
	portal := ""
	if portalHost != "" && portalHost != cpHost && portalHost != relayHost {
		portal = fmt.Sprintf(`
%s {
  tls /data/tls/portal/fullchain.pem /data/tls/portal/key.pem
  handle {
    reverse_proxy %s
  }
}
`, portalHost, cpUpstream)
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
%s`, relayHost, relayHandle, relayUpstream, cpHost, cpMcpUpstream, cpUpstream, portal)
}

// AppVhost is one agent-exposed service's site block: its own hostname, the
// internal upstream it proxies to, the cert slot that must ALREADY be
// installed (a vhost referencing a missing cert crash-loops the whole edge),
// and the console upstream its member gate forwards through ("" = ungated —
// an operator-only choice). ExemptPaths/ExemptBearer (operator-pinned, via
// the profile's [edge.apps]) shape the gate into the @gated dispatch: the
// exempted requests (matching paths, or carrying an Authorization bearer
// header — the app's own API clients) skip the member gate entirely and
// proxy straight through; everything else still authenticates. caddy 2.8.4
// rejects named matchers on forward_auth, so the dispatch shape is
// `@gated not { … }` + paired handle blocks — test-proven.
type AppVhost struct {
	FQDN        string
	Upstream    string
	Slot        string
	Gate        string
	ExemptPaths []string
	ExemptBearer bool
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
			// X-Original-Host/Uri: the gate reads WHICH app the request hit
			// (and where to return the member after login) — explicit header_up
			// beats betting on Caddy's default forwarded headers.
			if len(a.ExemptPaths) > 0 || a.ExemptBearer {
				b.WriteString("  @gated not {\n")
				if len(a.ExemptPaths) > 0 {
					b.WriteString("    path " + strings.Join(a.ExemptPaths, " ") + "\n")
				}
				if a.ExemptBearer {
					b.WriteString("    header Authorization Bearer*\n")
				}
				b.WriteString("  }\n")
				b.WriteString("  handle @gated {\n")
				b.WriteString("    forward_auth " + a.Gate + " {\n      uri /auth/verify\n")
				b.WriteString("      header_up X-Original-Host {host}\n")
				b.WriteString("      header_up X-Original-Uri {uri}\n    }\n")
				b.WriteString("    reverse_proxy " + a.Upstream + "\n  }\n")
				b.WriteString("  handle {\n")
				b.WriteString("    reverse_proxy " + a.Upstream + "\n  }\n")
			} else {
				b.WriteString("  forward_auth " + a.Gate + " {\n    uri /auth/verify\n")
				b.WriteString("    header_up X-Original-Host {host}\n")
				b.WriteString("    header_up X-Original-Uri {uri}\n  }\n")
				b.WriteString("  reverse_proxy " + a.Upstream + "\n")
			}
		} else {
			b.WriteString("  reverse_proxy " + a.Upstream + "\n")
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// RenderCaddyfileApps is the full edge config: the base vhosts plus the apps'
// site blocks (RenderAppVhosts) — the one renderer both appliers share.
func RenderCaddyfileApps(relayHost, relayUpstream, pairUpstream, cpHost, cpUpstream, cpMcpUpstream, portalHost string, apps []AppVhost) string {
	return RenderCaddyfile(relayHost, relayUpstream, pairUpstream, cpHost, cpUpstream, cpMcpUpstream, portalHost) +
		RenderAppVhosts(apps)
}
