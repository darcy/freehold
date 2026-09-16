package agenttools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFactsStoreRegisterRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "facts.json")
	f, err := OpenFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	// unregistered -> empty
	if len(f.Facts().Plane.Mounts) != 0 {
		t.Fatal("unregistered facts must be empty")
	}
	facts := WorldFacts{
		Domains: WorldDomains{Relay: "relay.example", CP: "cp.example", Proxy: "192.168.30.8"},
		Plane: WorldPlane{
			Backend: "pve", BackendKind: "lvmth", ThinPool: "freehold-thin",
			Mounts: []WorldPlaneMount{{Tenant: "cp", Source: "/freehold/x/cp", GuestPath: "/srv/data/cp", Backup: true}},
		},
		Certs: []WorldCert{{Slot: "relay", Domain: "relay.example", Expiry: "2027-09-08T00:00:00Z", Issuer: "lego"}},
	}
	if err := f.Register(facts); err != nil {
		t.Fatal(err)
	}
	// reload from disk (the durable store)
	f2, err := OpenFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	got := f2.Facts()
	if got.Domains.Relay != "relay.example" || len(got.Plane.Mounts) != 1 || len(got.Certs) != 1 {
		t.Fatalf("facts round-trip lost data: %+v", got)
	}
	if got.Plane.Mounts[0].GuestPath != "/srv/data/cp" || !got.Plane.Mounts[0].Backup {
		t.Fatalf("plane mount not preserved: %+v", got.Plane.Mounts[0])
	}
	// register is idempotent (the build's register-at-build)
	if err := f.Register(got); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); len(b) == 0 {
		t.Fatal("facts.json must be written")
	}
}