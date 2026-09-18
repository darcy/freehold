package console

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"freehold/contract/client"
	"freehold/contract/relay"
	"freehold/contract/state"
	"freehold/contract/wire"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/api/cpbuild"
	"freehold/control-plane/secret-management"
)

// Server is the Go console: the loopback admin/ops web surface (web.rs port).
// It serves the same /api/* routes with the same security guards. The Rust
// console is replaced by this at parity.
type Server struct {
	// Store is the CP state store (state.json).
	Store *state.StateStore
	// ConsoleSecret is the console's own Nostr secret (channel-owner + probe
	// signing identity), loaded from its state dir.
	ConsoleSecret []byte
	// ConsolePubkey is the console's Nostr pubkey.
	ConsolePubkey string
	// Auth is the NIP-98 operator auth; nil = loopback-only posture.
	Auth *Auth
	// PublicOrigin is the console's fronted public origin (DNS-rebinding guard).
	PublicOrigin *string
	// RelayHost is the relay community host (kind-9 reads send it explicitly).
	RelayHost string
	// StateDir is the console's own durable state dir (state.json), the same one
	// freehold-agent-tools fronts. It feeds the shared world-status inventory.
	StateDir string
	// AgentToolsDir is the freehold-agent-tools durable state dir — the home of
	// the authoritative agent registry (registry.json) + world facts (facts.json)
	// that /api/world serves publicly so every box sees the CP's status.
	AgentToolsDir string
	// Builder is the CP-owned world bring-up engine (cpbuild.Spec): the console
	// becomes the CP build executor — the operator-scoped /api/world-build route
	// drives it through the co-located runner, so a thin login box triggers the
	// CP to bring up the world WITHOUT depending on the relay roster (which
	// agent-tools needs) or on box-one hosting it. nil = world_build unsupported.
	Builder *cpbuild.Spec
}

// ServeHTTP routes /api/* (the Rust axum router equivalent).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	// Root: the single self-contained admin page.
	if path == "/" && method == http.MethodGet {
		s.index(w)
		return
	}
	if path == "/healthz" && method == http.MethodGet {
		w.Write([]byte("ok"))
		return
	}

	switch {
	case path == "/api/auth/challenge" && method == http.MethodGet:
		s.challenge(w, r)
	case path == "/api/auth/login" && method == http.MethodPost:
		s.login(w, r)
	case path == "/api/auth/portal" && method == http.MethodPost:
		s.portalToken(w, r)
	case strings.HasPrefix(path, "/api/auth/portal/") && method == http.MethodGet:
		s.portalLand(w, r, strings.TrimPrefix(path, "/api/auth/portal/"))
	case path == "/api/overview" && method == http.MethodGet:
		s.overview(w, r)
	case path == "/api/world" && method == http.MethodGet:
		s.world(w, r)
	case path == "/api/world-build" && method == http.MethodPost:
		s.worldBuild(w, r)
	case path == "/api/world-teardown" && method == http.MethodPost:
		s.worldTeardown(w, r)
	case path == "/api/teardown" && method == http.MethodPost:
		s.teardown(w, r)
	case path == "/api/provision" && method == http.MethodPost:
		s.provision(w, r)
	case path == "/api/rotate" && method == http.MethodPost:
		s.rotate(w, r)
	case path == "/api/revoke" && method == http.MethodPost:
		s.revoke(w, r)
	case path == "/api/grant" && method == http.MethodPost:
		s.grant(w, r)
	case path == "/api/revoke-grant" && method == http.MethodPost:
		s.revokeGrant(w, r)
	case path == "/api/runner-addr" && method == http.MethodPost:
		s.runnerAddr(w, r)
	case path == "/api/dns" && method == http.MethodGet:
		s.dnsList(w, r)
	case path == "/api/dns" && method == http.MethodPost:
		s.dnsUpsert(w, r)
	case path == "/api/dns" && method == http.MethodDelete:
		s.dnsRemove(w, r)
	case strings.HasPrefix(path, "/api/runner/") && strings.HasSuffix(path, "/channel") && method == http.MethodGet:
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/api/runner/"), "/channel")
		s.runnerChannel(w, r, name)
	case path == "/api/agents" && method == http.MethodGet:
		s.agentsList(w, r)
	case path == "/api/agents" && method == http.MethodPost:
		s.agentsRegister(w, r)
	case strings.HasPrefix(path, "/api/agents/") && method == http.MethodDelete:
		s.agentsRemove(w, r, strings.TrimPrefix(path, "/api/agents/"))
	case path == "/api/secrets" && method == http.MethodGet:
		s.secretsList(w, r)
	case path == "/api/secrets" && method == http.MethodPost:
		s.secretsWrite(w, r)
	default:
		writeErr(w, http.StatusNotFound, "no such route: "+method+" "+path)
	}
}

