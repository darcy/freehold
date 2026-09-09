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

	"freehold/control-plane/api/agenttools"
	"freehold/contract/crypto"
)

// freeholdToolDefs are the create/grant/manage tool schemas merged into
// buzz-dev-mcp's tools/list (they mirror internal/agenttools/server.go).
func freeholdToolDefs() []map[string]interface{} {
	i := func(props map[string]interface{}, req []string) map[string]interface{} {
		return map[string]interface{}{"type": "object", "properties": props, "required": req}
	}
	return []map[string]interface{}{
		{"name": "create_agent", "description": "Create a new conversational agent (name + one-line purpose). Returns the new agent's pubkey.",
			"inputSchema": i(map[string]interface{}{
				"name":    map[string]interface{}{"type": "string"},
				"purpose": map[string]interface{}{"type": "string"},
			}, []string{"name"})},
		{"name": "grant_agent", "description": "Bind agent pubkeys to a runner's whitelist.",
			"inputSchema": i(map[string]interface{}{
				"runner":  map[string]interface{}{"type": "string"},
				"pubkeys": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			}, []string{"runner", "pubkeys"})},
		{"name": "manage_agent", "description": "List registered agents, or (remove=<name>) drop one's registry row.",
			"inputSchema": i(map[string]interface{}{"remove": map[string]interface{}{"type": "string"}}, []string{})},
	}
}

func isFreeholdTool(name string) bool {
	switch name {
	// grant_agent is deliberately absent: it is operator-scoped (a grant hands
	// direct exec access to a runner), so the CPA's conversation+create-only
	// harness must not advertise or call it.
	case "create_agent", "manage_agent":
		return true
	}
	return false
}

// signedForward posts one JSON-RPC request to the agent-tools /mcp server with
// the shared signed-header scheme (the agent's nsec signs it) and returns the
// JSON-RPC response body.
func (b *mcpBridge) signedForward(raw []byte, audience string, secret []byte, callerPubkey string) ([]byte, error) {
	ts := time.Now().Unix()
	canonical := audience + "|" + strconv.FormatInt(ts, 10) + "|" + string(raw)
	digest := sha256.Sum256([]byte(canonical))
	sig, err := crypto.SignBIP340(secret, digest[:])
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, b.agentToolsURL+"/mcp", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(agenttools.PubkeyHeader, callerPubkey)
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

type mcpBridge struct {
	devCmd         string
	devArgs        []string
	agentToolsURL  string
	agentToolsPub  string
	secret         [32]byte
	callerPubkey   string
	hc             *http.Client
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
				line = mergeToolsList(line)
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
func mergeToolsList(line string) string {
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
	tools = append(tools, freeholdToolDefs()...)
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
				resp, err := b.signedForward(raw, b.agentToolsPub, b.secret[:], b.callerPubkey)
				if err != nil {
					logBridge("forward", call.Name, err.Error())
					logBridge("forward-error", err.Error())
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

// readMCPConfig resolves the agent-tools endpoint: env vars first, else the
// config file the pod bootstrap writes (space/newline-separated key=value).
func readMCPConfig() (url, pub string, err error) {
	url = os.Getenv("FREEHOLD_AGENT_TOOLS_URL")
	pub = os.Getenv("FREEHOLD_AGENT_TOOLS_PUBKEY")
	if url != "" && pub != "" {
		return url, pub, nil
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
				switch strings.TrimSpace(k) {
				case "url":
					if url == "" {
						url = strings.TrimSpace(v)
					}
				case "pubkey":
					if pub == "" {
						pub = strings.TrimSpace(v)
					}
				}
			}
		}
		if url != "" && pub != "" {
			return url, pub, nil
		}
	}
	if url == "" || pub == "" {
		return "", "", fmt.Errorf("freehold-agent-tools mcp needs FREEHOLD_AGENT_TOOLS_URL + PUBKEY (env or a config file)")
	}
	return url, pub, nil
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
	url, pub, err := readMCPConfig()
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
	b := &mcpBridge{
		devCmd:        devCmd,
		agentToolsURL: strings.TrimSuffix(url, "/"),
		agentToolsPub: pub,
		secret:        secret,
		callerPubkey:  caller,
		hc:            &http.Client{Timeout: 60 * time.Second},
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
