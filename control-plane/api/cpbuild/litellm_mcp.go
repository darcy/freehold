package cpbuild

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	provisioner "freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

// The LiteLLM MCP key plane — Phase 2 of the MCP capability plan. LiteLLM's
// MCP Gateway is the broker for the MCP capability plane: every MCP server is
// registered there, and each agent's access rides a DEDICATED MCP-only key
// whose object_permission the CP writes and rewrites. The CP is the grant
// authority; LiteLLM is the enforcement point.
//
// The posture is DEFAULT-DENY (require_key_mcp_access_defined in the
// gateway's config, litellm.tf): a key with no explicit mcp_servers list
// reaches no MCP server. This plane makes that hold for every key:
//   - each registry agent gets mcptool-<pod> — an MCP-ONLY key (models
//     ["no-default-models"]: it can call NO model), whose object_permission
//     is the union of the agent's MCP capability grants (no grants = the
//     no-mcp-servers sentinel);
//   - every OTHER key on the gateway (the model keys the CP minted, UI
//     keys, team keys) is swept to the sentinel — a UI-created key with no
//     list must reach no MCP server;
//   - the agent's own mcptool key is OVERWRITTEN from CP state every build —
//     whoever holds the gateway master key (the AI department's
//     litellm-api-admin door, a UI admin) is a second grant authority, and
//     the drift-overwrite + report is what keeps the CP authoritative.
//
// The broker token (LiteLLM → MCP service auth) rides a static_headers
// Authorization header. NOT auth_type/auth_value: on main-stable that path
// fails "requires a usable upstream credential" even from config.yaml
// (Phase 0 finding 7). The token is CP-generated, sealed into the CP's
// litellm store, shipped to LiteLLM as the static header, and sealed into
// the slot runner's package (AddSecret) so the MCP service reads it from its
// Secret — no human handles it.

const (
	// mcpAccessGroup is the access group every CP-registered MCP server
	// joins — a second net under the per-key lists (allow_all_keys is never
	// set by this plane).
	mcpAccessGroup = "fh-mcp"
)

// mcptoolKeyEnvKey is the CP litellm store's env key holding one agent's
// MCP-only virtual key, keyed by the pod name (the model key's convention).
func mcptoolKeyEnvKey(pod string) string {
	return "mcptool-" + pod
}

// mcpBrokerTokenEnvKey is the CP litellm store's env key holding an MCP
// capability's broker token (LiteLLM → MCP service), keyed by the
// capability's server slug.
func mcpBrokerTokenEnvKey(slug string) string {
	return "mcp-" + slug + "-token"
}

// agentMCPPermissionBody builds the object_permission payload for one
// agent's MCP key from the CP's MCP capability table: every capability whose
// Grantees include the agent contributes its server slug, narrowed to its
// allowed tools when the record pins a subset. No grants = the
// no-mcp-servers sentinel. Pure.
func agentMCPPermissionBody(store *state.StateStore, agentName string) map[string]interface{} {
	slugs := []string{}
	tools := map[string][]string{}
	for _, rec := range store.MCPCapabilityRecords() {
		for _, g := range rec.Grantees {
			if g != agentName {
				continue
			}
			slugs = append(slugs, rec.ServerName)
			if len(rec.Tools) > 0 {
				tools[rec.ServerName] = append([]string(nil), rec.Tools...)
			}
			break
		}
	}
	sort.Strings(slugs)
	if len(slugs) == 0 {
		return map[string]interface{}{"mcp_servers": []string{"no-mcp-servers"}}
	}
	if len(tools) == 0 {
		return map[string]interface{}{"mcp_servers": slugs}
	}
	return map[string]interface{}{"mcp_servers": slugs, "mcp_tool_permissions": tools}
}

// litellmMCPStore is one call's resolved gateway/store coordinates.
type litellmMCPStore struct {
	master string
	origin string
	// env + path + secret: the CP litellm store, writable via save.
	env    map[string]string
	path   string
	secret []byte
}

// openLitellmMCPStore resolves the CP litellm store for the MCP plane. A
// world without litellm returns nil (every caller skips).
func (s *Spec) openLitellmMCPStore() (*litellmMCPStore, error) {
	if s.LitellmBaseURL == "" {
		return nil, nil
	}
	path, secret, env, err := s.litellmStoreEnv()
	if err != nil {
		return nil, err
	}
	if env["master"] == "" {
		return nil, fmt.Errorf("CP litellm store is missing the master key")
	}
	return &litellmMCPStore{
		master: env["master"],
		origin: strings.TrimSuffix(s.LitellmBaseURL, "/v1"),
		env:    env, path: path, secret: secret,
	}, nil
}

// save re-seals the store after a mutation (a minted key or token).
func (ls *litellmMCPStore) save(s *Spec) error {
	if ls.path == "" {
		return nil
	}
	return s.saveLitellmStoreEnv(ls.path, ls.secret, ls.env)
}

// call issues one admin request and reads the response; non-200 is an error
// carrying the body.
func (ls *litellmMCPStore) call(method, path string, body any) ([]byte, error) {
	out, _, err := ls.callStatus(method, path, body)
	return out, err
}