// ---- auth routes ----

func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeErr(w, http.StatusNotFound, "console auth is not configured")
		return
	}
	nonce, err := s.Auth.IssueChallenge()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"nonce": nonce, "ts": nowSecs()})
}

type loginRequest struct {
	Nonce     string     `json:"nonce"`
	Pubkey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Tags      [][]string `json:"tags"`
	Sig       string     `json:"sig"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeErr(w, http.StatusNotFound, "console auth is not configured")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad login body")
		return
	}
	if !s.Auth.ConsumeChallenge(req.Nonce) {
		writeErr(w, http.StatusUnauthorized, "unknown, expired, or reused challenge")
		return
	}
	delta := nowSecs() - req.CreatedAt
	if delta < 0 {
		delta = -delta
	}
	if delta > authFreshnessSec {
		writeErr(w, http.StatusUnauthorized, "stale signature timestamp")
		return
	}
	if err := s.verifyNIP98(req.Pubkey, req.CreatedAt, req.Tags, req.Nonce, req.Sig); err != nil {
		writeErr(w, http.StatusUnauthorized, "signature verification failed")
		return
	}
	if !s.Auth.isAdmin(req.Pubkey) {
		writeErr(w, http.StatusForbidden, "not an operator (admin whitelist)")
		return
	}
	token, err := s.Auth.IssueSession(req.Pubkey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "pubkey": req.Pubkey})
}

func (s *Server) portalToken(w http.ResponseWriter, r *http.Request) {
	pk, err := s.requireSessionPubkey(r)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	token, err := s.Auth.IssuePortal(pk)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"token": token})
}

func (s *Server) portalLand(w http.ResponseWriter, r *http.Request, token string) {
	if s.Auth == nil {
		writeErr(w, http.StatusNotFound, "console auth is not configured")
		return
	}
	pk, ok := s.Auth.ConsumePortal(token)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown, expired, or already-used portal token")
		return
	}
	session, err := s.Auth.IssueSession(pk)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	setSessionCookie(w, session)
	w.Header().Set("Location", "/")
	w.WriteHeader(http.StatusFound)
}

// ---- world / overview ----

func (s *Server) world(w http.ResponseWriter, r *http.Request) {
	operator, err := s.requireSessionPubkey(r)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	// Re-read state.json so the separate `dns`/`services` process writes the
	// build landed are reflected (the in-memory snapshot is stale otherwise).
	_ = s.Store.Reload()
	snap := s.Store.Snapshot()
	// Serve the relay's PUBLIC edge on /api/world, derived from the recorded
	// relay_host, so a login box reaches the relay through Caddy (https://
	// <domain>) instead of adopting the internal LAN dial the console uses for
	// its own roster/event reads. relay_ws_url mirrors it (wss://<domain>).
	var relayURL, relayWS *string
	if snap.RelayHost != nil && *snap.RelayHost != "" && !strings.Contains(*snap.RelayHost, "://") {
		public := "https://" + *snap.RelayHost
		ws := "wss://" + *snap.RelayHost
		relayURL = &public
		relayWS = &ws
	} else if snap.RelayURL != nil {
		relayURL = snap.RelayURL
		w := wsOf(*snap.RelayURL)
		relayWS = &w
	}
	var relayHost string
	if snap.RelayHost != nil {
		relayHost = *snap.RelayHost
	}
	// Serve the agent-tools MCP surface publicly too: a thin box drives the
	// world (build/exec/migrate/door) through the CP over the public edge
	// (https://<cp>/mcp), not the LAN dial the console uses internally.
	agentToolsURL := snap.AgentToolsURL
	if s.PublicOrigin != nil && *s.PublicOrigin != "" {
		public := strings.TrimSuffix(*s.PublicOrigin, "/") + "/mcp"
		agentToolsURL = &public
	}
	// The world's services report includes the relay + control plane pillars
	// so a logged-in box renders the WHOLE world from /api/world — relay/cp
	// health included — never from local config. The box is CP-driven; local
	// config is only the offline fallback (the initial-bootstrap exception).
	services := []WorldServiceJSON{}
	if relayURL != nil {
		up, detail := false, "co-located relay"
		// Probe the relay through the PUBLIC edge (the proxy/Caddy IP is static),
		// NOT the recorded LAN dial: the relay LXC is DHCP, so its recorded LAN IP
		// goes stale every time the lease re-issues after a rebuild, and a probe
		// against a dead IP reports the relay down even though the relay + edge
		// are serving (the box reaches https://<relayHost> => 200). The proxy IP is
		// the stable host every box resolves the public relay to.
		up, detail = answered(*relayURL, false)
		services = append(services, WorldServiceJSON{Name: "relay", Kind: "relay", URL: *relayURL, Up: up, Detail: detail})
	}
	if s.PublicOrigin != nil && *s.PublicOrigin != "" {
		services = append(services, WorldServiceJSON{Name: "control plane", Kind: "cp", URL: *s.PublicOrigin, Up: true, Detail: "console serving"})
	}
	services = append(services, worldServiceRows(snap)...)
	payload := map[string]interface{}{
		"relay_url":          relayURL,
		"relay_ws_url":       relayWS,
		"relay_pubkey":       snap.RelayPubkey,
		"relay_host":         relayHost,
		"cp_url":             s.PublicOrigin,
		"cp_pubkey":          s.ConsolePubkey,
		"agent_tools_url":    agentToolsURL,
		"agent_tools_pubkey": snap.AgentToolsPubkey,
		"operator_pubkey":    operator,
		"services":           services,
	}
	// Fold the single-inventory status (agents + runners + dns + facts) served
	// on the same route the /mcp world_status tool shares — the authoritative
	// registry/facts, read live from the toolset's durable state. A /api/world
	// call now returns everything a box renders, publicly. A broken inventory
	// (malformed registry/facts) is surfaced in the log rather than silently
	// read as "world down" — the coords/services still serve normally.
	if inv, err := s.worldInventory(); err != nil {
		log.Printf("world inventory unavailable on /api/world: %v", err)
	} else {
		for k, v := range inv {
			payload[k] = v
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// stateDir returns the console's durable state dir — the explicit StateDir
// (production) or the Store's own dir (unit tests that build a Store).
func (s *Server) stateDir() string {
	if s.StateDir != "" {
		return s.StateDir
	}
	if s.Store != nil {
		return s.Store.Dir()
	}
	return ""
}

// worldBuild runs the CP-owned world bring-up through the co-located runner
// (the shared cpbuild engine) — the console is the CP build executor a thin
// login box triggers. Operator-scoped (a session-holder for /api/*), and the
// engine is bound only when the console was deployed with the world coords +
// its runner credential (Builder set). No relay dependency: unlike
// agent-tools, this does NOT read the relay roster to authorize.
func (s *Server) worldBuild(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.Builder == nil {
		writeErr(w, http.StatusServiceUnavailable, "world-build: the console has no build engine bound (deploy it with the world coords + runner credential, or run `freehold bootstrap` first)")
		return
	}
	applier := cpbuild.BuildWorldApply(s.Builder)
	report, err := applier()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "world-build: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "report": report})
}

// worldTeardown runs the CP-owned world teardown through the co-located runner
// (cpbuild) — the mirror of worldBuild for a thin login box. The CP LXC is
// destroyed last (detached), so this response lands before the console's own
// container goes. The CP's managed state (runners + secrets, agent registry,
// DNS store) is cleared AFTER the runner-driven teardown — clearing it first
// would remove the very co-located runner this runs through. Operator-scoped:
// requireSession only admits a pubkey from the console's admin whitelist
// (server.go login's isAdmin gate), i.e. an operator. Compute-only.
func (s *Server) worldTeardown(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.Builder == nil {
		writeErr(w, http.StatusServiceUnavailable, "world-teardown: the console has no build engine bound (deploy it with the world coords + runner credential, or run `freehold bootstrap` first)")
		return
	}
	applier := cpbuild.BuildWorldTeardownApply(s.Builder)
	report, err := applier()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "world-teardown: "+err.Error())
		return
	}
	// All runner-driven work is done. The CP + its co-located runner SURVIVE
	// (teardown is the inverse of build, not of uninstall), so only the world's
	// internal DNS records are cleared — runners/secrets and the agent registry
	// are durable CP state that the next build re-uses (identity stability).
	res, err := s.clearWorldDNS()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "world-teardown: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "report": report,
		"runners_removed": res.Runners, "agents_removed": res.Agents, "dns_removed": res.DNS,
	})
}

// worldInventory opens the authoritative agent registry + world facts (the
// toolset's durable state, readable from the co-located content plane) and
// returns the single-inventory status the console serves on /api/world and the
// /mcp world_status tool both resolve. Absent coords => empty inventory.
func (s *Server) worldInventory() (map[string]interface{}, error) {
	if s.AgentToolsDir == "" || s.StateDir == "" {
		return map[string]interface{}{}, nil
	}
	reg, err := agenttools.OpenRegistry(filepath.Join(s.AgentToolsDir, "registry.json"))
	if err != nil {
		return nil, err
	}
	facts, err := agenttools.OpenFacts(filepath.Join(s.AgentToolsDir, "facts.json"))
	if err != nil {
		return nil, err
	}
	return agenttools.WorldStatus(reg, facts, s.StateDir)
}

// stdSecrets are the CP-owned secret names the build ensures idempotently.
var stdSecrets = []string{"dns-relay", "dns-cp", "litellm"}

// secretsList reports which CP-owned secrets are present on disk (the idempotent
// inventory `build` uses to ask the operator only for what's missing).
func (s *Server) secretsList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSessionPubkey(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	dir := filepath.Join(s.stateDir(), "world-secrets")
	var present []string
	for _, name := range stdSecrets {
		if _, err := os.Stat(filepath.Join(dir, name+".json")); err == nil {
			present = append(present, name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"secrets": present})
}

// secretsWrite stores a sealed secret record (sealed to the console identity by
// the operator box) so the CP is its durable owner. Only allowlisted names are
// writable; the blob is written verbatim (never opened here — the cert /
// litellm steps open it in memory at build).
func (s *Server) secretsWrite(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSessionPubkey(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req struct {
		Name string          `json:"name"`
		File json.RawMessage `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad secret body: "+err.Error())
		return
	}
	allowed := false
	for _, n := range stdSecrets {
		if req.Name == n {
			allowed = true
			break
		}
	}
	if !allowed || req.Name == "" || len(req.File) == 0 {
		writeErr(w, http.StatusBadRequest, "refusing secret name "+req.Name)
		return
	}
	dir := filepath.Join(s.stateDir(), "world-secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "secret dir: "+err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, req.Name+".json"), req.File, 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, "write secret: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name})
}

