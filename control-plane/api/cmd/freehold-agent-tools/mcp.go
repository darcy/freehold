// The `mcp` subcommand: a stdio MCP bridge the CPA pod's harness spawns
// (BUZZ_ACP_MCP_COMMAND). buzz-acp builds exactly ONE stdio MCP server from
// that env, so this binary must aggregate BOTH the Buzz message tools (via a
// spawned buzz-dev-mcp) AND the freehold create/grant/manage tools (forwarded
// to the CP's freehold-agent-tools server over the shared signed-header
// scheme, signed as the agent whose nsec buzz-acp injects as BUZZ_PRIVATE_KEY).
//
// It is NDJSON in / NDJSON out on stdin/stdout, id-matched so unsolicited
// buzz-dev-mcp notifications are relayed without desyncing request/response.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"freehold/contract/crypto"
	"freehold/control-plane/api/agenttools"
)

// freeholdToolDefs are the create/grant/manage tool schemas merged into
// buzz-dev-mcp's tools/list (they mirror internal/agenttools/server.go). When
// the pod holds capability runners (`hasRunner`), the runner's exec/list are
// advertised too — the department's scoped path to its own runners. The CPA
// and custom agents have no runner coords, so they never see exec.
func freeholdToolDefs(hasRunner bool, targets []string) []map[string]interface{} {
	i := func(props map[string]interface{}, req []string) map[string]interface{} {
		return map[string]interface{}{"type": "object", "properties": props, "required": req}
	}
	defs := []map[string]interface{}{
		{"name": "create_agent", "description": "Create a new conversational agent (name + one-line purpose + the channel(s) to add it to; each channel is created if it doesn't exist, the operator is added, and the CPA is added to every channel). Returns the new agent's pubkey.",
			"inputSchema": i(map[string]interface{}{
				"name":     map[string]interface{}{"type": "string"},
				"purpose":  map[string]interface{}{"type": "string"},
				"channel":  map[string]interface{}{"type": "string"},
				"channels": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				"private":  map[string]interface{}{"type": "boolean"},
			}, []string{"name"})},
		{"name": "manage_agent", "description": "List registered agents, or (remove=<name>) drop one's registry row.",
			"inputSchema": i(map[string]interface{}{"remove": map[string]interface{}{"type": "string"}}, []string{})},
		{"name": "provision_runner", "description": "Stage a NEW capability runner on the fly and grant the named agents onto its roster (the grant-giving flow: new capability = new runner, named <target>-<protocol>-<identity>). The tool takes NO credential: kind=ssh mints the runner's own keypair and returns the public key to install on the target; api-class kinds (unifi) ship EMPTY — DM the operator the returned door page link and they fill the credential in the console web UI. Grants land live; the grantees' pods are re-applied with the new coords. Follow the granting skill: confirm with the operator when the ask did not come from them in this thread.",
			"inputSchema": i(map[string]interface{}{
				"name":     map[string]interface{}{"type": "string"},
				"kind":     map[string]interface{}{"type": "string"},
				"address":  map[string]interface{}{"type": "string"},
				"grant_to": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			}, []string{"name", "kind", "address", "grant_to"})},
	}
	if hasRunner {
		execDesc := "Run a shell command VERBATIM on a capability runner's target through the runner-owned connection (the credential is fixed per target by this pod's config). Pass no secrets. Returns {stdout, stderr, exit_code, timed_out}."
		if len(targets) > 1 {
			execDesc += " target: which capability to use — one of [" + strings.Join(targets, ", ") + "]."
		}
		defs = append(defs,
			map[string]interface{}{"name": "exec", "description": execDesc,
				"inputSchema": i(map[string]interface{}{
					"cmd":        map[string]interface{}{"type": "string"},
					"target":     map[string]interface{}{"type": "string"},
					"stream":     map[string]interface{}{"type": "boolean"},
					"session_id": map[string]interface{}{"type": "string"},
					"timeout_s":  map[string]interface{}{"type": "integer"},
				}, []string{"cmd"})},
			map[string]interface{}{"name": "list", "description": "List the capability targets this pod can reach.",
				"inputSchema": i(map[string]interface{}{}, []string{})},
		)
	}
	return defs
}

