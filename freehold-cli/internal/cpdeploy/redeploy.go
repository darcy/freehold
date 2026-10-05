package cpdeploy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/deploy"
	"freehold/providers/proxmox"
)

// runnerDir is the guest dir of the co-located runner package.
func (spec *DeployCpSpec) runnerDir() string {
	if spec.RunnerPackage == nil {
		return ""
	}
	return fmt.Sprintf("%s/runner/%s", spec.StateDir, filepathBase(*spec.RunnerPackage))
}

// Redeploy replaces the CP's binaries in an EXISTING plane — console,
// agent-tools, and (when provided) the co-located runner — then restarts serve
// and waits for /healthz green. It is the update path: unlike DeployCp it never
// touches runner identity, grants, or sealed secrets, and never rotates the
// substrate SSH key (those are install / re-adopt concerns). Serve flags come
// from the same serveFlags builder DeployCp uses, so the two can't drift.
func Redeploy(t Transport, spec *DeployCpSpec) error {
	if err := bootstrap.PlainPath(spec.StateDir); err != nil {
		return err
	}
	if err := deploy.SafeDeployDir(spec.StateDir); err != nil {
		return err
	}
	if err := bootstrap.PlainPath(spec.BinDir); err != nil {
		return err
	}
	if err := deploy.SafeDeployDir(spec.BinDir); err != nil {
		return err
	}
	if spec.BinaryPath == "" {
		return fmt.Errorf("redeploy needs the console binary path")
	}

	mkdir := fmt.Sprintf("mkdir -p %s/console && mkdir -p %s && rm -f %s/freehold-console.b64",
		spec.StateDir, spec.BinDir, spec.BinDir)
	if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, mkdir), "mkdir deploy dirs", 30); err != nil {
		return err
	}
	if err := stopPriorServe(t, spec, "stop prior control plane"); err != nil {
		return err
	}
	// Console binary.
	if err := shipFile(t, spec, spec.BinaryPath, spec.BinDir+"/freehold-console", "console binary"); err != nil {
		return err
	}
	// Agent-tools: capture its running argv BEFORE stopping, replace the
	// binary, then relaunch the SAME argv so the (new) server comes back with
	// its CP-derived serve flags. Otherwise update would leave the toolset down
	// and world_migrate would fail. Empty when it wasn't running.
	atArgv := ""
	if spec.AgentToolsBinary != nil && *spec.AgentToolsBinary != "" {
		atArgv = captureAgentToolsArgv(t, spec)
		if err := stopAgentTools(t, spec); err != nil {
			return err
		}
		if err := shipFile(t, spec, *spec.AgentToolsBinary, spec.BinDir+"/freehold-agent-tools", "agent-tools binary"); err != nil {
			return err
		}
	}

	restartRunner := spec.RunnerBinary != nil && *spec.RunnerBinary != "" && spec.RunnerPackage != nil
	// Capture the doors' argvs BEFORE stopping them (below) — the transient
	// units' argv exists only while they run, and the revive script re-launches
	// the doors from it (the build's reconcile re-stages them too, but the
	// guest-handoff revival runs between rollbacks and builds).
	doorArgvs := captureCapabilityRunnerArgvs(t, spec)
	if restartRunner {
		// The capability doors (freehold-runner-<name>) execute the SAME
		// binary — stop every one of them too (the world-build reconcile the
		// update triggers after the deploy re-stages them). The glob reaches
		// systemctl unquoted (LxcExec single-quotes the payload — no shell
		// quotes may appear inside); when nothing matches, systemctl errors
		// and the trailing true tolerates it.
		if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, "systemctl stop freehold-runner* 2>/dev/null; true"),
			"stop co-located runner", 30); err != nil {
			return err
		}
		if err := shipFile(t, spec, *spec.RunnerBinary, spec.BinDir+"/freehold-runner", "runner binary"); err != nil {
			return err
		}
	}

	flags, domain := serveFlags(spec)
	if spec.RelayHostIP != nil {
		host := strings.Split(strings.TrimSuffix(domain, "/"), ":")[0]
		hostsCmd := fmt.Sprintf("grep -Fq \"%s\" /etc/hosts 2>/dev/null || echo \"%s %s\" >> /etc/hosts", host, *spec.RelayHostIP, host)
		if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, hostsCmd), "pin relay host", 30); err != nil {
			return err
		}
	}
	if _, err := startServe(t, spec, flags); err != nil {
		return err
	}

	// Copy the new release's migration scripts. No markers are written here
	// either: update runs the pending queue through world_migrate (before it
	// promotes the version) and world_build runs it at the end of a bring-up.
	if spec.MigrationsDir != nil && *spec.MigrationsDir != "" {
		if err := ShipMigrations(t, spec, *spec.MigrationsDir); err != nil {
			return err
		}
	}

	// Restart the co-located runner so the new binary takes effect. Its
	// identity + sealed secrets are untouched: the unit re-reads them from its
	// own dir. No adoption, no substrate rotation. A REAL unit file (not a
	// transient): the restart rewrites it + restarts it.
	if restartRunner {
		if err := startRunnerUnit(t, spec, spec.runnerDir()); err != nil {
			return err
		}
	}
	if atArgv != "" {
		if err := restartAgentTools(t, spec, atArgv); err != nil {
			return err
		}
	}

	// The verb surface + the revive script LAST (everything it starts just
	// came up — the script captures the agent-tools argv that just worked).
	if err := shipVerbSurface(t, spec); err != nil {
		return err
	}
	return shipReviveScript(t, spec, flags, atArgv, doorArgvs)
}

