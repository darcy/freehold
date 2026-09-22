// Package console reproduces the the console client port (the console client/src/
// lib.rs) — a NIP-98-login HTTP client for the control-plane web console.
package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freehold/contract/config"
	"freehold/contract/version"
	"freehold/contract/wire"
)

// SESSION_COOKIE is the console session cookie name.
const SESSION_COOKIE = "fh_session"

// KindAuth is NIP-98 HTTP auth kind.
const kindAuth = 27235

// Overview mirrors the console client Overview.
type Overview struct {
	ConsolePubkey string   `json:"console_pubkey"`
	Runners       []Runner `json:"runners"`
}

// Runner mirrors the console client Runner.
type Runner struct {
	Name        string      `json:"name"`
	Status      string      `json:"status"`
	NostrPubkey string      `json:"nostr_pubkey"`
	EncPubkey   *string     `json:"enc_pubkey"`
	McpAddr     *string     `json:"mcp_addr"`
	Risk        *string     `json:"risk"`
	Secret      *SecretInfo `json:"secret"`
	Grants      []string    `json:"grants"`
	Readiness   interface{} `json:"readiness,omitempty"`
}

// SecretInfo mirrors the console client SecretInfo.
type SecretInfo struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Address   string `json:"address"`
	RotatedAt *int64 `json:"rotated_at,omitempty"`
	CreatedAt *int64 `json:"created_at,omitempty"`
}

// AgentInfo mirrors the console client AgentInfo.
type AgentInfo struct {
	Name      string  `json:"name"`
	Pubkey    string  `json:"pubkey"`
	CreatedAt uint64  `json:"created_at"`
	Available *bool   `json:"available,omitempty"`
	Note      *string `json:"note,omitempty"`
	// Purpose is the agent's one-line purpose, preserved by freehold-agent-tools'
	// local registry so a rebuild reconciler can recreate the agent's system
	// prompt verbatim. The console API does not carry it; the local registry row
	// does. omitempty keeps it out of the console client serializations upstream.
	Purpose string `json:"purpose,omitempty"`
	// Channel is the channel NAME the agent was created into (empty = the
	// default freehold channel), preserved by freehold-agent-tools' local
	// registry so a rebuild reconciler rejoins the same channel.
	Channel string `json:"channel,omitempty"`
	// Channels is the FULL channel list the agent was created into (a
	// multi-channel create, e.g. a department's #freehold + its own private
	// #<name>), preserved by the local registry so a rebuild rejoins every
	// channel, not just the primary. Empty for rows written before this carried
	// it (reconcile then falls back to Channel). The console API does not carry
	// it.
	Channels []string `json:"channels,omitempty"`
	// Private records that the agent's own channel(s) were created
	// visibility=private, so a rebuild recreates them private rather than open.
	Private bool `json:"private,omitempty"`
}

// ProvisionReq mirrors the console client ProvisionReq.
type ProvisionReq struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Address   string  `json:"address"`
	Secret    string  `json:"secret"`
	RunnerDir *string `json:"runner_dir,omitempty"`
	Risk      *string `json:"risk,omitempty"`
}

// SecretReq mirrors the console client SecretReq.
type SecretReq struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

// GrantReq mirrors the console client GrantReq.
type GrantReq struct {
	Name   string `json:"name"`
	Pubkey string `json:"pubkey"`
}

// Client is a logged-in (or not) connection to one console.
type Client struct {
	hc     *http.Client
	base   string
	cookie string
	pubkey string
}

// NormalizeCookie reduces a Set-Cookie header value (or bare token) down to
// `fh_session=<token>`.
func NormalizeCookie(raw string) string {
	first := strings.TrimSpace(strings.SplitN(raw, ";", 2)[0])
	if strings.HasPrefix(first, SESSION_COOKIE) {
		return first
	}
	return SESSION_COOKIE + "=" + first
}