func isFreeholdTool(name string) bool {
	switch name {
	// grant_agent is deliberately absent: it is operator-scoped (a grant hands
	// direct exec access to a runner), so the CPA's conversation+create-only
	// harness must not advertise or call it. provision_runner IS the CPA's
	// narrow grant-giving carve-out: it stages NEW capability runners only —
	// grants onto the build-time capability runners stay operator-only.
	case "create_agent", "manage_agent", "provision_runner":
		return true
	}
	return false
}

// isRunnerTool reports whether a tool call goes to the pod's capability runner
// (only advertised when runner coords are present).
func isRunnerTool(name string) bool {
	switch name {
	case "exec", "list":
		return true
	}
	return false
}

// signedForward posts one JSON-RPC request to the agent-tools /mcp server with
// the shared signed-header scheme (the agent's nsec signs it) and returns the
// JSON-RPC response body.
func (b *mcpBridge) signedForward(raw []byte, audience string) ([]byte, error) {
	return b.postSignedTo(b.agentToolsURL+"/mcp", raw, audience)
}

// postSignedTo signs `raw` as this agent (BIP-340 over the SAME canonical
// string a runner/agent-tools server verifies: audience|ts|body) and POSTs it
// to an MCP endpoint. The audience binds the signature to the peer, closing
// cross-peer replay — it is the agent-tools pubkey for the CP toolset and the
// RUNNER pubkey for a capability runner.
func (b *mcpBridge) postSignedTo(endpoint string, raw []byte, audience string) ([]byte, error) {
	ts := time.Now().Unix()
	canonical := audience + "|" + strconv.FormatInt(ts, 10) + "|" + string(raw)
	digest := sha256.Sum256([]byte(canonical))
	sig, err := crypto.SignBIP340(b.secret[:], digest[:])
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(agenttools.PubkeyHeader, b.callerPubkey)
	req.Header.Set(agenttools.SigHeader, hex.EncodeToString(sig))
	req.Header.Set(agenttools.TSHeader, strconv.FormatInt(ts, 10))
	resp, err := b.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, nil
}

// runnerCoords is one capability runner's pod-facing coords (the pod may hold
// several — one per capability its department's role grants it).
type runnerCoords struct {
	url, pub, target, secret string
}

// forwardRunner proxies an exec/list tools/call to the pod's capability
// runners (signed as this agent, audience = the runner pubkey). exec's target
// selects the runner — each runner is pinned to its OWN single target +
// credential, so the agent can never redirect a call at an unlisted target.
// The `list` tool is answered locally from this pod's config.
func (b *mcpBridge) forwardRunner(raw []byte) ([]byte, error) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	var call struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &call); err != nil {
		return nil, err
	}
	if call.Name == "list" {
		out, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0", "id": json.RawMessage(orNull(req.ID)),
			"result": map[string]interface{}{
				"content": []map[string]interface{}{{"type": "text", "text": b.targetListText()}},
				"isError": false,
			},
		})
		return out, nil
	}
	rc, err := b.routeRunner(&call)
	if err != nil {
		return nil, err
	}
	out, err := runnerCallRequest(raw, rc)
	if err != nil {
		return nil, err
	}
	return b.postSignedTo(strings.TrimSuffix(rc.url, "/")+"/mcp", out, rc.pub)
}

// routeRunner resolves which capability runner serves an exec call: an
// explicit target must be one this pod holds; absent, a single-runner pod
// pins to it and a multi-runner pod fails closed (naming the options).
func (b *mcpBridge) routeRunner(call *struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}) (*runnerCoords, error) {
	requested, _ := call.Arguments["target"].(string)
	if requested != "" {
		rc, ok := b.byTarget[requested]
		if !ok {
			return nil, fmt.Errorf("unknown target %q — this pod can reach [%s]", requested, strings.Join(b.targetNames(), ", "))
		}
		return rc, nil
	}
	if len(b.runners) == 1 {
		return &b.runners[0], nil
	}
	return nil, fmt.Errorf("target required — this pod can reach [%s]", strings.Join(b.targetNames(), ", "))
}

