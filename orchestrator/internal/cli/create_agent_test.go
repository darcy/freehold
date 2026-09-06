package cli

import (
	"testing"

	"freehold/orchestrator/internal/config"
)

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

// TestCreateAgentNameError: a created agent name must not sanitize to the CPA's
// pod name or an existing created agent's — that would clobber the CPA pod.
func TestCreateAgentNameError(t *testing.T) {
	cfg := &config.Config{CPAName: "freehold", Agents: []config.AgentSpec{{Name: "helper"}}}
	cases := []struct {
		name string
		want bool // true = should be rejected
	}{
		{"helper", true},
		{"Freehold", true}, // sanitize collides with CPA
		{"freehold", true},
		{"scribe", false},
		{"my-helper", false},
	}
	for _, c := range cases {
		err := createAgentNameError(cfg, c.name)
		if c.want && err == nil {
			t.Errorf("createAgentNameError(%q): expected rejection, got nil", c.name)
		}
		if !c.want && err != nil {
			t.Errorf("createAgentNameError(%q): unexpected rejection: %v", c.name, err)
		}
	}
}
