// The MCP surface: JSON-RPC over POST /mcp (Streamable HTTP shape — JSON
// responses; notifications get 202). Read/write tools over the pinned
// spreadsheet; the write tools vanish under FREEHOLD_READ_ONLY=1 (and the
// OAuth scope narrows with them — belt and suspenders with a Viewer SA).
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
)

type server struct {
	sheet    string
	readOnly bool
	auth     *googleAuth
	http     *http.Client
	// sheetsBase is the Sheets API origin (the real one; a test points it at
	// a fake).
	sheetsBase string
}

func base64StdDecode(s string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// routes assembles the handler: everything under /mcp is broker-gated.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// handleMCP authenticates the broker, then answers JSON-RPC.
func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if broker := osGetenv("FREEHOLD_BROKER_TOKEN"); broker != "" {
		if r.Header.Get("Authorization") != "Bearer "+broker {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"broker token missing or wrong"}`))
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var msg struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Method == "" {
		s.rpcError(w, nil, -32700, "parse error")
		return
	}
	if msg.ID == nil {
		// A notification (e.g. notifications/initialized): no reply body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch msg.Method {
	case "initialize":
		s.rpcResult(w, msg.ID, map[string]interface{}{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "freehold-mcp-sheets", "version": "0.1.0"},
		})
	case "ping":
		s.rpcResult(w, msg.ID, map[string]interface{}{})
	case "tools/list":
		s.rpcResult(w, msg.ID, map[string]interface{}{"tools": s.toolDefs()})
	case "tools/call":
		text, isErr := s.callTool(msg.Params.Name, msg.Params.Arguments)
		s.rpcResult(w, msg.ID, map[string]interface{}{
			"content": []map[string]interface{}{{"type": "text", "text": text}},
			"isError": isErr,
		})
	default:
		s.rpcError(w, msg.ID, -32601, "method not found: "+msg.Method)
	}
}

func (s *server) rpcResult(w http.ResponseWriter, id *json.RawMessage, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0", "id": orNull(id), "result": result,
	})
}

func (s *server) rpcError(w http.ResponseWriter, id *json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0", "id": orNull(id),
		"error": map[string]interface{}{"code": code, "message": msg},
	})
}

func orNull(id *json.RawMessage) json.RawMessage {
	if id == nil {
		return json.RawMessage("null")
	}
	return *id
}

// toolDefs — the pinned-sheet surface. Write tools only when not read-only.
func (s *server) toolDefs() []map[string]interface{} {
	ro := map[string]interface{}{"type": "boolean"}
	str := map[string]interface{}{"type": "string"}
	rows := map[string]interface{}{
		"type":     "array",
		"items":    map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
		"minItems": 1,
	}
	defs := []map[string]interface{}{
		{
			"name":        "get_info",
			"description": fmt.Sprintf("Get the pinned spreadsheet's metadata (title, sheet/tab names). The pinned spreadsheet is %q.", s.sheet),
			"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		},
		{
			"name":        "get_cells",
			"description": "Read values from the pinned spreadsheet. range is A1 notation, optionally with the tab ('Sheet1!A1:D10'; defaults to the first tab's used range).",
			"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"range": str}},
		},
	}
	if !s.readOnly {
		defs = append(defs,
			map[string]interface{}{
				"name":        "update_cells",
				"description": "Overwrite cells in the pinned spreadsheet. range is A1 notation ('Sheet1!A1'); data is a 2-D array of rows.",
				"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
					"range": str, "data": rows,
				}, "required": []string{"range", "data"}},
			},
			map[string]interface{}{
				"name":        "append_rows",
				"description": "Append rows after the pinned sheet's table (values only, formulas not evaluated as inputs). data is a 2-D array of rows; range (optional) picks the tab ('Sheet1').",
				"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
					"range": str, "data": rows,
				}, "required": []string{"data"}},
			})
	}
	// lock the unused-var linter for the ro/str/rows locals
	_, _, _ = ro, str, rows
	return defs
}

// callTool executes one tool call; (text, isError) back to the RPC layer.
func (s *server) callTool(name string, args json.RawMessage) (string, bool) {
	var a struct {
		Range string     `json:"range"`
		Data  [][]string `json:"data"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "invalid arguments: " + err.Error(), true
		}
	}
	if s.sheet == "" {
		return "misconfigured: FREEHOLD_SPREADSHEET_ID is not set", true
	}
	if s.readOnly && (name == "update_cells" || name == "append_rows") {
		return fmt.Sprintf("tool %q is not available: this capability is READ-ONLY", name), true
	}
	var out string
	var err error
	switch name {
	case "get_info":
		out, err = s.getInfo()
	case "get_cells":
		out, err = s.getCells(a.Range)
	case "update_cells":
		if len(a.Data) == 0 {
			return "update_cells: data is required (a 2-D array of rows)", true
		}
		out, err = s.putValues("PUT", a.Range, a.Data)
	case "append_rows":
		if len(a.Data) == 0 {
			return "append_rows: data is required (a 2-D array of rows)", true
		}
		out, err = s.putValues("POST", a.Range, a.Data)
	default:
		return "unknown tool: " + name, true
	}
	if err != nil {
		return name + " failed: " + err.Error(), true
	}
	log.Printf("tool %s range=%q ok", name, a.Range)
	return out, false
}

