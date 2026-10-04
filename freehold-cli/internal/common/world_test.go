package common

import (
	"net"
	"testing"

	"freehold/contract/config"
)

// TestAddrReachable: a listening socket is reachable; a closed port is not.
func TestAddrReachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !AddrReachable(l.Addr().String()) {
		t.Fatalf("listening addr %s must be reachable", l.Addr())
	}
	l.Close()
	if AddrReachable(l.Addr().String()) {
		t.Fatalf("closed addr %s must not be reachable", l.Addr())
	}
}

// TestRunnerKeyRefsNoPackage: with no readable package (thin box) there are no
// exact body refs — the caller's target-comment sweep removes the key.
func TestRunnerKeyRefsNoPackage(t *testing.T) {
	cfg := &config.Config{Runner: config.RunnerRef{Target: "proxmox-box", Pubkey: "deadbeef"}}
	if refs := RunnerKeyRefs(cfg, nil); len(refs) != 0 {
		t.Errorf("refs = %v, want none (target sweep handles it)", refs)
	}
}
