package agenttools

import (
	"encoding/json"
	"io"
	"net/http"

	"freehold/orchestrator/internal/agent"
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
	_ = caller // audit use later

	switch req.Method {
	case "tools/call":
		s.dispatch(w, req.ID, req.Params)
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
			"name": "create_agent", "description": "Create a new conversational agent (name + one-line purpose). Returns the new agent's pubkey.",
			"inputSchema": i(map[string]interface{}{
				"name":    map[string]interface{}{"type": "string"},
				"purpose": map[string]interface{}{"type": "string"},
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
	}
}

type createAgentArgs struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
}
type grantAgentArgs struct {
	Runner  string   `json:"runner"`
	Pubkeys []string `json:"pubkeys"`
}
type manageAgentArgs struct {
	Remove string `json:"remove"`
}

func (s *Server) dispatch(w http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
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
	switch call.Name {
	case "create_agent":
		var a createAgentArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "create_agent arguments: "+err.Error())
			return
		}
		pub, err := s.Tools.CreateAgent(a.Name, a.Purpose)
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