func wsOf(url string) string {
	if rest, ok := strings.CutPrefix(url, "https://"); ok {
		return "wss://" + rest
	}
	if rest, ok := strings.CutPrefix(url, "http://"); ok {
		return "ws://" + rest
	}
	return url
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	// Read fresh from disk (state.Open) so out-of-band deploy writes to
	// state.json are visible to a login-only box (the running Store is a
	// startup memory snapshot and would show a stale/empty runner list).
	fresh, err := state.Open(s.stateDir())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read CP state: "+err.Error())
		return
	}
	snap := fresh.Snapshot()

	type runnerOut struct {
		Name      string      `json:"name"`
		Status    string      `json:"status"`
		NostrPub  string      `json:"nostr_pubkey"`
		EncPub    string      `json:"enc_pubkey"`
		McpAddr   *string     `json:"mcp_addr"`
		Risk      *string     `json:"risk"`
		Secret    interface{} `json:"secret"`
		Grants    interface{} `json:"grants"`
		Readiness interface{} `json:"readiness,omitempty"`
	}
	runners := make([]runnerOut, 0, len(snap.Runners))
	type probe struct {
		name  string
		value interface{}
	}
	probes := make(chan probe, len(snap.Runners))
	probeCount := 0
	for name, rec := range snap.Runners {
		var secret interface{}
		if sc, ok := snap.Secrets[name]; ok {
			secret = map[string]interface{}{
				"name": sc.Runner, "kind": sc.Kind, "address": sc.Address,
				"rotated_at": sc.RotatedAt, "created_at": sc.CreatedAt,
			}
		}
		var grants interface{}
		if pkg, err := wire.Load(rec.PackageDir); err == nil {
			grants = pkg.Grants
		}
		out := runnerOut{
			Name: name, Status: string(rec.Status), NostrPub: rec.NostrPubkey,
			EncPub: rec.EncPubkey, McpAddr: rec.McpAddr, Risk: rec.RiskLevel,
			Secret: secret, Grants: grants,
		}
		// Live readiness probe (the console signs a status call as its own
		// identity — the runner still fails closed).
		if rec.Status == state.RunnerActive && rec.McpAddr != nil && len(s.ConsoleSecret) == 32 {
			probeCount++
			go func(name string, addr, rpk string) {
				probes <- probe{name, s.probeReadiness(addr, rpk)}
			}(name, *rec.McpAddr, rec.NostrPubkey)
		}
		runners = append(runners, out)
	}
	for i := 0; i < probeCount; i++ {
		p := <-probes
		for j := range runners {
			if runners[j].Name == p.name {
				runners[j].Readiness = p.value
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"console_pubkey": s.ConsolePubkey,
		"runners":        runners,
	})
}