// targetListText renders the list tool's answer from this pod's config.
func (b *mcpBridge) targetListText() string {
	return strings.Join(b.targetNames(), ", ")
}

func (b *mcpBridge) targetNames() []string {
	out := make([]string, 0, len(b.runners))
	for _, r := range b.runners {
		out = append(out, r.target)
	}
	return out
}

// runnerCallRequest rewrites an exec tools/call for the runner: the target +
// credential NAME are injected from the pod env (the agent can never redirect
// the call or name other secrets), and the caller's JSON-RPC id is preserved
// (the harness matches replies by id). Pure + unit-tested.
func runnerCallRequest(raw []byte, rc *runnerCoords) ([]byte, error) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	var call struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &call); err != nil {
		return nil, err
	}
	if call.Arguments == nil {
		call.Arguments = map[string]interface{}{}
	}
	// Drop the routing hint — the runner's exec schema has no target field,
	// and the runner would reject an unknown argument's extra name only via
	// its own target handling (it IS an arg: exec(target, ...)); the pin
	// REPLACES whatever the agent passed.
	call.Arguments["target"] = rc.target
	call.Arguments["secrets"] = []string{rc.secret}
	id := req.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]interface{}{"name": call.Name, "arguments": call.Arguments},
	})
}

// orNull returns the id or a JSON null.
func orNull(id json.RawMessage) string {
	if len(id) == 0 {
		return "null"
	}
	return string(id)
}

type mcpBridge struct {
	devCmd        string
	devArgs       []string
	agentToolsURL string
	agentToolsPub string
	secret        [32]byte
	callerPubkey  string
	hc            *http.Client

	// Optional capability runners (a department's dedicated runners): when
	// set, exec/list are advertised; exec routes by target to the pinned
	// runner+credential. Empty = no runner access.
	runners  []runnerCoords
	byTarget map[string]*runnerCoords

	dev            *exec.Cmd
	devIn          io.WriteCloser
	devOut         io.ReadCloser
	devReader      *bufio.Reader
	devInitialized bool
}

func (b *mcpBridge) startDev() error {
	if b.dev != nil {
		return nil
	}
	b.dev = exec.Command(b.devCmd, b.devArgs...)
	b.dev.Stderr = os.Stderr // buzz-dev-mcp diagnostics surface in the pod log
	in, err := b.dev.StdinPipe()
	if err != nil {
		return err
	}
	out, err := b.dev.StdoutPipe()
	if err != nil {
		return err
	}
	if err := b.dev.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", b.devCmd, err)
	}
	b.devIn, b.devOut = in, out
	b.devReader = bufio.NewReader(b.devOut)
	return nil
}

// forwardDev sends raw to buzz-dev-mcp stdin. If the request carries an id
// (expects a response), reads dev responses until the id-matching reply and
// returns it. Any unsolicited dev line is relayed to the harness through the
// SAME buffered writer runBridge owns (a second direct os.Stdout writer would
// interleave mid-line and desync the NDJSON stream). Merges freehold tools into
// a tools/list reply.
func (b *mcpBridge) forwardDev(raw []byte, out *bufio.Writer) ([]byte, error) {
	if err := b.startDev(); err != nil {
		return nil, err
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if _, err := b.devIn.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	if len(req.ID) == 0 {
		// notification — no response expected.
		return nil, nil
	}
	for {
		line, err := b.devReader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		var resp struct {
			ID     json.RawMessage         `json:"id"`
			Result *json.RawMessage        `json:"result"`
			Error  *map[string]interface{} `json:"error"`
		}
		_ = json.Unmarshal([]byte(line), &resp)
		logBridge("dev", req.Method, firstMethod([]byte(line)))
		if resp.ID != nil && bytes.Equal(resp.ID, req.ID) {
			// The reply for our request: merge freehold tools into tools/list.
			if req.Method == "tools/list" {
				line = mergeToolsList(line, len(b.runners) > 0, b.targetNames())
			}
			return append([]byte(nil), line...), nil
		}
		// Unsolicited (e.g. a notification from dev-mcp) — relay as-is to the
		// harness through the shared writer and keep looking for our reply.
		out.WriteString(line)
		out.Flush()
	}
}

// mergeToolsList appends the freehold agent tools to a tools/list result,
// preserving every other top-level field the server replied with (jsonrpc, id,
// _meta …) — a struct-based re-marshal dropped `jsonrpc`, and rmcp rejects a
// response that lacks it with a parse error, which is what deadlocked the CPA.
func mergeToolsList(line string, hasRunner bool, targets []string) string {
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return line
	}
	result, ok := env["result"].(map[string]interface{})
	if !ok {
		return line
	}
	var tools []map[string]interface{}
	if raw, present := result["tools"]; present {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &tools)
	}
	tools = append(tools, freeholdToolDefs(hasRunner, targets)...)
	result["tools"] = tools
	out, _ := json.Marshal(env)
	return string(out)
}

