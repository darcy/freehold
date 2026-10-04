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
		"systemctl start freehold-runner 2>/dev/null || true",
		"systemctl start freehold-runner-cp-local-root 2>/dev/null || true",
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

	// The post-reconcile refresh renders from CAPTURED argvs: the serve's
	// own --addr drives the healthz probe, every door re-launches.
	s = reviveScriptFromArgvs(
		&DeployCpSpec{StateDir: state, BinDir: bin},
		bin+"/freehold-console serve --state-dir "+state+" --addr 0.0.0.0:8080",
		"/srv/data/cp/bin/freehold-agent-tools serve --state-dir /srv/data/cp/agent-tools",
		[]string{"freehold-runner-cp-local-root.service /srv/data/cp/bin/freehold-runner serve --state-dir " + state + "/runner/cp-local-root --addr 0.0.0.0:8797"},
	)
	for _, want := range []string{
		"setsid nohup " + bin + "/freehold-console serve",
		"curl -fsS -m 3 http://127.0.0.1:8080/healthz",
		"systemctl start freehold-runner-cp-local-root.service 2>/dev/null || true",
		"setsid nohup /srv/data/cp/bin/freehold-agent-tools serve",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("refreshed script missing %q\n---\n%s", want, s)
		}
	}
}

func strPtr(s string) *string { return &s }

// TestRunnerUnitFilePinsResilience: the runner's unit is a REAL file —
// Restart=on-failure (a crash returns) + WantedBy=multi-user (enabled = a
// guest reboot returns). A transient (--collect) removed the unit on the
// first crash and the world's control path went dark with /healthz green.
func TestRunnerUnitFilePinsResilience(t *testing.T) {
	s := runnerUnitFile("freehold co-located runner", "/srv/data/cp/bin/freehold-runner serve --state-dir /x")
	for _, want := range []string{"Restart=on-failure", "RestartSec=5", "WantedBy=multi-user.target", "ExecStart=/srv/data/cp/bin/freehold-runner serve --state-dir /x"} {
		if !strings.Contains(s, want) {
			t.Fatalf("runner unit missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "--collect") {
		t.Fatalf("runner unit must not be a transient:\n%s", s)
	}
}
