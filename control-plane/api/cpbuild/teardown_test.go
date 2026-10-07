package cpbuild

import (
	"fmt"
	"slices"
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

// TestWorldTeardownCfgManagesGateway pins the leak fix: the CP-driven teardown
// manages the gateway too — resolved by name when the spec carries no recorded
// gateway vmid (an old world-config, or a cross-home re-render) — and destroys
// it LAST, after the guests it routes for. A host without the guest (flat-LAN
// world) keeps the relay/k3s scope.
func TestWorldTeardownCfgManagesGateway(t *testing.T) {
	newSpec := func(list string) *Spec {
		s := &Spec{Name: "librem", RelayHost: "relay.librem.example", RelayLxc: 100, K3sVmid: 102}
		s.execHook = func(cmd string, _ uint64, _ ...string) (string, error) {
			if cmd != "pct list" {
				return "", fmt.Errorf("unexpected cmd %q", cmd)
			}
			return list, nil
		}
		return s
	}
	withGateway := "VMID Status Name\n" +
		"100  running librem-relay\n" +
		"101  running librem-cp\n" +
		"102  running librem-k3s\n" +
		"109  running librem-gateway\n"
	flatLan := "VMID Status Name\n" +
		"100  running librem-relay\n" +
		"101  running librem-cp\n" +
		"102  running librem-k3s\n"

	cfg, err := worldTeardownCfg(newSpec(withGateway))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Managed, []string{"relay", "k3s", "gateway"}) {
		t.Fatalf("managed = %v, want [relay k3s gateway] (the gateway destroyed last)", cfg.Managed)
	}
	if gw := cfg.Vmid["gateway"]; gw == nil || *gw != 109 {
		t.Fatalf("gateway vmid = %v, want 109 (adopted by name from a record-blind spec)", gw)
	}

	cfg, err = worldTeardownCfg(newSpec(flatLan))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Managed, []string{"relay", "k3s"}) {
		t.Fatalf("flat-LAN managed = %v, want [relay k3s]", cfg.Managed)
	}
	if gw, ok := cfg.Vmid["gateway"]; ok {
		t.Fatalf("flat-LAN vmid map carries gateway = %v, want absent", gw)
	}
}