// agentToolsStateDir is the agent-tools durable root, a sibling of the console
// state dir under the CP plane.
func agentToolsStateDir(spec *DeployCpSpec) string {
	return filepath.Join(spec.StateDir, "..", "agent-tools")
}

// stopAgentTools kills a running agent-tools serve (if any) and clears its pid.
func stopAgentTools(t Transport, spec *DeployCpSpec) error {
	at := agentToolsStateDir(spec)
	cmd := fmt.Sprintf("p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && kill \"$p\" >/dev/null 2>&1; rm -f %s/serve.pid; true", at, at)
	if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, cmd), "stop prior agent-tools", 30); err != nil {
		return err
	}
	return stopBinary(t, spec, "freehold-agent-tools", "stop agent-tools process")
}

// captureAgentToolsArgv reads the running agent-tools serve argv (binary path +
// flags) from /proc so Redeploy can relaunch it identically after replacing the
// binary. "" when no agent-tools is running (e.g. before the first world build).
// The argv is re-run unquoted; agent-tools flags are paths/URLs/IPs with no
// shell metacharacters, so this is safe. ponytail: argv capture, not a stored
// serve-flags reconstruction — revisit if agent-tools args ever gain spaces.
func captureAgentToolsArgv(t Transport, spec *DeployCpSpec) string {
	at := agentToolsStateDir(spec)
	// Double quotes, not single: LxcExec wraps the whole payload in single
	// quotes, so a single quote inside would break the command. tr understands
	// \000 in double quotes.
	cmd := fmt.Sprintf("p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && tr \"\\000\" \" \" < /proc/$p/cmdline 2>/dev/null; true", at)
	out, err := execToOK(t, proxmox.LxcCmd(spec.LXc, cmd), "read agent-tools argv", 30)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out.Stdout)
}

// captureCapabilityRunnerArgvs reads every RUNNING capability-runner unit's
// serve argv (freehold-runner-* — cp-local-root, pve-ssh-root, the kube/API
// doors) from /proc, so the revive script can re-launch them after a
// guest-local rollback. The transient units do not survive a guest stop
// (--collect), and the build's reconcile is the only other thing that
// re-stages them — without this the doors stay down until the next build.
// Returns lines of "unit\x20argv". Best-effort: nothing running = empty.
func captureCapabilityRunnerArgvs(t Transport, spec *DeployCpSpec) []string {
	// Double quotes only: LxcExec single-quotes the whole payload — a single
	// quote inside would close/reopen the wrapper (the stop command above is
	// the pattern). The unquoted freehold-runner-* glob is a list-units
	// PATTERN, no shell expansion wanted; cut -d" " keeps the unit column.
	cmd := "for u in $(systemctl list-units freehold-runner-* --no-legend --plain 2>/dev/null | cut -d\" \" -f1); do p=$(systemctl show $u -p MainPID --value); [ -n \"$p\" ] && [ \"$p\" != 0 ] && printf \"%s %s\\n\" \"$u\" \"$(tr \"\\000\" \" \" < /proc/$p/cmdline 2>/dev/null)\"; done; true"
	out, err := execToOK(t, proxmox.LxcCmd(spec.LXc, cmd), "read capability-runner argvs", 30)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  note: the capability doors' argv capture failed (the revive script revives them on the next build): %v\n", err)
		return nil
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out.Stdout), "\n") {
		if strings.TrimSpace(l) != "" && strings.Contains(l, " serve ") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}