// probeReadiness signs a status call as the console and returns the runner's
// own self-check (a -32001 denial reads as "console not granted", never a
// side door).
func (s *Server) probeReadiness(addr, runnerPubkey string) interface{} {
	url := addr
	if !strings.Contains(addr, "://") {
		url = "http://" + addr + "/mcp"
	}
	auth := &client.AgentAuth{}
	copy(auth.Secret[:], s.ConsoleSecret)
	auth.Pubkey = s.ConsolePubkey
	mc, err := client.New(url, auth, runnerPubkey)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	mc.SetHTTPClient(&http.Client{Timeout: 4 * time.Second})
	m, err := mc.Readiness()
	if err != nil {
		if strings.Contains(err.Error(), "-32001") {
			return map[string]interface{}{"note": "console not granted — grant the console pubkey in the UI"}
		}
		return map[string]interface{}{"error": err.Error()}
	}
	return m
}

// ---- teardown ----

func (s *Server) teardown(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	res, err := s.clearManagedState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"runners_removed": res.Runners, "agents_removed": res.Agents, "dns_removed": res.DNS,
	})
}

// managedStateRemoved is what clearManagedState took out of the CP's store.
type managedStateRemoved struct{ Runners, Agents, DNS int }

// clearWorldDNS removes the CP's internal resolver records (the world's DNS
// mirror) while KEEPING runners/secrets and the agent registry — the
// CP-preserving teardown: the durable identities survive so the next build
// re-uses them.
func (s *Server) clearWorldDNS() (managedStateRemoved, error) {
	var res managedStateRemoved
	_ = s.Store.Reload()
	snap := s.Store.Snapshot()
	for name := range snap.DNS {
		res.DNS++
		s.Store.RemoveDNS(name)
	}
	if err := s.Store.Save(); err != nil {
		return res, fmt.Errorf("world-teardown failed to persist CP state")
	}
	return res, nil
}

