package cpbuild

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"freehold/contract/crypto"
	"freehold/contract/delegate"
	"freehold/contract/wire"
)

// fakeFirstRunRelay is a minimal relay double for the first-run stages: /query
// serves the configured result set, /events records what was published.
type fakeFirstRunRelay struct {
	srv     *httptest.Server
	mu      sync.Mutex
	query   []map[string]interface{}
	events  []string
	queries []string // raw filter bodies
}

func newFakeFirstRunRelay(t *testing.T, query []map[string]interface{}) *fakeFirstRunRelay {
	t.Helper()
	f := &fakeFirstRunRelay{query: query}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/query"):
			f.mu.Lock()
			f.queries = append(f.queries, string(body))
			// The real bridge honors body[0] only — model that exactly, so a
			// caller leaning on extra filters fails here the way it would
			// live. Match the first filter's kinds against the result set.
			var filters []map[string]interface{}
			_ = json.Unmarshal(body, &filters)
			// The first-run guard reads ANY #freehold message — a #t in its
			// filter is the re-post bug (the relay applies the SQL limit
			// before post-filtering tags), so fail the moment it reappears.
			for _, fl := range filters {
				if _, has := fl["#t"]; has {
					t.Errorf("the guard filter must not constrain #t, got %s", body)
					break
				}
			}
			var out []map[string]interface{}
			if len(filters) > 0 {
				if ks, ok := filters[0]["kinds"].([]interface{}); ok && len(ks) > 0 {
					want := int(ks[0].(float64))
					for _, ev := range f.query {
						if int(ev["kind"].(float64)) == want {
							out = append(out, ev)
						}
					}
				}
			}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/events"):
			var published map[string]interface{}
			_ = json.Unmarshal(body, &published)
			f.mu.Lock()
			f.events = append(f.events, string(body))
			// A relay serves what was published: published events join the
			// query result set, so a later dedupe read sees them.
			f.query = append(f.query, published)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFirstRunRelay) published() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// TestOperatorNameArgRoundTrip: the argv hop is base64 — one token, safe for
// any unicode (a raw space would silently eat the flags after it on the
// serve's argv; a quote would break the root sh -c wrapping).
func TestOperatorNameArgRoundTrip(t *testing.T) {
	for _, name := range []string{"Darcy", "Darcy Smith", "O'Brien", "Ana $HOME `id` \"x\"", "李雷"} {
		arg := operatorNameArg(name)
		if strings.ContainsAny(arg, " \t'\"$`") {
			t.Fatalf("%q encodes to argv-unsafe token %q", name, arg)
		}
		got, err := OperatorNameFromArg(arg)
		if err != nil || got != name {
			t.Fatalf("round trip %q: got %q err %v", name, got, err)
		}
	}
	if _, err := OperatorNameFromArg("!!!not base64!!!"); err == nil {
		t.Fatal("garbage must error, not silently render Operator")
	}
}

// TestAgentToolsServeFlagsOperatorName: the name rides b64 and never raw.
func TestAgentToolsServeFlagsOperatorName(t *testing.T) {
	s := &Spec{OperatorName: "Darcy Smith", AgentIdentityDir: "/tmp/x"}
	flags := s.agentToolsServeFlags()
	if !strings.Contains(flags, " --operator-name-b64 ") {
		t.Fatalf("serve flags must carry the b64 name, got %s", flags)
	}
	if strings.Contains(flags, "--operator-name ") {
		t.Fatalf("the raw name must never ride the argv, got %s", flags)
	}
	s2 := &Spec{AgentIdentityDir: "/tmp/x"}
	if strings.Contains(s2.agentToolsServeFlags(), "operator-name") {
		t.Fatalf("no name set: no flag, got %s", s2.agentToolsServeFlags())
	}
}