// callStatus is call with the response STATUS surfaced (a 404 is
// distinguished from a transport failure by callers that tolerate it).
func (ls *litellmMCPStore) callStatus(method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, ls.origin+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+ls.master)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return out, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, resp.StatusCode, nil
}

// keyInfo fetches /key/info for one key (token or hash): alias +
// object_permission.
func (ls *litellmMCPStore) keyInfo(key string) (alias string, perm map[string]interface{}, err error) {
	raw, err := ls.call(http.MethodGet, "/key/info?key="+url.QueryEscape(key), nil)
	if err != nil {
		return "", nil, err
	}
	var info struct {
		Info struct {
			KeyAlias         string                 `json:"key_alias"`
			ObjectPermission map[string]interface{} `json:"object_permission"`
		} `json:"info"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", nil, fmt.Errorf("decode /key/info: %w", err)
	}
	return info.Info.KeyAlias, info.Info.ObjectPermission, nil
}

// samePermission compares two object_permission blobs semantically: map key
// order ignored, an explicit null equal to absent.
func samePermission(a, b map[string]interface{}) bool {
	strip := func(m map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{}
		for k, v := range m {
			if v != nil {
				out[k] = v
			}
		}
		return out
	}
	na, _ := json.Marshal(strip(a))
	nb, _ := json.Marshal(strip(b))
	return string(na) == string(nb)
}

// setKeyPermissions overwrites one key's object_permission and returns a
// drift note when the gateway's copy differed (whoever edited it behind the
// CP's back gets named in the stage report).
func (ls *litellmMCPStore) setKeyPermissions(key, alias string, want map[string]interface{}) (string, error) {
	_, have, err := ls.keyInfo(key)
	if err != nil {
		return "", err
	}
	if samePermission(have, want) {
		return "", nil
	}
	if _, err := ls.call(http.MethodPost, "/key/update", map[string]interface{}{
		"key": key, "object_permission": want,
	}); err != nil {
		return "", err
	}
	haveJSON, _ := json.Marshal(have)
	return fmt.Sprintf("%s rewritten (was %s)", alias, trimJSON(string(haveJSON))), nil
}

// ensureAgentMCPKey returns the agent's MCP-only gateway key, minting it on
// first use (models ["no-default-models"] — the key can call NO model — and
// object_permission = the agent's MCP grants from CP state) and OVERWRITING
// the gateway's copy on every call (the drift-overwrite: whoever edited the
// key behind the CP's back is reported, and the CP state wins). The store's
// token is trusted only when the gateway still knows it (a wiped gateway DB
// invalidates minted keys — the re-mint is the heal). A world without
// litellm returns "".
func (s *Spec) ensureAgentMCPKey(name string, store *state.StateStore) (token, drift string, err error) {
	ls, err := s.openLitellmMCPStore()
	if err != nil || ls == nil {
		return "", "", err
	}
	pod := agent.PodName(name)
	envKey := mcptoolKeyEnvKey(pod)
	want := agentMCPPermissionBody(store, name)
	token = ls.env[envKey]
	if token != "" && s.litellmKeyLive(ls.origin, ls.master, token) {
		drift, err = ls.setKeyPermissions(token, "mcptool-"+pod, want)
		if err != nil {
			return "", "", fmt.Errorf("overwrite MCP permissions for %s: %w", name, err)
		}
		return token, drift, nil
	}
	out, err := ls.call(http.MethodPost, "/key/generate", map[string]interface{}{
		"key_alias":         "mcptool-" + pod,
		"models":            []string{"no-default-models"},
		"metadata":          map[string]string{"agent": name, "plane": "mcp"},
		"object_permission": want,
	})
	if err != nil {
		return "", "", fmt.Errorf("mint MCP key for %s: %w", name, err)
	}
	token, err = parseLitellmKey(out)
	if err != nil {
		return "", "", fmt.Errorf("mint MCP key for %s: %w", name, err)
	}
	ls.env[envKey] = token
	if err := ls.save(s); err != nil {
		return "", "", fmt.Errorf("persist %s MCP key: %w", pod, err)
	}
	return token, "", nil
}

// renameMCPKeyAlias normalizes the gateway-side key_alias after an agent
// rename. moveLitellmKeyRecord moves the STORE record (the token follows the
// agent), but the gateway's copy keeps alias mcptool-<oldpod> — and the
// fleet sweep classifies keys BY ALIAS, so a stale alias reads as an
// orphaned key: the sweep would strip the renamed agent's MCP grants on
// every build and report bogus drift. The rename therefore rewrites the
// alias in the same breath; a gateway failure fails the rename loudly (a
// silently stale alias would surface as the sweep's bogus drift instead).
// No record / no gateway / a key never minted = a no-op.
func (s *Spec) renameMCPKeyAlias(newPod string) error {
	ls, err := s.openLitellmMCPStore()
	if err != nil || ls == nil {
		return err
	}
	token := ls.env[mcptoolKeyEnvKey(newPod)]
	if token == "" {
		return nil
	}
	_, status, err := ls.callStatus(http.MethodPost, "/key/update", map[string]interface{}{
		"key": token, "key_alias": "mcptool-" + newPod,
	})
	if err != nil && status != http.StatusNotFound {
		return fmt.Errorf("rename mcptool key alias: %w", err)
	}
	// A 404 (the stored token is already dead on the gateway — a wiped
	// gateway DB between rename and build) is tolerated: there is no alias
	// to normalize — the next ensure re-mints with the CORRECT alias, and
	// the sweep has no stale-alias orphan to misread. Transient failures
	// (5xx, transport) still fail the rename loudly.
	return nil
}

// revokeAgentMCPKey deletes the agent's MCP-only gateway key by its stored
// TOKEN and drops the store record (ensure-revoked: 200 or 404 both clear,
// so a gateway that lost its DB cannot wedge a remove). A world without
// litellm, or a key never minted, is a no-op.
func (s *Spec) revokeAgentMCPKey(name string) error {
	if s.LitellmBaseURL == "" {
		return nil
	}
	pod := agent.PodName(name)
	path, secret, env, err := s.litellmStoreEnv()
	if err != nil {
		return err
	}
	envKey := mcptoolKeyEnvKey(pod)
	token := env[envKey]
	if token == "" {
		return nil
	}
	master := env["master"]
	if master == "" {
		return fmt.Errorf("CP litellm store is missing the master key")
	}
	body, err := json.Marshal(map[string]interface{}{"keys": []string{token}})
	if err != nil {
		return err
	}
	origin := strings.TrimSuffix(s.LitellmBaseURL, "/v1")
	req, err := http.NewRequest(http.MethodPost, origin+"/key/delete", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+master)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("revoke MCP key for %s: %w", name, err)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("revoke MCP key for %s: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	delete(env, envKey)
	return s.saveLitellmStoreEnv(path, secret, env)
}

// brokerTokenFor returns an MCP capability's broker token from the store,
// minting + persisting one on first use. The token authenticates LiteLLM →
// MCP service calls (the static_headers Authorization); the SAME token is
// sealed into the slot runner's package by registerLiteLLMMServer.
func (s *Spec) brokerTokenFor(ls *litellmMCPStore, slug string) (string, error) {
	envKey := mcpBrokerTokenEnvKey(slug)
	if t := ls.env[envKey]; t != "" {
		return t, nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	ls.env[envKey] = token
	if err := ls.save(s); err != nil {
		return "", fmt.Errorf("persist broker token for %s: %w", slug, err)
	}
	return token, nil
}

// registerLiteLLMMServer ensures the gateway carries one MCP capability's
// server registration: idempotent (an existing server_id is PUT to the
// record's shape — the drift-overwrite rule applies to servers too), joined
// to the access group, allowed_tools pinned from the record, the broker
// token as a static Authorization header. A SLOTTED capability's broker
// token is ALSO sealed into the slot runner's package (the MCP service reads
// it from its Secret) — AddSecret re-ships the package root-owned, and the
// slot unit's ExecStartPre heals ownership at its next start.
func (s *Spec) registerLiteLLMMServer(store *state.StateStore, rec state.MCPCapabilityRecord) error {
	if rec.ServerName == "" || rec.URL == "" {
		return fmt.Errorf("mcp capability %q: server_name and url are required", rec.ServerName)
	}
	ls, err := s.openLitellmMCPStore()
	if err != nil || ls == nil {
		return err
	}
	token := ""
	if rec.SecretRef != "" {
		if token, err = s.brokerTokenFor(ls, rec.ServerName); err != nil {
			return err
		}
	}
	registration := map[string]interface{}{
		"server_name": rec.ServerName,
		"server_id":   rec.ServerName,
		"url":         rec.URL,
		"transport":   rec.Transport,
		// One access group for every CP-registered server (a second net under
		// the per-key lists; allow_all_keys is never set by this plane).
		"mcp_access_groups": []string{mcpAccessGroup},
	}
	if len(rec.Tools) > 0 {
		registration["allowed_tools"] = append([]string(nil), rec.Tools...)
	}
	if token != "" {
		registration["static_headers"] = map[string]string{"Authorization": "Bearer " + token}
	}
	exists, err := ls.serverExists(rec.ServerName)
	if err != nil {
		return err
	}
	method, path := http.MethodPost, "/v1/mcp/server"
	if exists {
		method, path = http.MethodPut, "/v1/mcp/server"
	}
	if _, err := ls.call(method, path, registration); err != nil {
		return fmt.Errorf("register MCP server %s: %w", rec.ServerName, err)
	}
	// Seal the broker token into the slot runner's package so the MCP
	// service's Secret can be (re)filled from sealed material.
	if rec.Namespace != "" && rec.SecretRef != "" && token != "" {
		slotRunner := "kube-api-" + rec.Namespace
		if _, ok := store.GetRunner(slotRunner); ok {
			if _, err := provisioner.AddSecret(store, slotRunner, rec.SecretRef, []byte(token)); err != nil {
				return fmt.Errorf("seal broker token into %s: %w", slotRunner, err)
			}
		}
	}
	return nil
}

// serverExists reports whether the gateway already knows a server by id.
func (ls *litellmMCPStore) serverExists(serverID string) (bool, error) {
	raw, err := ls.call(http.MethodGet, "/v1/mcp/server", nil)
	if err != nil {
		return false, err
	}
	var list struct {
		Data []struct {
			ServerID string `json:"server_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err == nil && list.Data != nil {
		for _, s := range list.Data {
			if s.ServerID == serverID {
				return true, nil
			}
		}
		return false, nil
	}
	var plain []struct {
		ServerID string `json:"server_id"`
	}
	if err := json.Unmarshal(raw, &plain); err != nil {
		return false, fmt.Errorf("decode /v1/mcp/server: %w", err)
	}
	for _, s := range plain {
		if s.ServerID == serverID {
			return true, nil
		}
	}
	return false, nil
}

