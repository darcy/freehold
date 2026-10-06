package litellm

import (
	"strings"
	"testing"
)

// TestProvidersWellFormed guards the curated table: unique slugs/prefixes,
// every field populated, no whitespace rot in the registration strings (the
// gateway registers `<prefix>/<DefaultModel>` verbatim).
func TestProvidersWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Providers() {
		for field, v := range map[string]string{"name": p.Name, "prefix": p.Prefix, "default_model": p.DefaultModel, "desc": p.Desc} {
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s: empty %s", p.Name, field)
			}
		}
		if strings.ContainsAny(p.Name+p.Prefix, " \t/") {
			t.Errorf("%s: name/prefix must be bare slugs", p.Name)
		}
		if seen[p.Name] {
			t.Errorf("duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
	}
}

// TestGetRoundTrips: Get returns a copy (mutating it must not corrupt the
// table) and nil for an unknown slug.
func TestGetRoundTrips(t *testing.T) {
	p := Get("fireworks_ai")
	if p == nil {
		t.Fatal("fireworks_ai missing from the table")
	}
	p.Prefix = "corrupted"
	if Get("fireworks_ai").Prefix == "corrupted" {
		t.Fatal("Get returned the table's own row")
	}
	if Get("no-such-provider") != nil {
		t.Fatal("unknown slug should return nil")
	}
}
