// Package dnsman is freehold's provider-extensible DNS domain manager: the
// OPT-IN capability (DNS-MANAGEMENT) that creates/updates/removes the world's
// A records (relay.<domain> + cp.<domain> -> the proxy's static IP), distinct
// from the per-host Let's Encrypt certificate issuance. LE and management share
// one provider credential (the sealed builder-then dns-cred env map).
//
// Only Cloudflare is implemented today; a provider registry (
//
//	Register/For) lets others slot in with a Manager implementation.
package dnsman

import (
	"fmt"
	"strings"
)

// Manager creates/updates/removes the A records for the world's managed
// domains. name is the full host (e.g. "relay.librem.freehold.technology").
type Manager interface {
	// Provider returns the dnsman provider name (matches the lego DNS-01
	// provider name, e.g. "cloudflare").
	Provider() string
	// UpsertA points name at ip via an A record (DNS-only, so the edge's own
	// Caddy presents the LE cert rather than a CDN edge).
	UpsertA(name, ip string) error
	// DeleteA removes the A record for name.
	DeleteA(name string) error
	// DeleteTXT removes every TXT record at name (used to clear a leftover DNS-01
	// challenge record before issuing a fresh one).
	DeleteTXT(name string) error
}

// Ctor builds a Manager from a provider's credential env map (the same map
// sealed for the LE DNS-01 provider — manager and lego share the token).
type Ctor func(env map[string]string) (Manager, error)

var registry = map[string]Ctor{}

// Register installs a provider's Manager constructor. Call from init.
func Register(provider string, ctor Ctor) {
	if ctor == nil {
		panic("dnsman: nil constructor for " + provider)
	}
	registry[provider] = ctor
}

// For returns the registered Manager for provider, built from env. Unknown
// providers get an actionable error (management is broader than a challenge
// token, so not every DNS-01 provider can manage records yet).
func For(provider string, env map[string]string) (Manager, error) {
	ctor, ok := registry[provider]
	if !ok {
		return nil, fmt.Errorf(
			"freehold cannot manage DNS on %q yet — record management is implemented for: %s",
			provider, strings.Join(Supported(), ", "))
	}
	return ctor(env)
}

// Supported lists the providers with a Manager registered.
func Supported() []string {
	var out []string
	for p := range registry {
		out = append(out, p)
	}
	return out
}