// sweepLitellmMCPAccess enforces default-deny across EVERY key on the
// gateway: each registry agent's mcptool key is ensured (mint-or-overwrite),
// the CP's model keys and every foreign key (UI-created, team keys) are
// swept to the no-mcp-servers sentinel. Returns the drift report (what was
// rewritten); a non-empty report means someone edited key permissions behind
// the CP's back.
func (s *Spec) sweepLitellmMCPAccess(store *state.StateStore) (string, error) {
	ls, err := s.openLitellmMCPStore()
	if err != nil || ls == nil {
		return "", err
	}
	report := []string{}
	// 1. The owned plane: every registry agent's MCP key, mint-or-overwrite.
	//    A drift note (the gateway's copy differed from CP state) is the
	//    plane's headline signal — someone edited a key behind the CP's
	//    back — so it lands in the stage report, never discarded.
	agentPods := map[string]bool{}
	podToAgent := map[string]string{}
	for name := range store.Snapshot().Agents {
		pod := agent.PodName(name)
		agentPods[pod] = true
		podToAgent[pod] = name
		_, drift, err := s.ensureAgentMCPKey(name, store)
		if err != nil {
			return "", err
		}
		if drift != "" {
			report = append(report, drift)
		}
	}
	// 2. The fleet sweep: every key the CP did not just ensure, default-denied.
	raw, err := ls.call(http.MethodGet, "/key/list", nil)
	if err != nil {
		return "", fmt.Errorf("list keys for the MCP sweep: %w", err)
	}
	var list struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", fmt.Errorf("decode /key/list: %w", err)
	}
	for _, hash := range list.Keys {
		alias, perm, err := ls.keyInfo(hash)
		if err != nil {
			// A dead hash (a wiped row mid-sweep) is skippable; a LIVE key
			// that reads back 5xx could be holding widened MCP grants — say
			// so in the report rather than skipping silently (the sweep
			// re-runs next build).
			report = append(report, fmt.Sprintf("key %s… UNREADABLE (%v) — the sweep re-runs next build", hash[:8], err))
			continue
		}
		switch {
		case strings.HasPrefix(alias, "agentkey-"):
			// Store-env-keyed legacy shape (not minted by current code).
			fallthrough
		case agentPods[alias]:
			// The CP's MODEL key: the mint writes gateway key_alias = the
			// BARE pod name (departments.go's ensureAgentLitellmKey) — the
			// agentkey- prefix is only the store env key. MCP access rides
			// mcptool-<pod>, never the model key — sweep it to the sentinel
			// when it holds any grant.
			if note := s.maybeDenyMCP(ls, hash, alias, perm); note != "" {
				report = append(report, note+" (model keys never hold MCP grants)")
			}
		case strings.HasPrefix(alias, "mcptool-"):
			pod := strings.TrimPrefix(alias, "mcptool-")
			if !agentPods[pod] {
				if note := s.maybeDenyMCP(ls, hash, alias, perm); note != "" {
					report = append(report, note+" (no such agent)")
				}
				continue
			}
			// A LIVE agent's key: rewritten to CP state BY HASH, not skipped
			// by alias — a re-mint that hit a transient gateway error leaves
			// the old (still-live) key behind UNTRACKED under the same alias,
			// and step 1 only rewrote the store's token. Overwriting every
			// key of the alias converges the untracked duplicate to the same
			// legit grants instead of leaving a stale grant set live forever.
			want := agentMCPPermissionBody(store, podToAgent[pod])
			if !samePermission(perm, want) {
				if _, err := ls.call(http.MethodPost, "/key/update", map[string]interface{}{
					"key": hash, "object_permission": want,
				}); err != nil {
					return "", fmt.Errorf("overwrite %s: %w", alias, err)
				}
				hadJSON, _ := json.Marshal(perm)
				report = append(report, alias+" rewritten (was "+trimJSON(string(hadJSON))+")")
			}
		default:
			if note := s.maybeDenyMCP(ls, hash, alias, perm); note != "" {
				report = append(report, note+" (foreign key)")
			}
		}
	}
	return strings.Join(report, "\n"), nil
}

