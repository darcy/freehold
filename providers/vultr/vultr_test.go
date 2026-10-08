package vultr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(h http.Handler) (*Client, *httptest.Server) {
	srv := httptest.NewServer(h)
	return &Client{Token: "tok", BaseURL: srv.URL, HTTP: srv.Client()}, srv
}

func TestEnsureSSHKeyReusesExisting(t *testing.T) {
	creates := 0
	c, srv := testClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/ssh-keys"):
			w.Write([]byte(`{"ssh_keys":[{"id":"k1","ssh_key":"ssh-ed25519 AAA existing"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/ssh-keys":
			creates++
			w.Write([]byte(`{"ssh_key":{"id":"k2"}}`))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	id, err := c.EnsureSSHKey(context.Background(), "door", "ssh-ed25519 AAA existing")
	if err != nil || id != "k1" {
		t.Fatalf("EnsureSSHKey reuse: id=%q err=%v", id, err)
	}
	if creates != 0 {
		t.Fatalf("existing key was re-created %d times", creates)
	}
	id, err = c.EnsureSSHKey(context.Background(), "door", "ssh-ed25519 AAA fresh")
	if err != nil || id != "k2" {
		t.Fatalf("EnsureSSHKey create: id=%q err=%v", id, err)
	}
	if creates != 1 {
		t.Fatalf("expected exactly one create, got %d", creates)
	}
}

func TestCreateInstanceRequiresID(t *testing.T) {
	c, srv := testClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"instance":{"id":"i-1"}}`))
	}))
	defer srv.Close()
	id, err := c.CreateInstance(context.Background(), map[string]any{"region": "ewr", "plan": "vc2-4c-8gb", "label": "freehold-test"}, "k1")
	if err != nil || id != "i-1" {
		t.Fatalf("create: id=%q err=%v", id, err)
	}
}

func TestWaitActiveWaitsForAssignedIP(t *testing.T) {
	polls := 0
	c, srv := testClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		if polls < 3 {
			w.Write([]byte(`{"instance":{"status":"active","main_ip":"0.0.0.0"}}`))
			return
		}
		w.Write([]byte(`{"instance":{"status":"active","main_ip":"203.0.113.7"}}`))
	}))
	defer srv.Close()
	ip, err := c.WaitActive(context.Background(), "i-1", 20*time.Second)
	if err != nil || ip != "203.0.113.7" {
		t.Fatalf("wait: ip=%q err=%v (polls=%d)", ip, err, polls)
	}
}

func TestWaitActiveTimesOut(t *testing.T) {
	c, srv := testClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"instance":{"status":"pending","main_ip":"0.0.0.0"}}`))
	}))
	defer srv.Close()
	if _, err := c.WaitActive(context.Background(), "i-1", 100*time.Millisecond); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestDestroyIdempotentAndRetries(t *testing.T) {
	// Only the DELETEs count: this box's port-watcher probes new listeners
	// with a GET / (observed live) — environment noise, not the client.
	deletes := 0
	c, srv := testClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v2/instances/i-1" {
			return
		}
		deletes++
		if deletes == 1 {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"still settling"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := c.Destroy(context.Background(), "i-1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if deletes != 2 {
		t.Fatalf("expected retry after 409, deletes=%d", deletes)
	}
}

func TestDestroy404IsGone(t *testing.T) {
	c, srv := testClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := c.Destroy(context.Background(), "i-gone"); err != nil {
		t.Fatalf("destroy of a gone instance must be a no-op: %v", err)
	}
}
