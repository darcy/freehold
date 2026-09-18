package agenttools

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"freehold/control-plane/api/agent"
)

// Server is the freehold-agent-tools MCP server: JSON-RPC 2.0 over HTTP POST
// at /mcp (the same protocol shape the runner serves). tools/call is
// authorized with the shared signed-header scheme; each tool handler calls an
// in-process agent.Tools method (direct call, not a proxy, not exec).
type Server struct {
	// Audience is this server's own pubkey — the caller's signature binds to
	// it, closing cross-audience replay, exactly like a runner's pubkey.
	Audience string
	// Grants returns the CURRENT whitelist of caller pubkeys allowed to call
	// this server, read fresh for every tools/call. It is the server's own
	// relay roster (39002 channel membership), read live per call and
	// fail-closed on relay error — the same grant model a runner uses. Seeded
	// at bootstrap with the operator/build identity, and revocable without a
	// restart (revocation is a roster change, not a process state change).
	Grants func() ([]string, error)

	// Tools holds the bound agent-management actions (Console + the deploy
	// path). create_agent / grant_agent / manage_agent dispatch here.
	Tools *agent.Tools

	// Facts is the durable world-facts store (plane/certs/domains registered at
	// build). nil = the world-facts surface is unregistered.
	Facts *FactsStore

	// IsAgent reports whether a caller pubkey is a REGISTRY agent (a row in
	// the CP's agent registry). Scope rule: registry agents get the
	// create/grant/manage toolset only; operator callers (roster members NOT
	// in the registry — the admin/seed grants minted at bootstrap) get the
	// full toolset including the world_* actions. Keyed on the registry, read
	// fresh per call, so a revoked registry row loses world access on the
	// next request. nil = everyone is an operator (no registry filtering).
	IsAgent func(callerPubkey string) bool
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	raw := string(body)

	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		s.rpcError(w, req.ID, -32700, "parse error")
		return
	}
	if req.Method == "initialize" {
		if req.Params == nil {
			s.rpcError(w, req.ID, -32602, "initialize requires params")
			return
		}
		s.rpcResult(w, req.ID, map[string]interface{}{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "freehold-agent-tools", "version": "0.1.0"},
		})
		return
	}
	// tools/list is public (listing tool names reveals nothing), like the runner.
	if req.Method == "tools/list" {
		s.rpcResult(w, req.ID, map[string]interface{}{"tools": s.toolList()})
		return
	}

	// Everything else is authorized with the shared signed-header scheme. The
	// whitelist is this server's roster, read fresh per call (a revocation is
	// a relay roster change and lands on the very next request); a relay
	// outage fails closed (empty grants => deny all).
	grants, gerr := s.Grants()
	if gerr != nil {
		s.rpcError(w, req.ID, -32001, "authorization unreadable (roster): "+gerr.Error())
		return
	}
	caller, aerr := VerifyRequest(grants, s.Audience,
		r.Header.Get(PubkeyHeader), r.Header.Get(SigHeader), r.Header.Get(TSHeader), raw)
	if aerr != nil {
		s.rpcError(w, req.ID, -32001, aerr.Error())
		return
	}

	switch req.Method {
	case "tools/call":
		s.dispatch(w, req.ID, req.Params, caller)
	default:
		s.rpcError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// toolList is the semantic toolset — create/grant/manage. Deliberately NOT
// exec: exec stays the runner's funnel.
func (s *Server) toolList() []map[string]interface{} {
	i := func(props map[string]interface{}, req []string) map[string]interface{} {
		return map[string]interface{}{"type": "object", "properties": props, "required": req}
	}
	return []map[string]interface{}{
		{
			"name": "create_agent", "description": "Create a new conversational agent (name + one-line purpose + the channel(s) to add it to; each channel is created if it doesn't exist, the operator is added, and the CPA is added to every channel). Returns the new agent's pubkey.",
			"inputSchema": i(map[string]interface{}{
				"name":     map[string]interface{}{"type": "string"},
				"purpose":  map[string]interface{}{"type": "string"},
				"channel":  map[string]interface{}{"type": "string"},
				"channels": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				"private":  map[string]interface{}{"type": "boolean"},
			}, []string{"name"}),
		},
		{
			"name": "grant_agent", "description": "Bind agent pubkeys to a runner's whitelist.",
			"inputSchema": i(map[string]interface{}{
				"runner":  map[string]interface{}{"type": "string"},
				"pubkeys": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			}, []string{"runner", "pubkeys"}),
		},
		{
			"name": "manage_agent", "description": "List registered agents, or (remove=<name>) drop one's registry row.",
			"inputSchema": i(map[string]interface{}{
				"remove": map[string]interface{}{"type": "string"},
			}, []string{}),
		},
		{
			"name": "world_status", "description": "What the CP currently manages (its agent registry) — the box's post-login trigger surface.",
			"inputSchema": i(map[string]interface{}{}, []string{}),
		},
		{
			"name": "world_teardown", "description": "Clear the CP's managed agent registry (roster-gated world teardown).",
			"inputSchema": i(map[string]interface{}{}, []string{}),
		},
		{
			"name": "world_migrate", "description": "Run pending CP migrations (verify-gated: a migration is done only when its postcondition verifies, 🟢/🔴).",
			"inputSchema": i(map[string]interface{}{}, []string{}),
		},
		{
			"name": "world_build", "description": "Run the CP-owned world-build/reconcile stages through the CP's co-located runner (the box's login + trigger).",
			"inputSchema": i(map[string]interface{}{}, []string{}),
		},
		{
			"name": "world_exec", "description": "Run a command through the CP's co-located runner (operator-scoped drive-through-CP exec, so a thin login box has the build box's full operational surface).",
			"inputSchema": i(map[string]interface{}{
				"target":    map[string]interface{}{"type": "string"},
				"cmd":       map[string]interface{}{"type": "string"},
				"timeout_s": map[string]interface{}{"type": "integer"},
				"secrets":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			}, []string{"cmd"}),
		},
		{
			"name": "world_authorize_door", "description": "Append an operator box's public door key to the host door through the co-located runner (DOOR_SPEC).",
			"inputSchema": i(map[string]interface{}{
				"pubkey": map[string]interface{}{"type": "string"},
			}, []string{"pubkey"}),
		},
		{
			"name": "world_revoke_door", "description": "Remove an operator box's public door key from the host door.",
			"inputSchema": i(map[string]interface{}{
				"pubkey": map[string]interface{}{"type": "string"},
			}, []string{"pubkey"}),
		},
		{
			"name": "world_register_facts", "description": "Register the deployer-side world facts (plane/storage layout, canonical domains, cert metadata) so a management box renders DATA/Certs from the CP.",
			"inputSchema": i(map[string]interface{}{
				"facts": map[string]interface{}{"type": "object"},
			}, []string{"facts"}),
		},
	}
}

type createAgentArgs struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	// Channel is the single-channel form (the CPA's toolset); Channels is the
	// multi-channel form (a department joins #freehold + its own #<name>). Both
	// are accepted; Channel is prepended to Channels when both are present.
	Channel  string   `json:"channel"`
	Channels []string `json:"channels"`
	// Private makes an explicitly created channel visibility=private (the
	// per-department channels). The default freehold channel is always open.
	Private bool `json:"private"`
}
type grantAgentArgs struct {
	Runner  string   `json:"runner"`
	Pubkeys []string `json:"pubkeys"`
}
type manageAgentArgs struct {
	Remove string `json:"remove"`
}

