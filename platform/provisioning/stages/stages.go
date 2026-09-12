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
// single-quoted `pct exec ... -- bash -c '%s'` wrapper is single-quote-free.
package stages

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// RelayComposeDir is where the relay's docker-compose stack lives INSIDE the
// relay LXC's durable deploy mount (/srv/data/relay/deploy/compose), shared by
// the deploy-relay stage and agent-membership pct exec commands.
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

// DnsAddCmd is the pct exec that runs `freehold-console dns add <name> <ip>
// <source> [--domain <base>]` INSIDE the CP LXC (the dnsmasq resolver) — the Go
// console's `control-plane dns add` equivalent (the rust control-plane binary is
// gone; freehold-console carries the dns subcommand). Single-quote-wrapped at
// the innermost level only; callers pass validated names/IPs.
func DnsAddCmd(cpLxc uint32, binDir, stateDir, name, ip, source, searchBase string) string {
	rest := []string{"'add'", shellSingleQuote(name), shellSingleQuote(ip), shellSingleQuote(source)}
	if searchBase != "" {
		rest = append(rest, "'--domain'", shellSingleQuote(searchBase))
	}
	inner := fmt.Sprintf("'%s/freehold-console' 'dns' --state-dir '%s' %s",
		binDir, stateDir, strings.Join(rest, " "))
	return fmt.Sprintf("pct exec %d -- sh -c %s", cpLxc, shellSingleQuote(inner))
}

// DnsApexCmd is the pct exec that runs `freehold-console dns apex --apex <base>
// --ip <proxy>` inside the CP: it sets the resolver WILDCARD (all subdomains of
// the apex -> the proxy/Caddy IP) so the dotted public hosts (relay.<dom>,
// cp.<dom>) resolve to the TLS edge - never to a guest LXC (dnsmasq's bare
// guest records would otherwise leak the LXC IP into the FQDN query and break
// the edge's reachability from the CP).
func DnsApexCmd(cpLxc uint32, binDir, stateDir, apex, proxyIP string) string {
	inner := fmt.Sprintf("'%s/freehold-console' 'dns' 'apex' --state-dir '%s' --apex '%s' --ip '%s'",
		binDir, stateDir, apex, proxyIP)
	return fmt.Sprintf("pct exec %d -- sh -c %s", cpLxc, shellSingleQuote(inner))
}

// DnsPointCmd builds the two commands that point ONE guest at the CP resolver:
// the PVE-managed `pct set --nameserver` (durable across guest reboots) and the
// immediate resolv.conf rewrite (pct only regenerates it at the NEXT boot).
// router is the resolver's upstream nameserver — passed only for the CP guest
// itself (it keeps the router as a secondary so dnsmasq still has upstream);
// empty for relay/k3s.
func DnsPointCmd(vmid uint32, resolver, router string, searchBase string) (pctSet, resolvConf string) {
	nsList := resolver
	if router != "" {
		nsList += " " + router
	}
	parts := []string{"'set'", "'" + strconv.FormatUint(uint64(vmid), 10) + "'", "'--nameserver'", shellSingleQuote(nsList)}
	if searchBase != "" {
		parts = append(parts, "'--searchdomain'", shellSingleQuote(searchBase))
	}
	pctSet = "pct " + strings.Join(parts, " ")
	body := "nameserver " + resolver + "\n"
	if router != "" {
		body += "nameserver " + router + "\n"
	}
	if searchBase != "" {
		body = "search " + searchBase + "\n" + body
	}
	resolvConf = fmt.Sprintf("pct exec %d -- sh -c \"printf '%s' > /etc/resolv.conf\"", vmid, body)
	return pctSet, resolvConf
}

// DnsVerifyCmd asks the CP resolver's OWN loopback for a record and fails
// unless it answers exactly wantIP — proving the whole chain (render -> write ->
// dnsmasq load) landed, not merely that tcp/53 is open.
func DnsVerifyCmd(cpLxc uint32, name, wantIP string) string {
	return fmt.Sprintf("pct exec %d -- sh -c \"dig +short +time=2 +tries=1 %s @127.0.0.1 2>/dev/null | grep -qx '%s'\"",
		cpLxc, name, wantIP)
}

// ParsePctGateway extracts the `net0` `gw=` value from a `pct config` dump.
// Static guests carry `gw=<router>`; DHCP guests (`ip=dhcp`) have NO gw= —
// an empty result routes the caller to the default-route fallback.
func ParsePctGateway(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "net0:") {
			continue
		}
		for _, kv := range strings.Split(l, ",") {
			v, found := strings.CutPrefix(kv, "gw=")
			if found {
				v = strings.TrimSpace(v)
				if v != "" && strings.ContainsAny(v, "0123456789") {
					return v
				}
			}
		}
	}
	return ""
}

// CaddyCertInstallScript writes a slot's issued fullchain + the runner-injected
// private key ($CERT_KEY_<SLOT>) into the caddy-data PVC /data/tls/<slot> AND the
// durable mirror /srv/data/k8s-volumes/caddy-edge/<slot>, then rolls caddy. The
// private key is injected by the runner as an env var — never argv/logs.
func CaddyCertInstallScript(k3sVmid uint32, slot string, fullchain []byte) string {
	fcB64 := base64.StdEncoding.EncodeToString(fullchain)
	keyEnv := SlotCertKeyEnv(slot)
	return fmt.Sprintf(`set -e
printf '%%s' "${%s}" > /tmp/fh-key.pem
printf %s | base64 -d > /tmp/fh-fc.pem
pct push %d /tmp/fh-fc.pem /tmp/fc-%s.pem
pct push %d /tmp/fh-key.pem /tmp/key-%s.pem
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
rm -f /tmp/fh-key.pem /tmp/fh-fc.pem
`,
		keyEnv, shellSingleQuote(fcB64), k3sVmid, slot, k3sVmid, slot, k3sVmid, // 1-7
		slot, CaddyEdgeDurableDir(slot), // 8-9 (DIR tls/slot, DUR)
		slot, slot, slot, slot, slot, slot) // 10-15
}

func shellSingleQuote(s string) string { return "'" + s + "'" }