// maybeDenyMCP sweeps one key to the sentinel when its object_permission
// holds any MCP grant shape (or names nothing — an absent list is open
// under a lost flag, so it is stamped explicitly too); reports whether a
// write was needed. A write failure is reported to the build log and does
// not fail the sweep (the next build re-runs it).
// maybeDenyMCP sweeps one key to the sentinel when its object_permission
// holds any MCP grant shape (or names nothing — an absent list is open
// under a lost flag, so it is stamped explicitly too). Returns the report
// line: a successful write reports the sweep; a FAILED write reports that
// too (loud, in the stage report — the next build re-runs it), never just
// stderr.
func (s *Spec) maybeDenyMCP(ls *litellmMCPStore, hash, alias string, perm map[string]interface{}) string {
	raw, _ := json.Marshal(perm["mcp_servers"])
	if string(raw) == `["no-mcp-servers"]` {
		return ""
	}
	if _, err := ls.call(http.MethodPost, "/key/update", map[string]interface{}{
		"key":               hash,
		"object_permission": map[string]interface{}{"mcp_servers": []string{"no-mcp-servers"}},
	}); err != nil {
		return fmt.Sprintf("%s deny-write FAILED (the next build re-runs the sweep): %v", alias, err)
	}
	return alias + " swept to no-mcp-servers"
}