// captureServeArgv reads the RUNNING console serve's argv (its serve.pid) —
// the freshest source of its own serve flags.
func captureServeArgv(t Transport, spec *DeployCpSpec) string {
	cmd := fmt.Sprintf("p=$(cat %s/serve.pid 2>/dev/null); [ -n \"$p\" ] && tr \"\\000\" \" \" < /proc/$p/cmdline 2>/dev/null; true", spec.StateDir)
	out, err := execToOK(t, proxmox.LxcCmd(spec.LXc, cmd), "read console serve argv", 30)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out.Stdout)
}

// RefreshReviveScript re-ships the guest-local revival script from the
// CURRENTLY RUNNING processes: the console serve's argv, the co-located
// runner + every capability door's unit argv, the agent-tools argv. The
// deploy-time script can only capture what ran at deploy — the update's
// reconcile re-stages the doors AFTER it, and a first build stages doors the
// script never saw. Call after a reconcile (update's reconcileWorld, a build
// tail); needs only the guest's state + bin dirs. Components not running
// keep their deploy-time rendering (absent here = the deploy script's).
func RefreshReviveScript(t Transport, spec *DeployCpSpec) error {
	serveArgv := captureServeArgv(t, spec)
	atArgv := captureAgentToolsArgv(t, spec)
	doorArgvs := captureCapabilityRunnerArgvs(t, spec)
	final := spec.BinDir + "/revive-cp.sh"
	cmd := fmt.Sprintf("mkdir -p %[1]s && echo %[2]s | base64 -d > %[3]s && chmod 755 %[3]s",
		spec.BinDir, base64StdEncode([]byte(reviveScriptFromArgvs(spec, serveArgv, atArgv, doorArgvs))), final)
	if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, cmd), "ship revive script", 60); err != nil {
		return err
	}
	return nil
}

// reviveScriptFromArgvs renders the revival script from captured argvs —
// kept for the door-unit NAMES (the console and agent-tools are real enabled
// units now: the script only ever STARTS them).
func reviveScriptFromArgvs(spec *DeployCpSpec, serveArgv, atArgv string, doorArgvs []string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# freehold CP revival — generated by deploy-cp; do not edit by hand.\n")
	b.WriteString("# Brings the console serve, the co-located runner, the capability doors\n")
	b.WriteString("# and agent-tools back after a rollback stopped this guest. All are\n")
	b.WriteString("# enabled units: the boot re-runs them, these starts are belt and\n")
	b.WriteString("# suspenders.\n")
	b.WriteString("systemctl start freehold-console 2>/dev/null || true\n")
	b.WriteString("up=0\nfor i in $(seq 1 15); do curl -fsS -m 3 http://127.0.0.1:8080/healthz >/dev/null 2>&1 && { up=1; break; }; sleep 2; done\n")
	b.WriteString("[ \"$up\" = 1 ] || { echo 'console serve did not answer /healthz — journalctl -u freehold-console' >&2; exit 1; }\n")
	for _, line := range doorArgvs {
		unit, _, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		b.WriteString(fmt.Sprintf("systemctl start %s 2>/dev/null || true\n", unit))
	}
	b.WriteString("systemctl start freehold-agent-tools 2>/dev/null || true\n")
	return b.String()
}

// restartAgentTools installs the agent-tools REAL systemd unit (the captured
// argv is its ExecStart — a guest reboot brings it back with the console)
// and restarts it, so the immediately-following world_migrate doesn't race
// bind. The restart is also how a registry/facts rewrite reloads.
func restartAgentTools(t Transport, spec *DeployCpSpec, argv string) error {
	if strings.TrimSpace(argv) == "" {
		return nil // nothing was running; nothing to restart
	}
	unit := runnerUnitFile("freehold agent-tools", argv)
	write := fmt.Sprintf("echo %s | base64 -d > /etc/systemd/system/freehold-agent-tools.service && systemctl daemon-reload && systemctl enable --now freehold-agent-tools && sleep 1 && systemctl is-active freehold-agent-tools",
		base64StdEncode([]byte(unit)))
	if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, write), "install agent-tools unit", 90); err != nil {
		return err
	}
	probe := "for i in $(seq 1 15); do curl -s -m 3 -o /dev/null http://127.0.0.1:8089/mcp && exit 0; sleep 2; done; exit 1"
	// The loop can run ~75s; give execToOK headroom so it isn't cut off.
	_, err := execToOK(t, proxmox.LxcCmd(spec.LXc, probe), "agent-tools healthz", 120)
	return err
}