// TestPostFreeholdWelcomeMarkerGuard: an empty #freehold posts once (kind 9,
// h+p+t tags, operator mentioned); ANY prior #freehold message — welcome tag
// or not — posts nothing (the guard is history, never a tag lookup).
func TestPostFreeholdWelcomeMarkerGuard(t *testing.T) {
	ownerPub := strings.Repeat("ab", 32)
	s := &Spec{
		RelayURL: "", RelayAuthURL: "", // filled below (dial = the fake)
		RelayHost:    "relay.example", // overridden per-server; see specFor
		OwnerPub:     ownerPub,
		OperatorName: "Darcy",
	}
	specFor := func(f *fakeFirstRunRelay) *Spec {
		c := *s
		c.RelayURL = f.srv.URL
		c.RelayAuthURL = f.srv.URL
		c.RelayHost = f.srv.Listener.Addr().String() // host:port => dial hits the fake
		return &c
	}

	sec := make([]byte, 32)
	for i := range sec {
		sec[i] = byte(i + 1)
	}

	f := newFakeFirstRunRelay(t, nil)
	if err := specFor(f).postFreeholdWelcome(sec); err != nil {
		t.Fatal(err)
	}
	pub := f.published()
	if len(pub) != 1 {
		t.Fatalf("an empty #freehold must post exactly one message, got %d events", len(pub))
	}
	var ev struct {
		Kind    int        `json:"kind"`
		Content string     `json:"content"`
		Tags    [][]string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(pub[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != int(delegate.StreamMsgKind) {
		t.Fatalf("kind = %d, want %d", ev.Kind, delegate.StreamMsgKind)
	}
	var h, p, m bool
	for _, tag := range ev.Tags {
		if len(tag) >= 2 && tag[0] == "h" && tag[1] == relayFreeholdChannel {
			h = true
		}
		if len(tag) >= 2 && tag[0] == "p" && tag[1] == ownerPub {
			p = true
		}
		if len(tag) >= 2 && tag[0] == "t" && tag[1] == freeholdWelcomeMarker {
			m = true
		}
	}
	if !h || !p || !m {
		t.Fatalf("tags must carry h(channel)/p(operator)/t(fh-welcome), got %v", ev.Tags)
	}
	if !strings.Contains(ev.Content, "@Darcy") {
		t.Fatalf("the welcome must mention the operator by name, got %q", ev.Content)
	}
	if !strings.Contains(ev.Content, "#general") {
		t.Fatalf("the welcome should point at #general, got %q", ev.Content)
	}

	// A #freehold that already has ANY message — welcome tag or not — must
	// not re-post (the guard is history, not a tag lookup).
	f2 := newFakeFirstRunRelay(t, []map[string]interface{}{{"id": "x", "kind": float64(wire.ChannelMessage)}})
	if err := specFor(f2).postFreeholdWelcome(sec); err != nil {
		t.Fatal(err)
	}
	if got := f2.published(); len(got) != 0 {
		t.Fatalf("existing #freehold history (no tag) must not re-post, got %d events", len(got))
	}

	// No recorded name: the mention comes from the operator's own kind:0 —
	// the name they're known by on the relay (an updated world's case).
	f3 := newFakeFirstRunRelay(t, []map[string]interface{}{
		{"id": "p", "kind": float64(0), "content": `{"name":"Darcy Smith"}`},
	})
	s3 := specFor(f3)
	s3.OperatorName = "" // an updated world has no recorded name
	if err := s3.postFreeholdWelcome(sec); err != nil {
		t.Fatal(err)
	}
	var ev3 struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(f3.published()[0]), &ev3); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ev3.Content, "@Darcy Smith") || strings.Contains(ev3.Content, "@Operator") {
		t.Fatalf("the mention must use the profile name, got %q", ev3.Content)
	}

	// A profile with no usable name falls back to the default.
	f4 := newFakeFirstRunRelay(t, []map[string]interface{}{
		{"id": "p", "kind": float64(0), "content": `{"about":"nameless"}`},
	})
	s4 := specFor(f4)
	s4.OperatorName = ""
	if err := s4.postFreeholdWelcome(sec); err != nil {
		t.Fatal(err)
	}
	var ev4 struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal([]byte(f4.published()[0]), &ev4)
	if !strings.Contains(ev4.Content, "@Operator") {
		t.Fatalf("a nameless profile must fall back to the default, got %q", ev4.Content)
	}

	// The two reads: the history guard (any kind-9) first; the profile read
	// fires only when there is no history.
	f5 := newFakeFirstRunRelay(t, []map[string]interface{}{
		{"id": "m", "kind": float64(delegate.StreamMsgKind), "content": "welcome"},
		{"id": "p", "kind": float64(0), "content": `{"name":"Darcy"}`},
	})
	if err := specFor(f5).postFreeholdWelcome(sec); err != nil {
		t.Fatal(err)
	}
	if got := f5.published(); len(got) != 0 {
		t.Fatalf("existing #freehold history must guard even alongside a profile, got %d events", len(got))
	}
}

// TestStageOperatorProfile: the kind:0 publishes once with the operator's
// name, is skipped when a profile exists, defaults to "Operator", and degrades
// to a WARN (no publish) when the owner key is unresolvable.
func TestStageOperatorProfile(t *testing.T) {
	sec := make([]byte, 32) // scalar 1 — a valid secp256k1 secret
	sec[31] = 1
	pk, err := crypto.PubkeyFromSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	specFor := func(f *fakeFirstRunRelay, name string) *Spec {
		return &Spec{
			RelayURL:     f.srv.URL,
			RelayAuthURL: f.srv.URL,
			RelayHost:    f.srv.Listener.Addr().String(),
			OwnerPub:     pk,
			OwnerSecret:  sec,
			OperatorName: name,
		}
	}

	f := newFakeFirstRunRelay(t, nil)
	report := specFor(f, "Darcy").stageOperatorProfile(nil)
	if got := f.published(); len(got) != 1 {
		t.Fatalf("an absent profile must publish exactly one event, got %d", len(got))
	}
	var ev struct {
		Kind    int    `json:"kind"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(f.published()[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != 0 {
		t.Fatalf("kind = %d, want 0 (profile)", ev.Kind)
	}
	if !strings.Contains(ev.Content, `"name":"Darcy"`) {
		t.Fatalf("the profile must carry the operator's name, got %q", ev.Content)
	}
	if len(report) != 1 || !strings.Contains(report[0], "operator profile published") {
		t.Fatalf("report must name the publish, got %v", report)
	}

	// A profile already on the relay: no publish, "already present" report.
	f2 := newFakeFirstRunRelay(t, []map[string]interface{}{{"id": "x", "kind": float64(0)}})
	report = specFor(f2, "Darcy").stageOperatorProfile(nil)
	if got := f2.published(); len(got) != 0 {
		t.Fatalf("an existing profile must never be overwritten, got %d events", len(got))
	}
	if len(report) != 1 || !strings.Contains(report[0], "already present") {
		t.Fatalf("report must say already present, got %v", report)
	}

	// No OperatorName: the default "Operator" renders.
	f3 := newFakeFirstRunRelay(t, nil)
	if report := specFor(f3, "").stageOperatorProfile(nil); len(f3.published()) != 1 {
		t.Fatalf("the default name must still publish, report %v", report)
	}
	var ev3 struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal([]byte(f3.published()[0]), &ev3)
	if !strings.Contains(ev3.Content, `"name":"Operator"`) {
		t.Fatalf("the default name must be Operator, got %q", ev3.Content)
	}

	// An unresolvable owner key degrades to a WARN, publishing nothing.
	f4 := newFakeFirstRunRelay(t, nil)
	s := specFor(f4, "Darcy")
	s.OwnerSecret = nil // ownerKey falls through to the (absent) sealed store
	report = s.stageOperatorProfile(nil)
	if got := f4.published(); len(got) != 0 {
		t.Fatalf("a key failure must not publish, got %d events", len(got))
	}
	if len(report) != 1 || !strings.HasPrefix(report[0], "WARN: operator profile:") {
		t.Fatalf("a key failure must WARN, got %v", report)
	}
}
