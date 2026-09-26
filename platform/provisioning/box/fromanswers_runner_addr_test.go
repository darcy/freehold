package box

import "testing"

// The install's config must record the box-side runner's loopback addr: the
// uninstall picks its runner path by cfg.Runner.Addr — an empty addr forces
// the transient path, whose pct-destroy-on-running-guest is broken.
func TestFromAnswersDerivesRunnerAddrFromLocalPort(t *testing.T) {
	e := &Engine{F: Flags{
		RelayDomain: "relay.freehold-test.darcydev.net",
		CpDomain:    "cp.freehold-test.darcydev.net",
		LocalPort:   8787,
	}}
	cfg := e.fromAnswers()
	if cfg.Runner.Addr != "127.0.0.1:8787" {
		t.Fatalf("fromAnswers Runner.Addr = %q, want 127.0.0.1:8787 (LocalPort %d)", cfg.Runner.Addr, e.F.LocalPort)
	}
}
