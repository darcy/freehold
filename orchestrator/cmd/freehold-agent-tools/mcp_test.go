package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDevSrc is a stdlib-only fake buzz-dev-mcp: respond to requests with a
// canned tools/list (one "buzz_send" tool) and a canned tools/call result.
const fakeDevSrc = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req map[string]interface{}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if _, ok := req["id"]; !ok {
			continue // notification
		}
		var resp map[string]interface{}
		if req["method"] == "tools/list" {
			resp = map[string]interface{}{
				"jsonrpc": "2.0", "id": req["id"],
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{{"name": "buzz_send", "description": "send a buzz message", "inputSchema": map[string]interface{}{"type": "object"}}},
				},
			}
		} else {
			resp = map[string]interface{}{
				"jsonrpc": "2.0", "id": req["id"],
				"result": map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "dev-ok"}}},
			}
		}
		b, _ := json.Marshal(resp)
		fmt.Println(string(b))
	}
}
`

// buildFakeDev compiles the stdlib-only fake dev-mcp to a temp binary.
func buildFakeDev(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fakedev.go")
	if err := os.WriteFile(src, []byte(fakeDevSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fakedev")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build fake dev: %v\n%s", err, out)
	}
	return bin
}

// TestBridgeProxyAndDispatch drives the bridge end-to-end with a fake
// buzz-dev-mcp + a fake agent-tools server: the tools/list must merge both
// toolkits, a buzz tool call must route to dev-mcp, and a freehold call must
// route to agent-tools WITH the signed headers.
func TestBridgeProxyAndDispatch(t *testing.T) {
	devBin := buildFakeDev(t)

	var gotPub, gotSig, gotTS string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPub, gotSig, gotTS = r.Header.Get(agentToolPubHeader()), r.Header.Get(agentToolSigHeader()), r.Header.Get(agentToolTSHeader())
		io.ReadAll(r.Body)
		w.Write([]byte(`{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"from-agent-tools"}]}}`))
	}))
	defer ts.Close()

	secret := [32]byte{}
	secret[0] = 42
	b := &mcpBridge{
		devCmd:        devBin,
		agentToolsURL: ts.URL,
		agentToolsPub: strings.Repeat("ab", 32),
		secret:        secret,
		callerPubkey:  strings.Repeat("cd", 32),
		hc:            ts.Client(),
	}

	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"buzz_send","arguments":{"text":"hi"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"manage_agent","arguments":{}}}
`
	var out bytes.Buffer
	if err := runBridge(strings.NewReader(input), &out, b); err != nil {
		t.Fatalf("runBridge: %v", err)
	}
	// Exactly one NDJSON line per response — a doubled trailing newline would
	// emit a blank line that desyncs the harness's reader.
	if strings.Contains(out.String(), "\n\n") {
		t.Fatalf("blank line emitted between responses:\n%q", out.String())
	}
	lines := nonEmptyLines2(out.String())
	if len(lines) != 4 {
		t.Fatalf("expected 4 response lines, got %d:\n%s", len(lines), out.String())
	}

	if listLine := findJSONByID2(lines, 2); !strings.Contains(listLine, "buzz_send") || !strings.Contains(listLine, "create_agent") || !strings.Contains(listLine, "manage_agent") {
		t.Fatalf("tools/list not merged: %s", listLine)
	}
	if callLine := findJSONByID2(lines, 3); !strings.Contains(callLine, "dev-ok") {
		t.Fatalf("buzz tools/call not proxied to dev: %s", callLine)
	}
	if fhLine := findJSONByID2(lines, 4); !strings.Contains(fhLine, "from-agent-tools") {
		t.Fatalf("freehold tools/call not forwarded to agent-tools: %s", fhLine)
	}
	if gotPub == "" || gotSig == "" || gotTS == "" {
		t.Fatalf("signed headers missing: pub=%q sig=%q ts=%q", gotPub, gotSig, gotTS)
	}
}

func agentToolPubHeader() string { return "x-freehold-pubkey" }
func agentToolSigHeader() string { return "x-freehold-sig" }
func agentToolTSHeader() string  { return "x-freehold-ts" }

func nonEmptyLines2(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func findJSONByID2(lines []string, wantID int) string {
	for _, l := range lines {
		var v struct {
			ID interface{} `json:"id"`
		}
		_ = json.Unmarshal([]byte(l), &v)
		if f, ok := v.ID.(float64); ok && int(f) == wantID {
			return l
		}
	}
	return ""
}
