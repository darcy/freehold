package box

import (
	"testing"

	"freehold/contract/config"
)

// A run whose answers carry no runner addr (the CP-driven reconcile, a
// thin-box build) must not WIPE the recorded addr at FinalSave — the pubkey is
// always loaded, so the addr needs its own preserve rule or uninstall later
// takes the broken transient path.
func TestMergeFromAnswersPreservesRunnerAddr(t *testing.T) {
	prev := &config.Config{Runner: config.RunnerRef{Addr: "127.0.0.1:8787", Pubkey: "aa", Target: "proxmox-box"}}
	ans := &config.Config{Runner: config.RunnerRef{Addr: "", Pubkey: "aa", Target: "proxmox-box"}}
	if got := mergeFromAnswers(ans, prev).Runner.Addr; got != "127.0.0.1:8787" {
		t.Fatalf("mergeFromAnswers wiped the recorded runner addr: %q", got)
	}
}

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
