package console

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/relay"
	"freehold/contract/version"
	"freehold/contract/wire"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/api/cpbuild"
	"freehold/control-plane/secret-management"
	"freehold/control-plane/state"
	"freehold/platform/migrations"
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
	// Members is the member identity tier — the appliance's users (family,
	// team): NIP-07 relay-membership login + device-link invites, gated apps
	// verify through /auth/verify. Constructed beside Auth; nil = disabled.
	Members *Members
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
	// Version is the world's stamped version identity, read from
	// <StateDir>/version.json at startup. Zero when unstamped.
	Version version.Pin
	// Builder is the CP-owned world bring-up engine (cpbuild.Spec): the console
	// becomes the CP build executor — the operator-scoped /api/world-build route
	// drives it through the co-located runner, so a thin login box triggers the
	// CP to bring up the world WITHOUT depending on the relay roster (which
	// agent-tools needs) or on box-one hosting it. nil = world_build unsupported.
	Builder *cpbuild.Spec

	// RestartDoor restarts a capability door's runner unit (nil = the real
	// systemd restart on this guest). Injectable for tests.
	RestartDoor func(name string, port int) error

	// memberMu/memberCache cache the relay's membership answers behind
	// memberRelayAllowed (lazy — tests build Server literals without them).
	memberMu    sync.Mutex
	memberCache map[string]memberCheck
}

// versionPin re-reads <StateDir>/version.json per call so an update's repin is
// reflected without a serve restart; falls back to the startup snapshot.
func (s *Server) versionPin() version.Pin {
	if p, err := version.Read(filepath.Join(s.StateDir, version.FileName)); err == nil && p.Version != "" {
		return p
	}
	return s.Version
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
	// Per-door page: the same SPA, which reads the path and opens that door's
	// fill form (a deep link the agents hand the operator — login preserves
	// the path, so the form re-opens after the session lands).
	if strings.HasPrefix(path, "/runner/") && method == http.MethodGet {
		s.index(w)
		return
	}
	if path == "/healthz" && method == http.MethodGet {
		pin := s.versionPin()
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]interface{}{
			"status":  "ok",
			"version": pin.Version,
			"channel": pin.Channel,
			"commit":  pin.Commit,
		})
		w.Write(body)
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
	// The member identity tier: the login page + gate endpoint the edge's
	// forward_auth calls, plus the operator's invite surface. /auth/* are
	// page routes (no /api prefix) so they ride the same CP vhost.
	case path == "/auth" && method == http.MethodGet:
		s.memberLoginPage(w, r)
	case path == "/auth/verify" && method == http.MethodGet:
		s.forwardAuthVerify(w, r)
	case strings.HasPrefix(path, "/auth/link/") && method == http.MethodGet:
		s.memberLinkLand(w, r, strings.TrimPrefix(path, "/auth/link/"))
	case path == "/api/auth/member/challenge" && method == http.MethodGet:
		s.memberChallenge(w, r)
	case path == "/api/auth/member/login" && method == http.MethodPost:
		s.memberLogin(w, r)
	case path == "/api/auth/member/logout" && method == http.MethodPost:
		s.memberLogout(w, r)
	case path == "/api/members" && method == http.MethodGet:
		s.membersList(w, r)
	case path == "/api/members/invites" && method == http.MethodPost:
		s.memberInviteMint(w, r)
	case strings.HasPrefix(path, "/api/members/invites/") && method == http.MethodDelete:
		s.memberInviteRevoke(w, r, strings.TrimPrefix(path, "/api/members/invites/"))
	case strings.HasPrefix(path, "/api/members/sessions/") && method == http.MethodDelete:
		s.memberDrop(w, r, strings.TrimPrefix(path, "/api/members/sessions/"))
	case path == "/api/overview" && method == http.MethodGet:
		s.overview(w, r)
	case path == "/api/world" && method == http.MethodGet:
		s.world(w, r)
	case path == "/api/world-build" && method == http.MethodPost:
		s.worldBuild(w, r)
	case path == "/api/world-teardown" && method == http.MethodPost:
		s.worldTeardown(w, r)
	case path == "/api/world-exec" && method == http.MethodPost:
		s.worldExec(w, r)
	case path == "/api/world-migrate" && method == http.MethodPost:
		s.worldMigrate(w, r)
	case path == "/api/world-door" && method == http.MethodPost:
		s.worldDoor(w, r)
	case path == "/api/teardown" && method == http.MethodPost:
		s.teardown(w, r)
	case path == "/api/provision" && method == http.MethodPost:
		s.provision(w, r)
	case path == "/api/rotate" && method == http.MethodPost:
		s.rotate(w, r)
	case path == "/api/enroll-confirm" && method == http.MethodPost:
		s.enrollConfirm(w, r)
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
	case path == "/api/jobs" && method == http.MethodGet:
		s.jobsList(w, r)
	case path == "/api/agents" && method == http.MethodPost:
		s.agentsRegister(w, r)
	case strings.HasPrefix(path, "/api/agents/") && method == http.MethodDelete:
		s.agentsRemove(w, r, strings.TrimPrefix(path, "/api/agents/"))
	case path == "/api/secrets" && method == http.MethodGet:
		s.secretsList(w, r)
	case path == "/api/secrets" && method == http.MethodPost:
		s.secretsWrite(w, r)
	case path == "/api/settings" && method == http.MethodGet:
		s.settingsGet(w, r)
	case path == "/api/settings" && method == http.MethodPost:
		s.settingsSet(w, r)
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
	// Two roles log in: an operator (the admin whitelist — the full admin/ops
	// surface) and a member (any relay community member — the scheduled-jobs
	// read of their own rows). Everyone else is refused.
	role := RoleOperator
	if !s.Auth.isAdmin(req.Pubkey) {
		snap := s.Store.Snapshot()
		member, merr := s.isRelayMember(snap, req.Pubkey)
		if merr != nil {
			writeErr(w, http.StatusInternalServerError, "relay membership check failed: "+merr.Error())
			return
		}
		if !member {
			writeErr(w, http.StatusForbidden, "not an operator (admin whitelist) and not a relay member")
			return
		}
		role = RoleMember
	}
	token, err := s.Auth.IssueSessionRole(req.Pubkey, role)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "pubkey": req.Pubkey, "role": role})
}

