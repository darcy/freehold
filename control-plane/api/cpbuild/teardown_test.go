package cpbuild

import (
	"strings"
	"testing"

	"freehold/platform/provisioning/planebase"
)

// TestCpDestroyDetached pins the risky shape: the CP LXC (which hosts the
// console + co-located runner) must be destroyed DETACHED so the runner's exec
// returns before its own container goes away. A blocking pct destroy here would
// hang every CP-owned teardown.
func TestCpDestroyDetached(t *testing.T) {
	cmd := cpDestroyDetached(100)
	if !strings.HasPrefix(cmd, "setsid ") {
		t.Fatalf("cp destroy must be detached (setsid), got %q", cmd)
	}
	if !strings.HasSuffix(cmd, " &") {
		t.Fatalf("cp destroy must run in the background, got %q", cmd)
	}
	for _, want := range []string{"pct stop 100 --skiplock", "pct destroy 100"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("cp destroy missing %q: %q", want, cmd)
		}
	}
}

// TestVmidPtr treats 0 as unrecorded (nil = teardown's "already gone") and a
// real id as recorded.
func TestVmidPtr(t *testing.T) {
	if vmidPtr(0) != nil {
		t.Fatal("vmid 0 must be nil (never created)")
	}
	if p := vmidPtr(101); p == nil || *p != 101 {
		t.Fatalf("vmidPtr(101) = %v, want 101", p)
	}
}

func TestTenantFromName(t *testing.T) {
	for name, want := range map[string]planebase.Tenant{
		"relay":       planebase.TenantRelay,
		"cp":          planebase.TenantCp,
		"k3s-volumes": planebase.TenantK3sVolumes,
	} {
		got, err := tenantFromName(name)
		if err != nil || got != want {
			t.Fatalf("tenantFromName(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := tenantFromName("nope"); err == nil {
		t.Fatal("tenantFromName(nope) must error")
	}
}
