package cpbuild

import (
	"reflect"
	"testing"
)

// TestCoordsSpecRoundTrip proves Coords ⇄ Spec stay in lockstep (and that the
// non-secret Coords subset round-trips): an empty field on either side would
// silently lose a coordinate the CP build engine needs.
func TestCoordsSpecRoundTrip(t *testing.T) {
	c := Coords{
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
