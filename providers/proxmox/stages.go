// Package proxmox's stage builders: the pct/zfs-specific command strings that
// used to live in platform/provisioning/stages. Generic manifest/coordinate
// helpers (DnsRecords, RelayComposeDir, CaddyEdgeDurableDir, GenSecretHex)
// stay in platform/stages.
package proxmox

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"freehold/platform/provisioning/stages"
)

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

// DnsApexCmd is the pct exec that runs `freehold-console dns` with --state-dir/
// --apex/--ip and the `apex` verb. It puts the FLAGS BEFORE the verb (Go's
// flag.Parse stops at the first non-flag arg, so a --state-dir after the verb
// is silently unparsed and state.Open fails on an empty path).
func DnsApexCmd(cpLxc uint32, binDir, stateDir, apex, proxyIP string) string {
	inner := fmt.Sprintf("'%s/freehold-console' 'dns' --state-dir '%s' --apex '%s' --ip '%s' 'apex'",
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
	keyEnv := stages.SlotCertKeyEnv(slot)
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
		slot, stages.CaddyEdgeDurableDir(slot), // 8-9 (DIR tls/slot, DUR)
		slot, slot, slot, slot, slot, slot) // 10-15
}

func shellSingleQuote(s string) string { return "'" + s + "'" }
