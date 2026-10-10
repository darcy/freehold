package pangolin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The fake serves the verified integration-API shapes (Pangolin 1.24.0):
// every response rides the {success, error, data} envelope, the Bearer key
// is `apiKeyId.secret`, and the routes match what the client calls.
func fakePangolin(t *testing.T) (*Client, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer yi5o.thesecret" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"success":false,"error":true,"message":"Invalid API key"}`))
			return
		}
		body := `{"success":true,"error":false,"data":`
		switch r.URL.Path {
		case "/v1/org/freehold/domains":
			body += `{"domains":[{"domainId":"domain1","baseDomain":"pangolin.local","verified":true,"type":"wildcard"}]}`
		case "/v1/org":
			body += `{"orgId":"freehold","name":"freehold","subnet":"100.90.0.0/16"}`
		case "/v1/org/freehold/site":
			body += `{"siteId":1,"name":"librem","type":"newt","newtId":"noo1","newtSecret":"sek","address":"100.90.0.0/16","online":false}`
		case "/v1/org/freehold/site/1/resource":
			body += `{"resourceId":1,"name":"litellm","subdomain":"litellm","fullDomain":"litellm.pangolin.local","domainId":"domain1","ssl":true,"enabled":true}`
		case "/v1/resource/1/target":
			body += `{"targetId":1,"siteId":1,"ip":"10.78.0.13","port":31400,"method":"http"}`
		case "/v1/resource/1":
			if r.Method == http.MethodPost {
				body += `null` // the sso gate update
			} else {
				body += `null` // the delete
			}
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"success":false,"error":true,"message":"The requests url is not found"}"`))
			return
		}
		body += `,"message":"ok","status":200}`
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, Key: "yi5o.thesecret"}, &calls
}

func TestClientDrivesTheVerifiedShapes(t *testing.T) {
	c, calls := fakePangolin(t)
	ctx := context.Background()

	if err := c.CreateOrg(ctx, "freehold", "freehold", "100.90.0.0/16", "100.91.0.0/24"); err != nil {
		t.Fatal(err)
	}
	s, err := c.CreateSite(ctx, "freehold", "librem")
	if err != nil {
		t.Fatal(err)
	}
	if s.SiteID != 1 || s.NewtID != "noo1" || s.NewtSecret != "sek" {
		t.Fatalf("the site must carry the newt identity: %+v", s)
	}
	domains, err := c.ListDomains(ctx, "freehold")
	if err != nil || len(domains) != 1 || domains[0].BaseDomain != "pangolin.local" {
		t.Fatalf("domains: %+v %v", domains, err)
	}
	r, err := c.CreateResource(ctx, "freehold", s.SiteID, "litellm", "litellm", "domain1")
	if err != nil {
		t.Fatal(err)
	}
	if r.FullDomain != "litellm.pangolin.local" {
		t.Fatalf("the resource must resolve the fqdn: %+v", r)
	}
	if err := c.SetTarget(ctx, r.ResourceID, s.SiteID, "10.78.0.13", 31400); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteResource(ctx, r.ResourceID); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /v1/org",
		"PUT /v1/org/freehold/site",
		"GET /v1/org/freehold/domains",
		"PUT /v1/org/freehold/site/1/resource",
		"POST /v1/resource/1", // the sso gate — created resources are never open
		"PUT /v1/resource/1/target",
		"DELETE /v1/resource/1",
	}
	for i, w := range want {
		if (*calls)[i] != w {
			t.Fatalf("call %d: got %s want %s (all: %v)", i, (*calls)[i], w, *calls)
		}
	}
}

func TestClientSurfacesTheErrorEnvelope(t *testing.T) {
	c, _ := fakePangolin(t)
	c.Key = "yi5o.wrong"
	if err := c.CreateOrg(context.Background(), "x", "x", "100.90.0.0/16", "100.91.0.0/24"); err == nil || err.Error() != "pangolin: PUT /v1/org: HTTP 401: Invalid API key" {
		t.Fatalf("the error must carry the API's message: %v", err)
	}
}