// isRelayMember checks the relay's community membership list (kind 13534,
// relay-signed) for the pubkey — the member-role gate for console login.
// Dialed + signed over the console's own dial/auth URL pair (an auth whose
// `u` is the LAN origin is refused by the relay).
func (s *Server) isRelayMember(snap state.ControlPlaneState, pubkey string) (bool, error) {
	if snap.RelayURL == nil || snap.RelayPubkey == nil || len(s.ConsoleSecret) != 32 {
		return false, fmt.Errorf("relay not configured (need relay_url + relay_pubkey + the console identity)")
	}
	dial, auth := s.relayDialAuth(snap)
	return relay.IsCommunityMemberAuth(dial, auth, s.ConsoleSecret, *snap.RelayPubkey, pubkey)
}

func (s *Server) portalToken(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeErr(w, statusFor(errAuthNotConfigured), errAuthNotConfigured.Error())
		return
	}
	pk, role, err := s.sessionFor(r)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if role == "" {
		role = RoleOperator
	}
	token, err := s.Auth.IssuePortalRole(pk, role)
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
	pk, role, ok := s.Auth.ConsumePortal(token)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown, expired, or already-used portal token")
		return
	}
	session, err := s.Auth.IssueSessionRole(pk, role)
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
	operator, err := s.requireAdmin(r)
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
	//
	// A FRESH world's console is deployed BEFORE the relay boots (install is
	// CP-only), so the state may carry no relay scope while the builder's
	// world-config does. Fall back to it, else /api/world omits the relay
	// service entirely and the box's relay pillar can never turn green.
	builderHost, builderURL := "", ""
	if s.Builder != nil {
		builderHost, builderURL = s.Builder.RelayHost, s.Builder.RelayURL
	}
	var relayURL, relayWS *string
	switch {
	case snap.RelayHost != nil && *snap.RelayHost != "" && !strings.Contains(*snap.RelayHost, "://"):
		public := "https://" + *snap.RelayHost
		ws := "wss://" + *snap.RelayHost
		relayURL = &public
		relayWS = &ws
	case snap.RelayURL != nil:
		relayURL = snap.RelayURL
		w := wsOf(*snap.RelayURL)
		relayWS = &w
	case builderHost != "":
		public := "https://" + builderHost
		ws := "wss://" + builderHost
		relayURL = &public
		relayWS = &ws
	case builderURL != "":
		relayURL = &builderURL
		w := wsOf(builderURL)
		relayWS = &w
	}
	var relayHost string
	if snap.RelayHost != nil {
		relayHost = *snap.RelayHost
	}
	if relayHost == "" {
		relayHost = builderHost
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
		// Probe the relay the way THIS guest dials it: the LAN dial by HOSTNAME
		// (the /etc/hosts pin re-reads the relay's current DHCP lease every
		// converge — the same URL relayDialFor publishes through), never the
		// public https origin. On the CP guest the public name resolves through
		// that same pin to the raw relay LXC, where only buzz :3000 listens —
		// the TLS edge is the proxy, so an https dial here is refused while the
		// relay is actually serving.
		dial := config.RelayLanDial(relayHost)
		if dial == "" {
			dial = *relayURL
		}
		up, detail = answered(dial, false)
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
		"console_enc_pubkey": s.consoleEncPubkey(),
		"agent_tools_url":    agentToolsURL,
		"agent_tools_pubkey": snap.AgentToolsPubkey,
		"operator_pubkey":    operator,
		"services":           services,
		"version":            s.versionPin(),
	}
	// Pending migration count (scripts without a completion marker) so a box's
	// `update --check` / `status` can report it without running anything.
	if n, err := migrations.PendingCount(filepath.Join(s.StateDir, "migrations")); err == nil {
		payload["migrations_pending"] = n
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
	if _, err := s.requireAdmin(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.Builder == nil {
		writeErr(w, http.StatusServiceUnavailable, "world-build: the console has no build engine bound (deploy it with the world coords + runner credential, or run `freehold install` first)")
		return
	}
	applier := cpbuild.BuildWorldApply(s.Builder)
	report, err := applier()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "world-build: "+err.Error())
		return
	}
	// Hand back the world coords the build resolved (relay/cp/k3s vmids + IPs).
	// A teardown clears the operator box's recorded coords, so the build caller
	// must write these back to its profile or the next uninstall cannot find the
	// guests it created. Non-secret.
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "report": report, "coords": s.Builder.Coords()})
}

