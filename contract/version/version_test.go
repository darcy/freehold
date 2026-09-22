package version

import "testing"

func TestChannel(t *testing.T) {
	cases := map[string]string{
		"v0.7.0":               "stable",
		"v0.7.0-rc.1":          "rc",
		"v0.7.0-rc.12":         "rc",
		"v0.7.0-4-gabc123":     "dev",
		"v0.7.0-rc.1-4-gabc123": "dev",
		"v0.7.0-rc.1-dirty":    "rc",
		"main-gabc123":         "dev",
		"v0.7.0-4-gabc-dirty":  "dev",
		"dev":                  "dev",
		"":                     "dev",
		"0.7.0":                "dev",
		"v0.7.0-beta.1":        "dev",
	}
	for in, want := range cases {
		if got := Channel(in); got != want {
			t.Errorf("Channel(%q) = %q, want %q", in, got, want)
		}
	}
}
