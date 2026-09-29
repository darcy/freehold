package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"freehold/control-plane/state"
)

// settingsServer builds a console Server over a temp CP state dir with no auth
// (the loopback posture: requireSession admits everything).
func settingsServer(t *testing.T) *Server {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	return &Server{Store: store}
}

// TestSettingsRoute: GET serves the (empty) settings, POST persists a valid
// timezone (visible to a fresh read), a bad IANA name is a 400, and an empty
// value clears it.
func TestSettingsRoute(t *testing.T) {
	srv := settingsServer(t)
	h := httptest.NewServer(srv)
	defer h.Close()

	get := func() (string, int) {
		res, err := http.Get(h.URL + "/api/settings")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer res.Body.Close()
		var v struct {
			Settings *state.Settings `json:"settings"`
		}
		if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
			t.Fatalf("decode: %v", err)
		}
		tz := ""
		if v.Settings != nil {
			tz = v.Settings.OperatorTZ
		}
		return tz, res.StatusCode
	}
	post := func(body string) int {
		res, err := http.Post(h.URL+"/api/settings", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	if tz, code := get(); code != 200 || tz != "" {
		t.Fatalf("fresh GET = %q/%d, want empty/200", tz, code)
	}
	if code := post(`{"operator_tz": "America/Chicago"}`); code != 200 {
		t.Fatalf("valid tz POST = %d, want 200", code)
	}
	if tz, _ := get(); tz != "America/Chicago" {
		t.Fatalf("after POST GET = %q, want America/Chicago", tz)
	}
	// Persisted where the build reads it (state.json on disk, not just memory).
	if ro, err := state.LoadReadOnly(srv.Store.Dir()); err != nil || ro.Settings == nil || ro.Settings.OperatorTZ != "America/Chicago" {
		t.Fatalf("settings not persisted: %+v (%v)", ro, err)
	}
	if code := post(`{"operator_tz": "Mars/Olympus"}`); code != 400 {
		t.Fatalf("bad tz POST = %d, want 400", code)
	}
	if code := post(`{"operator_tz": ""}`); code != 200 {
		t.Fatalf("clear POST = %d, want 200", code)
	}
	if tz, _ := get(); tz != "" {
		t.Fatalf("after clear GET = %q, want empty", tz)
	}
	if code := post(`{"other": 1}`); code != 400 {
		t.Fatalf("body without operator_tz = %d, want 400", code)
	}
}