// trimJSON keeps a short prefix of a JSON blob for a drift note.
func trimJSON(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// stageLitellmMCP is the build/reconcile stage: register every MCP
// capability's server on the gateway, ensure every registry agent's MCP key
// (mint-or-overwrite), then sweep the fleet default-deny. Idempotent; wired
// beside stageLitellmAliases (before the pods re-apply).
func (s *Spec) stageLitellmMCP(store *state.StateStore) (string, error) {
	if s.LitellmBaseURL == "" {
		return "", nil
	}
	report := []string{}
	records := store.MCPCapabilityRecords()
	for _, rec := range records {
		if err := s.registerLiteLLMMServer(store, rec); err != nil {
			return "", err
		}
	}
	if len(records) > 0 {
		report = append(report, fmt.Sprintf("%d MCP servers ensured", len(records)))
	}
	drift, err := s.sweepLitellmMCPAccess(store)
	if err != nil {
		return "", err
	}
	if drift != "" {
		report = append(report, "MCP DRIFT overwritten:\n"+drift)
	}
	return strings.Join(report, "\n"), nil
}

// provision_mcp — the MCP plane's grant-giving flow, the sibling of
// provision_runner: the operator/CPA asks in conversation ("give Wanda my
// Q4 sheet, read-only"), the CPA calls this, and the capability is
// registered + granted in one auditable step. The guards mirror the runner
// flow's: kebab-case name, the transport/URL/secret shapes per slotted vs
// external, grantees must be registry agents, an operator-origin record is
// not the CPA's to re-provision, and a retired name is refused until an
// operator re-enables it. The credential-blind rule holds: NO secret rides
// the call — the broker token is CP-generated and sealed; the service's own
// Google SA key never enters this flow at all.
//
// The SLOTTED shape's URL is CP-DERIVED — an agent-stated endpoint would
// hand the broker token to a server the agent controls (the kube-slot
// rule). The EXTERNAL shape's URL is the thing being stated, and it carries
// no freehold credential (secret_ref is refused with it).
func (s *Spec) provisionMCP(args agent.ProvisionMCPArgs, callerIsOperator bool, reg *agenttools.Registry, store *state.StateStore) (string, error) {
	name := strings.TrimSpace(args.Name)
	if !runnerNameRe.MatchString(name) {
		return "", fmt.Errorf("provision_mcp: name must be kebab-case [a-z0-9-] (got %q)", args.Name)
	}
	transport := strings.ToLower(strings.TrimSpace(args.Transport))
	if transport == "" {
		transport = "http"
	}
	if transport != "http" && transport != "sse" {
		return "", fmt.Errorf("provision_mcp %s: transport must be \"http\" or \"sse\" (got %q)", name, args.Transport)
	}
	ns := strings.TrimSpace(args.Namespace)
	slotted := ns != ""

	// The retired guard reads FIRST (the mirror of provision_runner's): an
	// agent may not re-mint a capability it was just told to take away.
	if _, retired := store.GetRetiredMCP(name); retired && !callerIsOperator {
		return "", fmt.Errorf("provision_mcp %s: this capability was retired by revoke_capability — an agent may not re-mint a capability it was just told to take away; the operator re-enables the name by provisioning it again", name)
	}

	var rec state.MCPCapabilityRecord
	if prev, ok := store.GetMCPCapability(name); ok {
		// Re-provision: the shape is fixed at first provision (a different
		// shape is a NEW capability, new name); an operator-origin record is
		// not the CPA's to re-provision.
		if !prev.AgentProvisioned() && !callerIsOperator {
			return "", fmt.Errorf("provision_mcp %s: this capability was provisioned by the operator — it is not yours to re-provision", name)
		}
		if prev.Namespace != ns {
			return "", fmt.Errorf("provision_mcp %s: namespace %q is fixed (first provisioned as %q) — a different namespace is a NEW capability (new name)", name, ns, prev.Namespace)
		}
		rec = prev
	} else if !callerIsOperator {
		if _, retired := store.GetRetiredMCP(name); retired {
			return "", fmt.Errorf("provision_mcp %s: this capability was retired — the operator re-enables the name", name)
		}
	}

	// The shape resolution: slotted derives its URL; external states it.
	urlPath := "/mcp"
	if transport == "sse" {
		urlPath = "/sse"
	}
	if slotted {
		if args.URL != "" {
			return "", fmt.Errorf("provision_mcp %s: url is refused for a slotted capability — the CP derives it from the namespace (an agent-stated endpoint would hand the broker token to a server the agent controls)", name)
		}
		if err := validateKubeNS(ns); err != nil {
			return "", fmt.Errorf("provision_mcp %s: %w", name, err)
		}
		if args.ServicePort <= 0 || args.ServicePort > 65535 {
			return "", fmt.Errorf("provision_mcp %s: service_port is required for a slotted capability (the MCP service's port)", name)
		}
		if _, ok := store.GetRunner("kube-api-" + ns); !ok {
			return "", fmt.Errorf("provision_mcp %s: no kube-api-%s slot door — carve the mcp slot first (Compute applies the slot manifest through kube-api-root, then provision the door)", name, ns)
		}
		rec.URL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d%s", name, ns, args.ServicePort, urlPath)
		rec.Namespace = ns
		rec.ServicePort = args.ServicePort
		rec.SecretRef = strings.TrimSpace(args.SecretRef)
	} else {
		if strings.TrimSpace(args.URL) == "" {
			return "", fmt.Errorf("provision_mcp %s: url is required for an external capability (the MCP server's endpoint)", name)
		}
		if strings.TrimSpace(args.SecretRef) != "" {
			return "", fmt.Errorf("provision_mcp %s: secret_ref is a SLOTTED field — an external server carries no freehold-sealed credential", name)
		}
		rec.URL = strings.TrimSpace(args.URL)
		rec.Namespace = ""
		rec.ServicePort = 0
		rec.SecretRef = ""
	}
	rec.ServerName = name
	rec.Transport = transport
	rec.Scope = strings.TrimSpace(args.Scope)
	rec.Tools = dedupNonBlank(args.Tools)
	rec.Origin = state.OriginOperator
	if !callerIsOperator {
		rec.Origin = state.OriginAgent
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = uint64(time.Now().Unix())
	}

	// Resolve EVERY grantee up front (the runner rule: a typo'd name must
	// not leave a live registration + record behind).
	grantTo := dedupNonBlank(args.GrantTo)
	if len(grantTo) == 0 {
		return "", fmt.Errorf("provision_mcp %s: grant_to is required (which agent does this capability belong to?)", name)
	}
	for _, g := range grantTo {
		if _, err := registryRow(reg, g); err != nil {
			return "", fmt.Errorf("provision_mcp %s: %w", name, err)
		}
	}

	// Record BEFORE the side effects so a failure after this point heals on
	// the next build (the record re-registers + re-grants).
	rec.Grantees = append([]string(nil), grantTo...)
	if err := store.InsertMCPCapability(name, rec); err != nil {
		return "", fmt.Errorf("record MCP capability: %w", err)
	}

	if err := s.registerLiteLLMMServer(store, rec); err != nil {
		return "", err
	}

	// Grant each grantee (their MCP key's object_permission is rewritten
	// from CP state — which now includes this capability).
	granted := []string{}
	for _, g := range grantTo {
		if _, _, err := s.ensureAgentMCPKey(g, store); err != nil {
			return "", fmt.Errorf("grant %s → %s: %w", name, g, err)
		}
		granted = append(granted, g)
	}

	report := fmt.Sprintf("mcp capability %s (%s → %s) registered on the gateway and granted to [%s].",
		name, transport, rec.URL, strings.Join(granted, ", "))
	if slotted {
		if rec.SecretRef != "" {
			report += fmt.Sprintf("\nThe MCP service itself deploys into the %s slot (kube-api-%s) — its Secret carries the broker token sealed from the CP (sealed into kube-api-%s's package as %q); the service owner stands the Deployment + Service up (manifests in the capability's record shape).", ns, ns, ns, rec.SecretRef)
		} else {
			report += fmt.Sprintf("\nThe MCP service itself deploys into the %s slot (kube-api-%s); the service owner stands the Deployment + Service up (manifests in the capability's record shape). No broker token was declared for this capability (secret_ref empty) — the service answers unauthenticated on the cluster network.", ns, ns)
		}
	}
	report += "\nVerify with the agent's MCP key (its object_permission now carries " + name + ") before claiming the capability is live."
	return report, nil
}

// grant_capability / revoke_capability — the MCP plane's grant surface. The
// RUNNER plane is refused with routing: a door's roster grants stay
// operator-scoped on their existing audited surfaces (the console's grant
// page, grant_agent, provision_runner's union-on-re-provision) — a second
// grant path for exec seats would duplicate an audited surface for no new
// capability. revoke_capability's runner case DELEGATES to revoke_runner's
// audited path unchanged.
func (s *Spec) grantCapability(args agent.CapabilityGrantArgs, callerIsOperator bool, reg *agenttools.Registry, store *state.StateStore, revoke agent.RevokeRunnerFn) (string, error) {
	name := strings.TrimSpace(args.Name)
	add := dedupNonBlank(args.GrantTo)
	drop := dedupNonBlank(args.RevokeFrom)
	if len(add) == 0 && len(drop) == 0 {
		return "", fmt.Errorf("grant_capability %s: name at least one of grant_to / revoke_from", name)
	}
	if _, isRunner := store.GetCapability(name); isRunner {
		return "", fmt.Errorf("grant_capability %s: a RUNNER door's roster grants stay operator-scoped — use the console's grant page, grant_agent (operator), or re-provision with the widened roster (the CPA's union rule); this tool grants MCP capabilities", name)
	}
	rec, ok := store.GetMCPCapability(name)
	if !ok {
		return "", fmt.Errorf("grant_capability %s: no such capability (neither an MCP capability nor a runner door)", name)
	}
	if !rec.AgentProvisioned() && !callerIsOperator {
		return "", fmt.Errorf("grant_capability %s: this capability was provisioned by the operator — grants onto it stay operator-scoped", name)
	}
	for _, g := range append(append([]string(nil), add...), drop...) {
		if _, err := registryRow(reg, g); err != nil {
			return "", fmt.Errorf("grant_capability %s: %w", name, err)
		}
	}

	grants := map[string]bool{}
	for _, g := range rec.Grantees {
		grants[g] = true
	}
	changes := []string{}
	for _, g := range drop {
		if grants[g] {
			delete(grants, g)
			changes = append(changes, "-"+g)
		} else {
			return "", fmt.Errorf("grant_capability %s: %s holds no grant (the roster is [%s]) — removing a non-grantee is a no-op that must be named, not a silent pass", name, g, strings.Join(rec.Grantees, ", "))
		}
	}
	for _, g := range add {
		if !grants[g] {
			grants[g] = true
			changes = append(changes, "+"+g)
		}
	}
	if len(changes) == 0 {
		return fmt.Sprintf("grant_capability %s: nothing to change (the roster is [%s])", name, strings.Join(rec.Grantees, ", ")), nil
	}
	rec.Grantees = sortedKeys(grants)
	if err := store.InsertMCPCapability(name, rec); err != nil {
		return "", fmt.Errorf("record grant: %w", err)
	}
	// Every CHANGED grantee's MCP key is rewritten from CP state (the
	// removed agent's key drops the capability; the added one gains it).
	affected := map[string]bool{}
	for _, g := range append(append([]string(nil), add...), drop...) {
		affected[g] = true
	}
	for g := range affected {
		if _, _, err := s.ensureAgentMCPKey(g, store); err != nil {
			return "", fmt.Errorf("rewrite %s's MCP key: %w", g, err)
		}
	}
	return fmt.Sprintf("mcp capability %s granted [%s] (roster now [%s]); every changed grantee's MCP key was rewritten from CP state.",
		name, strings.Join(changes, " "), strings.Join(rec.Grantees, ", ")), nil
}

func (s *Spec) revokeCapability(args agent.CapabilityGrantArgs, callerIsOperator bool, reg *agenttools.Registry, store *state.StateStore, revoke agent.RevokeRunnerFn) (string, error) {
	name := strings.TrimSpace(args.Name)
	// The runner plane delegates to revoke_runner's audited path unchanged.
	if _, isRunner := store.GetCapability(name); isRunner {
		return revoke(agent.RetireArgs{Name: name, RevokeFrom: args.RevokeFrom})
	}
	rec, ok := store.GetMCPCapability(name)
	if !ok {
		if _, retired := store.GetRetiredMCP(name); retired {
			return "", fmt.Errorf("revoke_capability %s: already retired — the name stays refused until the operator re-enables it by provisioning the capability again", name)
		}
		return "", fmt.Errorf("revoke_capability %s: no such capability", name)
	}
	if !rec.AgentProvisioned() && !callerIsOperator {
		return "", fmt.Errorf("revoke_capability %s: this capability was provisioned by the operator — revoking it stays operator-scoped", name)
	}

	drop := dedupNonBlank(args.RevokeFrom)
	// Whole retirement: the record drops, the gateway registration is
	// deleted, every grantee's MCP key is re-stamped, and the guard note
	// keeps the name out of the agent surface until an operator re-enables
	// it. The MCP SERVICE deployment is the owner's to remove (reported).
	if len(drop) == 0 {
		if err := s.deleteLiteLLMMServer(rec.ServerName); err != nil {
			return "", err
		}
		formerGrantees := append([]string(nil), rec.Grantees...)
		if err := store.RemoveMCPCapability(name); err != nil {
			return "", err
		}
		if err := store.InsertRetiredMCP(name, state.RetiredCapability{
			RevokedAt: uint64(time.Now().Unix()), RetiredBy: retiredBy(callerIsOperator),
			LastRoster: formerGrantees,
		}); err != nil {
			return "", err
		}
		report := []string{}
		for _, g := range formerGrantees {
			if _, _, err := s.ensureAgentMCPKey(g, store); err != nil {
				return "", fmt.Errorf("re-stamp %s's MCP key: %w", g, err)
			}
			report = append(report, "-"+g)
		}
		return fmt.Sprintf("mcp capability %s RETIRED: the gateway registration is deleted, the grantees' keys re-stamped [%s], the record dropped under a re-enablable guard note. The MCP service's deployment is the owner's to remove — the capability plane no longer reaches it, but a running service with its SA key is a known state, not a silent one.",
			name, strings.Join(report, " ")), nil
	}

	// A from-the-roster removal.
	grants := map[string]bool{}
	for _, g := range rec.Grantees {
		grants[g] = true
	}
	changed := []string{}
	for _, g := range drop {
		if !grants[g] {
			return "", fmt.Errorf("revoke_capability %s: %s holds no grant (the roster is [%s]) — a no-op removal must be named, not a silent pass", name, g, strings.Join(rec.Grantees, ", "))
		}
		delete(grants, g)
		changed = append(changed, "-"+g)
	}
	rec.Grantees = sortedKeys(grants)
	if err := store.InsertMCPCapability(name, rec); err != nil {
		return "", err
	}
	for _, g := range drop {
		if _, _, err := s.ensureAgentMCPKey(g, store); err != nil {
			return "", fmt.Errorf("rewrite %s's MCP key: %w", g, err)
		}
	}
	return fmt.Sprintf("mcp capability %s revoked [%s] (roster now [%s]); every removed grantee's MCP key was rewritten from CP state.",
		name, strings.Join(changed, " "), strings.Join(rec.Grantees, ", ")), nil
}

// renameMCPGranteeState rewrites a renamed agent's NAME across the MCP
// capability table's Grantees: the grants ride the agent NAME, and a rename
// that left them would strip the agent's MCP access at the next build's
// permission rewrite (the new name matches nothing → the sentinel). Called
// by the update_agent rename flow (with the reverse on its rollback path).
func (s *Spec) renameMCPGranteeState(oldName, newName string) error {
	store, err := state.Open(s.consoleStateDir())
	if err != nil {
		return err
	}
	return store.RenameMCPGrantee(oldName, newName)
}

// deleteLiteLLMMServer removes the gateway's registration for a retired
// capability (the server stops serving tools through the broker).
// Ensure-revoked: a 404 (already gone — a retry after a later leg failed, or
// a wiped gateway DB) clears too, so retirement cannot be wedged by a leg
// that already landed.
func (s *Spec) deleteLiteLLMMServer(serverID string) error {
	ls, err := s.openLitellmMCPStore()
	if err != nil || ls == nil {
		return err
	}
	if _, status, err := ls.callStatus(http.MethodDelete, "/v1/mcp/server/"+url.PathEscape(serverID), nil); err != nil && status != http.StatusNotFound {
		return fmt.Errorf("delete MCP server registration %s: %w", serverID, err)
	}
	return nil
}

func retiredBy(callerIsOperator bool) string {
	if callerIsOperator {
		return state.OriginOperator
	}
	return state.OriginAgent
}

// sortedKeys returns the map's keys sorted (the roster convention).
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BuildProvisionMCP binds provision_mcp to a Spec + the agent registry.
func BuildProvisionMCP(spec *Spec, reg *agenttools.Registry) agent.ProvisionMCPFn {
	return func(args agent.ProvisionMCPArgs, callerIsOperator bool) (string, error) {
		store, err := state.Open(spec.consoleStateDir())
		if err != nil {
			return "", fmt.Errorf("open CP state: %w", err)
		}
		return spec.provisionMCP(args, callerIsOperator, reg, store)
	}
}

// BuildGrantCapability binds grant_capability to a Spec + the agent registry.
func BuildGrantCapability(spec *Spec, reg *agenttools.Registry) agent.CapabilityGrantFn {
	return func(args agent.CapabilityGrantArgs, callerIsOperator bool) (string, error) {
		store, err := state.Open(spec.consoleStateDir())
		if err != nil {
			return "", fmt.Errorf("open CP state: %w", err)
		}
		return spec.grantCapability(args, callerIsOperator, reg, store, nil)
	}
}

// BuildRevokeCapability binds revoke_capability to a Spec + the agent
// registry + the runner plane's audited take-away path (the runner case
// delegates to it unchanged).
func BuildRevokeCapability(spec *Spec, reg *agenttools.Registry, revoke agent.RevokeRunnerFn) agent.CapabilityRevokeFn {
	return func(args agent.CapabilityGrantArgs, callerIsOperator bool) (string, error) {
		store, err := state.Open(spec.consoleStateDir())
		if err != nil {
			return "", fmt.Errorf("open CP state: %w", err)
		}
		return spec.revokeCapability(args, callerIsOperator, reg, store, revoke)
	}
}
