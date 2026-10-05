package console

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

// TestResolveAgentRoster pins the console /api/provision rosters contract:
// rosters are agent NAMES resolved through the agent-tools registry (the same
// representation the rebuild re-assertion consumes); an unknown name is a 400,
// never a silently-unresolvable roster entry.
func TestResolveAgentRoster(t *testing.T) {
	dir := t.TempDir()
	reg, err := agenttools.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("ai", "aabb", "ai"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("deployer", "ccdd", "deployer"); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveAgentRoster(dir, []string{"ai", "deployer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 2 || resolved[0] != "aabb" || resolved[1] != "ccdd" {
		t.Fatalf("resolved: %v", resolved)
	}
	if _, err := resolveAgentRoster(dir, []string{"ai", "nobody"}); err == nil ||
		!strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("unknown roster name must be refused, got %v", err)
	}
}

// TestRotateRestartsCapabilityDoors pins the fill flow's pickup: rotating a
// recorded capability door's credential restarts the unit (the runner holds
// its package in memory from boot — the seal lands only after a restart); a
// non-capability runner's rotate does not.
func TestRotateRestartsCapabilityDoors(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"unifi-api-admin", "ops-vultr"} {
		if _, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
			Name: name, Kind: "unifi", Address: "https://" + name,
			Secret: []byte("old"), RunnerDir: filepath.Join(dir, "runner", name),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.InsertCapability("unifi-api-admin", state.CapabilityRecord{
		Kind: "unifi", Address: "https://unifi.local", Port: 8801, Rosters: []string{"ai"},
	}); err != nil {
		t.Fatal(err)
	}
	hooked := []string{}
	s := &Server{Store: store, RestartDoor: func(name string, port int) error {
		hooked = append(hooked, fmt.Sprintf("%s:%d", name, port))
		return nil
	}}
	post := func(name string) {
		body := `{"name":"` + name + `","secret":"new-cred"}`
		r := httptest.NewRequest(http.MethodPost, "/api/rotate", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		if rec.Code != 200 {
			t.Fatalf("rotate %s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	post("ops-vultr")
	if len(hooked) != 0 {
		t.Fatalf("a non-capability rotate must not restart anything: %v", hooked)
	}
	post("unifi-api-admin")
	if len(hooked) != 1 || hooked[0] != "unifi-api-admin:8801" {
		t.Fatalf("the capability door's rotate must restart its unit, got %v", hooked)
	}
}

// TestGrantSelfHostedMaintainsRosters pins the durable-grant contract: a
// console grant onto a self-hosted door whose pubkey belongs to a REGISTRY
// agent joins the capability record's Rosters (pod coords + the rebuild's
// re-assertion), an unknown pubkey rides the relay roster alone, and an
// ungrant removes the name (the rebuild would otherwise re-add the revoked
// member). No package is touched (self-hosted doors have none).
func TestGrantSelfHostedMaintainsRosters(t *testing.T) {
	dir := t.TempDir()
	reg, err := agenttools.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("deployer", strings.Repeat("c", 64), "deployer"); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.EnrollRunner(store, "dev-local-lxcadmin", "local", "lxcadmin@h",
		strings.Repeat("a", 64), strings.Repeat("b", 64), "192.168.30.50:8800"); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCapability("dev-local-lxcadmin", state.CapabilityRecord{
		Kind: "local", Address: "lxcadmin@h", Port: 8800, Rosters: []string{},
		Hosted: state.HostedSelf, Host: "192.168.30.50",
		EnrollConfirmedAt: &[]uint64{7}[0],
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store, StateDir: dir, AgentToolsDir: dir}
	grant := func(pubkey string) {
		r := httptest.NewRequest(http.MethodPost, "/api/grant", strings.NewReader(`{"name":"dev-local-lxcadmin","pubkey":"`+pubkey+`"}`))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		if rec.Code != 200 {
			t.Fatalf("grant %s: %d %s", pubkey[:8], rec.Code, rec.Body.String())
		}
	}
	ungrant := func(pubkey string) {
		r := httptest.NewRequest(http.MethodPost, "/api/revoke-grant", strings.NewReader(`{"name":"dev-local-lxcadmin","pubkey":"`+pubkey+`"}`))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		if rec.Code != 200 {
			t.Fatalf("ungrant %s: %d %s", pubkey[:8], rec.Code, rec.Body.String())
		}
	}
	rosters := func() []string {
		fresh, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		rec, _ := fresh.GetCapability("dev-local-lxcadmin")
		return rec.Rosters
	}

	grant(strings.Repeat("c", 64)) // a registry agent: joins the durable roster
	if got := rosters(); len(got) != 1 || got[0] != "deployer" {
		t.Fatalf("registry-agent grant must join Rosters: %v", got)
	}
	grant(strings.Repeat("d", 64)) // the operator/unknown: relay-only
	if got := rosters(); len(got) != 1 {
		t.Fatalf("an unknown pubkey must not join Rosters: %v", got)
	}
	ungrant(strings.Repeat("d", 64))
	if got := rosters(); len(got) != 1 {
		t.Fatalf("an unknown pubkey's ungrant must not touch Rosters: %v", got)
	}
	ungrant(strings.Repeat("c", 64))
	if got := rosters(); len(got) != 0 {
		t.Fatalf("the ungrant must remove the agent from Rosters: %v", got)
	}
}

// TestRotateSelfHostedReturnsPackage pins the fill flow for a SELF-HOSTED
// runner: no CP-side package dir exists, so the rotate seals to the
// presented key and RETURNS the package JSON (the caller carries it to the
// guest and restarts the unit there) — the console restarts nothing.
func TestRotateSelfHostedReturnsPackage(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	encSecret := []byte(strings.Repeat("k", 32))
	encPub, err := crypto.X25519PublicKey(encSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.EnrollRunner(store, "dev-local-lxcadmin", "local", "lxcadmin@h",
		strings.Repeat("a", 64), hex.EncodeToString(encPub), "192.168.30.50:8800"); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCapability("dev-local-lxcadmin", state.CapabilityRecord{
		Kind: "local", Address: "lxcadmin@h", Port: 8800, Rosters: []string{"deployer"},
		Hosted: state.HostedSelf, Host: "192.168.30.50",
	}); err != nil {
		t.Fatal(err)
	}
	hooked := []string{}
	s := &Server{Store: store, RestartDoor: func(name string, port int) error {
		hooked = append(hooked, name)
		return nil
	}}
	post := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/rotate", strings.NewReader(`{"name":"dev-local-lxcadmin","secret":"git-deploy-key"}`))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		return rec
	}
	// The fill is REFUSED until the operator confirmed the presented pubkeys
	// on the door page (the barrier that binds the fill to the key the guest
	// holds — not to whatever the provisioning agent presented).
	if rec := post(); rec.Code != 400 || !strings.Contains(rec.Body.String(), "not confirmed") {
		t.Fatalf("an unconfirmed self-hosted fill must be refused, got %d %s", rec.Code, rec.Body.String())
	}
	now := uint64(time.Now().Unix())
	if err := store.ConfirmEnrollment("dev-local-lxcadmin", now); err != nil {
		t.Fatal(err)
	}
	rec := post()
	if rec.Code != 200 {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	if len(hooked) != 0 {
		t.Fatalf("a self-hosted rotate must not restart anything CP-side: %v", hooked)
	}
	var out struct {
		SelfHosted  bool   `json:"self_hosted"`
		PackageJSON string `json:"package_json"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.SelfHosted || out.PackageJSON == "" {
		t.Fatalf("response must carry the self-hosted package: %s", rec.Body.String())
	}
	// The returned package decrypts with the presented key.
	var pkg wire.SecretPackage
	if err := json.Unmarshal([]byte(out.PackageJSON), &pkg); err != nil {
		t.Fatal(err)
	}
	blob, err := hex.DecodeString(pkg.Secrets["dev-local-lxcadmin"])
	if err != nil {
		t.Fatal(err)
	}
	plain, err := crypto.Open(encSecret, []byte("dev-local-lxcadmin"), blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "git-deploy-key" {
		t.Fatalf("decrypted %q, want git-deploy-key", plain)
	}
}

// TestRevokeGrantPreHostedRecord pins the ungrant path for a door whose
// capability record predates the hosted field (no hosted, empty
// package_dir — the runner is resident on its target but the console can't
// see that). The revoke must be roster-only: skip the package strip, drop
// the name from the record's Rosters, land the relay remove-user. Before the
// fix this fell into the package branch and died on wire.Load's missing
// secrets.json.
func TestRevokeGrantPreHostedRecord(t *testing.T) {
	dir := t.TempDir()
	reg, err := agenttools.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("deployer", strings.Repeat("c", 64), "deployer"); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.EnrollRunner(store, "dev-local-lxcadmin", "local", "lxcadmin@h",
		strings.Repeat("a", 64), strings.Repeat("b", 64), "192.168.30.50:8800"); err != nil {
		t.Fatal(err)
	}
	// Pre-hosted shape: no Hosted field at all, and no capability record's
	// enroll confirmation — the exact shape on worlds built before the field.
	if err := store.InsertCapability("dev-local-lxcadmin", state.CapabilityRecord{
		Kind: "local", Address: "lxcadmin@h", Port: 8800, Rosters: []string{"deployer"},
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store, StateDir: dir, AgentToolsDir: dir}
	ungrant := func(pubkey string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/revoke-grant", strings.NewReader(`{"name":"dev-local-lxcadmin","pubkey":"`+pubkey+`"}`))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		return rec.Code
	}
	// A rostered agent's ungrant lands (roster-only) instead of 500ing on the
	// missing package.
	if code := ungrant(strings.Repeat("c", 64)); code != 200 {
		t.Fatalf("pre-hosted ungrant must be 200, got %d", code)
	}
	fresh, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := fresh.GetCapability("dev-local-lxcadmin")
	if len(rec.Rosters) != 0 {
		t.Fatalf("the ungrant must empty Rosters: %v", rec.Rosters)
	}
	// The runner row (no package either) alone also reads roster-only.
	if err := fresh.InsertCapability("dev-local-lxcadmin", state.CapabilityRecord{
		Kind: "local", Address: "lxcadmin@h", Port: 8800, Rosters: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	if code := ungrant(strings.Repeat("d", 64)); code != 200 {
		t.Fatalf("runner-row-only ungrant must be 200, got %d", code)
	}
}

// TestGrantPreHostedRecord pins the grant-side mirror of the revoke fallback:
// a pre-hosted record (no hosted, empty package_dir) grants roster-only
// instead of dying in provisioner.GrantAgent's wire.Load on the missing
// package. The agent joins Rosters; the runner row gates re-enroll.
func TestGrantPreHostedRecord(t *testing.T) {
	dir := t.TempDir()
	reg, err := agenttools.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("deployer", strings.Repeat("c", 64), "deployer"); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.EnrollRunner(store, "dev-local-lxcadmin", "local", "lxcadmin@h",
		strings.Repeat("a", 64), strings.Repeat("b", 64), "192.168.30.50:8800"); err != nil {
		t.Fatal(err)
	}
	// Pre-hosted shape: no Hosted field at all.
	if err := store.InsertCapability("dev-local-lxcadmin", state.CapabilityRecord{
		Kind: "local", Address: "lxcadmin@h", Port: 8800, Rosters: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store, StateDir: dir, AgentToolsDir: dir}
	r := httptest.NewRequest(http.MethodPost, "/api/grant", strings.NewReader(`{"name":"dev-local-lxcadmin","pubkey":"`+strings.Repeat("c", 64)+`"}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("pre-hosted grant must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	fresh, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cap, _ := fresh.GetCapability("dev-local-lxcadmin")
	if len(cap.Rosters) != 1 || cap.Rosters[0] != "deployer" {
		t.Fatalf("pre-hosted grant must join Rosters: %v", cap.Rosters)
	}
}

// TestGrantRecordlessRunnerRow pins the crash-window shape: a runner row with
// NO capability record (pre-hosted worlds) is roster-only for the live grant,
// and the recordless path must NOT upsert a zero-value record (Kind "" /
// Port 0) — the next build would read that as a CP-guest dynamic door.
func TestGrantRecordlessRunnerRow(t *testing.T) {
	dir := t.TempDir()
	reg, err := agenttools.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The pubkey MUST resolve in the registry: only then does the join run and
	// an unguarded InsertCapability upsert the zero record. An unregistered
	// pubkey makes the pin vacuous (the join never fires).
	if _, err := reg.RegisterAgent("deployer", strings.Repeat("c", 64), "deployer"); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.EnrollRunner(store, "orphan-door", "local", "lxcadmin@h",
		strings.Repeat("a", 64), strings.Repeat("b", 64), "192.168.30.50:8800"); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store, StateDir: dir, AgentToolsDir: dir}
	r := httptest.NewRequest(http.MethodPost, "/api/grant", strings.NewReader(`{"name":"orphan-door","pubkey":"`+strings.Repeat("c", 64)+`"}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("recordless grant must be 200, got %d: %s", rec.Code, rec.Body.String())
	}
	fresh, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, isCap := fresh.GetCapability("orphan-door"); isCap {
		t.Fatal("a recordless grant must not upsert a capability record")
	}
}