// Login performs the NIP-98 login: challenge -> sign nonce -> session cookie.
func Login(base string, secret []byte, timeout time.Duration) (*Client, error) {
	hc := &http.Client{Timeout: timeout}
	base = strings.TrimSuffix(base, "/")

	// Challenge.
	chResp, err := hc.Get(base + "/api/auth/challenge")
	if err != nil {
		return nil, fmt.Errorf("GET %s/api/auth/challenge — console reachable? %v", base, err)
	}
	chBody, _ := io.ReadAll(chResp.Body)
	chResp.Body.Close()
	if chResp.StatusCode != 200 {
		return nil, fmt.Errorf("challenge refused (HTTP %d): %s", chResp.StatusCode, chBody)
	}
	var ch map[string]interface{}
	if err := json.Unmarshal(chBody, &ch); err != nil || ch["nonce"] == nil {
		return nil, fmt.Errorf("challenge response missing nonce: %s", chBody)
	}
	nonce, _ := ch["nonce"].(string)

	// NIP-98 login event (kind 27235, content = nonce, u = base, method=login).
	ts := time.Now().Unix()
	tags := [][]string{{"u", base}, {"method", "login"}}
	pubkey, _, sig, err := wire.SignEvent(secret, kindAuth, ts, tags, nonce)
	if err != nil {
		return nil, fmt.Errorf("nip98 signing: %v", err)
	}
	body := map[string]interface{}{
		"nonce": nonce, "pubkey": pubkey, "created_at": ts, "tags": tags, "sig": sig,
	}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, base+"/api/auth/login", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	loginResp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /api/auth/login — %v", err)
	}
	loginBody, _ := io.ReadAll(loginResp.Body)
	loginResp.Body.Close()
	if loginResp.StatusCode != 200 {
		return nil, fmt.Errorf("login refused (HTTP %d): %s", loginResp.StatusCode, loginBody)
	}
	rawCookie := loginResp.Header.Get("Set-Cookie")
	if rawCookie == "" {
		return nil, fmt.Errorf("login response carried no session cookie")
	}
	return &Client{hc: hc, base: base, cookie: NormalizeCookie(rawCookie), pubkey: pubkey}, nil
}

// WithCookie attaches an existing session (e.g. the cookie console-login
// printed) without re-signing. `cookie` may be a full Set-Cookie or bare
// `fh_session=<token>`.
func WithCookie(base, cookie string) *Client {
	return &Client{
		hc:     &http.Client{Timeout: 15 * time.Second},
		base:   strings.TrimSuffix(base, "/"),
		cookie: NormalizeCookie(cookie),
	}
}

// Base returns the console base URL.
func (c *Client) Base() string { return c.base }

// Pubkey returns the operator pubkey this session was issued to (login only).
func (c *Client) Pubkey() string { return c.pubkey }

// Cookie returns the session cookie.
func (c *Client) Cookie() string { return c.cookie }

// request performs an authed HTTP call with the given method/path/body and
// returns the parsed JSON (or empty object for empty body). Non-2xx returns an
// Api error with the server's {"error": "..."} message.
func (c *Client) request(method, path string, body interface{}) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s — %v", method, path, err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s -> HTTP %d: %s", method, path, resp.StatusCode, errorMessage(string(text)))
	}
	if len(strings.TrimSpace(string(text))) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var v json.RawMessage
	if err := json.Unmarshal(text, &v); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return v, nil
}

// WorldSummary is the connection/desire-profile slice a fresh operator box
// seeds from the CP after `freehold login` (the root-free recovery path): the
// box learns where the relay is and who the relay/CP trust without anything
// that lived only on the lost box.
type WorldSummary struct {
	RelayURL    string `json:"relay_url"`
	RelayWsURL  string `json:"relay_ws_url,omitempty"`
	RelayPubkey string `json:"relay_pubkey,omitempty"`
	RelayHost   string `json:"relay_host,omitempty"`
	CPURL       string `json:"cp_url"`
	CPPubkey    string `json:"cp_pubkey"`
	// ConsoleEncPubkey is the console identity's X25519 encryption public key
	// (64-hex). A build box seals the CP-owned secrets (DNS creds, litellm) to
	// it, so it must be reachable over the public API — a thin box has no runner
	// to exec into the CP to read it.
	ConsoleEncPubkey string         `json:"console_enc_pubkey,omitempty"`
	OperatorPubkey   string         `json:"operator_pubkey,omitempty"`
	AgentToolsURL    string         `json:"agent_tools_url,omitempty"`
	AgentToolsPubkey string         `json:"agent_tools_pubkey,omitempty"`
	Services         []WorldService `json:"services,omitempty"`
	// Agents holds the authoritative CP agent registry served on /api/world;
	// Facts holds the world facts (plane/certs/domains) as the raw object a
	// box renders for DATA/Certs. Kept as raw so contract stays free of the
	// control-plane agenttools types.
	Agents  []AgentInfo     `json:"agents,omitempty"`
	Runners []StatusRunner  `json:"runners,omitempty"`
	DNS     []StatusDNS     `json:"dns,omitempty"`
	Facts   json.RawMessage `json:"facts,omitempty"`
	// Version is the CP's stamped world version identity (version.json).
	Version version.Pin `json:"version"`
	// MigrationsPending is the count of migration scripts without a completion
	// marker on the CP (/api/world).
	MigrationsPending int `json:"migrations_pending,omitempty"`
}

// StatusRunner is one CP runner line in the /api/world inventory.
type StatusRunner struct {
	Name        string `json:"name,omitempty"`
	NostrPubkey string `json:"nostr_pubkey,omitempty"`
	McpAddr     string `json:"mcp_addr,omitempty"`
}