// clearManagedState removes what the CP manages — runners + their secrets, the
// agent registry, DNS records — from its durable store. Shared by /api/teardown
// (the CP-first hand-off a box drives before destroying the CP) and the
// CP-owned world-teardown (which clears it AFTER its runner-driven teardown, so
// the co-located runner it drives is never removed out from under it).
func (s *Server) clearManagedState() (managedStateRemoved, error) {
	var res managedStateRemoved
	_ = s.Store.Reload()
	snap := s.Store.Snapshot()
	for name := range snap.Runners {
		res.Runners++
		s.Store.RemoveRunner(name)
		s.Store.RemoveSecret(name)
	}
	for name := range snap.Agents {
		res.Agents++
		s.Store.RemoveAgent(name)
	}
	for name := range snap.DNS {
		res.DNS++
		s.Store.RemoveDNS(name)
	}
	if err := s.Store.Save(); err != nil {
		return res, fmt.Errorf("teardown failed to persist CP state")
	}
	return res, nil
}

// ---- provision / rotate / revoke / grant / revoke-grant / runner-addr ----

type nameReq struct {
	Name string `json:"name"`
}
type secretReq struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
}
type grantReq struct {
	Name   string `json:"name"`
	Pubkey string `json:"pubkey"`
}
type addrReq struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}
type provisionReq struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Address   string  `json:"address"`
	Secret    string  `json:"secret"`
	RunnerDir *string `json:"runner_dir"`
	Risk      *string `json:"risk"`
}