// runBridge is the NDJSON request/response loop, testable via injected fds.
func runBridge(in io.Reader, out io.Writer, b *mcpBridge) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	w := bufio.NewWriter(out)
	for sc.Scan() {
		raw := append([]byte(nil), sc.Bytes()...)
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
		logBridge("req", req.Method, trunc(raw))
		// A freehold tool call is answered by agent-tools (signed), never
		// buzz-dev-mcp.
		if req.Method == "tools/call" {
			var call struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &call)
			if isFreeholdTool(call.Name) {
				resp, err := b.signedForward(raw, b.agentToolsPub)
				if err != nil {
					logBridge("forward", call.Name, err.Error())
					logBridge("forward-error", err.Error())
					resp = rpcErrorWrapper(raw, err)
				}
				writeResp(w, resp)
				continue
			}
			// A capability-runner tool (exec/list) is proxied to the pod's
			// runners, signed as this agent. Only present when runner coords are
			// wired, so the CPA/custom agents never reach here.
			if len(b.runners) > 0 && isRunnerTool(call.Name) {
				resp, err := b.forwardRunner(raw)
				if err != nil {
					logBridge("runner-forward", call.Name, err.Error())
					resp = rpcErrorWrapper(raw, err)
				}
				writeResp(w, resp)
				continue
			}
		}
		resp, err := b.forwardDev(raw, w)
		if err != nil {
			logBridge("forward-error", err.Error())
			resp = rpcErrorWrapper(raw, err)
		}
		if resp != nil {
			writeResp(w, resp)
		}
	}
	return sc.Err()
}

// writeResp emits exactly one NDJSON line per response. Responses may already
// end in a newline (ReadString keeps the delimiter; the agent-tools server's
// json.Encoder appends one) — trimming before the single delimiter prevents a
// blank line that could desync the harness's NDJSON reader.
func writeResp(w *bufio.Writer, resp []byte) {
	logBridge("reply", firstMethod(resp))
	resp = bytes.TrimRight(resp, "\r\n")
	w.Write(resp)
	w.WriteByte('\n')
	w.Flush()
}

// firstMethod extracts a JSON-RPC method name for diagnostics ("" absent).
func firstMethod(raw []byte) string {
	var v struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(raw, &v)
	if v.Method != "" {
		return v.Method
	}
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != nil {
		return "err:" + e.Error.Message
	}
	return ""
}

// rpcErrorWrapper returns a JSON-RPC error envelope for a failed forward/sign,
// carrying the original request id when present.
func rpcErrorWrapper(raw []byte, err error) []byte {
	id := json.RawMessage("null")
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &req) == nil && len(req.ID) > 0 {
		id = req.ID
	}
	b, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]interface{}{"code": -32000, "message": err.Error()},
	})
	return b
}

// mcpConf is the bridge's resolved config: the agent-tools endpoint plus the
// optional capability-runner coords (aligned comma lists — one entry per
// runner). env vars take precedence; the pod bootstrap's config file is the
// authoritative source (buzz-acp spawns the bridge and reads that file, so
// env alone is not reliable).
type mcpConf struct {
	URL, Pubkey string
	Runners     []runnerCoords
}