// StatusDNS is one CP DNS record in the /api/world inventory.
type StatusDNS struct {
	Name string `json:"name,omitempty"`
	IP   string `json:"ip,omitempty"`
}

// RunnersList returns the runners in the world inventory (may be empty when a
// CP hasn't folded the inventory into /api/world).
func (w *WorldSummary) RunnersList() []StatusRunner {
	if w == nil {
		return nil
	}
	return w.Runners
}

// DNSRecords returns the DNS records in the world inventory (may be empty).
func (w *WorldSummary) DNSRecords() []StatusDNS {
	if w == nil {
		return nil
	}
	return w.DNS
}

// WorldService is the /api/world world-health row the CP serves: the recorded
// coords plus the co-located live-probe result, so a logging-in management box
// renders the live world instead of only the box that deployed it.
type WorldService struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Up     bool   `json:"up"`
	Detail string `json:"detail"`
}

// World fetches the CP's world summary for a fresh-box login seed.
func (c *Client) World() (*WorldSummary, error) {
	raw, err := c.request(http.MethodGet, "/api/world", nil)
	if err != nil {
		return nil, err
	}
	var w WorldSummary
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	if w.CPURL == "" {
		w.CPURL = c.base
	}
	return &w, nil
}

// TeardownResult is what /api/teardown removed from the CP's managed scope.
type TeardownResult struct {
	RunnersRemoved int `json:"runners_removed"`
	AgentsRemoved  int `json:"agents_removed"`
	DnsRemoved     int `json:"dns_removed"`
}

