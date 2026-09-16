package cert

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/miekg/dns"
)

// waitAuthoritativePropagation waits until the DNS-01 challenge TXT record is
// queryable DIRECTLY at the authoritative nameservers for the zone.
//
// Why authoritative, not the recursive resolver: the custom resumable acme flow
// calls Provider.Present and then immediately asks LE to validate. If a
// recursive resolver (this box's, or LE's) holds a stale negative/TTL cache for
// the just-created `_acme-challenge` name (e.g. from a prior NXDOMAIN attempt),
// LE can mark the authorization invalid before the record ever reaches it. The
// authoritative zone has the record the moment Cloudflare creates it, and LE's
// DNS-01 validation also reads the authoritative servers — so confirming there
// is both necessary and sufficient before POSTing "ready".
func waitAuthoritativePropagation(fqdn, value string, timeout time.Duration) error {
	zone, err := dns01.FindZoneByFqdn(fqdn)
	if err != nil {
		return fmt.Errorf("find zone for %s: %w", fqdn, err)
	}
	nss, err := authoritativeNss(zone)
	if err != nil || len(nss) == 0 {
		return fmt.Errorf("no authoritative nameservers for %s: %w", zone, err)
	}
	deadline := time.Now().Add(timeout)
	lastErr := err
	for time.Now().Before(deadline) {
		for _, ns := range nss {
			ok, qerr := authoritativeHasTXT(ns, fqdn, value)
			if qerr == nil && ok {
				return nil
			}
			if qerr != nil {
				lastErr = qerr
			}
		}
		time.Sleep(5 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("TXT not served by any authoritative NS within %s", timeout)
	}
	return fmt.Errorf("dns-01 %s did not propagate: %w", fqdn, lastErr)
}

func authoritativeNss(zone string) ([]string, error) {
	nsList, err := net.DefaultResolver.LookupNS(context.Background(), zone)
	if err != nil || len(nsList) == 0 {
		return nil, err
	}
	out := make([]string, 0, len(nsList))
	for _, ns := range nsList {
		h := strings.TrimSuffix(ns.Host, ".")
		if h != "" {
			out = append(out, net.JoinHostPort(h, "53"))
		}
	}
	return out, nil
}

func authoritativeHasTXT(ns, fqdn, value string) (bool, error) {
	m := new(dns.Msg)
	m.SetQuestion(fqdn, dns.TypeTXT)
	c := &dns.Client{Timeout: 5 * time.Second}
	local := &net.Dialer{Timeout: 5 * time.Second}
	c.Dialer = local
	resp, _, err := c.Exchange(m, ns)
	if err != nil {
		return false, nil // a dead/unreachable NS is skipped, not a failure
	}
	if resp.Rcode != dns.RcodeSuccess {
		return false, fmt.Errorf("authoritative NS %s answered %s", ns, dns.RcodeToString[resp.Rcode])
	}
	for _, a := range resp.Answer {
		txt, ok := a.(*dns.TXT)
		if !ok {
			continue
		}
		for _, t := range txt.Txt {
			if t == value {
				return true, nil
			}
		}
	}
	return false, nil
}