func (s *Server) provision(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req provisionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad provision body")
		return
	}
	runnerDir := "./.freehold/runner/" + req.Name
	if req.RunnerDir != nil {
		runnerDir = *req.RunnerDir
	}
	res, err := provisioner.ProvisionRunner(s.Store, &provisioner.ProvisionRequest{
		Name: req.Name, Kind: req.Kind, Address: req.Address,
		Secret: []byte(req.Secret), RunnerDir: runnerDir,
		Grants: []string{s.ConsolePubkey}, RiskLevel: req.Risk,
	})
	if err != nil {
		writeErr(w, statusForAction(err), err.Error())
		return
	}
	var relayURL *string
	snap := s.Store.Snapshot()
	relayURL = snap.RelayURL
	if relayURL != nil {
		if err := provisioner.SyncRunnerChannel(s.Store, *relayURL, req.Name, s.Store.Dir()); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "name": res.Name, "nostr_pubkey": res.NostrPubkey,
		"enc_pubkey": res.EncPubkey, "package_dir": res.PackageDir,
		"granted": []string{s.ConsolePubkey}, "relay": relayURL,
	})
}

func (s *Server) rotate(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req secretReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad rotate body")
		return
	}
	if _, err := provisioner.RotateSecret(s.Store, req.Name, []byte(req.Secret)); err != nil {
		writeErr(w, statusForAction(err), err.Error())
		return
	}
	var relayURL *string
	snap := s.Store.Snapshot()
	relayURL = snap.RelayURL
	if relayURL != nil {
		if err := provisioner.SyncRunnerChannel(s.Store, *relayURL, req.Name, s.Store.Dir()); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name, "relay": relayURL})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req nameReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad revoke body")
		return
	}
	if _, err := provisioner.RevokeRunner(s.Store, req.Name); err != nil {
		writeErr(w, statusForAction(err), err.Error())
		return
	}
	var relayURL *string
	snap := s.Store.Snapshot()
	relayURL = snap.RelayURL
	if relayURL != nil {
		if err := provisioner.RevokeRunnerChannel(s.Store, *relayURL, req.Name, s.Store.Dir()); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name, "relay": relayURL})
}

func (s *Server) grant(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req grantReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad grant body")
		return
	}
	grants, err := provisioner.GrantAgent(s.Store, req.Name, req.Pubkey)
	if err != nil {
		writeErr(w, statusForAction(err), err.Error())
		return
	}
	var relayURL *string
	snap := s.Store.Snapshot()
	relayURL = snap.RelayURL
	if relayURL != nil {
		if err := provisioner.PutUserMembership(s.Store, *relayURL, req.Name, req.Pubkey, s.Store.Dir()); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name, "granted": grants, "relay": relayURL})
}

func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req grantReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad revoke-grant body")
		return
	}
	grants, err := provisioner.RevokeGrant(s.Store, req.Name, req.Pubkey)
	if err != nil {
		writeErr(w, statusForAction(err), err.Error())
		return
	}
	var relayURL *string
	snap := s.Store.Snapshot()
	relayURL = snap.RelayURL
	if relayURL != nil {
		if err := provisioner.RemoveUserMembership(s.Store, *relayURL, req.Name, req.Pubkey, s.Store.Dir()); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name, "granted": grants, "relay": relayURL})
}

