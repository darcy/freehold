package console

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"freehold/control-plane/state"
)

// ---- internal resolver ----
// The CP runs dnsmasq inside ITS OWN LXC: explicit name→IP records render into
// addn-hosts, everything else forwards upstream. Explicit records only.

// ValidateName validates a record name: bare hostname (lowercase/digits/dash)
// or dotted FQDN, 1-253 chars, no leading/trailing dash/dot, no '..'.
func ValidateName(name string) error {
	if name == "" || len(name) > 253 ||
		!allChars(name, isNameChar) ||
		strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") ||
		strings.HasSuffix(name, "-") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") {
		return fmt.Errorf("invalid record name %q: must match [a-z0-9-.]+, 1-253 chars, no leading/trailing dash/dot", name)
	}
	return nil
}

func isNameChar(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.'
}

func allChars(s string, ok func(rune) bool) bool {
	for _, c := range s {
		if !ok(c) {
			return false
		}
	}
	return true
}

// ValidateIP validates an IPv4 literal.
func ValidateIP(ip string) error {
	p := net.ParseIP(ip)
	if p == nil || p.To4() == nil {
		return fmt.Errorf("invalid record IP %q: expected an IPv4 literal", ip)
	}
	return nil
}

// RenderAddnHosts renders `<ip> <name>` per record (+ the domain-qualified form).
func RenderAddnHosts(records map[string]state.DnsRecord, domain *string) string {
	var out strings.Builder
	for name, rec := range records {
		out.WriteString(rec.IP + " " + name + "\n")
		if domain != nil && *domain != "" {
			out.WriteString(rec.IP + " " + name + "." + *domain + "\n")
		}
	}
	return out.String()
}

// Upsert adds/updates a record + persists state (rolls back on save failure).
func Upsert(store *state.StateStore, name, ip, source string) (*state.DnsRecord, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if err := ValidateIP(ip); err != nil {
		return nil, err
	}
	rec := state.DnsRecord{IP: ip, Source: source, CreatedAt: uint64(time.Now().Unix())}
	store.InsertDNS(name, rec)
	if err := store.Save(); err != nil {
		store.RemoveDNS(name)
		return nil, err
	}
	return &rec, nil
}

// RemoveDNSRecord removes a record + persists (missing = ok, idempotent).
func RemoveDNSRecord(store *state.StateStore, name string) error {
	store.RemoveDNS(name)
	return store.Save()
}

// AddnHostsPath is the addn-hosts SOURCE in the CP state dir; reloadDnsmasq
// deploys it to DeployedAddnHostsPath (the state dir is 0700 — the store
// re-tightens it on every open — and dnsmasq must never depend on its perms).
func AddnHostsPath(stateDir string) string { return filepath.Join(stateDir, "dnsmasq.addn-hosts") }

// DeployedAddnHostsPath is where reloadDnsmasq lands the addn-hosts:
// root-owned 0644 in the resolver's own dir, readable by dnsmasq regardless
// of the state dir's perms. Deliberately NOT inside /etc/dnsmasq.d/: the
// Debian default CONFIG_DIR points dnsmasq's own conf load AND the unit's
// checkconfig at that directory with no extension filter, so a hosts-format
// file there is parsed as config ("bad option at line 1") and dnsmasq can
// never start.
const DeployedAddnHostsPath = "/etc/freehold/dnsmasq.addn-hosts"

// DeprecatedAddnHostsPath is where the pre-conf-dir-fix render deployed the
// addn-hosts — worlds that ran it carry a hosts-format file inside Debian's
// conf-dir, where its mere presence keeps dnsmasq's checkconfig failing.
// reloadDnsmasq removes it best-effort on every sync.
const DeprecatedAddnHostsPath = "/etc/dnsmasq.d/freehold-names.hosts"

// DnsmasqConfPath is the dnsmasq config file.
func DnsmasqConfPath(stateDir string) string { return filepath.Join(stateDir, "dnsmasq.conf") }

// RenderDnsmasqConf renders the full dnsmasq conf body. `no-hosts` keeps the
// CP guest's own /etc/hosts OUT of the resolver's answers: the relay-host pin
// written there (for CP-side dials, which resolve via glibc's files-first) is
// the RELAY GUEST's LAN IP — if dnsmasq served it, every pod's wss:// relay
// dial would land on the relay guest's :443 (where nothing listens) instead
// of the edge answer, and the agent would sit in its connect backoff forever
// while the pod shows Running. The pin stays CP-local; the subnet gets these
// records only.
func RenderDnsmasqConf(stateDir string, apex, ip *string) string {
	body := fmt.Sprintf("addn-hosts=%s\nno-hosts\nno-negcache\n", DeployedAddnHostsPath)
	if apex != nil && ip != nil && *apex != "" && *ip != "" {
		return body + "address=/." + *apex + "/" + *ip + "\n"
	}
	return body
}

// SyncResolver writes the addn-hosts + dnsmasq conf and reloads dnsmasq. Takes
// write/reload closures so the caller injects the target's local exec.
func SyncResolver(stateDir string, records map[string]state.DnsRecord, domain *string,
	apex, ip *string, write func(string, string) error, reload func() error) error {
	path := AddnHostsPath(stateDir)
	if err := write(path, RenderAddnHosts(records, domain)); err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	if err := write(DnsmasqConfPath(stateDir), RenderDnsmasqConf(stateDir, apex, ip)); err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	if err := reload(); err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	return nil
}
