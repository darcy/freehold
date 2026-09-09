package dnsman

import "testing"

func TestMatchZone(t *testing.T) {
	zones := []cfZone{
		{ID: "apex", Name: "freehold.technology"},
		{ID: "other", Name: "example.com"},
	}
	cases := []struct {
		host string
		want string // zone id; "" = no match
	}{
		{"relay.librem.freehold.technology", "apex"},
		{"cp.librem.freehold.technology", "apex"},
		{"_acme-challenge.freehold.technology", "apex"},
		{"freehold.technology.", "apex"},         // trailing dot normalized
		{"relay.freehold-test.darcydev.net", ""}, // not in this account
		{"relay.example.com", "other"},
	}
	for _, c := range cases {
		z, ok := matchZone(c.host, zones)
		if (c.want == "") == ok {
			t.Fatalf("matchZone(%q) ok=%v, want %q", c.host, ok, c.want)
		}
		if ok && z.ID != c.want {
			t.Fatalf("matchZone(%q) = %s, want %s", c.host, z.ID, c.want)
		}
	}
}

// NewCloudflare requires the API token (management is record CRUD, not a
// challenge-only token class).
func TestNewCloudflareRequiresToken(t *testing.T) {
	if _, err := NewCloudflare(map[string]string{}); err == nil {
		t.Fatal("expected an error without " + EnvToken)
	}
}
