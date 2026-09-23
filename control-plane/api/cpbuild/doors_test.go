package cpbuild

import (
	"strings"
	"testing"

	"freehold/agents"
)

// TestCapabilityRunnerTable pins the capability-runner model: unique names +
// ports, every roster entry a real department, and the shared host runner
// rostered by all three box-capable departments.
func TestCapabilityRunnerTable(t *testing.T) {
	runners := capabilityRunners()
	if len(runners) < 6 {
		t.Fatalf("want at least 6 capability runners, got %d", len(runners))
	}
	seenName := map[string]bool{}
	seenPort := map[int]bool{}
	departments := map[string]bool{}
	for _, d := range agents.DepartmentNames() {
		departments[d] = true
	}
	for _, r := range runners {
		if seenName[r.name] {
			t.Fatalf("duplicate runner name %q", r.name)
		}
		seenName[r.name] = true
		if seenPort[r.port] {
			t.Fatalf("duplicate runner port %d", r.port)
		}
		seenPort[r.port] = true
		// Runner names are capability-named <target>-<protocol>-<identity>:
		// never a consumer's name.
		for _, dept := range []string{"network", "compute", "data", "ai"} {
			if strings.HasPrefix(r.name, dept+"-") || strings.Contains(r.name, "-"+dept+"-") && !strings.HasPrefix(r.name, "kube-api-") {
				t.Fatalf("runner %s is consumer-named", r.name)
			}
		}
		if len(r.rosters) == 0 {
			t.Fatalf("runner %s has an empty roster", r.name)
		}
		for _, dept := range r.rosters {
			if !departments[dept] {
				t.Fatalf("runner %s roster entry %q is not a department", r.name, dept)
			}
		}
		switch r.kind {
		case "ssh", "kubernetes", "litellm", "local":
		default:
			// dynamic dns-provider kinds (cloudflare) are appended at staging
			t.Fatalf("runner %s has unexpected static kind %q", r.name, r.kind)
		}
		if r.kind == "kubernetes" && (r.tokenSecret == "" || r.tokenNS == "") {
			t.Fatalf("kube door %s is missing its token secret/namespace", r.name)
		}
	}
	// The shared host runner: one runner, three roster pubkeys.
	var pve *capabilityRunner
	for i := range runners {
		if runners[i].name == "pve-ssh-root" {
			pve = &runners[i]
		}
	}
	if pve == nil {
		t.Fatal("pve-ssh-root missing from the table")
	}
	if len(pve.rosters) != 3 {
		t.Fatalf("pve-ssh-root should carry network+compute+data, got %v", pve.rosters)
	}
}

// TestZoneOf pins the zone derivation for the per-zone DNS doors.
func TestZoneOf(t *testing.T) {
	if got := zoneOf("cp.example.com"); got != "example.com" {
		t.Fatalf("zoneOf(cp.example.com) = %q", got)
	}
	if got := zoneOf("relay.deep.sub.example.org"); got != "deep.sub.example.org" {
		t.Fatalf("zoneOf(relay.deep.sub.example.org) = %q", got)
	}
	if got := zoneOf("barehost"); got != "" {
		t.Fatalf("zoneOf(barehost) = %q", got)
	}
}

// TestEnvToLower pins the round-trip with the runner's env_name(): a stored
// provider env var seals under a secret name that re-derives the SAME env.
func TestEnvToLower(t *testing.T) {
	if got := envToLower("CF_DNS_API_TOKEN"); got != "cf-dns-api-token" {
		t.Fatalf("envToLower = %q", got)
	}
}

// TestDNSTokenKey pins the door credential pick: the bearer the runner's
// verify probe uses must be the API token, never a non-credential env.
func TestDNSTokenKey(t *testing.T) {
	if got := dnsTokenKey(map[string]string{"CF_ACCOUNT_ID": "x", "CF_DNS_API_TOKEN": "t"}); got != "CF_DNS_API_TOKEN" {
		t.Fatalf("dnsTokenKey = %q", got)
	}
	if got := dnsTokenKey(map[string]string{"CF_ACCOUNT_ID": "x", "OTHER_TOKEN": "t"}); got != "OTHER_TOKEN" {
		t.Fatalf("dnsTokenKey = %q", got)
	}
	if got := dnsTokenKey(map[string]string{"CF_ACCOUNT_ID": "x", "AAAA": "t"}); got != "AAAA" {
		t.Fatalf("dnsTokenKey should fall back to the first sorted key: %q", got)
	}
}

// TestCPACarriesGrantingSkill pins the CPA prompt composition: the granting
// skill's rules ship inside the CPA's system prompt (the only grant-capable
// identity), not just as a file.
func TestCPACarriesGrantingSkill(t *testing.T) {
	prompt := agents.CPASystemPrompt("")
	for _, want := range []string{"The unit of grant is the RUNNER", "pve-ssh-root", "containment failure"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("CPA prompt missing granting-skill material %q", want)
		}
	}
	// Departments do NOT get it — capability execution is theirs, granting is
	// not.
	for _, dept := range agents.DepartmentNames() {
		p, _ := agents.DepartmentPrompt(dept)
		if strings.Contains(p, "unit of grant is the RUNNER") {
			t.Fatalf("department %s must not carry the granting skill", dept)
		}
	}
}
