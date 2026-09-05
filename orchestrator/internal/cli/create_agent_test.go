package cli

import "testing"

func TestParseCreateReq(t *testing.T) {
	cases := []struct {
		in      string
		name    string
		purpose string
		ok      bool
	}{
		{"create-agent name: helper purpose: helps with installs", "helper", "helps with installs", true},
		{"create-agent helper", "helper", "", true},
		{"Create-Agent: helper purpose: please monitor builds", "helper", "please monitor builds", true},
		{"create-agent name=helper purpose=do the thing", "helper", "do the thing", true},
		{"just a normal hello", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		name, purpose, ok := parseCreateReq(c.in)
		if ok != c.ok || (ok && (name != c.name || (c.purpose != "" && purpose != c.purpose))) {
			t.Errorf("parseCreateReq(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, name, purpose, ok, c.name, c.purpose, c.ok)
		}
	}
}