// readMCPConfig resolves the bridge config: env vars first, else the config
// file the pod bootstrap writes (space/newline-separated key=value). Runner
// keys are read the same way, so a department pod gets exec/list even when the
// harness does not forward the pod env. The old singular runner_* keys parse
// as a one-entry list.
func readMCPConfig() (mcpConf, error) {
	c := mcpConf{
		URL:    os.Getenv("FREEHOLD_AGENT_TOOLS_URL"),
		Pubkey: os.Getenv("FREEHOLD_AGENT_TOOLS_PUBKEY"),
	}
	if urls := firstNonBlank(os.Getenv("FREEHOLD_RUNNER_URLS"), os.Getenv("FREEHOLD_RUNNER_URL")); urls != "" {
		c.Runners = parseRunnerLists(
			urls,
			firstNonBlank(os.Getenv("FREEHOLD_RUNNER_PUBKEYS"), os.Getenv("FREEHOLD_RUNNER_PUBKEY")),
			firstNonBlank(os.Getenv("FREEHOLD_RUNNER_TARGETS"), os.Getenv("FREEHOLD_RUNNER_TARGET")),
			firstNonBlank(os.Getenv("FREEHOLD_RUNNER_SECRETS"), os.Getenv("FREEHOLD_RUNNER_SECRET")),
		)
	}
	set := func(k, v string) {
		switch k {
		case "url":
			if c.URL == "" {
				c.URL = v
			}
		case "pubkey":
			if c.Pubkey == "" {
				c.Pubkey = v
			}
		case "runner_urls", "runner_url":
			// Seed (or reset) the runner list with the url column; the other
			// columns fill as their conf keys arrive.
			c.Runners = parseRunnerLists(v, "", "", "")
			c.Runners = fillRunnersFrom(c.Runners, nil, nil, nil)
		case "runner_pubkeys", "runner_pubkey":
			c.Runners = fillRunnersFrom(c.Runners, strings.Split(v, ","), nil, nil)
		case "runner_targets", "runner_target":
			c.Runners = fillRunnersFrom(c.Runners, nil, strings.Split(v, ","), nil)
		case "runner_secrets", "runner_secret":
			c.Runners = fillRunnersFrom(c.Runners, nil, nil, strings.Split(v, ","))
		}
	}
	paths := []string{os.Getenv("FREEHOLD_AGENT_TOOLS_CONF"), "/tmp/freehold-agent-tools.conf", "/usr/local/etc/freehold-agent-tools.conf"}
	for _, path := range paths {
		if path == "" {
			continue
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		for _, tok := range strings.Fields(string(raw)) {
			if k, v, ok := strings.Cut(tok, "="); ok {
				set(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
	}
	if c.URL == "" || c.Pubkey == "" {
		return mcpConf{}, fmt.Errorf("freehold-agent-tools mcp needs FREEHOLD_AGENT_TOOLS_URL + PUBKEY (env or a config file)")
	}
	return c, nil
}

// parseRunnerLists renders aligned comma lists into runner coords. An empty
// secrets list defaults each entry's secret to its target name (the provision
// convention ties the two together). An entry survives on its URL alone so the
// conf file's sequential fill (urls first, then pubkeys/targets/secrets) can
// complete it — cmdMCP validates the full set before advertising exec.
func parseRunnerLists(urls, pubs, targets, secrets string) []runnerCoords {
	u := splitCSV(urls)
	p := splitCSV(pubs)
	t := splitCSV(targets)
	s := splitCSV(secrets)
	out := make([]runnerCoords, 0, len(u))
	for i := range u {
		if u[i] == "" {
			continue
		}
		rc := runnerCoords{url: u[i]}
		if i < len(p) {
			rc.pub = p[i]
		}
		if i < len(t) {
			rc.target = t[i]
		}
		rc.secret = firstNonBlank(listAt(s, i), rc.target)
		out = append(out, rc)
	}
	return out
}

// fillRunnersFrom backfills a coordinate column into a runner list (the conf
// file's keys arrive one at a time). A parsed singular url (no other columns
// yet) grows columns as they land.
func fillRunnersFrom(existing []runnerCoords, pubs, targets, secrets []string) []runnerCoords {
	n := len(existing)
	if len(pubs) > n {
		n = len(pubs)
	}
	if len(targets) > n {
		n = len(targets)
	}
	if len(secrets) > n {
		n = len(secrets)
	}
	out := make([]runnerCoords, n)
	copy(out, existing)
	for i := range out {
		if i < len(pubs) && pubs[i] != "" {
			out[i].pub = pubs[i]
		}
		if i < len(targets) && targets[i] != "" {
			out[i].target = targets[i]
		}
		if i < len(secrets) && secrets[i] != "" {
			out[i].secret = secrets[i]
		} else if out[i].secret == "" {
			out[i].secret = out[i].target
		}
	}
	return out
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func listAt(list []string, i int) string {
	if i < len(list) {
		return list[i]
	}
	return ""
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func cmdMCP(args []string) {
	// Bridge diagnostics land in a world-writable file inside the pod (stderr
	// from a buzz-acp-spawned MCP server isn't surfaced to the container log).
	logFile := "/tmp/freehold-agent-tools.log"
	if f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		defer f.Close()
		fmt.Fprintf(f, "%s mcp start args=%v\n", time.Now().Format(time.RFC3339), args)
		// tee errors into the file
		bridgeLog = f
	}
	devCmd := envOr("FREEHOLD_DEV_MCP", "/usr/local/bin/buzz-dev-mcp")
	conf, err := readMCPConfig()
	if err != nil {
		logNsec("mcp: " + err.Error())
		os.Exit(1)
	}
	// buzz-acp injects the agent's nsec into the MCP server env as
	// BUZZ_PRIVATE_KEY (bech32 nsec1).
	nsec := os.Getenv("BUZZ_PRIVATE_KEY")
	if nsec == "" {
		logNsec("mcp: no BUZZ_PRIVATE_KEY env (buzz-acp must inject the agent nsec)")
		os.Exit(1)
	}
	secret, err := crypto.NsecToSecret(strings.TrimSpace(nsec))
	if err != nil {
		logNsec("mcp: bad BUZZ_PRIVATE_KEY: " + err.Error())
		os.Exit(1)
	}
	caller, err := crypto.PubkeyFromSecret(secret[:])
	if err != nil {
		logNsec("mcp: " + err.Error())
		os.Exit(1)
	}
	// A department pod carries its capability runners' coords (config file or
	// env); the CPA and custom pods carry none, so no exec is advertised.
	byTarget := map[string]*runnerCoords{}
	for i := range conf.Runners {
		byTarget[conf.Runners[i].target] = &conf.Runners[i]
	}
	b := &mcpBridge{
		devCmd:        devCmd,
		agentToolsURL: strings.TrimSuffix(conf.URL, "/"),
		agentToolsPub: conf.Pubkey,
		secret:        secret,
		callerPubkey:  caller,
		hc:            &http.Client{Timeout: 60 * time.Second},
		runners:       conf.Runners,
		byTarget:      byTarget,
	}
	if len(b.runners) > 0 {
		for _, r := range b.runners {
			if r.url == "" || r.pub == "" || r.target == "" {
				logNsec("mcp: runner coord missing url/pubkey/target — exec disabled")
				b.runners, b.byTarget = nil, map[string]*runnerCoords{}
				break
			}
		}
	}
	if len(b.runners) > 0 {
		logBridge("mcp: capability runners wired", strings.Join(b.targetNames(), ","))
	}
	if err := runBridge(os.Stdin, os.Stdout, b); err != nil {
		logBridge("runbridge: " + err.Error())
		logNsec("mcp: " + err.Error())
		os.Exit(1)
	}
	logBridge("mcp: clean exit")
}

// bridgeLog is an optional diagnostics sink inside the pod (nil = stderr only).
var bridgeLog *os.File

// logBridge writes a line to the pod-side bridge log (best-effort).
func logBridge(v ...interface{}) {
	if bridgeLog != nil {
		fmt.Fprintln(bridgeLog, append([]interface{}{time.Now().Format(time.RFC3339)}, v...)...)
	}
}

// trunc keeps a short diagnostic prefix of a line.
func trunc(b []byte) string {
	s := strings.ReplaceAll(string(b), "\n", "\\n")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// logNsec mirrors log.Print but named to avoid a clash.
func logNsec(v ...interface{}) { fmt.Fprintln(os.Stderr, v...) }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// firstNonEmpty returns the first non-blank of its arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
