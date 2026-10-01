package cpdeploy

import (
	"strings"
	"testing"
)

// TestReviveScript captures the revival contract: the serve + co-located
// runner start lines are the deploy's own, the healthz poll gates the exit,
// and the agent-tools block appears only when its argv is known.
func TestReviveScript(t *testing.T) {
	bin, state, bind := "/srv/data/cp/bin", "/srv/data/cp/control-plane", "127.0.0.1:8080"
	spec := &DeployCpSpec{
		StateDir:         state,
		BinDir:           bin,
		BindAddr:         bind,
		RunnerBinary:     strPtr("/x/runner"),
		RunnerPackage:    strPtr("/box/runner/proxmox-box"),
		AgentToolsBinary: strPtr("/x/agent-tools"),
	}
	s := reviveScript(spec, " --relay-url https://relay.example", "/x/freehold-agent-tools serve --state-dir /srv/data/cp/agent-tools",
		[]string{"freehold-runner-cp-local-root /srv/data/cp/bin/freehold-runner serve --state-dir /srv/data/cp/control-plane/runner/cp-local-root --addr 0.0.0.0:8797"})
	for _, want := range []string{
		"#!/bin/sh\n",
		bin + "/freehold-console serve --state-dir " + state + " --addr " + bind,
		"for i in $(seq 1 15)",
		"curl -fsS -m 3 http://" + bind + "/healthz",
		"systemd-run --unit=freehold-runner --collect " + bin + "/freehold-runner serve --state-dir " + state + "/runner/proxmox-box",
		"systemd-run --unit=freehold-runner-cp-local-root --collect /srv/data/cp/bin/freehold-runner serve",
		"setsid nohup /x/freehold-agent-tools serve --state-dir /srv/data/cp/agent-tools",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("revive script missing %q\n---\n%s", want, s)
		}
	}

	// No runner shipped → no runner block; no argv → no agent-tools block.
	bare := &DeployCpSpec{StateDir: state, BinDir: bin, BindAddr: bind}
	s = reviveScript(bare, "", "", nil)
	if strings.Contains(s, "freehold-runner serve") || strings.Contains(s, "freehold-agent-tools serve") {
		t.Fatalf("revive script started absent components:\n%s", s)
	}
}

func strPtr(s string) *string { return &s }