func (s *Server) runnerAddr(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req addrReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad runner-addr body")
		return
	}
	if err := s.Store.SetRunnerMcpAddr(req.Name, &req.Addr); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.Store.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name})
}

// ---- DNS ----

func (s *Server) dnsList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	// Read the authoritative state fresh from disk (state.Open, not the running
	// s.Store's once-loaded memory snapshot): the world-build stages write
	// state.json out-of-band (co-located runner), so the Store would serve
	// stale/empty DNS to a login-only box. Re-reading the file is always as
	// fresh or fresher (writes Save() through the same file).
	fresh, err := state.Open(s.stateDir())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read CP state: "+err.Error())
		return
	}
	snap := fresh.Snapshot()
	records := make([]map[string]interface{}, 0, len(snap.DNS))
	names := make([]string, 0, len(snap.DNS))
	for name := range snap.DNS {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rec := snap.DNS[name]
		records = append(records, map[string]interface{}{
			"name": name, "ip": rec.IP, "source": rec.Source, "created_at": rec.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"dns":               records,
		"resolver_wildcard": snap.ResolverWildcard,
		"addn_hosts":        RenderAddnHosts(snap.DNS, snap.ResolverDomain),
	})
}

type dnsReq struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	Source string `json:"source"`
}

func (s *Server) dnsUpsert(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req dnsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad dns body")
		return
	}
	source := req.Source
	if source == "" {
		source = "api"
	}
	rec, err := Upsert(s.Store, req.Name, req.IP, source)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.syncResolver(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name, "ip": rec.IP})
}

func (s *Server) dnsRemove(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req dnsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad dns body")
		return
	}
	if err := RemoveDNSRecord(s.Store, req.Name); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.syncResolver(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name})
}

// syncResolver writes the dnsmasq files under the CP state dir and reloads.
func (s *Server) syncResolver() error {
	snap := s.Store.Snapshot()
	var apex, ip *string
	if snap.ResolverWildcard != nil {
		apex = &snap.ResolverWildcard.Apex
		ip = &snap.ResolverWildcard.IP
	}
	write := func(path, body string) error { return writeFile(path, body) }
	reload := func() error { return reloadDnsmasq(s.Store.Dir()) }
	return SyncResolver(s.Store.Dir(), snap.DNS, snap.ResolverDomain, apex, ip, write, reload)
}

// ---- runner channel view ----

func (s *Server) runnerChannel(w http.ResponseWriter, r *http.Request, name string) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	snap := s.Store.Snapshot()
	if snap.RelayURL == nil {
		writeErr(w, http.StatusBadRequest, "relay not configured — run `serve --relay-url`")
		return
	}
	if snap.RelayPubkey == nil {
		writeErr(w, http.StatusBadRequest, "relay pubkey not configured — run `serve --relay-pubkey`")
		return
	}
	rec, ok := snap.Runners[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "runner "+name+" not found")
		return
	}
	channelID := relay.RunnerChannelID(rec.NostrPubkey)
	members, err := relay.QueryChannelRoster(*snap.RelayURL, *snap.RelayPubkey, rec.NostrPubkey, s.ConsoleSecret)
	var membersJSON interface{}
	if err != nil {
		membersJSON = map[string]interface{}{"error": err.Error()}
	} else {
		membersJSON = members
	}
	metas, err := relay.QueryRunnerMetas(*snap.RelayURL, s.ConsolePubkey, s.ConsoleSecret)
	var profile interface{}
	if err == nil {
		for _, m := range metas {
			if m.NostrPubkey == rec.NostrPubkey {
				profile = m
				break
			}
		}
	}
	msgs, err := relay.QueryEvents(*snap.RelayURL, s.ConsoleSecret, []interface{}{map[string]interface{}{
		"kinds": []interface{}{wire.ChannelMessage}, "#h": []interface{}{channelID}, "limit": 25,
	}})
	var messages interface{}
	if err != nil {
		messages = map[string]interface{}{"error": err.Error()}
	} else {
		out := []map[string]interface{}{}
		for _, e := range msgs {
			if isProfileMessage(e) {
				continue
			}
			out = append(out, map[string]interface{}{
				"pubkey": e["pubkey"], "created_at": e["created_at"], "content": e["content"],
			})
		}
		messages = out
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name": name, "channel": channelID, "members": membersJSON,
		"profile": profile, "messages": messages,
	})
}

