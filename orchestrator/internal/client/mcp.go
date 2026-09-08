// Package client is the MCP-over-HTTP client the orchestrator uses to drive a
// Rust runner. Every tools/call body is signed with the agent identity (grants
// enforce it runner-side), reproducing orchestrator/src/client.rs.
package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"freehold/orchestrator/internal/crypto"
)

// MCP auth headers (core/src/auth.rs).
const (
	TSWindowSecs   = 60
	PubkeyHeader   = "x-freehold-pubkey"
	SigHeader      = "x-freehold-sig"
	TSHeader       = "x-freehold-ts"
	BaseTimeoutSec = 30
)

// AgentAuth carries the agent identity: the secret key (signing) and the
// pubkey (granted to runners). The secret is zeroed on Zero() — callers defer
// it at function bounds (Go has no RAII zeroize; a GC may copy plaintext,
// matching the Rust-side admission in AGENTS.md).
type AgentAuth struct {
	Secret [32]byte
	Pubkey string
}

// Zero wipes the secret.
func (a *AgentAuth) Zero() {
	for i := range a.Secret {
		a.Secret[i] = 0
	}
}

// McpClient is a signed MCP-over-HTTP client to a runner.
type McpClient struct {
	URL          string
	Auth         *AgentAuth
	RunnerPubkey string
	client       *http.Client
}

// ExecOutcome maps the runner's exec tool result.
type ExecOutcome struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode *int   `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
}

// New builds an McpClient, validating the runner pubkey (the signature
// audience) is 64 hex.
func New(url string, auth *AgentAuth, runnerPubkey string) (*McpClient, error) {
	if len(runnerPubkey) != 64 {
		return nil, fmt.Errorf("runner pubkey must be 64 hex chars for the signature audience")
	}
	if _, err := hex.DecodeString(runnerPubkey); err != nil {
		return nil, fmt.Errorf("runner pubkey must be 64 hex chars for the signature audience")
	}
	// A wedged runner must not hang the orchestrator forever.
	hc := &http.Client{Timeout: BaseTimeoutSec * time.Second}
	return &McpClient{URL: url, Auth: auth, RunnerPubkey: runnerPubkey, client: hc}, nil
}

// ConnectURL builds the MCP endpoint from a --addr that may be host:port or a
// full URL, always appending /mcp (a scheme must not change the path).
func ConnectURL(addr string) string {
	base := trimSuffix(addr, "/")
	withScheme := base
	if !contains(base, "://") {
		withScheme = "http://" + base
	}
	if hasSuffix(withScheme, "/mcp") {
		return withScheme
	}
	return withScheme + "/mcp"
}

// signBody signs the raw body for the target runner, reproducing
// core::auth::sign_body over the canonical string "{runner_pubkey}|{ts}|{raw}".
func signBody(secret []byte, runnerPubkey string, ts int64, raw string) (sigHex string, err error) {
	canonical := runnerPubkey + "|" + strconv.FormatInt(ts, 10) + "|" + raw
	digest := sha256.Sum256([]byte(canonical))
	sig, err := crypto.SignBIP340(secret, digest[:])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig), nil
}

// rawWith signs and POSTs body to the MCP endpoint with the given HTTP client.
func (c *McpClient) rawWith(hc *http.Client, body []byte) (json.RawMessage, error) {
	raw := string(body)
	ts := time.Now().Unix()
	sig, err := signBody(c.Auth.Secret[:], c.RunnerPubkey, ts, raw)
	if err != nil {
		return nil, fmt.Errorf("sign body: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("http error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(PubkeyHeader, c.Auth.Pubkey)
	req.Header.Set(SigHeader, sig)
	req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http error: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("http error: %w", err)
	}
	var v json.RawMessage
	if err := json.Unmarshal(rb, &v); err != nil {
		return nil, fmt.Errorf("http error: bad json: %w", err)
	}
	return v, nil
}

// execAgent returns an HTTP client whose deadline is runner_timeout + margin,
// so a slow command surfaces as timed_out:true (the runner's watchdog), never
// as a client-side error while the command keeps running.
func execAgent(runnerTimeoutS uint64) *http.Client {
	return &http.Client{Timeout: time.Duration(runnerTimeoutS+30) * time.Second}
}

func defaultAgent() *http.Client { return &http.Client{Timeout: BaseTimeoutSec * time.Second} }

// call issues tools/call with the request signed by the agent.
func (c *McpClient) call(hc *http.Client, name string, arguments interface{}) (json.RawMessage, error) {
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": name, "arguments": arguments},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	resp, err := c.rawWith(hc, raw)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			IsError bool             `json:"isError"`
			Content []map[string]any `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, fmt.Errorf("json-rpc error: bad envelope: %w", err)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("json-rpc error: %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if envelope.Result == nil {
		return nil, fmt.Errorf("json-rpc error: missing result")
	}
	if envelope.Result.IsError {
		text := "(no text)"
		if len(envelope.Result.Content) > 0 {
			if t, ok := envelope.Result.Content[0]["text"].(string); ok {
				text = t
			}
		}
		return nil, fmt.Errorf("tool error: %s", text)
	}
	return resp, nil
}

