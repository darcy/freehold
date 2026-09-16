package cert

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPurgeChallengeRecords(t *testing.T) {
	var del int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			w.WriteHeader(200)
			// zone covering freehold.technology (the covering one) + a neighbour.
			_, _ = w.Write([]byte(`{"success":true,"result_info":{"page":1,"total_pages":1},
				"result":[{"id":"z-neighbour","name":"example.com"},{"id":"z-fh","name":"freehold.technology"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/zones/z-fh/dns_records":
			// two stacked TXT challenge records (the redundancy we purge).
			_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"r1"},{"id":"r2"}]}`))
		case r.Method == http.MethodDelete && len(r.URL.Path) > 0:
			del++
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	n, err := purgeChallengeRecords("relay.migrate.freehold.technology", "cloudflare", map[string]string{
		"CLOUDFLARE_DNS_API_TOKEN": "test-token",
		"CLOUDFLARE_BASE_URL":      srv.URL,
	})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged %d records, want 2", n)
	}
	if del != 2 {
		t.Fatalf("delete calls = %d, want 2", del)
	}

	// Non-cloudflare providers are a no-op (their lego impl cleans internally).
	n, err = purgeChallengeRecords("relay.migrate.freehold.technology", "route53", nil)
	if err != nil || n != 0 {
		t.Fatalf("non-cloudflare purge: n=%d err=%v, want 0,nil", n, err)
	}
}

func TestPurgeChallengeRecords_NameFilterNoTrailingDot(t *testing.T) {
	var gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones":
			_, _ = w.Write([]byte(`{"success":true,"result_info":{"page":1,"total_pages":1},"result":[{"id":"z-fh","name":"freehold.technology"}]}`))
		case r.URL.Path == "/zones/z-fh/dns_records":
			gotName = r.URL.Query().Get("name")
			if r.URL.Query().Get("type") != "TXT" {
				t.Errorf("expected type=TXT, got %q", r.URL.Query().Get("type"))
			}
			_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	defer srv.Close()

	_, err := purgeChallengeRecords("relay.migrate.freehold.technology", "cloudflare", map[string]string{
		"CLOUDFLARE_DNS_API_TOKEN": "t",
		"CLOUDFLARE_BASE_URL":      srv.URL,
	})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if gotName != "_acme-challenge.relay.migrate.freehold.technology" {
		t.Fatalf("name filter = %q, want no trailing dot", gotName)
	}
}
