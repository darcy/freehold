package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/console"
)

// TestAgentPodPrepare verifies the create-agent deploy path (A4): preparing an
// agent pod mints/reuses a durable identity and builds the identity-secret +
// pod-manifest scripts (the exact scripts the runner applies).
func TestAgentPodPrepare(t *testing.T) {
	dir := t.TempDir()
	pod := &AgentPod{
		Name:             "helper",
		Purpose:          "help with installs",
		K3sVmid:          105,
		RelayURL:         "wss://relay.test",
		SystemPromptPath: "/p/PROMPT.md",
		IdentityDir:      filepath.Join(dir, "helper-id"),
		OwnerPub:         strings.Repeat("a", 64),
	}
	pub1, idScr, manScr, err := pod.Prepare()
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(pub1) != 64 {
		t.Errorf("minted pubkey not 64 hex: %q", pub1)
	}
	// The object names are derived from the agent's own name, never a
	// shared/fixed CPA name — a second agent must never apply over the CPA's
	// objects.
	if !strings.Contains(idScr, "create secret generic helper-identity -n agents") {
		t.Errorf("identity script missing secret creation: %q", idScr)
	}
	if !strings.Contains(manScr, "name: helper") {
		t.Errorf("manifest script missing the helper pod: %q", manScr)
	}
	if strings.Contains(idScr, "cpa-identity") || strings.Contains(manScr, "cpa-identity") {
		t.Errorf("agent scripts must reference helper-identity, not cpa-identity: %q / %q", idScr, manScr)
	}
	// Reusing the same dir must return the SAME identity (durability).
	pub2, _, _, err := pod.Prepare()
	if err != nil {
		t.Fatalf("re-prepare: %v", err)
	}
	if pub1 != pub2 {
		t.Errorf("identity not durable across prepares: %q vs %q", pub1, pub2)
	}
}

// fakeConsole serves the console agent-surface the CPA toolset calls. It
// records the last /api/agents + /api/grant state so the tools' side effects
// are observable.
func fakeConsole(t *testing.T) (*httptest.Server, func() []console.AgentInfo) {
	t.Helper()
	var agents []console.AgentInfo
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/agents":
			var req struct {
				Name    string `json:"name"`
				Pubkey  string `json:"pubkey"`
				Channel string `json:"channel"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			agents = append(agents, console.AgentInfo{Name: req.Name, Pubkey: req.Pubkey})
			w.Write([]byte(`{"ok":true}`))
		case "GET /api/agents":
			b, _ := json.Marshal(map[string]interface{}{"agents": agents})
			w.Write(b)
		case "DELETE /api/agents/helper":
			agents = nil
			w.Write([]byte(`{"ok":true}`))
		case "POST /api/grant":
			w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"no route"}`))
		}
	}))
	read := func() []console.AgentInfo {
		// return a copy
		out := make([]console.AgentInfo, len(agents))
		copy(out, agents)
		return out
	}
	return srv, read
}

// TestToolsSet verifies the CPA toolset (A4): create-agent registers the
// deployed agent, grant-agent binds pubkeys, manage-agent lists/removes.
func TestToolsSet(t *testing.T) {
	srv, read := fakeConsole(t)
	defer srv.Close()
	c := console.WithCookie(srv.URL, "abc")
	tools := &Tools{Console: c, Create: func(name, purpose string, channels []string, private bool) (string, error) {
		return strings.Repeat("b", 64), nil
	}}

	pub, err := tools.CreateAgent("helper", "help with installs", []string{"ops"}, false)
	if err != nil {
		t.Fatalf("create-agent: %v", err)
	}
	if len(pub) != 64 {
		t.Errorf("created pubkey = %q", pub)
	}
	if got := read(); len(got) != 1 || got[0].Name != "helper" {
		t.Errorf("create-agent did not register: %+v", got)
	}

	if err := tools.GrantAgent("proxmox-box", []string{pub}); err != nil {
		t.Fatalf("grant-agent: %v", err)
	}

	agents, err := tools.ManageAgent("")
	if err != nil {
		t.Fatalf("manage-agent list: %v", err)
	}
	if len(agents) != 1 {
		t.Errorf("manage-agent list got %d, want 1", len(agents))
	}
	if _, err := tools.ManageAgent("helper"); err != nil {
		t.Fatalf("manage-agent remove: %v", err)
	}
	if got := read(); len(got) != 0 {
		t.Errorf("manage-agent remove did not clear: %+v", got)
	}
}

// TestPrimaryChannel: the registry row records the first non-empty channel, or
// "" (the default freehold channel) when the list is empty.
func TestPrimaryChannel(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"", "  "}, ""},
		{[]string{"", "#freehold"}, "#freehold"},
		{[]string{"#vault", "#other"}, "#vault"},
	} {
		if got := primaryChannel(tc.in); got != tc.want {
			t.Errorf("primaryChannel(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestToolsNilGuards: the toolset refuses cleanly when the console isn't bound.
func TestToolsNilGuards(t *testing.T) {
	var tools Tools
	if _, err := tools.CreateAgent("x", "", nil, false); err == nil {
		t.Error("create-agent with nil console must fail")
	}
	if err := tools.GrantAgent("r", []string{strings.Repeat("a", 64)}); err == nil {
		t.Error("grant-agent with nil console must fail")
	}
	if _, err := tools.ManageAgent(""); err == nil {
		t.Error("manage-agent with nil console must fail")
	}
}
