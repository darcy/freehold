package proxmox

import "testing"

// ---- pct output parsing ------------------------------------------------------

func TestFindVmidInList(t *testing.T) {
	out := `VMID       Status     Lock         Name
100        running                 freehold-test-darcydev-net-relay
101        running                 freehold-test-darcydev-net-cp
200        stopped                 other-darcydev-net-relay
`
	vmid, err := findVmidInList(out, "freehold-test-darcydev-net-cp")
	if err != nil {
		t.Fatal(err)
	}
	if vmid != 101 {
		t.Errorf("vmid = %d, want 101", vmid)
	}
	// suffix-only names must NOT match a different world's container.
	if _, err := findVmidInList(out, "darcydev-net-relay"); err == nil {
		t.Error("a non-exact name should not match")
	}
	if _, err := findVmidInList(out, "missing-relay"); err == nil {
		t.Error("absent name should error")
	}
}

func TestParseLxcIP(t *testing.T) {
	ip, err := parseLxcIP("2: eth0    inet 192.168.30.8/24 brd 192.168.30.255 scope global eth0", "100")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.168.30.8/24" {
		t.Errorf("ip = %q", ip)
	}
	// loopback alone => error.
	if _, err := parseLxcIP("1: lo    inet 127.0.0.1/8 scope host lo", "100"); err == nil {
		t.Error("loopback-only should error")
	}
}

func TestParsePctMounts(t *testing.T) {
	out := `arch: amd64
mp0: pve:freehold-test-darcydev-net/relay,mp=/var/lib/docker
mp1: pve:freehold-test-darcydev-net/deploy,mp=/srv/data/relay
net0: name=eth0,bridge=vmbr0,ip=dhcp
mptmp: something
`
	mounts := parsePctMounts(out)
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2: %v", len(mounts), mounts)
	}
	if mounts[0] != "/var/lib/docker" || mounts[1] != "/srv/data/relay" {
		t.Errorf("mounts = %v", mounts)
	}
}
