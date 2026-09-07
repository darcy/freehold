package console

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTeardownParsesResult verifies the client hits POST /api/teardown with the
// session cookie and parses the returned counts (the CP-first teardown hand-off).
func TestTeardownParsesResult(t *testing.T) {
	var gotPath, gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"runners_removed":3,"agents_removed":1,"dns_removed":2}`))
	}))
	defer srv.Close()

	c := WithCookie(srv.URL+"/", "fh_session=tok123")
	gotPath, gotCookie = "", ""
	res, err := c.Teardown()
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/teardown" {
		t.Fatalf("path = %q, want /api/teardown", gotPath)
	}
	if gotCookie != "fh_session=tok123" {
		t.Fatalf("cookie = %q", gotCookie)
	}
	if res.RunnersRemoved != 3 || res.AgentsRemoved != 1 || res.DnsRemoved != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
}
