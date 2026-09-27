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
	r := httptest.NewRequest(http.MethodPost, "/api/rotate", strings.NewReader(`{"name":"dev-local-lxcadmin","secret":"git-deploy-key"}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
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
