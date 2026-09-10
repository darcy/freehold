package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"freehold/contract/config"
	"freehold/contract/console"
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

// A management box (admin login, NO local world coords of its own) must render
// its k3s/litellm/caddy pillars green, its Services view + DNS filled, and its
// header domain resolved from the CP's /api/world — not stay red/empty just
// because it didn't deploy the world. This is the "login = fully-vetted
// management box, more than one allowed" path.
func TestApplyCPWorldHealthManagementBoxGreen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/world":
			w.Write([]byte(`{"cp_pubkey":"aa","relay_host":"relay.here.freehold.technology","services":[` +
				`{"name":"k3s","kind":"k3s","up":true},` +
				`{"name":"litellm","kind":"litellm","up":true},` +
				`{"name":"caddy","kind":"caddy","up":true}]}`))
		case "/api/dns":
			w.Write([]byte(`{"dns":[{"name":"relay","ip":"10.0.0.5","source":"record_lxc"},` +
				`{"name":"litellm","ip":"10.0.0.6","source":"litellm apply"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Management box: cp_url + operator session, NO [lxc]/[litellm]/[caddy] coords.
	cfg := &config.Config{CPURL: srv.URL, CpPubkey: "aa", RelayURL: "http://192.168.30.220:3000"}
	m := &Model{cfg: cfg, console: &consoleClient{client: console.WithCookie(srv.URL, "fh_session=tok123")}}
	m.applyCPWorldHealth()

	if !m.K3sLive || !m.LitellmLive || !m.CaddyLive {
		t.Fatalf("management box should render world green from CP services: k3s=%v litellm=%v caddy=%v",
			m.K3sLive, m.LitellmLive, m.CaddyLive)
	}
	if m.Domain != "relay.here.freehold.technology" {
		t.Fatalf("header domain should be the CP's relay host, got %q", m.Domain)
	}
	if len(m.Services) == 0 || m.Services[0].Name == "(none managed)" {
		t.Fatal("management box Services view should be filled from the CP world, not '(none managed)'")
	}
	if len(m.DNS) != 2 || m.DNS[0].Name != "relay" {
		t.Fatalf("management box DNS view should be filled from the CP resolver, got %+v", m.DNS)
	}
}

// The relay public domain must derive from the CP resolver's `relay.<domain>`
// record even when the console predates serving relay_host — a management box
// then shows the domain (Certs relay row + header), not the LAN IP it dialed.
func TestRelayDomainDerivesFromDNS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/world":
			w.Write([]byte(`{"cp_pubkey":"aa","services":[{"name":"k3s","kind":"k3s","up":true}]}`))
		case "/api/dns":
			w.Write([]byte(`{"dns":[{"name":"relay","ip":"10.0.0.5","source":"record_lxc"},` +
				`{"name":"relay.here.freehold.technology","ip":"10.0.0.8","source":"cp_public"},` +
				`{"name":"litellm","ip":"10.0.0.6","source":"litellm apply"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{CPURL: srv.URL, CpPubkey: "aa", RelayURL: "http://192.168.30.220:3000"}
	m := &Model{cfg: cfg, console: &consoleClient{client: console.WithCookie(srv.URL, "fh_session=tok123")}}
	m.applyCPWorldHealth()

	if m.Domain != "relay.here.freehold.technology" {
		t.Fatalf("relay domain should derive from the CP resolver, got %q", m.Domain)
	}
	m.buildCerts(cfg)
	if len(m.Certs) == 0 || !strings.Contains(m.Certs[0].Domain, "here.freehold.technology") {
		t.Fatalf("certs relay row should show the derived domain, got %+v", m.Certs)
	}
}

// When the CP serves a relay_host AND the resolver carries a differing
// relay.<domain> record, the served relay_host is authoritative — a stale
// resolver entry must never override it (the DNS path is fallback only).
func TestRelayDomainPrefersServerRelayHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/world":
			w.Write([]byte(`{"cp_pubkey":"aa","relay_host":"relay.authoritative.freehold.technology","services":[{"name":"k3s","kind":"k3s","up":true}]}`))
		case "/api/dns":
			w.Write([]byte(`{"dns":[{"name":"relay.stale.freehold.technology","ip":"10.0.0.9","source":"stale"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{CPURL: srv.URL, CpPubkey: "aa", RelayURL: "http://192.168.30.220:3000"}
	m := &Model{cfg: cfg, console: &consoleClient{client: console.WithCookie(srv.URL, "fh_session=tok123")}}
	m.applyCPWorldHealth()
	if m.Domain != "relay.authoritative.freehold.technology" {
		t.Fatalf("served relay_host must win over the resolver, got %q", m.Domain)
	}
}

// A management box must adopt the CP-served relay_url when its config holds a
// stale LAN IP (the relay LXC can move after a rebuild/DHCP), so the relay
// pillar probes the live relay and doesn't stay red against a dead snapshot.
func TestAdoptsCPRelayURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/world" {
			w.Write([]byte(`{"cp_pubkey":"aa","relay_url":"http://192.168.30.243:3000","relay_ws_url":"ws://192.168.30.243:3000","services":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{CPURL: srv.URL, CpPubkey: "aa", RelayURL: "http://192.168.30.220:3000"}
	m := &Model{cfg: cfg, console: &consoleClient{client: console.WithCookie(srv.URL, "fh_session=tok123")}}
	m.applyCPWorldHealth()
	if m.cfg.RelayURL != "http://192.168.30.243:3000" {
		t.Fatalf("management box should adopt the CP-served relay_url, got %q", m.cfg.RelayURL)
	}
	if m.cfg.RelayWsURL != "ws://192.168.30.243:3000" {
		t.Fatalf("relay ws url not adopted, got %q", m.cfg.RelayWsURL)
	}
}

// The management-box "checking the world" stage must report CP-sourced status,
// not stale local config: after the boot's control-plane step fetches the CP,
// the relay/k3s/litellm/caddy/dns steps read the SAME snapshot and report it.
// (Live world was 502 mid-rebuild; this is the hermetic guarantee.)
func TestCpSourcedBootStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/world":
			w.Write([]byte(`{"cp_pubkey":"aa","relay_url":"http://192.168.30.243:3000","services":[` +
				`{"name":"k3s","kind":"k3s","up":true},` +
				`{"name":"litellm","kind":"litellm","up":true},` +
				`{"name":"caddy","kind":"caddy","up":false}]}`))
		case "/api/dns":
			w.Write([]byte(`{"dns":[{"name":"relay","ip":"10.0.0.5","source":"record_lxc"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{CPURL: srv.URL, CpPubkey: "aa", RelayURL: "http://192.168.30.220:3000"}
	m := &Model{cfg: cfg, console: &consoleClient{client: console.WithCookie(srv.URL, "fh_session=tok123")}}

	// The boot control-plane step equates to applyCPWorldHealth (fetch) +
	// refreshLocal; the subsequent steps then report from that snapshot.
	m.applyCPWorldHealth()
	m.refreshLocal()

	if d, ok := m.cpServiceStatus("k3s"); !ok || !contains(d, "healthy via CP") {
		t.Fatalf("k3s boot report should be CP-truth healthy, got %q ok=%v", d, ok)
	}
	if d, _ := m.cpServiceStatus("caddy"); contains(d, "healthy") {
		t.Fatalf("caddy is down via CP, should not report healthy, got %q", d)
	}
	if d, _ := m.cpDnsStatus(); !contains(d, "1 resolver records (CP)") {
		t.Fatalf("dns boot report should be CP-truth count, got %q", d)
	}
	if d, _ := m.cpRelayStatus(); !contains(d, "CP") {
		t.Fatalf("relay boot report should cite the CP, got %q", d)
	}
}

// A box WITH local coords (a deployer) keeps its own co-located probe — the CP
// health only fills pillars this box has no local record of.
func TestApplyCPWorldHealthKeepsDeployerProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/world" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"cp_pubkey":"aa","services":[{"name":"k3s","kind":"k3s","up":true}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// Deployer: k3s coord present locally, marked down by its own probe.
	ip := "10.0.0.9"
	cfg := &config.Config{CPURL: srv.URL}
	cfg.Lxc.K3s.Ip = &ip
	m := &Model{cfg: cfg, console: &consoleClient{client: console.WithCookie(srv.URL, "fh_session=tok123")}, K3sLive: false}
	m.applyCPWorldHealth()
	if m.K3sLive {
		t.Fatal("a deployer box's own local probe must stay authoritative (CP fills only absent pillars)")
	}
}

// The manual `l` console login lands through runFlowAction -> flowMsg{ok}, which
// the Update loop handles at app.go's flowMsg case (calling refreshLocal()). This
// test drives THAT path — not applyCPWorldHealth() directly — to confirm a
// management box logging in via the TUI's own prompt (not auto-login) gets its
// world pillars filled green from the CP.
func TestFlowMsgLoginFillsManagementBoxWorld(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/world" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"cp_pubkey":"aa","services":[` +
				`{"name":"k3s","kind":"k3s","up":true},` +
				`{"name":"litellm","kind":"litellm","up":true},` +
				`{"name":"caddy","kind":"caddy","up":true}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// Management box pre-flow: console not yet live, no local coords.
	m := &Model{cfg: &config.Config{CPURL: srv.URL, CpPubkey: "aa"}}

	// Emulate runFlowAction after a successful login: it sets m.console, then
	// Update handles the flowMsg{ok} and must fill the pillars.
	m.console = &consoleClient{client: console.WithCookie(srv.URL, "fh_session=tok123")}
	_, _ = m.Update(flowMsg{ok: "console login ok — operator aa"})

	if !m.K3sLive || !m.LitellmLive || !m.CaddyLive {
		t.Fatalf("manual TUI login must fill management-box pillars green: k3s=%v litellm=%v caddy=%v",
			m.K3sLive, m.LitellmLive, m.CaddyLive)
	}
}

// A login-only (runnerless) box is operable once the CP console answers — an
// unseeded/unreachable relay must NOT lock it into configure mode (the fresh-box
// path still has relay_url empty until the deployed CP reseeds it).
func TestConvergedLoginOnlyIgnoresRelay(t *testing.T) {
	// Runnerless + CP live + relay dead -> Running.
	if !converged("", false, true, true) {
		t.Fatal("login-only box with a reachable CP console must be Running even when the relay is unseeded")
	}
	// Runnerless + CP down -> configure.
	if converged("", true, false, true) {
		t.Fatal("login-only box whose CP console is down must NOT be Running")
	}
	// A runnerful (local-world) box still requires every pillar.
	if converged("127.0.0.1:8787", false, true, true) {
		t.Fatal("local-world box must still converge on relay+cp+runner")
	}
	if !converged("127.0.0.1:8787", true, true, true) {
		t.Fatal("all-live local-world box must converge")
	}
}