// Call issues tools/call with the default (short) deadline.
func (c *McpClient) Call(name string, arguments interface{}) (json.RawMessage, error) {
	return c.call(defaultAgent(), name, arguments)
}

// CallLong issues tools/call with a LONG deadline (a world_build trigger runs
// its stages synchronously for minutes — the short default would time out
// awaiting the response while the CP keeps working).
func (c *McpClient) CallLong(name string, arguments interface{}) (json.RawMessage, error) {
	return c.call(&http.Client{Timeout: 15 * time.Minute}, name, arguments)
}

// CallText parses the tool's text payload.
func (c *McpClient) CallText(name string, arguments interface{}) (json.RawMessage, error) {
	resp, err := c.call(defaultAgent(), name, arguments)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Result *struct {
			Content []map[string]any `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, fmt.Errorf("tool error: missing text content")
	}
	if envelope.Result == nil || len(envelope.Result.Content) == 0 {
		return nil, fmt.Errorf("tool error: missing text content")
	}
	text, ok := envelope.Result.Content[0]["text"].(string)
	if !ok {
		return nil, fmt.Errorf("tool error: missing text content")
	}
	var v json.RawMessage
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return nil, fmt.Errorf("tool error: bad text json: %w", err)
	}
	return v, nil
}

// Readiness returns the runner's status map (targets -> readiness string).
func (c *McpClient) Readiness() (map[string]interface{}, error) {
	resp, err := c.CallText("status", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(resp, &m); err != nil {
		return nil, fmt.Errorf("tool error: status: %w", err)
	}
	return m, nil
}

// Exec runs a command on target with a runner-side watchdog of timeoutS.
func (c *McpClient) Exec(target, cmd string, secrets []string, timeoutS uint64) (*ExecOutcome, error) {
	// The runner is Rust: serde wants a SEQUENCE for `secrets` — a Go nil
	// slice marshals as `null` and is rejected. Always send an array.
	if secrets == nil {
		secrets = []string{}
	}
	arguments := map[string]interface{}{
		"cmd":       cmd,
		"target":    target,
		"secrets":   secrets,
		"timeout_s": timeoutS,
	}
	resp, err := c.call(execAgent(timeoutS), "exec", arguments)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Result *struct {
			Content []map[string]any `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, fmt.Errorf("tool error: missing exec text: %w", err)
	}
	if envelope.Result == nil || len(envelope.Result.Content) == 0 {
		return nil, fmt.Errorf("tool error: missing exec text")
	}
	text, ok := envelope.Result.Content[0]["text"].(string)
	if !ok {
		return nil, fmt.Errorf("tool error: missing exec text")
	}
	var out ExecOutcome
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("tool error: exec json: %w", err)
	}
	return &out, nil
}

// Upload streams a LOCAL file to the target via the runner's sftp upload tool.
func (c *McpClient) Upload(target, localPath, remotePath string, timeoutS uint64) (uint64, error) {
	arguments := map[string]interface{}{
		"target":      target,
		"local_path":  localPath,
		"remote_path": remotePath,
	}
	resp, err := c.call(execAgent(timeoutS), "upload", arguments)
	if err != nil {
		return 0, err
	}
	var envelope struct {
		Result *struct {
			Content []map[string]any `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return 0, fmt.Errorf("tool error: upload json: %w", err)
	}
	text := "{}"
	if envelope.Result != nil && len(envelope.Result.Content) > 0 {
		if t, ok := envelope.Result.Content[0]["text"].(string); ok {
			text = t
		}
	}
	var v map[string]interface{}
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return 0, fmt.Errorf("tool error: upload json: %w", err)
	}
	if u, ok := v["uploaded"].(float64); ok {
		return uint64(u), nil
	}
	return 0, nil
}

func trimSuffix(s, suffix string) string {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
}

func contains(s, sub string) bool {
	return len(sub) == 0 || indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
