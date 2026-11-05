package cpbuild

import (
	"strings"
	"testing"

	"freehold/agents"
	provisioner "freehold/control-plane/secret-management"
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
	// The data department's local door: exec ON the cp guest (kind local),
	// rostered to data only — the verbs deploy-cp ships run there.
	var cpLocal *capabilityRunner
	for i := range runners {
		if runners[i].name == "cp-local-root" {
			cpLocal = &runners[i]
		}
	}
	if cpLocal == nil {
		t.Fatal("cp-local-root missing from the table")
	}
	if cpLocal.kind != "local" {
		t.Fatalf("cp-local-root kind = %q, want local", cpLocal.kind)
	}
	if len(cpLocal.rosters) != 1 || cpLocal.rosters[0] != "data" {
		t.Fatalf("cp-local-root should carry data only, got %v", cpLocal.rosters)
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
	prompt := agents.CPASystemPrompt("", "")
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

// TestDoorUnitShapePinsResilience: the doors' staged units are REAL files —
// Restart=on-failure + enabled (a crash or a guest reboot returns; a
// --collect transient removed the unit on the first crash and the door's
// port went dark with no reaper). A LOCAL door runs as root — root IS the
// job on its own host — so its unit carries no sandbox.
func TestDoorUnitShapePinsResilience(t *testing.T) {
	s := doorUnitText("freehold capability runner cp-local-root", "cp-local-root",
		"/srv/data/cp/bin/freehold-runner", "--state-dir /x --addr 0.0.0.0:8797", "")
	for _, want := range []string{"Restart=on-failure", "RestartSec=5", "WantedBy=multi-user.target", "ExecStart=/srv/data/cp/bin/freehold-runner serve --state-dir /x --addr 0.0.0.0:8797"} {
		if !strings.Contains(s, want) {
			t.Fatalf("door unit missing %q\n---\n%s", want, s)
		}
	}
	for _, banned := range []string{"User=", "NoNewPrivileges", "ProtectSystem"} {
		if strings.Contains(s, banned) {
			t.Fatalf("local door unit must run as root (the documented contract); found %q\n---\n%s", banned, s)
		}
	}
}

// TestDoorUnitSandboxPinsLeastPrivilege: an ssh/api door's unit runs as its
// own unprivileged user with a read-only filesystem except its own package
// dir — an api door's exec is curl, never root; a compromised door must not
// be root on the CP guest (which holds CP state, the console identity, and
// the operator's sealed signing key).
func TestDoorUnitSandboxPinsLeastPrivilege(t *testing.T) {
	pkgDir := "/srv/data/cp/control-plane/runner/litellm-api-admin"
	s := doorUnitText("freehold capability runner litellm-api-admin", "litellm-api-admin",
		"/srv/data/cp/bin/freehold-runner", "--state-dir "+pkgDir+" --addr 0.0.0.0:8795",
		provisioner.DoorSandbox("fhd-litellm-api-admin", pkgDir))
	for _, want := range []string{
		"User=fhd-litellm-api-admin",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ReadWritePaths=" + pkgDir,
		"PrivateTmp=yes",
		"RestrictSUIDSGID=yes",
		"ExecStart=",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("sandboxed door unit missing %q\n---\n%s", want, s)
		}
	}
}

// TestDoorUserDerivation: local doors and the co-located runner stay root
// (""); names derive deterministically; long names stay within the 32-char
// username limit WITHOUT colliding (two long names sharing a prefix must
// derive different users).
func TestDoorUserDerivation(t *testing.T) {
	if u, err := provisioner.DoorUser("local", "cp-local-root"); err != nil || u != "" {
		t.Fatalf("local door must stay root, got %q, %v", u, err)
	}
	u, err := provisioner.DoorUser("api", "litellm-api-admin")
	if err != nil || u != "fhd-litellm-api-admin" {
		t.Fatalf("short name derives verbatim, got %q, %v", u, err)
	}
	long := "cloudflare-api-a-very-long-zone-name-exceeding-the-username-lim"
	u1, err := provisioner.DoorUser("api", long)
	if err != nil {
		t.Fatalf("long name: %v", err)
	}
	u2, _ := provisioner.DoorUser("api", long)
	if u1 != u2 {
		t.Fatalf("derivation must be deterministic: %q vs %q", u1, u2)
	}
	if len(u1) > 32 {
		t.Fatalf("derived user %q exceeds the 32-char username limit", u1)
	}
	long2 := long[:len(long)-1] + "x"
	u3, _ := provisioner.DoorUser("api", long2)
	if u3 == u1 {
		t.Fatalf("two distinct long names must not collide on one user: %q", u1)
	}
	for _, u := range []string{u1, u3} {
		if !strings.HasPrefix(u, "fhd-") {
			t.Fatalf("derived user %q must carry the fhd- prefix", u)
		}
	}
	if _, err := provisioner.DoorUser("api", "bad name; rm -rf"); err == nil {
		t.Fatalf("an unsafe name must be refused, never silently run as root")
	}
}

// TestRetireScriptClosesUnits: the build's rename-retire must close out the
// now-real unit files the way revoke_runner's retire does — disable --now +
// file removal + reload — not just stop them (an enabled unit file survives
// the stop and resurrects the runner at the next CP-guest boot).
func TestRetireScriptClosesUnits(t *testing.T) {
	script := retireUnitsScript("data-pve")
	for _, want := range []string{
		"systemctl disable --now freehold-runner-data-pve",
		"rm -f /etc/systemd/system/freehold-runner-data-pve.service /etc/systemd/system/freehold-runner-data.service",
		"systemctl daemon-reload",
		"systemctl reset-failed freehold-runner-data-pve",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("retire script missing %q\n---\n%s", want, script)
		}
	}
}