// isWorldTool reports whether a tool is an operator-scoped action: granting an
// agent onto a runner's whitelist, the world_* actions, and the door
// authorize/revoke all mutate what the operator owns. grant_agent is
// operator-only because a grant hands direct exec access to a runner's MCP
// surface — letting a prompt-reachable agent (e.g. the CPA) bind an arbitrary
// pubkey onto an arbitrary runner (incl. the CP's own co-located runner) would
// bypass this very boundary. The CPA's agent toolset is create + manage only.
//
// This is also the enforcement point for the two-tier agent org: a raw
// capability grant (proxy/backup/compute/model) attaches to the department
// identity that owns it, never to a custom agent. Since only an operator can
// grant, and a registry agent is denied here (-32003), a custom agent cannot
// self-serve a second, ungoverned path to a department-owned capability.
func isWorldTool(name string) bool {
	switch name {
	case "grant_agent", "world_status", "world_teardown", "world_migrate", "world_build",
		"world_exec", "world_authorize_door", "world_revoke_door", "world_register_facts":
		return true
	}
	return false
}

func (s *Server) dispatch(w http.ResponseWriter, id json.RawMessage, params json.RawMessage, caller string) {
	if s.Tools == nil {
		s.rpcError(w, id, -32002, "freehold-agent-tools not bound (no agent actions)")
		return
	}
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		s.rpcError(w, id, -32602, "tools/call requires name")
		return
	}
	// Scope auth (per-channel tool visibility): the world_* actions and
	// grant_agent mutate what the operator owns — registry AGENTS are excluded
	// from them (conversation + create/manage only); only OPERATOR callers
	// (roster members not in the registry) drive them. The CPA's stdio bridge
	// already filters to create/manage; this is the same boundary enforced
	// server-side so it cannot be bypassed by calling the server directly.
	if isWorldTool(call.Name) && s.IsAgent != nil && s.IsAgent(caller) {
		s.rpcError(w, id, -32003, "unauthorized: registry agents cannot call "+call.Name+" (operator-scoped)")
		return
	}
	switch call.Name {
	case "create_agent":
		var a createAgentArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "create_agent arguments: "+err.Error())
			return
		}
		channels := append([]string(nil), a.Channels...)
		if strings.TrimSpace(a.Channel) != "" {
			channels = append([]string{a.Channel}, channels...)
		}
		pub, err := s.Tools.CreateAgent(a.Name, a.Purpose, channels, a.Private)
		// Persist the purpose + the full channel list/private flag on the created
		// agent's registry row so a rebuild reconciler recreates its system prompt
		// verbatim and rejoins every channel (not just the primary) with the
		// right visibility. Registry-only (the console client has no such row).
		if err == nil {
			if reg, ok := s.Tools.Console.(*Registry); ok {
				_ = reg.SetPurpose(a.Name, a.Purpose)
				_ = reg.SetChannels(a.Name, channels, a.Private)
			}
		}
		s.textResult(w, id, err, pub)
	case "grant_agent":
		var a grantAgentArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "grant_agent arguments: "+err.Error())
			return
		}
		err := s.Tools.GrantAgent(a.Runner, a.Pubkeys)
		s.textResult(w, id, err, "granted")
	case "manage_agent":
		var a manageAgentArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "manage_agent arguments: "+err.Error())
			return
		}
		agents, err := s.Tools.ManageAgent(a.Remove)
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		b, _ := json.Marshal(agents)
		s.textResult(w, id, nil, string(b))
	case "world_status":
		out, err := s.Tools.WorldStatus()
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		out["cp_pubkey"] = s.Audience
		b, _ := json.Marshal(out)
		s.textResult(w, id, nil, string(b))
	case "world_teardown":
		n, err := s.Tools.WorldTeardown()
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, fmt.Sprintf("removed %d agent(s)", n))
	case "world_migrate":
		res, err := s.Tools.WorldMigrate()
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		b, _ := json.Marshal(res)
		s.textResult(w, id, nil, string(b))
	case "world_build":
		out, err := s.Tools.WorldBuild()
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, out)
	case "world_exec":
		var a struct {
			Target   string   `json:"target"`
			Cmd      string   `json:"cmd"`
			TimeoutS uint64   `json:"timeout_s"`
			Secrets  []string `json:"secrets"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || a.Cmd == "" {
			s.rpcError(w, id, -32602, "world_exec arguments: cmd required")
			return
		}
		out, err := s.Tools.WorldExec(a.Target, a.Cmd, a.TimeoutS, a.Secrets...)
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, out)
	case "world_authorize_door":
		var a struct {
			Pubkey string `json:"pubkey"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || a.Pubkey == "" {
			s.rpcError(w, id, -32602, "world_authorize_door arguments: pubkey required")
			return
		}
		if err := s.Tools.AuthorizeDoor(a.Pubkey); err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, "door key authorized")
	case "world_revoke_door":
		var a struct {
			Pubkey string `json:"pubkey"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || a.Pubkey == "" {
			s.rpcError(w, id, -32602, "world_revoke_door arguments: pubkey required")
			return
		}
		if err := s.Tools.RevokeDoor(a.Pubkey); err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, "door key revoked")
	case "world_register_facts":
		var a struct {
			Facts json.RawMessage `json:"facts"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || len(a.Facts) == 0 {
			s.rpcError(w, id, -32602, "world_register_facts arguments: facts required")
			return
		}
		if s.Facts == nil {
			s.textResult(w, id, fmt.Errorf("world-register-facts: no facts store bound"), "")
			return
		}
		var facts WorldFacts
		if err := json.Unmarshal(a.Facts, &facts); err != nil {
			s.textResult(w, id, fmt.Errorf("world-register-facts: bad facts: %w", err), "")
			return
		}
		if err := s.Facts.Register(facts); err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, "world facts registered")
	default:
		s.rpcError(w, id, -32601, "unknown tool: "+call.Name)
	}
}

func (s *Server) textResult(w http.ResponseWriter, id json.RawMessage, err error, text string) {
	if err != nil {
		s.rpcError(w, id, -32000, err.Error())
		return
	}
	s.rpcResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
	})
}

func (s *Server) rpcResult(w http.ResponseWriter, id json.RawMessage, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *Server) rpcError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]interface{}{"code": code, "message": msg},
	})
}
