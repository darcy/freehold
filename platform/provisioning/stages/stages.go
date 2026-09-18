// Package stages is the shared "what to run" library of the world bring-up —
// the pure stage shell-scripts, manifest builders, and coordinate helpers that
// were authored in the operator-box `internal/cli/rebuild.go` pipeline. It
// deliberately carries NO execution: it builds command strings only. Both the
// box executor (`freehold build`, through its local runner) and the CP executor
// (`freehold-agent-tools world_build`, through its co-located runner) invoke the
// SAME stages through whatever runner handle they own — so a stage moved onto
// the CP runs byte-identically to the box.
//
// Secrets discipline: operators' keys never appear in these command strings.
// CP-generated values (litellm master/postgres) are single-quoted shell
// literals; provider keys and cert private keys ride the runner package and are
// injected by name at exec time. Every script that travels inside a
// single-quoted guest-exec `sh -c` wrapper is single-quote-free.
package stages

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// RelayComposeDir is where the relay's docker-compose stack lives INSIDE the
// relay LXC's durable deploy mount (/srv/data/relay/deploy/compose), shared by
// the deploy-relay stage and agent-membership guest-exec commands.
const RelayComposeDir = "/srv/data/relay/deploy/compose"

// CaddyEdgeDurableDir is the durable (backup=1) mirror of a slot's edge cert
// under /srv/data/k8s-volumes/caddy-edge/<slot>, read by the cert reuse gate and
// written by the cert install, so a teardown+rebuild reuses the cert.
func CaddyEdgeDurableDir(slot string) string {
	return "/srv/data/k8s-volumes/caddy-edge/" + slot
}

// SlotCertKeyEnv is the runner-injected env var name that carries a slot's cert
// PRIVATE KEY (sealed secret) so it never crosses argv/logs.
func SlotCertKeyEnv(slot string) string {
	return "CERT_KEY_" + strings.ToUpper(slot)
}

// GenSecretHex mints a 32-byte random hex secret (master key / postgres pw).
// Returns "" on RNG failure (the caller rejects empty).
func GenSecretHex() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// DnsRec is one explicit record the CP-owned resolver serves (split-horizon
// <name> -> <ip>, registered via `control-plane dns add` inside the CP LXC).
type DnsRec struct {
	Name   string
	IP     string
	Source string
}

// DnsRecords is the world's explicit resolver records, in register order: the
// guest bare names (relay/cp/proxy/k3s/litellm) plus the dotted public hosts
// (relayHost/cpHost) that must resolve to the PROXY (Caddy), never directly to
// a LXC. A caller drops a record by passing an empty value for its field.
func DnsRecords(relayHost, relayIP, cpHost, cpIP, proxyIP, litellmIP string) []DnsRec {
	var recs []DnsRec
	if relayIP != "" {
		recs = append(recs, DnsRec{Name: "relay", IP: relayIP, Source: "world-build relay"})
	}
	if cpIP != "" {
		recs = append(recs, DnsRec{Name: "cp", IP: cpIP, Source: "world-build cp"})
	}
	if proxyIP != "" {
		recs = append(recs, DnsRec{Name: "proxy", IP: proxyIP, Source: "world-build proxy-static"})
		recs = append(recs, DnsRec{Name: "k3s", IP: proxyIP, Source: "world-build proxy-static"})
	}
	if litellmIP != "" {
		recs = append(recs, DnsRec{Name: "litellm", IP: litellmIP, Source: "world-build litellm"})
	}
	if relayHost != "" && proxyIP != "" {
		recs = append(recs, DnsRec{Name: relayHost, IP: proxyIP, Source: "world-build relay-via-proxy"})
	}
	if cpHost != "" && proxyIP != "" {
		recs = append(recs, DnsRec{Name: cpHost, IP: proxyIP, Source: "world-build cp-via-proxy"})
	}
	return recs
}
