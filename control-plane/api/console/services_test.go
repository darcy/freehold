package console

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"freehold/contract/state"
)

// TestWorldServiceRows registers live local probes for each service kind and
// verifies worldServiceRows reports the co-located health (k3s = any HTTP
// answer, litellm = 2xx, caddy = any answer). The probes use real httptest
// servers so no service URL is ever guessed blind.
func TestWorldServiceRows(t *testing.T) {
	ln := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ln.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // k3s's normal "no client cert" answer
	}))
	defer tls.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	snap := state.ControlPlaneState{Services: map[string]state.WorldService{
		"k3s":     {Kind: "k3s", URL: tls.URL},    // any answer incl 401 = up
		"litellm": {Kind: "litellm", URL: ln.URL}, // 200 = up
		"caddy":   {Kind: "caddy", URL: ln.URL},   // any answer = up
	}}
	rows := worldServiceRows(snap)
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	for _, r := range rows {
		if !r.Up {
			t.Errorf("service %s should be up, got detail=%q", r.Name, r.Detail)
		}
	}

	// A litellm 5xx must read as DOWN (gateway health requires 2xx).
	snap.Services["litellm"] = state.WorldService{Kind: "litellm", URL: bad.URL}
	for _, r := range worldServiceRows(snap) {
		if r.Name == "litellm" && r.Up {
			t.Error("litellm 500 should report down")
		}
	}
}
