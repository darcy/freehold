package delegate

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
