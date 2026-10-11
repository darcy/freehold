package cpdeploy

import (
	"strings"
	"testing"
)

// TestFilterArgvToFlags pins the tolerant relaunch: captured unit argvs replay
// against the NEW binary after a deploy, and a flag the new binary no longer
// defines is a flag-parse exit (systemd crash-loop) — the filter drops exactly
// those (flag + its value), loudly (the caller prints the drops), and keeps
// everything else byte-intact.
func TestFilterArgvToFlags(t *testing.T) {
	usage := strings.Join([]string{
		"Usage of freehold-agent-tools serve:",
		"  -addr string",
		"  -state-dir string",
		"  -relay-url string",
		"  -cp-host string",
		"  -cp-lxc uint",
		"  -pangolin-b64 string",
	}, "\n")

	// A flag removed upstream (with its value) goes; the rest survives.
	argv := "/srv/data/cp/bin/freehold-agent-tools serve --state-dir /srv/data/cp/agent-tools --addr 0.0.0.0:8089 --dead-flag deadvalue --relay-url http://relay.librem.freehold.technology:3000 --cp-host cp.librem.freehold.technology --cp-lxc 103"
	got, dropped := filterArgvToFlags(argv, usage)
	want := "/srv/data/cp/bin/freehold-agent-tools serve --state-dir /srv/data/cp/agent-tools --addr 0.0.0.0:8089 --relay-url http://relay.librem.freehold.technology:3000 --cp-host cp.librem.freehold.technology --cp-lxc 103"
	if got != want {
		t.Fatalf("filtered argv:\n got %s\nwant %s", got, want)
	}
	if len(dropped) != 1 || dropped[0] != "dead-flag" {
		t.Fatalf("dropped: %v", dropped)
	}

	// A trailing flag with NO value (defensive): drop just the flag.
	got, dropped = filterArgvToFlags("serve --state-dir /x --dead", usage)
	if got != "serve --state-dir /x" || len(dropped) != 1 || dropped[0] != "dead" {
		t.Fatalf("trailing unknown flag: %q %v", got, dropped)
	}

	// A dash-leading token IS a flag by the argv shape contract (no flag's
	// VALUE starts with a dash): "-notavalue" walks as an undefined flag and
	// drops on its own.
	got, dropped = filterArgvToFlags("serve --dead -notavalue --cp-host cp.example.test", usage)
	if got != "serve --cp-host cp.example.test" || len(dropped) != 2 {
		t.Fatalf("dash-leading token handling: %q %v", got, dropped)
	}

	// Fully-known argv: byte-identical.
	good := "/bin/freehold-agent-tools serve --state-dir /srv/data/cp/agent-tools --cp-lxc 103"
	got, dropped = filterArgvToFlags(good, usage)
	if got != good || len(dropped) != 0 {
		t.Fatalf("known argv must pass through: %q %v", got, dropped)
	}

	// An empty usage (a failed probe) defines nothing: the caller keeps the
	// argv unfiltered — assert the helper itself never eats a known argv's
	// flags when handed nothing... it must treat EVERYTHING as unknown, which
	// is why the caller guards on an empty probe result.
	got, dropped = filterArgvToFlags("serve --state-dir /x", "")
	if got != "serve" || len(dropped) != 1 {
		t.Fatalf("empty usage drops all flags: %q %v", got, dropped)
	}
}
