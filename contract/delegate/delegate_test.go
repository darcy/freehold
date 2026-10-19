package delegate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"freehold/contract/wire"
)

// TestEditChannelAuthPublishesKind9002 pins the migration wire: an edit publishes
// a kind-9002 event carrying the h tag plus every supplied metadata tag (name /
// visibility), signed by the supplied identity.
func TestEditChannelAuthPublishesKind9002(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	err := EditChannelAuth(srv.URL, srv.URL, secret, "chan-id",
		[]string{"name", "#freehold-ai"},
		[]string{"visibility", "private"},
	)
	if err != nil {
		t.Fatalf("EditChannelAuth: %v", err)
	}
	if got == nil {
		t.Fatal("no event published")
	}
	if k := int(got["kind"].(float64)); k != wire.EditMetadata {
		t.Fatalf("kind = %d, want %d (9002)", k, wire.EditMetadata)
	}
	tags := map[string]string{}
	for _, raw := range got["tags"].([]interface{}) {
		row := raw.([]interface{})
		if len(row) >= 2 {
			tags[row[0].(string)] = row[1].(string)
		}
	}
	if tags["h"] != "chan-id" || tags["name"] != "#freehold-ai" || tags["visibility"] != "private" {
		t.Fatalf("tags = %v, want h + name + visibility", tags)
	}
}

// TestQueryTaggedAuthVerifiesAndKeepsTags pins the mention-watcher query: the
// relay's filter rides #p/#e/ids/authors as given, signature-verified events
// keep their tags, and an unverifiable event is dropped — never surfaced.
func TestQueryTaggedAuthVerifiesAndKeepsTags(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	pk, id, sig, err := wire.SignEvent(secret, wire.TextNote, 1700, [][]string{
		{"e", "issue-id", "", "root"},
		{"p", "agent-pk"},
		{"a", "1621:author-pk:d"},
	}, "please look at this")
	if err != nil {
		t.Fatalf("SignEvent: %v", err)
	}
	good := map[string]interface{}{
		"id": id, "pubkey": pk, "created_at": 1700, "kind": wire.TextNote,
		"tags":    [][]string{{"e", "issue-id", "", "root"}, {"p", "agent-pk"}, {"a", "1621:author-pk:d"}},
		"content": "please look at this", "sig": sig,
	}
	bad := map[string]interface{}{
		"id": "bad", "pubkey": pk, "created_at": 1701, "kind": wire.TextNote,
		"tags": [][]string{{"p", "agent-pk"}}, "content": "forged", "sig": "00",
	}
	var gotFilter map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body) > 0 {
			gotFilter = body[0]
		}
		_ = json.NewEncoder(w).Encode([]interface{}{good, bad})
	}))
	defer srv.Close()

	events, err := QueryTaggedAuth(srv.URL, srv.URL, secret, TaggedFilter{
		Kinds: []uint32{wire.TextNote, wire.GitIssue}, P: []string{"agent-pk"}, Since: 1600, Limit: 42,
	})
	if err != nil {
		t.Fatalf("QueryTaggedAuth: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 (the forged one dropped)", len(events))
	}
	ev := events[0]
	if ev.ID != id || ev.Author != pk || ev.Content != "please look at this" {
		t.Fatalf("event fields wrong: %+v", ev)
	}
	if ev.Root() != "issue-id" || ev.Tag("a") != "1621:author-pk:d" || ev.Tag("p") != "agent-pk" {
		t.Fatalf("tag accessors wrong: root=%q a=%q p=%q", ev.Root(), ev.Tag("a"), ev.Tag("p"))
	}
	if ev.HasChannel() {
		t.Fatal("HasChannel true on a channel-less mention")
	}
	ev.Tags = [][]string{{"h", "chan"}}
	if !ev.HasChannel() {
		t.Fatal("HasChannel false with an h tag")
	}
	if gotFilter == nil {
		t.Fatal("relay saw no filter")
	}
	// Kinds MUST be JSON numbers: the relay's filter parses them numerically,
	// so a string-kind filter collapses to kind 0 and matches nothing.
	ks, ok := gotFilter["kinds"].([]interface{})
	if !ok || len(ks) != 2 {
		t.Fatalf("kinds filter = %v, want two entries", gotFilter["kinds"])
	}
	for _, k := range ks {
		if _, ok := k.(float64); !ok {
			t.Fatalf("kind %v (%T) is not a JSON number", k, k)
		}
	}
	if f := fmt.Sprint(gotFilter["#p"]); !strings.Contains(f, "agent-pk") {
		t.Fatalf("#p filter = %v", gotFilter["#p"])
	}
	if _, ok := gotFilter["limit"]; !ok {
		t.Fatal("limit not sent")
	}
}
