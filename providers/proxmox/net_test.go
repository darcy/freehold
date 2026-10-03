package proxmox

import "testing"

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// The gateway's net shapes (docs/NETWORK.md): a tagged internal guest with
// the gateway as route; the gateway guest's own two NICs — eth0 LAN, eth1
// tagged internal. Untagged/legacy specs render byte-identical to before.
func TestNetArgs(t *testing.T) {
	spec := &ProxmoxLxcSpec{Bridge: "vmbr0", NetIP: strPtr("10.77.0.11/24"), NetGW: strPtr("10.77.0.1"), Tag: intPtr(77)}
	want := " --net0 name=eth0,bridge=vmbr0,ip=10.77.0.11/24,gw=10.77.0.1,type=veth,tag=77"
	if got := netArgs(spec); got != want {
		t.Fatalf("tagged = %q; want %q", got, want)
	}
	spec = &ProxmoxLxcSpec{Bridge: "vmbr0"}
	if got := netArgs(spec); got != " --net0 name=eth0,bridge=vmbr0,ip=dhcp,type=veth" {
		t.Fatalf("legacy = %q", got)
	}
	spec = &ProxmoxLxcSpec{
		Bridge: "vmbr0", NetIP: strPtr("192.168.30.8/24"), NetGW: strPtr("192.168.30.1"),
		Net1IP: strPtr("10.77.0.1/24"), Net1Tag: intPtr(77),
	}
	want = " --net0 name=eth0,bridge=vmbr0,ip=192.168.30.8/24,gw=192.168.30.1,type=veth" +
		" --net1 name=eth1,bridge=vmbr0,ip=10.77.0.1/24,tag=77,type=veth"
	if got := netArgs(spec); got != want {
		t.Fatalf("gateway two-NIC = %q; want %q", got, want)
	}
}