// worldTeardown runs the CP-owned world teardown through the co-located runner
// (cpbuild) — the mirror of worldBuild for a thin login box. The CP LXC is
// destroyed last (detached), so this response lands before the console's own
// container goes. The CP's managed state (runners + secrets, agent registry,
// DNS store) is cleared AFTER the runner-driven teardown — clearing it first
// would remove the very co-located runner this runs through. Operator-scoped:
// requireAdmin only admits a session stamped with the operator role (the
// login's admin whitelist), i.e. an operator. Compute-only.
func (s *Server) worldTeardown(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.Builder == nil {
		writeErr(w, http.StatusServiceUnavailable, "world-teardown: the console has no build engine bound (deploy it with the world coords + runner credential, or run `freehold install` first)")
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

// worldExec runs one command through the CP's co-located runner — the
// drive-through-CP exec a thin login box uses, now session-authed here instead
// of operator-signed on the agent-tools MCP (no relay roster). Operator-scoped
// like worldBuild: requireAdmin only admits an operator-role session.
func (s *Server) worldExec(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.Builder == nil {
		writeErr(w, http.StatusServiceUnavailable, "world-exec: the console has no build engine bound (deploy it with the world coords + runner credential, or run `freehold install` first)")
		return
	}
	var req struct {
		Target   string   `json:"target"`
		Cmd      string   `json:"cmd"`
		TimeoutS uint64   `json:"timeout_s"`
		Secrets  []string `json:"secrets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cmd == "" {
		writeErr(w, http.StatusBadRequest, "world-exec: cmd required")
		return
	}
	exec := cpbuild.BuildWorldExec(s.Builder)
	out, err := exec(req.Target, req.Cmd, req.TimeoutS, req.Secrets...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "world-exec: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "text": out})
}

// worldMigrate proxies into the agent-tools serve's world_migrate tool: the
// scripts must run in THAT process to take Registry.WithRegistryLocked (the
// registry-write race — see the migration window), so the console cannot
// execute them itself. It signs as the console identity — the serve's local
// admin peer — over the same signed-header scheme, no relay roster.
func (s *Server) worldMigrate(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	snap := s.Store.Snapshot()
	var atURL, atPK string
	if snap.AgentToolsURL != nil {
		atURL = *snap.AgentToolsURL
	}
	if snap.AgentToolsPubkey != nil {
		atPK = *snap.AgentToolsPubkey
	}
	if atURL == "" || len(atPK) != 64 {
		writeErr(w, http.StatusServiceUnavailable, "world-migrate: no agent-tools coords recorded (the CP predates the world toolset)")
		return
	}
	if len(s.ConsoleSecret) != 32 {
		writeErr(w, http.StatusInternalServerError, "world-migrate: console identity unreadable")
		return
	}
	auth := &client.AgentAuth{Pubkey: s.ConsolePubkey}
	copy(auth.Secret[:], s.ConsoleSecret)
	mc, err := client.New(client.ConnectURL(atURL), auth, atPK)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "world-migrate: "+err.Error())
		return
	}
	// Migrations can run for many minutes — the hop that EXECUTES them needs
	// the long deadline, not the client-facing 30s default.
	resp, err := mc.CallLong("world_migrate", map[string]interface{}{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "world-migrate: "+err.Error())
		return
	}
	var envelope struct {
		Result *struct {
			Content []map[string]any `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil || envelope.Result == nil || len(envelope.Result.Content) == 0 {
		writeErr(w, http.StatusInternalServerError, "world-migrate: bad tool envelope")
		return
	}
	text, _ := envelope.Result.Content[0]["text"].(string)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "report": text})
}

// worldDoor authorizes or removes an operator box's public door key on the
// host door (DOOR_SPEC) through the co-located runner — the session-authed
// mirror of the toolset's world_authorize_door/world_revoke_door.
func (s *Server) worldDoor(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.Builder == nil {
		writeErr(w, http.StatusServiceUnavailable, "world-door: the console has no build engine bound (deploy it with the world coords + runner credential, or run `freehold install` first)")
		return
	}
	var req struct {
		Action string `json:"action"`
		Pubkey string `json:"pubkey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Pubkey == "" {
		writeErr(w, http.StatusBadRequest, "world-door: pubkey required")
		return
	}
	authorize, revoke := cpbuild.BuildWorldDoor(s.Builder)
	switch req.Action {
	case "authorize":
		if err := authorize(req.Pubkey); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "revoke":
		if err := revoke(req.Pubkey); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "world-door: action must be authorize or revoke")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
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
// "operator" is the box's operator identity ledger (the world's owner): the
// memory plane attests agent pods with it, so it ships sealed to the console
// identity alongside the DNS/litellm creds. The cert-seed pair is the box cert
// cache shipped for the worldCert pre-seed gate — the box OVERWRITES it on
// every build it ships (the box cache is that record's durable owner, unlike
// the secrets above, which the CP owns).
var stdSecrets = []string{"dns-relay", "dns-cp", "litellm", "operator", "cert-seed-relay", "cert-seed-cp"}

// secretsList reports which CP-owned secrets are present on disk (the idempotent
// inventory `build` uses to ask the operator only for what's missing).
func (s *Server) secretsList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
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
	if _, err := s.requireAdmin(r); err != nil {
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
	if _, err := s.requireAdmin(r); err != nil {
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
		// grants_source: "live" (the relay-signed 39002 roster — what actually
		// gates exec) or "package" (the shipped fallback — the co-located
		// runner's mode). Empty when neither was readable (grants = null).
		GrantsSource string `json:"grants_source,omitempty"`
		Readiness    interface{} `json:"readiness,omitempty"`
		// colocated marks the CP's own co-located runner.
		Colocated bool `json:"colocated,omitempty"`
		// Self-hosted (a resident runner) + whether the operator confirmed its
		// presented pubkeys (the fill unlocks on confirm).
		SelfHosted      bool `json:"self_hosted,omitempty"`
		EnrollConfirmed bool `json:"enroll_confirmed,omitempty"`
	}
	runners := make([]runnerOut, 0, len(snap.Runners))
	// Per-runner package state, kept by slice index so the roster results can
	// fall back to it after the concurrent collects.
	pkgGrants := make([][]string, 0, len(snap.Runners))
	pkgReadable := make([]bool, 0, len(snap.Runners))
	type probe struct {
		name  string
		value interface{}
	}
	probes := make(chan probe, len(snap.Runners))
	probeCount := 0
	rosters := make(chan probe, len(snap.Runners))
	rosterCount := 0
	// Roster reads need the relay coords (the runner's channel lives there)
	// and the console's own secret (it is every channel's owner).
	relayDial, relayAuth := s.relayDialAuth(snap)
	canReadRoster := relayDial != "" &&
		snap.RelayPubkey != nil && *snap.RelayPubkey != "" && len(s.ConsoleSecret) == 32
	for name, rec := range snap.Runners {
		var secret interface{}
		if sc, ok := snap.Secrets[name]; ok {
			secret = map[string]interface{}{
				"name": sc.Runner, "kind": sc.Kind, "address": sc.Address,
				"rotated_at": sc.RotatedAt, "created_at": sc.CreatedAt,
			}
		}
		var pkg []string
		readable := false
		if p, err := wire.Load(rec.PackageDir); err == nil {
			pkg, readable = p.Grants, true
		}
		pkgGrants = append(pkgGrants, pkg)
		pkgReadable = append(pkgReadable, readable)
		colocated := s.Builder != nil && name == s.Builder.RunnerTarget
		out := runnerOut{
			Name: name, Status: string(rec.Status), NostrPub: rec.NostrPubkey,
			EncPub: rec.EncPubkey, McpAddr: rec.McpAddr, Risk: rec.RiskLevel,
			Secret: secret, Colocated: colocated,
		}
		// The package result rides in upfront (it is the final answer for a
		// package-mode runner); a live roster result below overwrites it for
		// relay-mode runners.
		out.Grants, out.GrantsSource = grantsFor(nil, nil, pkg, readable, false)
		if capability, ok := snap.Capabilities[name]; ok && capability.SelfHosted() {
			out.SelfHosted = true
			out.EnrollConfirmed = capability.EnrollConfirmedAt != nil
		}
		// Live readiness probe (the console signs a status call as its own
		// identity — the runner still fails closed).
		if rec.Status == state.RunnerActive && rec.McpAddr != nil && len(s.ConsoleSecret) == 32 {
			probeCount++
			go func(name string, addr, rpk string) {
				probes <- probe{name, s.probeReadiness(addr, rpk)}
			}(name, *rec.McpAddr, rec.NostrPubkey)
		}
		// Live grants: the relay-signed 39002 roster, read fresh per call (the
		// same source the runner itself authorizes against, same dial/auth
		// split). Relay-mode only — the co-located runner has no channel (its
		// package IS its whitelist), and a revoked runner's channel is moot
		// (its package is gone; the report must stay null).
		if canReadRoster && rec.Status == state.RunnerActive && !colocated {
			rosterCount++
			go func(name string, rpk, runnerPK string) {
				members, rerr := relay.QueryChannelRosterAuth(relayDial, relayAuth, rpk, runnerPK, s.ConsoleSecret)
				rosters <- probe{name, rosterResult{members, rerr}}
			}(name, *snap.RelayPubkey, rec.NostrPubkey)
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
	for i := 0; i < rosterCount; i++ {
		p := <-rosters
		for j := range runners {
			if runners[j].Name == p.name {
				res := p.value.(rosterResult)
				runners[j].Grants, runners[j].GrantsSource = grantsFor(res.members, res.err, pkgGrants[j], pkgReadable[j], true)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"console_pubkey": s.ConsolePubkey,
		"runners":        runners,
	})
}

// rosterResult is one concurrent roster read's outcome.
type rosterResult struct {
	members []string
	err     error
}

// relayDialAuth derives the relay DIAL + CANONICAL NIP-98 auth URLs for the
// console's own relay reads: the LAN dial by HOSTNAME (buzz keys the
// community to the request Host — an IP dial presents a Host no community is
// configured for — and the /etc/hosts pin re-resolves it), the auth against
// the canonical public origin (a dial-URL-signed auth 401s "URL mismatch").
// Mirrors world()'s relay probe and the build's dial/sign split; the single
// recorded relay_url (the raw LAN IP:port) is the last fallback for both.
func (s *Server) relayDialAuth(snap state.ControlPlaneState) (dial, auth string) {
	relayHost := ""
	if snap.RelayHost != nil {
		relayHost = *snap.RelayHost
	}
	if relayHost == "" && s.Builder != nil {
		relayHost = s.Builder.RelayHost
	}
	if relayHost != "" {
		dial = config.RelayLanDial(relayHost)
		auth = "https://" + relayHost
	}
	if dial == "" && snap.RelayURL != nil {
		dial = *snap.RelayURL
	}
	if auth == "" && snap.RelayURL != nil {
		auth = *snap.RelayURL
	}
	return dial, auth
}

// grantsFor picks a runner's reported grants + source. queryLive marks a
// RELAY-MODE runner whose roster was actually read: the roster is the ONLY
// gate on its exec, so a successful read is reported verbatim — an EMPTY
// roster is an honest fail-closed (never masked by the stale package
// fallback) — and a failed read is "unavailable" (the console cannot see the
// whitelist; the runner itself still fails closed on the same outage).
// !queryLive (the co-located runner — no relay channel — or a CP with no
// relay coords) reports the shipped package grants, and an unreadable
// package stays nil so clients keep rendering the anomaly. A readable
// package always yields a NON-nil slice: null on the wire is reserved for
// "unreadable / unavailable".
func grantsFor(live []string, liveErr error, pkg []string, pkgReadable bool, queryLive bool) (grants []string, source string) {
	if queryLive {
		if liveErr == nil {
			return nonNil(live), "live"
		}
		return nil, "unavailable"
	}
	if pkgReadable {
		return nonNil(pkg), "package"
	}
	return nil, ""
}

// nonNil keeps an honest empty list marshaling as [] (never null).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
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
	if _, err := s.requireAdmin(r); err != nil {
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
		// The CP survives teardown: keep its own bare resolver entry (`cp` ->
		// the CP IP) so the control plane still resolves itself. The WORLD's
		// records (relay/k3s/litellm/proxy + the dotted relay host) go; the
		// dotted cp host points at the proxy, which is torn down with k3s.
		if name == "cp" {
			continue
		}
		res.DNS++
		s.Store.RemoveDNS(name)
	}
	if err := s.Store.Save(); err != nil {
		return res, fmt.Errorf("world-teardown failed to persist CP state")
	}
	// Reflect the removals in the live dnsmasq resolver, not just the store
	// mirror (otherwise the guests' records keep answering until a rebuild).
	if err := s.syncResolver(); err != nil {
		return res, fmt.Errorf("world-teardown sync resolver: %w", err)
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
	// Rosters (optional) records the runner as a capability whose grant is the
	// given agent NAMES (resolved through the agent registry; kind-9000
	// put-user, live) and makes it rebuild-safe: the recorded capability
	// re-stages + re-asserts the grants on every build — the record's rosters
	// are NAMES so the rebuild re-assertion resolves them the same way the
	// agent flow's do. Port (optional) pins the runner's MCP bind port on the
	// CP LXC (allocated above the capability table when 0). The runner UNIT
	// starts on the next build/world_build reconcile.
	Rosters []string `json:"rosters"`
	Port    int      `json:"port"`
	// Probe (optional) is the door's verify arm — "<METHOD> <path> [auth]
	// [want]" — shipped in the package so the self-check is data; empty kind
	// arms fall back to the runner's built-in match. ProbeBody is its
	// optional literal JSON request body.
	Probe     string `json:"probe"`
	ProbeBody string `json:"probe_body"`
}

// relayAuthFor returns the NIP-98 canonical URL for relay writes: the relay's
// public https origin when the community host is known, else "" (the callee
// falls back to the dial URL — a LAN-only relay with no public domain).
func (s *Server) relayAuthFor() string {
	if s.RelayHost != "" {
		return "https://" + strings.TrimSuffix(s.RelayHost, "/")
	}
	return ""
}

// relayDialFor returns the relay DIAL URL for buzz publishes: the relay's own
// hostname when known, because buzz keys the community to the HOST header — a
// raw-IP dial (the recorded state RelayURL is the LAN-IP form) publishes to
// "no community is configured for this host". The CP guest's /etc/hosts pin
// (deploy drops it, every build re-asserts it) maps the hostname to the
// relay's LAN IP, so the host-form dial resolves LAN-side while NIP-98 signs
// the CANONICAL public origin (relayAuthFor) — the same dial-LAN /
// sign-public split agent-tools and cpbuild use. The port is the relay's LAN
// HTTP port (pre-Caddy), 3000 as everywhere else.
func (s *Server) relayDialFor(snapURL *string) string {
	if d := config.RelayLanDial(s.RelayHost); d != "" {
		return d
	}
	if snapURL != nil {
		return *snapURL
	}
	return ""
}

func (s *Server) provision(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
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
		Probe: req.Probe, ProbeBody: req.ProbeBody,
	})
	if err != nil {
		writeErr(w, statusForAction(err), err.Error())
		return
	}
	var relayURL *string
	snap := s.Store.Snapshot()
	relayURL = snap.RelayURL
	if dial := s.relayDialFor(snap.RelayURL); dial != "" {
		if err := provisioner.SyncRunnerChannel(s.Store, dial, s.relayAuthFor(), req.Name, s.Store.Dir()); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	// Capability rosters: record the runner as an agent-provisioned capability
	// (rebuild-safe) and grant each roster agent onto its channel live. The
	// record's rosters hold agent NAMES — the same representation the rebuild
	// re-assertion consumes — resolved to pubkeys here for the live grant.
	granted := []string{s.ConsolePubkey}
	var capability *state.CapabilityRecord
	if len(req.Rosters) > 0 {
		resolved, err := resolveAgentRoster(s.AgentToolsDir, req.Rosters)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		port := req.Port
		if port == 0 {
			port = s.capabilityPortAbove(s.Builder)
		}
		rec := state.CapabilityRecord{
			Kind: req.Kind, Address: req.Address, Port: port,
			Rosters:   append([]string(nil), req.Rosters...),
			Origin:    state.OriginOperator,
			CreatedAt: uint64(time.Now().Unix()),
		}
		if err := s.Store.InsertCapability(req.Name, rec); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
		capability = &rec
		for _, pk := range resolved {
			if err := provisioner.PutUserMembership(s.Store, dialOr(relayURL), s.relayAuthFor(), req.Name, pk, s.Store.Dir()); err != nil {
				writeErr(w, statusForAction(err), err.Error())
				return
			}
			granted = append(granted, pk)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "name": res.Name, "nostr_pubkey": res.NostrPubkey,
		"enc_pubkey": res.EncPubkey, "package_dir": res.PackageDir,
		"granted": granted, "relay": relayURL, "capability": capability,
	})
}

// dialOr returns the relay dial URL or a non-nil fallback for put-user.
func dialOr(u *string) string {
	if u == nil {
		return ""
	}
	return *u
}

// resolveAgentRoster resolves agent NAMES to their registry pubkeys (the
// authoritative registry lives in the agent-tools state dir). An unknown name
// is a 400 — a recorded roster that cannot resolve would silently lose its
// grants on rebuild.
func resolveAgentRoster(agentToolsDir string, names []string) ([]string, error) {
	reg, err := agenttools.OpenRegistry(filepath.Join(agentToolsDir, "registry.json"))
	if err != nil {
		return nil, fmt.Errorf("open agent registry: %w", err)
	}
	rows, err := reg.Agents()
	if err != nil {
		return nil, fmt.Errorf("read agent registry: %w", err)
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		found := ""
		for _, a := range rows {
			if a.Name == name {
				found = a.Pubkey
				break
			}
		}
		if found == "" {
			return nil, fmt.Errorf("unknown agent %q — create it first, then grant", name)
		}
		out = append(out, found)
	}
	return out, nil
}

// agentNameForPubkey resolves a registry agent's pubkey back to its NAME (the
// reverse of resolveAgentRoster) — "" when the pubkey is not a registry agent
// (the operator's, an external identity's). err != nil is a REGISTRY READ
// failure (callers that must not silently skip the durable bookkeeping fail
// on it; best-effort callers may ignore it).
func agentNameForPubkey(agentToolsDir, pubkey string) (string, error) {
	reg, err := agenttools.OpenRegistry(filepath.Join(agentToolsDir, "registry.json"))
	if err != nil {
		return "", fmt.Errorf("open agent registry: %w", err)
	}
	rows, err := reg.Agents()
	if err != nil {
		return "", fmt.Errorf("read agent registry: %w", err)
	}
	for _, a := range rows {
		if a.Pubkey == pubkey {
			return a.Name, nil
		}
	}
	return "", nil
}

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// capabilityPortAbove allocates the first free MCP port above the dynamic
// base. With a build Spec, the SAME allocator cpbuild uses runs (it also
// skips the per-zone DNS doors' derived ports — two allocators must never
// disagree); without one (a console not bound as the build executor), the
// recorded + static ports are the only known occupancy.
func (s *Server) capabilityPortAbove(builder *cpbuild.Spec) int {
	if builder != nil {
		return builder.NextCapabilityPort(s.Store)
	}
	occupied := cpbuild.OccupiedCapabilityPorts(s.Store)
	port := 8800
	for occupied[port] {
		port++
	}
	return port
}

func (s *Server) rotate(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
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
	// A SELF-HOSTED runner has no CP-side package dir to re-ship: seal to its
	// presented key and return the package JSON — the grantee carries it to
	// the guest through its own door (writes secrets.json beside the runner's
	// identity.json) and restarts the unit there. Ciphertext in an agent's
	// context is the system's normal trust level (the runner's key decrypts
	// it, nothing else). The fill is refused until the operator CONFIRMED the
	// presented pubkeys on this page — the barrier that keeps a compromised
	// provisioning agent from sealing the credential to its own key.
	//
	// State is read FRESH from disk (not the startup memory snapshot): the
	// capability record was written by the agent-tools PROCESS (the
	// provision_runner flow), out-of-band from this serve — the same reason
	// overview() re-opens.
	fresh, err := state.Open(s.stateDir())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read CP state: "+err.Error())
		return
	}
	// Self-hosted-ness routes on the RUNNER record too (PackageDir empty =
	// resident): a failure between EnrollRunner and the record write must
	// never drop a self-hosted runner into the CP-guest path below (whose
	// seal assumes a package dir to re-ship).
	runnerRec, hasRunner := fresh.GetRunner(req.Name)
	cap, isCap := fresh.GetCapability(req.Name)
	isSelfHosted := (hasRunner && runnerRec.PackageDir == "") || (isCap && cap.SelfHosted())
	if isSelfHosted {
		if !isCap || cap.EnrollConfirmedAt == nil {
			writeErr(w, http.StatusBadRequest, "self-hosted door not confirmed — verify the presented pubkeys on this page against the guest's own `runner enroll` output (Compute's report), then confirm the enrollment; the fill unlocks after that")
			return
		}
		rec, pkgJSON, err := provisioner.RotateSecretSelfHosted(fresh, req.Name, []byte(req.Secret))
		if err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
		if dial := s.relayDialFor(fresh.Snapshot().RelayURL); dial != "" {
			if err := provisioner.SyncRunnerChannel(fresh, dial, s.relayAuthFor(), req.Name, fresh.Dir()); err != nil {
				writeErr(w, statusForAction(err), err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok": true, "name": req.Name, "self_hosted": true,
			"package_json": string(pkgJSON), "rotated_at": rec.RotatedAt,
			"restarted": false,
		})
		return
	}
	// The CP-guest path rotates through the FRESH store too: the runner (and
	// its secret) may have been provisioned by the agent-tools process after
	// this serve started.
	if _, err := provisioner.RotateSecret(fresh, req.Name, []byte(req.Secret)); err != nil {
		writeErr(w, statusForAction(err), err.Error())
		return
	}
	var relayURL *string
	snap := fresh.Snapshot()
	relayURL = snap.RelayURL
	if dial := s.relayDialFor(snap.RelayURL); dial != "" {
		if err := provisioner.SyncRunnerChannel(fresh, dial, s.relayAuthFor(), req.Name, fresh.Dir()); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	// A capability door picks a rotated credential up ONLY on restart (the
	// runner holds its package in memory from boot) — the console runs on the
	// same guest, so restart the unit + wait for the listen. The seal stands
	// even when the restart fails (soft: reported, never blocks the rotate).
	restarted := false
	restartErr := ""
	if rec, ok := fresh.GetCapability(req.Name); ok {
		if err := s.restartDoor(req.Name, rec.Port); err != nil {
			restartErr = err.Error()
		} else {
			restarted = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "name": req.Name, "relay": relayURL,
		"restarted": restarted, "restart_error": restartErr,
	})
}

// enrollConfirm records the operator's confirmation of a self-hosted door's
// presented pubkeys. The operator verifies them against the guest's own
// `runner enroll` output (Compute's audited report in the thread) BEFORE
// confirming — this is what binds the later credential fill to the key the
// guest actually holds, not to whatever the provisioning agent presented.
// State is read fresh from disk: the capability was recorded by the
// agent-tools process (out-of-band from this serve's startup snapshot).
func (s *Server) enrollConfirm(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body")
		return
	}
	fresh, err := state.Open(s.stateDir())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read CP state: "+err.Error())
		return
	}
	if err := fresh.ConfirmEnrollment(req.Name, uint64(time.Now().Unix())); err != nil {
		writeErr(w, statusForAction(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name, "confirmed": true})
}

// restartDoor restarts a capability door's runner unit on THIS guest (the
// console runs there; the unit binds 0.0.0.0:<port>) and waits for the listen.
func (s *Server) restartDoor(name string, port int) error {
	if s.RestartDoor != nil {
		return s.RestartDoor(name, port)
	}
	unit := "freehold-runner-" + name
	cmd := exec.Command("systemctl", "restart", unit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restart %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, derr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if derr == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not listen on %d after restart", unit, port)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
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
		if err := provisioner.RevokeRunnerChannel(s.Store, *relayURL, s.relayAuthFor(), req.Name, s.Store.Dir()); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name, "relay": relayURL})
}

func (s *Server) grant(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
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
	// A SELF-HOSTED runner's whitelist is the LIVE relay roster only (no
	// shipped package exists to append grants to) — the grant is a pure
	// put-user; state read fresh from disk (the capability was recorded by
	// the agent-tools process, out-of-band from this serve).
	//
	// Records that predate the hosted field read the same way (no hosted,
	// empty package_dir — the runner is resident on its target and the CP
	// holds no package to append to); the runner row says the same thing.
	// Mirror of revoke-grant's fallback.
	fresh, err := state.Open(s.stateDir())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read CP state: "+err.Error())
		return
	}
	var grants []string
	cap, isCap := fresh.GetCapability(req.Name)
	runnerOnlyNoPackage := false
	if rec, ok := fresh.GetRunner(req.Name); ok {
		runnerOnlyNoPackage = rec.PackageDir == ""
	}
	selfHosted := (isCap && cap.SelfHosted()) || runnerOnlyNoPackage
	if selfHosted {
		if !provisioner.IsPubkey(req.Pubkey) {
			writeErr(w, http.StatusBadRequest, "invalid pubkey")
			return
		}
		// The door's runner row must exist + be active: the record alone (a
		// runner row that vanished) yields coords that can never resolve —
		// grant the re-enroll, not a ghost.
		if rec, ok := fresh.GetRunner(req.Name); !ok || rec.Status == state.RunnerRevoked {
			writeErr(w, http.StatusBadRequest, "runner "+req.Name+" is not active — re-enroll it on the target first")
			return
		}
		grants = []string{req.Pubkey}
		// A pubkey that belongs to a REGISTRY agent joins the record's
		// Rosters: that list drives the pod coords (the grantee's exec
		// surface) AND the rebuild's grant re-assertion. An unknown pubkey
		// (the operator, an external identity) rides the relay roster alone —
		// nothing prunes it, so it persists across builds without a record.
		// Best-effort here: a registry-read failure still lands the live
		// grant; only the durable bookkeeping is skipped.
		if isCap {
			if agentName, _ := agentNameForPubkey(s.AgentToolsDir, req.Pubkey); agentName != "" {
				rosters := cap.Rosters
				if !containsString(rosters, agentName) {
					rosters = append(rosters, agentName)
					cap.Rosters = rosters
					if err := fresh.InsertCapability(req.Name, cap); err != nil {
						writeErr(w, statusForAction(err), err.Error())
						return
					}
				}
			}
		}
	} else {
		grants, err = provisioner.GrantAgent(fresh, req.Name, req.Pubkey)
		if err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	var relayURL *string
	snap := fresh.Snapshot()
	relayURL = snap.RelayURL
	if relayURL != nil {
		if err := provisioner.PutUserMembership(fresh, s.relayDialFor(relayURL), s.relayAuthFor(), req.Name, req.Pubkey, fresh.Dir()); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name, "granted": grants, "relay": relayURL})
}

func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
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
	// Mirror of grant: a SELF-HOSTED runner's whitelist is the live roster —
	// no package to strip; state read fresh from disk.
	//
	// Records that predate the hosted field read the same way: no hosted, no
	// package_dir — the runner is resident on its target and the CP holds no
	// package, so there is nothing to strip and the relay remove-user is the
	// whole revoke. The runner row (empty PackageDir) says the same thing.
	fresh, err := state.Open(s.stateDir())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read CP state: "+err.Error())
		return
	}
	var grants []string
	cap, isCap := fresh.GetCapability(req.Name)
	runnerOnlyNoPackage := false
	if rec, ok := fresh.GetRunner(req.Name); ok {
		runnerOnlyNoPackage = rec.PackageDir == ""
	}
	selfHosted := (isCap && cap.SelfHosted()) || runnerOnlyNoPackage
	if selfHosted {
		grants = []string{}
		if isCap {
			// Mirror of grant: a rostered agent's pubkey leaves the record's
			// Rosters too — else the rebuild's re-assertion re-adds the member
			// the operator just revoked. The name resolution is LOUD here (not
			// best-effort): a registry-read failure must not silently leave the
			// revoked member in the durable roster.
			agentName, nameErr := agentNameForPubkey(s.AgentToolsDir, req.Pubkey)
			if nameErr != nil {
				writeErr(w, http.StatusInternalServerError, "revoke-grant: resolve the pubkey in the agent registry: "+nameErr.Error())
				return
			}
			if agentName != "" {
				kept := make([]string, 0, len(cap.Rosters))
				for _, r := range cap.Rosters {
					if r != agentName {
						kept = append(kept, r)
					}
				}
				cap.Rosters = kept
				if err := fresh.InsertCapability(req.Name, cap); err != nil {
					writeErr(w, statusForAction(err), err.Error())
					return
				}
			}
		}
	} else {
		grants, err = provisioner.RevokeGrant(fresh, req.Name, req.Pubkey)
		if err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	var relayURL *string
	snap := fresh.Snapshot()
	relayURL = snap.RelayURL
	if relayURL != nil {
		if err := provisioner.RemoveUserMembership(fresh, s.relayDialFor(relayURL), s.relayAuthFor(), req.Name, req.Pubkey, fresh.Dir()); err != nil {
			writeErr(w, statusForAction(err), err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": req.Name, "granted": grants, "relay": relayURL})
}

func (s *Server) runnerAddr(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
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
	if _, err := s.requireAdmin(r); err != nil {
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
	if _, err := s.requireAdmin(r); err != nil {
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
	if _, err := s.requireAdmin(r); err != nil {
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
	if _, err := s.requireAdmin(r); err != nil {
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
	dial, auth := s.relayDialAuth(snap)
	members, err := relay.QueryChannelRosterAuth(dial, auth, *snap.RelayPubkey, rec.NostrPubkey, s.ConsoleSecret)
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

// jobsList is the scheduled-jobs read — the ONE route a member role reaches.
// Ownership is the privacy boundary: an owner's own rows carry the prompt and
// label; every other row is metadata only (owner npub, agent npub, channel,
// schedule, last run, created date) — the operator included. A member sees
// ONLY their own rows; the operator (and the loopback posture) sees all.
func (s *Server) jobsList(w http.ResponseWriter, r *http.Request) {
	pk, role, err := s.sessionFor(r)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if err := checkOrigin(r, s.PublicOrigin); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.AgentToolsDir == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": []interface{}{}})
		return
	}
	jobs, err := agenttools.LoadJobsReadOnly(filepath.Join(s.AgentToolsDir, agenttools.JobsFile))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	isOperator := s.Auth == nil || role == RoleOperator
	out := make([]map[string]interface{}, 0, len(jobs))
	for _, j := range jobs {
		mine := pk != "" && j.Owner == pk
		if !isOperator && !mine {
			continue // a member sees only their own jobs
		}
		row := map[string]interface{}{
			"id": j.ID, "owner": j.Owner, "agent": j.Agent, "channel": j.Channel,
			"cron": j.Cron, "at": j.At, "tz": j.TZ,
			"created_at": j.CreatedAt, "next_run_at": j.NextRunAt, "paused": j.Paused,
		}
		if last := j.LastRun(); last != nil {
			row["last_run"] = last
		}
		if mine {
			row["label"] = j.Label
			row["prompt"] = j.Prompt
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": out})
}

func (s *Server) agentsList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireAdmin(r); err != nil {
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
	if _, err := s.requireAdmin(r); err != nil {
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
	if _, err := s.requireAdmin(r); err != nil {
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
