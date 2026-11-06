package cpbuild

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	provisioner "freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

// mcpKeyGateway is the fake gateway's KEY + MCP-server state: enough of the
// admin API for the MCP plane — /key/generate, /key/info (by token OR hash),
// /key/update (by token OR hash), /key/delete, /key/list, and
// /v1/mcp/server (GET/POST/PUT). Everything records: minted bodies, updated
// permissions, registrations. liveDB=false simulates a wiped gateway DB
// (every stored key goes dead — the re-mint heal path).
type mcpKeyGateway struct {
	mu sync.Mutex

	keys int
	// byToken: the raw sk- token → record. byHash: the hash → same record.
	byToken map[string]*keyRecord
	byHash  map[string]*keyRecord
	liveDB  bool

	genBodies []string
	servers   map[string]map[string]interface{}
	regBodies []map[string]interface{}
	deleted   []string
}

type keyRecord struct {
	token  string
	hash   string
	alias  string
	perm   map[string]interface{}
	models []string
}

func (g *mcpKeyGateway) recordFor(keyOrHash string) *keyRecord {
	if r, ok := g.byToken[keyOrHash]; ok {
		return r
	}
	return g.byHash[keyOrHash]
}

func newMCPKeyGateway() *mcpKeyGateway {
	return &mcpKeyGateway{
		byToken: map[string]*keyRecord{},
		byHash:  map[string]*keyRecord{},
		liveDB:  true,
		servers: map[string]map[string]interface{}{},
	}
}

var hashRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (g *mcpKeyGateway) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/key/generate":
			var body struct {
				KeyAlias         string                 `json:"key_alias"`
				Models           []string               `json:"models"`
				ObjectPermission map[string]interface{} `json:"object_permission"`
			}
			_ = json.Unmarshal(raw, &body)
			g.genBodies = append(g.genBodies, string(raw))
			g.keys++
			rec := &keyRecord{
				token:  fmt.Sprintf("sk-mcp-%d", g.keys),
				hash:   fmt.Sprintf("%064x", g.keys),
				alias:  body.KeyAlias,
				perm:   body.ObjectPermission,
				models: body.Models,
			}
			if rec.perm == nil {
				rec.perm = map[string]interface{}{}
			}
			g.byToken[rec.token] = rec
			g.byHash[rec.hash] = rec
			w.Write([]byte(`{"key":"` + rec.token + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/key/info":
			rec := g.recordFor(r.URL.Query().Get("key"))
			if !g.liveDB || rec == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write([]byte(`{"info":{"key_alias":` + js(rec.alias) + `,"object_permission":` + jsAny(rec.perm) + `}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/key/update":
			var body struct {
				Key              string                 `json:"key"`
				KeyAlias         string                 `json:"key_alias"`
				ObjectPermission map[string]interface{} `json:"object_permission"`
			}
			_ = json.Unmarshal(raw, &body)
			rec := g.recordFor(body.Key)
			if !g.liveDB || rec == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if body.KeyAlias != "" {
				rec.alias = body.KeyAlias
			}
			if body.ObjectPermission != nil {
				rec.perm = body.ObjectPermission
			}
			w.Write([]byte(`{"key_alias":` + js(rec.alias) + `,"object_permission":` + jsAny(rec.perm) + `}`))
		case r.Method == http.MethodPost && r.URL.Path == "/key/delete":
			var body struct {
				Keys []string `json:"keys"`
			}
			_ = json.Unmarshal(raw, &body)
			for _, k := range body.Keys {
				g.deleted = append(g.deleted, k)
				if rec := g.recordFor(k); rec != nil {
					delete(g.byToken, rec.token)
					delete(g.byHash, rec.hash)
				}
			}
			w.Write([]byte(`{"deleted_keys":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/key/list":
			hashes := []string{}
			if g.liveDB {
				for h := range g.byHash {
					hashes = append(hashes, h)
				}
			}
			w.Write([]byte(`{"keys":` + jsAny(hashes) + `}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/mcp/server":
			out := []map[string]interface{}{}
			for id, s := range g.servers {
				out = append(out, map[string]interface{}{"server_id": id, "body": s})
			}
			w.Write([]byte(jsAny(out)))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/mcp/server":
			var body map[string]interface{}
			_ = json.Unmarshal(raw, &body)
			g.regBodies = append(g.regBodies, body)
			id, _ := body["server_id"].(string)
			if _, exists := g.servers[id]; exists {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"detail":{"error":"MCP Server with id %s already exists. Cannot create another."}}`, id)
				return
			}
			g.servers[id] = body
			w.Write([]byte(`{"server_id":` + js(id) + `}`))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/mcp/server":
			var body map[string]interface{}
			_ = json.Unmarshal(raw, &body)
			id, _ := body["server_id"].(string)
			if _, exists := g.servers[id]; !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			g.servers[id] = body
			g.regBodies = append(g.regBodies, body)
			w.Write([]byte(`{"server_id":` + js(id) + `}`))
		default:
			t.Logf("fake gateway: unhandled %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// js quotes a Go string as a JSON string literal.
func js(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// jsAny marshals v, keeping nil maps out (the gateway omits unset fields).
func jsAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// keyInfoFor returns a token's stored permission record via the info path.
func (g *mcpKeyGateway) keyInfoFor(token string) (string, map[string]interface{}, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.byToken[token]
	if !g.liveDB || rec == nil {
		return "", nil, fmt.Errorf("HTTP 404")
	}
	return rec.alias, rec.perm, nil
}

// denyList reads a record's mcp_servers list (nil = absent/open).
func (g *mcpKeyGateway) denyList(t *testing.T, token string) []string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.byToken[token]
	if rec == nil || rec.perm == nil {
		return nil
	}
	raw, _ := rec.perm["mcp_servers"].([]any)
	out := []string{}
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// seededMCPStore opens a fresh state store with one slotted MCP capability
// granted to "Helper", a kube slot runner row, and a REAL runner package dir
// (identity minted — AddSecret re-seals to the runner's key), over a
// litellmFixture'd CP state dir.
func seededMCPStore(t *testing.T, dir string) *state.StateStore {
	t.Helper()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	pkgDir := filepathJoin(dir, "runner", "kube-api-mcp")
	if _, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: "kube-api-mcp", Kind: "kubernetes", Address: "https://k3s:6443",
		Secret: []byte("slot-token"), RunnerDir: pkgDir,
	}); err != nil {
		t.Fatalf("provision slot runner package: %v", err)
	}
	if err := store.InsertCapability("kube-api-mcp", state.CapabilityRecord{
		Kind: "kubernetes", NS: "mcp", Port: 8801, Origin: state.OriginAgent,
	}); err != nil {
		t.Fatalf("insert slot capability: %v", err)
	}
	if err := store.InsertMCPCapability("google_sheets_a_ro", state.MCPCapabilityRecord{
		ServerName: "google_sheets_a_ro", Namespace: "mcp", ServicePort: 8000,
		Transport: "http", URL: "http://google-sheets-a-ro.mcp.svc.cluster.local:8000/mcp",
		Scope: "spreadsheet:abc:readonly", Tools: []string{"read_thing"},
		Grantees: []string{"Helper"}, Origin: state.OriginAgent,
		SecretRef: "google-sheets-a-ro-token", CreatedAt: 5,
	}); err != nil {
		t.Fatalf("insert mcp capability: %v", err)
	}
	return store
}

// seedAgents registers two agents in the store (the sweep's owner plane).
func seedAgents(t *testing.T, store *state.StateStore, names ...string) {
	t.Helper()
	for _, n := range names {
		store.InsertAgent(n, state.AgentRecord{Pubkey: strings.Repeat("a", 64), CreatedAt: 1})
	}
}

func filepathJoin(a, b, c string) string { return a + "/" + b + "/" + c }

// TestAgentMCPPermissionBody: an agent's object_permission is the union of
// its granted MCP capabilities (tools pinned when the record pins a subset,
// whole-server when not); no grants = the no-mcp-servers sentinel.
func TestAgentMCPPermissionBody(t *testing.T) {
	dir := t.TempDir()
	store := seededMCPStore(t, (&Spec{StateDir: dir}).consoleStateRoot())
	if err := store.InsertMCPCapability("sheet_b_rw", state.MCPCapabilityRecord{
		ServerName: "sheet_b_rw", Transport: "http", URL: "http://b/mcp",
		Grantees: []string{"Helper", "Writer"}, Origin: state.OriginAgent, CreatedAt: 6,
	}); err != nil {
		t.Fatalf("insert b: %v", err)
	}
	body := agentMCPPermissionBody(store, "Helper")
	if got := fmt.Sprint(body["mcp_servers"]); !strings.Contains(got, "google_sheets_a_ro") || !strings.Contains(got, "sheet_b_rw") {
		t.Fatalf("granted servers = %v", body["mcp_servers"])
	}
	tools, ok := body["mcp_tool_permissions"].(map[string][]string)
	if !ok || strings.Join(tools["google_sheets_a_ro"], ",") != "read_thing" || len(tools["sheet_b_rw"]) != 0 {
		t.Fatalf("tool pinning = %v", body["mcp_tool_permissions"])
	}
	denied := agentMCPPermissionBody(store, "Nobody")
	if fmt.Sprint(denied["mcp_servers"]) != "[no-mcp-servers]" {
		t.Fatalf("ungranted agent = %v", denied)
	}
}

// TestEnsureAgentMCPKey: mint-once semantics — the MCP-only key mints with
// models ["no-default-models"] + the state-derived object_permission,
// persists in the store, re-issues WITHOUT re-minting, and OVERWRITES a
// drifted gateway copy from CP state (with a drift note).
func TestEnsureAgentMCPKey(t *testing.T) {
	dir := t.TempDir()
	stateDir := dir + "/agent-tools"
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	store := seededMCPStore(t, stateDir)
	seedAgents(t, store, "Helper")
	gw := newMCPKeyGateway()
	srv := httptest.NewServer(gw.handler(t))
	defer srv.Close()
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}

	token, drift, err := spec.ensureAgentMCPKey("Helper", store)
	if err != nil || token == "" || drift != "" {
		t.Fatalf("mint: token=%q drift=%q err=%v", token, drift, err)
	}
	if len(gw.genBodies) != 1 {
		t.Fatalf("want one mint, got %v", gw.genBodies)
	}
	var minted struct {
		KeyAlias         string   `json:"key_alias"`
		Models           []string `json:"models"`
		ObjectPermission struct {
			MCPServers []string            `json:"mcp_servers"`
			ToolPerms  map[string][]string `json:"mcp_tool_permissions"`
		} `json:"object_permission"`
	}
	if err := json.Unmarshal([]byte(gw.genBodies[0]), &minted); err != nil {
		t.Fatalf("decode mint body: %v", err)
	}
	if minted.KeyAlias != "mcptool-helper" {
		t.Fatalf("key_alias = %q, want mcptool-helper", minted.KeyAlias)
	}
	if len(minted.Models) != 1 || minted.Models[0] != "no-default-models" {
		t.Fatalf("models = %v, want [no-default-models]", minted.Models)
	}
	if len(minted.ObjectPermission.MCPServers) != 1 || minted.ObjectPermission.MCPServers[0] != "google_sheets_a_ro" {
		t.Fatalf("mcp_servers = %v", minted.ObjectPermission.MCPServers)
	}
	if strings.Join(minted.ObjectPermission.ToolPerms["google_sheets_a_ro"], ",") != "read_thing" {
		t.Fatalf("tool perms = %v", minted.ObjectPermission.ToolPerms)
	}
	// The store holds the token (the env key is pod-keyed).
	if env := storeEnvForTest(t, stateDir); env[mcptoolKeyEnvKey("helper")] != token {
		t.Fatalf("store env missing the minted token")
	}
	// Second call: NO new mint (the live token is reused).
	if _, _, err := spec.ensureAgentMCPKey("Helper", store); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if len(gw.genBodies) != 1 {
		t.Fatalf("a live key must not re-mint, got %v", gw.genBodies)
	}
	// Drift: the gateway's copy is rewritten from CP state with a note.
	gw.mu.Lock()
	gw.byToken[token].perm = map[string]interface{}{"mcp_servers": []string{"evil_server"}}
	gw.mu.Unlock()
	_, drift, err = spec.ensureAgentMCPKey("Helper", store)
	if err != nil || drift == "" || !strings.Contains(drift, "rewritten") {
		t.Fatalf("drift overwrite: drift=%q err=%v", drift, err)
	}
	if denied := gw.denyList(t, token); len(denied) != 1 || denied[0] != "google_sheets_a_ro" {
		t.Fatalf("after overwrite, mcp_servers = %v", denied)
	}
}

// TestEnsureAgentMCPKeyReMintOnWipedDB: a dead stored token (a wiped gateway
// DB) re-mints and re-persists — a store-trusted dead key never locks the
// plane.
func TestEnsureAgentMCPKeyReMintOnWipedDB(t *testing.T) {
	dir := t.TempDir()
	stateDir := dir + "/agent-tools"
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	store := seededMCPStore(t, stateDir)
	seedAgents(t, store, "Helper")
	gw := newMCPKeyGateway()
	srv := httptest.NewServer(gw.handler(t))
	defer srv.Close()
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}

	token, _, err := spec.ensureAgentMCPKey("Helper", store)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	gw.mu.Lock()
	gw.liveDB = false // the wipe
	gw.mu.Unlock()
	if _, _, err := spec.ensureAgentMCPKey("Helper", store); err != nil {
		t.Fatalf("re-mint on wiped DB: %v", err)
	}
	if len(gw.genBodies) != 2 {
		t.Fatalf("a dead key must re-mint, got %d mints", len(gw.genBodies))
	}
	if env := storeEnvForTest(t, stateDir); env[mcptoolKeyEnvKey("helper")] == token {
		t.Fatalf("the dead token must have been replaced in the store")
	}
}

// TestRevokeAgentMCPKey: delete by stored token + drop the store record;
// ensure-revoked (a 404 gateway clears too).
func TestRevokeAgentMCPKey(t *testing.T) {
	dir := t.TempDir()
	stateDir := dir + "/agent-tools"
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	store := seededMCPStore(t, stateDir)
	seedAgents(t, store, "Helper")
	gw := newMCPKeyGateway()
	srv := httptest.NewServer(gw.handler(t))
	defer srv.Close()
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}

	if err := spec.revokeAgentMCPKey("Helper"); err != nil {
		t.Fatalf("revoke with no key is a no-op: %v", err)
	}
	token, _, err := spec.ensureAgentMCPKey("Helper", store)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := spec.revokeAgentMCPKey("Helper"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if env := storeEnvForTest(t, stateDir); env[mcptoolKeyEnvKey("helper")] != "" {
		t.Fatalf("the store record must be dropped")
	}
	if len(gw.deleted) != 1 || gw.deleted[0] != token {
		t.Fatalf("gateway deletes = %v", gw.deleted)
	}
}

// TestRegisterLiteLLMMServer: idempotent registration — POST creates with
// static_headers + access group + allowed_tools; a re-run PUTs (no
// duplicate); the broker token is minted into the store AND sealed into the
// slot runner's package.
func TestRegisterLiteLLMMServer(t *testing.T) {
	dir := t.TempDir()
	stateDir := dir + "/agent-tools"
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	store := seededMCPStore(t, stateDir)
	gw := newMCPKeyGateway()
	srv := httptest.NewServer(gw.handler(t))
	defer srv.Close()
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}

	rec, _ := store.GetMCPCapability("google_sheets_a_ro")
	if err := spec.registerLiteLLMMServer(store, rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := spec.registerLiteLLMMServer(store, rec); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	gw.mu.Lock()
	srvs := len(gw.servers)
	last := gw.regBodies[len(gw.regBodies)-1]
	tokenEnv := storeEnvForTest(t, stateDir)["mcp-google_sheets_a_ro-token"]
	gw.mu.Unlock()
	if srvs != 1 {
		t.Fatalf("want exactly one registered server, got %d", srvs)
	}
	// Two registrations: the create (POST) and the idempotent re-run (PUT) —
	// a duplicate POST would have been refused by the gateway (400).
	if len(gw.regBodies) != 2 {
		t.Fatalf("want one POST create + one PUT update, got %d registrations", len(gw.regBodies))
	}
	headers, _ := last["static_headers"].(map[string]interface{})
	if headers == nil || headers["Authorization"] != "Bearer "+tokenEnv {
		t.Fatalf("static_headers = %v, want the store's broker token %q", headers, tokenEnv)
	}
	groups, _ := last["mcp_access_groups"].([]any)
	if len(groups) != 1 || groups[0] != mcpAccessGroup {
		t.Fatalf("access groups = %v, want [%s]", groups, mcpAccessGroup)
	}
	tools, _ := last["allowed_tools"].([]any)
	if len(tools) != 1 || tools[0] != "read_thing" {
		t.Fatalf("allowed_tools = %v, want [read_thing]", tools)
	}
	// The broker token is ALSO sealed into the slot runner's package.
	pkgRaw, err := os.ReadFile(filepathJoin(stateDir, "runner", "kube-api-mcp") + "/secrets.json")
	if err != nil {
		t.Fatalf("read slot package: %v", err)
	}
	var pkg struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(pkgRaw, &pkg); err != nil {
		t.Fatalf("decode package: %v", err)
	}
	if _, ok := pkg.Secrets["google-sheets-a-ro-token"]; !ok {
		t.Fatalf("the broker token must be sealed into the slot runner's package, got %v", pkg.Secrets)
	}
}

// TestSweepLitellmMCPAccess: the fleet sweep — the CP's model keys and
// foreign keys go to the sentinel; a live agent's mcptool key is overwritten
// from state; an ORPHANED mcptool key (agent gone) is swept; the report
// names every rewrite.
func TestSweepLitellmMCPAccess(t *testing.T) {
	dir := t.TempDir()
	stateDir := dir + "/agent-tools"
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	store := seededMCPStore(t, stateDir)
	seedAgents(t, store, "Helper")
	gw := newMCPKeyGateway()
	srv := httptest.NewServer(gw.handler(t))
	defer srv.Close()
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}

	// The CP's model key is minted through the REAL path — its gateway
	// key_alias is the BARE pod name ("helper"; "agentkey-" is only the
	// store env key) — and holds a drifted MCP grant.
	modelToken, err := spec.ensureAgentLitellmKey("Helper")
	if err != nil {
		t.Fatalf("mint model key: %v", err)
	}
	// A foreign UI key with grants + an orphaned mcptool key (agent gone).
	foreign := []string{}
	gw.mu.Lock()
	for _, alias := range []string{"ui-key", "mcptool-ghost"} {
		gw.keys++
		rec := &keyRecord{token: fmt.Sprintf("sk-%d", gw.keys), hash: fmt.Sprintf("%064x", gw.keys), alias: alias, perm: map[string]interface{}{"mcp_servers": []string{"evil"}}}
		gw.byToken[rec.token] = rec
		gw.byHash[rec.hash] = rec
		foreign = append(foreign, rec.token)
	}
	gw.mu.Unlock()

	drift, err := spec.sweepLitellmMCPAccess(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// The live agent's mcptool key exists with the state's grants.
	helperToken := storeEnvForTest(t, stateDir)[mcptoolKeyEnvKey("helper")]
	if denied := gw.denyList(t, helperToken); len(denied) != 1 || denied[0] != "google_sheets_a_ro" {
		t.Fatalf("helper's mcp key = %v", denied)
	}
	// Every other key is at the sentinel.
	for _, tok := range append([]string{modelToken}, foreign...) {
		if denied := gw.denyList(t, tok); len(denied) != 1 || denied[0] != "no-mcp-servers" {
			t.Fatalf("%s must be swept to the sentinel, got %v", tok, denied)
		}
	}
	// The report names the sweeps.
	for _, want := range []string{"helper swept", "ui-key", "mcptool-ghost"} {
		if !strings.Contains(drift, want) {
			t.Fatalf("drift report must name %s, got:\n%s", want, drift)
		}
	}
	// The model key was swept (the fake's records hold the sentinel now).
	if denied := gw.denyList(t, modelToken); len(denied) != 1 || denied[0] != "no-mcp-servers" {
		t.Fatalf("the model key must be swept to the sentinel, got %v", denied)
	}
	// Drift on a LIVE agent's mcptool key lands in the sweep's report (the
	// overwrite is never silent).
	gw.mu.Lock()
	for _, rec := range gw.byToken {
		if rec.alias == "mcptool-helper" {
			rec.perm = map[string]interface{}{"mcp_servers": []string{"evil"}}
		}
	}
	gw.mu.Unlock()
	driftWanted, err := spec.sweepLitellmMCPAccess(store)
	if err != nil {
		t.Fatalf("drift sweep: %v", err)
	}
	if !strings.Contains(driftWanted, "mcptool-helper rewritten") {
		t.Fatalf("a drifted agent MCP key must be reported, got:\n%s", driftWanted)
	}
	// A second sweep is quiet (idempotent).
	drift2, err := spec.sweepLitellmMCPAccess(store)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if strings.Contains(drift2, "swept") {
		t.Fatalf("a converged sweep must be quiet, got:\n%s", drift2)
	}
}

// TestStageLitellmMCPWired: the stage registers + sweeps in one call and
// skips cleanly on a world without litellm.
func TestStageLitellmMCPWired(t *testing.T) {
	// No litellm: a clean no-op (the store is never read).
	spec := &Spec{StateDir: t.TempDir()}
	noStore, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := spec.stageLitellmMCP(noStore); err != nil || rep != "" {
		t.Fatalf("no-litellm stage must no-op, got %q, %v", rep, err)
	}

	stateDir := t.TempDir() + "/agent-tools"
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	store := seededMCPStore(t, stateDir)
	seedAgents(t, store, "Helper")
	gw := newMCPKeyGateway()
	srv := httptest.NewServer(gw.handler(t))
	defer srv.Close()
	spec = &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}
	rep, err := spec.stageLitellmMCP(store)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if !strings.Contains(rep, "1 MCP servers ensured") {
		t.Fatalf("stage report = %q", rep)
	}
}

// TestRenameMCPKeyAliasFollowsAgent: a rename moves the store record AND
// rewrites the gateway's key_alias — the fleet sweep classifies by alias, so
// a stale alias would read as an orphaned key and strip the renamed agent's
// MCP grants on every build.
func TestRenameMCPKeyAliasFollowsAgent(t *testing.T) {
	dir := t.TempDir()
	stateDir := dir + "/agent-tools"
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	store := seededMCPStore(t, stateDir)
	seedAgents(t, store, "Helper")
	gw := newMCPKeyGateway()
	srv := httptest.NewServer(gw.handler(t))
	defer srv.Close()
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}

	token, _, err := spec.ensureAgentMCPKey("Helper", store)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := spec.moveLitellmKeyRecord("helper", "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// The store record moved...
	if env := storeEnvForTest(t, stateDir); env[mcptoolKeyEnvKey("helper")] != "" || env[mcptoolKeyEnvKey("renamed")] != token {
		t.Fatalf("the store record must move with the agent")
	}
	// ...and the gateway alias was rewritten to the new pod (found via a
	// /key/info lookup by the token).
	_, perm, err := gw.keyInfoFor(token)
	if err != nil {
		t.Fatalf("info after rename: %v", err)
	}
	if perm == nil {
		t.Fatalf("the renamed key must stay live")
	}
	gw.mu.Lock()
	alias := ""
	for _, rec := range gw.byToken {
		if rec.token == token {
			alias = rec.alias
		}
	}
	gw.mu.Unlock()
	if alias != "mcptool-renamed" {
		t.Fatalf("gateway alias = %q, want mcptool-renamed", alias)
	}
}

// TestSweepConvergesUntrackedDuplicate: a re-mint that hit a transient
// gateway error leaves the OLD key live on the gateway under the same
// mcptool-<pod> alias, untracked by the store. The sweep must converge it
// to the agent's CURRENT grants by hash — a stale grant set must never stay
// live just because it is not the store's token.
func TestSweepConvergesUntrackedDuplicate(t *testing.T) {
	dir := t.TempDir()
	stateDir := dir + "/agent-tools"
	litellmFixture(t, stateDir, "master-key", "provider-key", "openai", "gpt-4o")
	store := seededMCPStore(t, stateDir)
	seedAgents(t, store, "Helper")
	gw := newMCPKeyGateway()
	srv := httptest.NewServer(gw.handler(t))
	defer srv.Close()
	spec := &Spec{StateDir: stateDir, LitellmBaseURL: srv.URL + "/v1"}

	token, _, err := spec.ensureAgentMCPKey("Helper", store)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// The untracked duplicate: a second key with the SAME alias, holding a
	// stale (wider) grant set, whose token the store does not know.
	gw.mu.Lock()
	gw.keys++
	dup := &keyRecord{
		token: fmt.Sprintf("sk-dup-%d", gw.keys), hash: fmt.Sprintf("%064x", gw.keys),
		alias: "mcptool-helper", perm: map[string]interface{}{"mcp_servers": []string{"evil"}},
	}
	gw.byToken[dup.token] = dup
	gw.byHash[dup.hash] = dup
	gw.mu.Unlock()

	if _, err := spec.sweepLitellmMCPAccess(store); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// The duplicate converged to the state's grants (it was rewritten by
	// hash, not skipped because its alias belongs to a live agent).
	if denied := gw.denyList(t, dup.token); len(denied) != 1 || denied[0] != "google_sheets_a_ro" {
		t.Fatalf("the untracked duplicate must converge to the state's grants, got %v", denied)
	}
	// The tracked key is untouched in its grants.
	if denied := gw.denyList(t, token); len(denied) != 1 || denied[0] != "google_sheets_a_ro" {
		t.Fatalf("the tracked key must hold the state's grants, got %v", denied)
	}
}