// sheetsCall does one authenticated Sheets-API REST call.
func (s *server) sheetsCall(method, path string, body interface{}) ([]byte, error) {
	tok, err := s.auth.token()
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, s.sheetsBase+"/v4/spreadsheets/"+s.sheet+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, trim(out))
	}
	return out, nil
}

func (s *server) getInfo() (string, error) {
	out, err := s.sheetsCall(http.MethodGet, "?fields=properties.title,sheets.properties", nil)
	if err != nil {
		return "", err
	}
	var meta struct {
		Properties struct {
			Title string `json:"title"`
		} `json:"properties"`
		Sheets []struct {
			Properties struct {
				Title string `json:"title"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return "", err
	}
	tabs := []string{}
	for _, sh := range meta.Sheets {
		tabs = append(tabs, sh.Properties.Title)
	}
	return fmt.Sprintf("title: %s\ntabs: %s", meta.Properties.Title, strings.Join(tabs, ", ")), nil
}

func (s *server) getCells(rng string) (string, error) {
	if rng == "" {
		rng = "A1:Z1000"
	}
	escaped := urlPathEscape(rng)
	if escaped == "" {
		return "", fmt.Errorf("range %q: A1 notation only (letters, digits, underscore, colon, apostrophe, hyphen, space)", rng)
	}
	out, err := s.sheetsCall(http.MethodGet, "/values/"+escaped, nil)
	if err != nil {
		return "", err
	}
	var vals struct {
		Values [][]interface{} `json:"values"`
	}
	if err := json.Unmarshal(out, &vals); err != nil {
		return "", err
	}
	rows := []string{}
	for _, row := range vals.Values {
		cells := []string{}
		for _, c := range row {
			cells = append(cells, fmt.Sprint(c))
		}
		rows = append(rows, strings.Join(cells, "\t"))
	}
	if len(rows) == 0 {
		return "(empty range)", nil
	}
	return strings.Join(rows, "\n"), nil
}

func (s *server) putValues(method, rng string, data [][]string) (string, error) {
	if rng == "" {
		return "", fmt.Errorf("a range is required ('Sheet1!A1' for update, 'Sheet1' for append)")
	}
	if urlPathEscape(rng) == "" {
		return "", fmt.Errorf("range %q: A1 notation only (letters, digits, underscore, colon, apostrophe, hyphen, space)", rng)
	}
	path := "/values/" + urlPathEscape(rng)
	if method == http.MethodPut {
		path += "?valueInputOption=USER_ENTERED"
	} else {
		path += ":append?valueInputOption=USER_ENTERED&insertDataOption=INSERT_ROWS"
	}
	out, err := s.sheetsCall(method, path, map[string]interface{}{"values": data})
	if err != nil {
		return "", err
	}
	var res struct {
		UpdatedCells    int `json:"updatedCells"`
		UpdatedRowCount int `json:"updatedRowCount"`
	}
	_ = json.Unmarshal(out, &res)
	if method == http.MethodPut {
		return fmt.Sprintf("updated %d cells", res.UpdatedCells), nil
	}
	return fmt.Sprintf("appended %d rows", res.UpdatedRowCount), nil
}

// rangeRe is A1 notation's SAFE charset (letters, digits, underscore, colon,
// apostrophe, exclamation, space, hyphen): the agent-controlled range must
// never reach the URL raw — a "../"-style range retargets the request at a
// different spreadsheet with the SA's credential (servers normalize dot
// segments). Dots are rejected too — rename tabs to avoid them; they are not
// worth a path-traversal shape.
var rangeRe = regexp.MustCompile(`^[A-Za-z0-9_:'!\- ]+$`)

func urlPathEscape(s string) string {
	if !rangeRe.MatchString(s) {
		return ""
	}
	return strings.ReplaceAll(strings.TrimSpace(s), " ", "%20")
}

func osGetenv(k string) string { return os.Getenv(k) }