func isProfileMessage(e map[string]interface{}) bool {
	tags, _ := e["tags"].([]interface{})
	for _, t := range tags {
		tt, ok := t.([]interface{})
		if !ok || len(tt) < 2 {
			continue
		}
		k, _ := tt[0].(string)
		v, _ := tt[1].(string)
		if k == "t" && v == relay.ProfileMessageTag() {
			return true
		}
	}
	return false
}

// ---- agents ----

func (s *Server) agentsList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	snap := s.Store.Snapshot()
	names := make([]string, 0, len(snap.Agents))
	for name := range snap.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	agents := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		rec := snap.Agents[name]
		agents = append(agents, map[string]interface{}{
			"name": name, "pubkey": rec.Pubkey, "created_at": rec.CreatedAt,
			"channel": rec.Channel, "available": nil, "note": nil,
		})
	}
	// Live presence probes (kind-9 within the window) when a relay is set.
	if snap.RelayURL != nil && len(s.ConsoleSecret) == 32 {
		for i := range agents {
			ch, _ := agents[i]["channel"].(*string)
			pk, _ := agents[i]["pubkey"].(string)
			name, _ := agents[i]["name"].(string)
			ok, note := s.probeAgentPresence(*snap.RelayURL, ch, pk)
			agents[i]["available"] = nil
			if note != "" {
				agents[i]["note"] = note
			} else {
				agents[i]["available"] = ok
			}
			_ = name
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"agents": agents})
}

// probeAgentPresence queries kind-9 from the agent's pubkey on its channel
// within the last 150s window.
func (s *Server) probeAgentPresence(relayURL string, channel *string, agentPubkey string) (bool, string) {
	if channel == nil || *channel == "" {
		return false, "no channel registered for this agent"
	}
	since := nowSecs() - 150
	filter := []interface{}{map[string]interface{}{
		"kinds": []interface{}{wire.ChannelMessage}, "authors": []interface{}{agentPubkey},
		"#h": []interface{}{*channel}, "since": since,
	}}
	events, err := relay.QueryEvents(relayURL, s.ConsoleSecret, filter)
	if err != nil {
		return false, err.Error()
	}
	return len(events) > 0, ""
}

type agentReq struct {
	Name    string  `json:"name"`
	Pubkey  string  `json:"pubkey"`
	Channel *string `json:"channel"`
}

func (s *Server) agentsRegister(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req agentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad agents body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if !isHex64(req.Pubkey) {
		writeErr(w, http.StatusBadRequest, "pubkey must be 64-hex")
		return
	}
	snap := s.Store.Snapshot()
	createdAt := uint64(time.Now().Unix())
	if existing, ok := snap.Agents[req.Name]; ok {
		createdAt = existing.CreatedAt
	}
	s.Store.InsertAgent(req.Name, state.AgentRecord{Pubkey: req.Pubkey, CreatedAt: createdAt, Channel: req.Channel})
	if err := s.Store.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name, "pubkey": req.Pubkey})
}

func (s *Server) agentsRemove(w http.ResponseWriter, r *http.Request, name string) {
	if _, err := s.requireSession(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if _, ok := s.Store.Snapshot().Agents[name]; !ok {
		writeErr(w, http.StatusNotFound, "no such agent: "+name)
		return
	}
	s.Store.RemoveAgent(name)
	if err := s.Store.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": name})
}

func (s *Server) index(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]interface{}{"error": msg})
}

func statusFor(err error) int {
	switch err {
	case errUnauthorized:
		return http.StatusUnauthorized
	case errAuthNotConfigured:
		return http.StatusNotFound
	case errForbidden:
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func statusForAction(err error) int {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "already exists"), strings.Contains(msg, "is revoked"):
		return http.StatusConflict
	case strings.Contains(msg, "not found"):
		return http.StatusNotFound
	case strings.Contains(msg, "invalid"):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func writeFile(path, body string) error {
	return osWriteFile(path, []byte(body))
}
