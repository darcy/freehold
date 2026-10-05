package provisioner

import (
	"testing"

	"freehold/contract/wire"
	"freehold/control-plane/state"
)

func TestValidateProbe(t *testing.T) {
	for probe, want := range map[string]string{
		"GET /user/tokens/verify":        "GET /user/tokens/verify bearer 200",
		"get /v2/account":                "GET /v2/account bearer 200",
		"POST /api/auth/login json-body": "POST /api/auth/login json-body 200",
		"GET /x none 201":                "GET /x none 201",
	} {
		got, err := ValidateProbe(probe)
		if err != nil {
			t.Fatalf("ValidateProbe(%q): %v", probe, err)
		}
		if got != want {
			t.Fatalf("ValidateProbe(%q) = %q, want %q", probe, got, want)
		}
	}
	if _, err := ValidateProbe("GET /b2api/v3/b2_authorize_account basic"); err != nil {
		t.Fatalf("basic auth: %v", err)
	}
	for _, bad := range []string{
		"", "GET", "GET /p bearer 200 extra", "DELETE /p", "TRACE /p",
		"GET p", "GET /p hmac", "GET /p bearer 20", "GET /p bearer 2a0",
		"GET /p;ls bearer", "curl http://evil",
	} {
		if _, err := ValidateProbe(bad); err == nil {
			t.Fatalf("ValidateProbe(%q): want error", bad)
		}
	}
}

func TestValidateProbeBody(t *testing.T) {
	if err := ValidateProbeBody(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`); err != nil {
		t.Fatalf("valid JSON body refused: %v", err)
	}
	for _, bad := range []string{
		"not json", `{"a":1}'; rm -rf /`, `{"a":"` + "$" + `(id)"}`, "{a:1}",
	} {
		if err := ValidateProbeBody(bad); err == nil {
			t.Fatalf("ValidateProbeBody(%q): want error", bad)
		}
	}
}

// TestProvisionShipsProbe pins the verify-arm transport: a provisioned
// request's probe lands in the shipped package's TargetMeta AND the state
// record, and the rotate paths carry it forward unchanged.
func TestProvisionShipsProbe(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	req := &ProvisionRequest{
		Name: "nas-api-admin", Kind: "truenas", Address: "https://nas.example",
		Secret: []byte("token"), RunnerDir: t.TempDir() + "/runner/nas-api-admin",
		Probe: "GET /api/v2/me",
	}
	if _, err := ProvisionRunner(store, req); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.GetSecret("nas-api-admin")
	if rec.Probe != "GET /api/v2/me bearer 200" {
		t.Fatalf("state record probe = %q", rec.Probe)
	}
	pkg, err := wire.Load(req.RunnerDir)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Targets["nas-api-admin"].Probe != "GET /api/v2/me bearer 200" {
		t.Fatalf("shipped probe = %+v", pkg.Targets["nas-api-admin"])
	}

	// Rotate re-ships the SAME verify arm (the fill must not lose it).
	if _, err := RotateSecret(store, "nas-api-admin", []byte("token2")); err != nil {
		t.Fatal(err)
	}
	pkg, err = wire.Load(req.RunnerDir)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Targets["nas-api-admin"].Probe != "GET /api/v2/me bearer 200" {
		t.Fatalf("probe lost on rotate: %+v", pkg.Targets["nas-api-admin"])
	}

	// A bad probe refuses provision before anything ships.
	bad := &ProvisionRequest{
		Name: "nas-api-bad", Kind: "truenas", Address: "https://nas.example",
		Secret: []byte("t"), RunnerDir: t.TempDir() + "/runner/nas-api-bad",
		Probe: "GET /p; curl evil",
	}
	if _, err := ProvisionRunner(store, bad); err == nil {
		t.Fatal("bad probe accepted")
	}
	if _, ok := store.GetRunner("nas-api-bad"); ok {
		t.Fatal("bad-provision runner row left behind")
	}
}
