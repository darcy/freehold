package box

import (
	"fmt"
	"strings"
	"testing"

	"freehold/contract/client"
	"freehold/platform/provisioning"
)

// scriptProviderFunc adapts a func to the Provider seam: the canned stdout is
// keyed on the exec'd script (the probe pings the CANDIDATE's own offsets, so
// the func can tell which candidate ran).
type scriptProviderFunc func(cmd string) string

func (f scriptProviderFunc) GuestExec(guest, cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
	return &client.ExecOutcome{Stdout: f(cmd)}, nil
}
func (f scriptProviderFunc) ListGuests() ([]provisioning.Guest, error) { return nil, nil }
func (f scriptProviderFunc) NextFreeVMID() (uint32, error)             { return 0, nil }
func (f scriptProviderFunc) GuestIPv4(string) (string, error)          { return "", errNotFound }
func (f scriptProviderFunc) GuestMounts(string) ([]string, error)      { return nil, nil }
func (f scriptProviderFunc) DestroyGuest(string) error                 { return nil }
func (f scriptProviderFunc) StopGuestCmd(uint32) string                { return "" }
func (f scriptProviderFunc) LocalLvmStatus() (string, int, error)      { return "", 0, nil }
func (f scriptProviderFunc) RepointLocalLvm(string) error              { return nil }

// TestProbeSubnetClaimed: the probe ARPs the candidate's world addresses and
// reads the neigh table — a claimant's lladdr line and a pre-existing host
// route are both claims; a silent L2 and INCOMPLETE rows are not.
func TestProbeSubnetClaimed(t *testing.T) {
	claim77 := scriptProviderFunc(func(cmd string) string {
		if strings.Contains(cmd, "ping -c1 -W1") {
			// `ip neigh show dev <bridge>` drops the dev column.
			return "10.77.0.13 lladdr bc:24:11:57:7c:df REACHABLE\n" +
				"10.77.0.12 INCOMPLETE\n"
		}
		return ""
	})
	ev, err := probeSubnetClaimed(claim77, "10.77.0.0/24", "vmbr0")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(ev) != 1 || !strings.Contains(ev[0], "10.77.0.13 answers from bc:24:11:57:7c:df") {
		t.Errorf("evidence = %v, want the one lladdr claimant", ev)
	}
	if strings.Contains(strings.Join(ev, ";"), "10.77.0.12") {
		t.Errorf("INCOMPLETE counted as a claim: %v", ev)
	}
	// The script must clean up its temp route.
	seen := ""
	seenSpy := scriptProviderFunc(func(cmd string) string { seen = cmd; return "" })
	if _, err := probeSubnetClaimed(seenSpy, "10.77.0.0/24", "vmbr0"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(seen, "ip route del 10.77.0.0/24 dev vmbr0") {
		t.Errorf("probe script missing the route cleanup:\n%s", seen)
	}

	// A pre-existing host route for the cidr is itself the claim.
	routed := scriptProviderFunc(func(string) string { return "ROUTE 10.77.0.0/24\n" })
	if ev, err := probeSubnetClaimed(routed, "10.77.0.0/24", "vmbr0"); err != nil || len(ev) != 1 ||
		!strings.Contains(ev[0], "host route for 10.77.0.0/24") {
		t.Errorf("route claim: ev=%v err=%v", ev, err)
	}

	// A clean L2 (no neigh rows, no route row) = no evidence.
	empty := scriptProviderFunc(func(string) string { return "" })
	if ev, err := probeSubnetClaimed(empty, "10.77.0.0/24", "vmbr0"); err != nil || len(ev) != 0 {
		t.Errorf("clean probe: ev=%v err=%v, want none", ev, err)
	}

	// Only /24s are probed.
	if ev, err := probeSubnetClaimed(empty, "10.77.0.0/16", "vmbr0"); err != nil || ev != nil {
		t.Errorf("non-/24 must skip the probe: ev=%v err=%v", ev, err)
	}
}

// TestDeriveGatewayCIDRBumpsOnClaim: the derive takes the first clean
// candidate and prints the bump + its evidence; all-claimed fails loud.
func TestDeriveGatewayCIDRBumpsOnClaim(t *testing.T) {
	var out strings.Builder
	// The host claims every 10.77.0.x probe (another world's subnet) but
	// nothing on 10.78 — the derive settles on 10.78.0.0/24 and says why.
	e := &Engine{F: Flags{ProxyIP: "192.168.30.5/24", Bridge: "vmbr0"}, Out: &out}
	e.Provider = scriptProviderFunc(func(cmd string) string {
		if strings.Contains(cmd, "10.77.0.") {
			return "10.77.0.13 lladdr bc:24:11:57:7c:df REACHABLE\n"
		}
		return ""
	})
	cidr, err := e.deriveGatewayCIDR()
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if cidr != "10.78.0.0/24" {
		t.Errorf("derived %q, want 10.78.0.0/24 (10.77 claimed)", cidr)
	}
	if !strings.Contains(out.String(), "10.77.0.0/24 is claimed on this LAN") ||
		!strings.Contains(out.String(), "bc:24:11:57:7c:df") {
		t.Errorf("the bump must print the claimed candidate + evidence:\n%s", out.String())
	}

	// Clean first candidate: silent, no bump line.
	out.Reset()
	e.Provider = scriptProviderFunc(func(string) string { return "" })
	if cidr, err = e.deriveGatewayCIDR(); err != nil || cidr != "10.77.0.0/24" || out.String() != "" {
		t.Errorf("clean derive: cidr=%q err=%v out=%q, want 10.77.0.0/24/nil/silent", cidr, err, out.String())
	}

	// Everything claimed: loud failure (the operator pins one by hand).
	e.Provider = scriptProviderFunc(func(cmd string) string {
		if strings.Contains(cmd, fmt.Sprintf("ping -c1 -W1")) {
			return "ROUTE 10.0.0.0\n"
		}
		return ""
	})
	if _, err := e.deriveGatewayCIDR(); err == nil || !strings.Contains(err.Error(), "--gateway-cidr") {
		t.Errorf("all-claimed must fail loud pointing at --gateway-cidr, got %v", err)
	}
}
