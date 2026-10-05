package cpdeploy

import (
	"strings"
	"testing"
)

// TestReviveScript captures the revival contract: the console, the co-located
// runner, the doors and agent-tools are all STARTED as real enabled units
// (the boot re-runs them — the script is belt and suspenders), and the
// healthz poll gates the exit.
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
		"systemctl start freehold-console 2>/dev/null || true",
		"for i in $(seq 1 15)",
		"curl -fsS -m 3 http://" + bind + "/healthz",
		"systemctl start freehold-runner 2>/dev/null || true",
		"systemctl start freehold-runner-cp-local-root 2>/dev/null || true",
		"systemctl start freehold-agent-tools 2>/dev/null || true",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("revive script missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "nohup") {
		t.Fatalf("revive script must not nohup-launch anything (the units own the processes):\n%s", s)
	}

	// The post-reconcile refresh renders the same unit-start contract from
	// the captured door list.
	s = reviveScriptFromArgvs(
		&DeployCpSpec{StateDir: state, BinDir: bin},
		bin+"/freehold-console serve --state-dir "+state+" --addr 0.0.0.0:8080",
		"/srv/data/cp/bin/freehold-agent-tools serve --state-dir /srv/data/cp/agent-tools",
		[]string{"freehold-runner-cp-local-root.service /srv/data/cp/bin/freehold-runner serve --state-dir " + state + "/runner/cp-local-root --addr 0.0.0.0:8797"},
	)
	for _, want := range []string{
		"systemctl start freehold-console 2>/dev/null || true",
		"curl -fsS -m 3 http://127.0.0.1:8080/healthz",
		"systemctl start freehold-runner-cp-local-root.service 2>/dev/null || true",
		"systemctl start freehold-agent-tools 2>/dev/null || true",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("refreshed script missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "nohup") {
		t.Fatalf("refreshed script must not nohup-launch anything:\n%s", s)
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