// Teardown asks the CP to remove what IT manages (runners + secrets, the agent
// registry, DNS records) before the box destroys the CP itself — the CP-first
// hand-off in `freehold teardown`. Session-gated.
func (c *Client) Teardown() (*TeardownResult, error) {
	raw, err := c.request(http.MethodPost, "/api/teardown", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var v TeardownResult
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Overview fetches the console overview.
func (c *Client) Overview() (*Overview, error) {
	raw, err := c.request(http.MethodGet, "/api/overview", nil)
	if err != nil {
		return nil, err
	}
	var o Overview
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// Provision posts a provision request.
func (c *Client) Provision(req *ProvisionReq) (json.RawMessage, error) {
	return c.request(http.MethodPost, "/api/provision", req)
}

// Rotate posts a secret rotation.
func (c *Client) Rotate(req *SecretReq) (json.RawMessage, error) {
	return c.request(http.MethodPost, "/api/rotate", req)
}

// Revoke revokes a runner by name.
func (c *Client) Revoke(name string) (json.RawMessage, error) {
	return c.request(http.MethodPost, "/api/revoke", map[string]string{"name": name})
}

// Grant grants an agent pubkey to a runner.
func (c *Client) Grant(name, pubkey string) (json.RawMessage, error) {
	return c.request(http.MethodPost, "/api/grant", GrantReq{Name: name, Pubkey: pubkey})
}

// RevokeGrant revokes an agent grant from a runner.
func (c *Client) RevokeGrant(name, pubkey string) (json.RawMessage, error) {
	return c.request(http.MethodPost, "/api/revoke-grant", GrantReq{Name: name, Pubkey: pubkey})
}

// RunnerAddr sets a runner's MCP address.
func (c *Client) RunnerAddr(name, addr string) (json.RawMessage, error) {
	return c.request(http.MethodPost, "/api/runner-addr", map[string]string{"name": name, "addr": addr})
}

// Channel fetches a runner's channel view.
func (c *Client) Channel(name string) (json.RawMessage, error) {
	return c.request(http.MethodGet, "/api/runner/"+name+"/channel", nil)
}

// Agents lists registered agents.
func (c *Client) Agents() ([]AgentInfo, error) {
	raw, err := c.request(http.MethodGet, "/api/agents", nil)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Agents []AgentInfo `json:"agents"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Agents, nil
}

// UnregisterAgent drops an agent's registry row.
func (c *Client) UnregisterAgent(name string) (json.RawMessage, error) {
	return c.request(http.MethodDelete, "/api/agents/"+name, nil)
}

// RegisterAgent registers a named agent (channel = kind-9 presence channel).
func (c *Client) RegisterAgent(name, pubkey, channel string) (json.RawMessage, error) {
	return c.request(http.MethodPost, "/api/agents",
		map[string]string{"name": name, "pubkey": pubkey, "channel": channel})
}

// PortalURL mints a single-use portal token and returns the URL to open in a
// browser.
func (c *Client) PortalURL() (string, error) {
	raw, err := c.request(http.MethodPost, "/api/auth/portal", map[string]interface{}{})
	if err != nil {
		return "", err
	}
	var v struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Token == "" {
		return "", fmt.Errorf("portal response missing token")
	}
	return c.base + "/api/auth/portal/" + v.Token, nil
}

// SecretNames returns the names of CP-owned secrets present (dns-relay, dns-cp,
// litellm) — the idempotent inventory `build` uses to ask the operator only for
// the missing ones ("ask if not there, don't ask if there").
func (c *Client) SecretNames() ([]string, error) {
	raw, err := c.request(http.MethodGet, "/api/secrets", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Secrets []string `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Secrets, nil
}

// PutSecret uploads a sealed secret record (sealed to the console identity) so
// the CP becomes its durable owner. name must be in {dns-relay, dns-cp,
// litellm}; file is the cert.SaveCreds-style record ({provider,sealed,aad}).
func (c *Client) PutSecret(name string, file json.RawMessage) error {
	_, err := c.request(http.MethodPost, "/api/secrets", map[string]interface{}{
		"name": name, "file": file,
	})
	return err
}

// WorldBuildResult is what /api/world-build returned: the stage report plus the
// world coords the CP resolved during the build (relay/cp/k3s vmids + IPs). A
// teardown clears the box's recorded coords, so the build caller must write
// these back — otherwise the next uninstall cannot find the guests it created.
type WorldBuildResult struct {
	Report string        `json:"report"`
	Coords config.Coords `json:"coords"`
}

// WorldBuild triggers the CP-owned world bring-up (/api/world-build) and
// returns the stage report + resolved world coords. The console drives the
// shared cpbuild engine through its co-located runner — the drive-through-CP
// build a thin box uses. The build runs for minutes, so this switches to a
// long client timeout.
func (c *Client) WorldBuild() (*WorldBuildResult, error) {
	c.hc = &http.Client{Timeout: 20 * time.Minute}
	raw, err := c.request(http.MethodPost, "/api/world-build", nil)
	if err != nil {
		return nil, err
	}
	var v WorldBuildResult
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("world-build response: %w", err)
	}
	return &v, nil
}

// WorldTeardownResult is what the CP-owned world-teardown did: the stage report
// plus the CP-managed state it cleared.
type WorldTeardownResult struct {
	Report         string `json:"report"`
	RunnersRemoved int    `json:"runners_removed"`
	AgentsRemoved  int    `json:"agents_removed"`
	DnsRemoved     int    `json:"dns_removed"`
}

// WorldTeardown triggers the CP-owned world teardown (/api/world-teardown) and
// returns the stage report + the CP state it cleared. The console drives the
// shared teardown engine through its co-located runner — the WorldBuild mirror
// a thin (login-only) box uses. The CP LXC is destroyed last (detached), so this
// returns before the console's own container goes. Long client timeout: each
// runner command carries its own multi-minute budget, so the whole request can
// exceed 20 minutes.
func (c *Client) WorldTeardown() (*WorldTeardownResult, error) {
	c.hc = &http.Client{Timeout: 45 * time.Minute}
	raw, err := c.request(http.MethodPost, "/api/world-teardown", nil)
	if err != nil {
		return nil, err
	}
	var v WorldTeardownResult
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("world-teardown response: %w", err)
	}
	return &v, nil
}

// DnsRecord mirrors the console's DNS record row (C0 resolver).
type DnsRecord struct {
	Name      string `json:"name"`
	IP        string `json:"ip"`
	Source    string `json:"source"`
	CreatedAt uint64 `json:"created_at"`
}

// DnsView is the console's DNS surface: records + the rendered addn-hosts.
type DnsView struct {
	DNS       []DnsRecord `json:"dns"`
	AddnHosts string      `json:"addn_hosts"`
}

// ListDNS reads the CP resolver's explicit records (read-only panel).
func (c *Client) ListDNS() (*DnsView, error) {
	raw, err := c.request(http.MethodGet, "/api/dns", nil)
	if err != nil {
		return nil, err
	}
	var v DnsView
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// UpsertDNS registers/updates one explicit record on the CP resolver
// (registration-owned: rebuild calls this after record_lxc; the litellm
// apply registers the gateway name).
func (c *Client) UpsertDNS(name, ip, source string) (json.RawMessage, error) {
	req := map[string]string{"name": name, "ip": ip, "source": source}
	return c.request(http.MethodPost, "/api/dns", req)
}

// RemoveDNS drops a record (missing = ok).
func (c *Client) RemoveDNS(name string) (json.RawMessage, error) {
	return c.request(http.MethodDelete, "/api/dns", map[string]string{"name": name})
}

func errorMessage(text string) string {
	var v struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &v); err == nil && v.Error != "" {
		return v.Error
	}
	return text
}
