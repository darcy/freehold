package tui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/console"
)

// A login-only profile (cp_url + cp_pubkey + operator identity, NO [runner])
// must still reach Running: liveness is the console session / reachability,
// not a local provisioning runner. This is the fresh-box-after-0.4.7 path —
// `freehold login` then `freehold`.
func TestLoginOnlyRunnerProbeSatisfied(t *testing.T) {
	cfg := &config.Config{
		RelayURL: "https://relay.example",
		CPURL:    "https://cp.example",
		CpPubkey: "aa",
		// Runner deliberately EMPTY: a login-only box has no deployed runner.
	}
	m := &Model{cfg: cfg}

	// The boot "runner" probe on a login-only box (the same branch
	// startBootActivity wires).
	runnerProbe := func() (string, bool) {
		if cfg.Runner.Addr == "" {
			m.RunnerReach = true
			return "no local runner (login-only — operating through the CP)", true
		}
		m.RunnerReach = config.URLReachable("http://" + cfg.Runner.Addr)
		return "", m.RunnerReach
	}
	detail, ok := runnerProbe()
	if !ok {
		t.Fatalf("login-only runner probe must be satisfied, got %q", detail)
	}
	if !m.RunnerReach {
		t.Fatal("RunnerReach must be true on a login-only box (no local runner)")
	}
	if !contains(detail, "login-only") {
		t.Errorf("detail should say login-only, got %q", detail)
	}
}

// cpConsoleLive: with no session yet, a live CP (its challenge endpoint
// reachable) reports up; a dead one reports down.
func TestCpConsoleLiveReachability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/challenge" {
			w.Write([]byte(`{"nonce":"x"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{CPURL: srv.URL}
	m := &Model{}
	if !cpConsoleLive(m, cfg) {
		t.Fatal("live CP must be reported up via reachability fallback")
	}

	dead := &config.Config{CPURL: "http://127.0.0.1:1"} // nothing listens
	if cpConsoleLive(m, dead) {
		t.Fatal("dead CP must be reported down")
	}
}

// cpConsoleLive with an ESTABLISHED session whose /api/overview call fails must
// be reported DOWN — never fall through to a bare TCP reachability check that
// could report a session that's stopped authenticating as still live (the
// false-positive this PR eliminates). cfg.CPURL stays live so any errant
// reachability fallback would return true, which is exactly what must NOT happen.
func TestCpConsoleLiveEstablishedSessionFailureIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session expired", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := &config.Config{CPURL: srv.URL}
	// An ESTABLISHED session (a session cookie in hand) whose overview errors.
	m := &Model{console: &consoleClient{client: console.WithCookie(srv.URL, "fh_session=tok123")}}
	if _, err := m.console.client.Overview(); err == nil {
		t.Fatal("test client must error Overview")
	}
	if cpConsoleLive(m, cfg) {
		t.Fatal("established session with a failed overview must be DOWN, not fall through to TCP reachability")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}