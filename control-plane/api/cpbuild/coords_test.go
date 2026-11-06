package cpbuild

import (
	"reflect"
	"testing"

	"freehold/contract/config"
)

// TestCoordsSpecRoundTrip proves Coords ⇄ Spec stay in lockstep (and that the
// non-secret Coords subset round-trips): an empty field on either side would
// silently lose a coordinate the CP build engine needs.
func TestCoordsSpecRoundTrip(t *testing.T) {
	c := Coords{
		Name:     "librem2",
		StateDir: "/srv/data/cp/control-plane", RelayURL: "https://relay.example", RelayAuthURL: "https://relay.example",
		RelayWS: "wss://relay.example", RelayHost: "relay.example", RelayIP: "192.168.30.10",
		CpHost: "cp.example", CpIP: "192.168.30.11", CpLxc: 100, ProxyIP: "192.168.30.8",
		LitellmIP: "192.168.30.8", PlanePool: "pve", PlaneKind: "zfs", ThinPool: "",
		SizeGB: 10, PoolSizeGB: 40, RootfsGB: 16, MemoryMB: 2048, StorageName: "local-lvm",
		RelayGW: "192.168.30.1", Bridge: "vmbr0", RelayLxc: 101, RelayCompose: "/srv/data/relay/deploy/compose",
		K3sVmid: 102, RunnerAddr: "127.0.0.1:8787", RunnerPK: "1111111111111111111111111111111111111111111111111111111111111111",
		RunnerTarget: "proxmox-box", CpaName: "freehold", OwnerPub: "1dc07610f4", LitellmBaseURL: "http://192.168.30.8:31400",
		SelfURL: "http://192.168.30.11:8080",
	}
	spec := NewSpec(c, make([]byte, 32), "aud")
	// Sec/Audience come from the driving identity, not Coords.
	if string(spec.Sec) != string(make([]byte, 32)) || spec.Audience != "aud" {
		t.Fatal("NewSpec must set Sec/Audience from the identity, not Coords")
	}
	// Coords() must round-trip the non-secret fields exactly.
	back := spec.Coords()
	if !reflect.DeepEqual(c, back) {
		t.Fatalf("Coords ⇄ Spec round-trip mismatch:\n want %+v\n  got %+v", c, back)
	}
}

// TestApplyCoordsReseatsTheSpec: a built Spec re-seated on new coords takes
// every coordinate (an explicit pangolin CLEAR included: the next render must
// not resurrect it) while the driving identity (Sec/Audience) is untouched.
func TestApplyCoordsReseatsTheSpec(t *testing.T) {
	sec := make([]byte, 32)
	sec[0] = 0x42
	base := Coords{Name: "librem", CpHost: "cp.example", ProxyIP: "192.168.30.8"}
	spec := NewSpec(base, sec, "aud")

	next := base
	next.ProxyIP = "192.168.30.9"
	next.Pangolin = &config.PangolinCoords{Org: "freehold", Site: "librem", Domain: "librem.example.test", DNSIP: "203.0.113.9"}
	spec.ApplyCoords(next)
	if spec.ProxyIP != "192.168.30.9" || spec.Pangolin == nil || spec.Pangolin.DNSIP != "203.0.113.9" {
		t.Fatalf("apply must re-seat the coords: %+v", spec.Coords())
	}

	cleared := next
	cleared.Pangolin = nil
	spec.ApplyCoords(cleared)
	if spec.Pangolin != nil {
		t.Fatalf("an explicit clear must stick (got %+v)", spec.Pangolin)
	}
	if string(spec.Sec) != string(sec) || spec.Audience != "aud" {
		t.Fatal("apply must not touch the driving identity")
	}
}
