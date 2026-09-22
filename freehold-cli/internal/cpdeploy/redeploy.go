package cpdeploy

import (
	"fmt"
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
	if restartRunner {
		if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, "systemctl stop freehold-runner 2>/dev/null; true"),
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
	// own dir. No adoption, no substrate rotation.
	if restartRunner {
		startRunner := fmt.Sprintf(
			"systemctl reset-failed freehold-runner 2>/dev/null; systemd-run --unit=freehold-runner --collect %s/freehold-runner serve --state-dir %s >/dev/null 2>&1; sleep 2; systemctl is-active freehold-runner",
			spec.BinDir, spec.runnerDir())
		if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, startRunner), "start co-located runner", 60); err != nil {
			return err
		}
	}
	if atArgv != "" {
		if err := restartAgentTools(t, spec, atArgv); err != nil {
			return err
		}
	}
	return nil
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

// restartAgentTools relaunches agent-tools with a captured argv and waits for
// it to answer, so the immediately-following world_migrate doesn't race bind.
func restartAgentTools(t Transport, spec *DeployCpSpec, argv string) error {
	if strings.TrimSpace(argv) == "" {
		return nil // nothing was running; nothing to restart
	}
	at := agentToolsStateDir(spec)
	start := fmt.Sprintf("setsid nohup %s >> %s/serve.log 2>&1 < /dev/null & echo $! | tee %s/serve.pid", argv, at, at)
	if _, err := execToOK(t, proxmox.LxcCmd(spec.LXc, start), "restart agent-tools", 60); err != nil {
		return err
	}
	probe := "for i in $(seq 1 15); do curl -s -m 3 -o /dev/null http://127.0.0.1:8089/mcp && exit 0; sleep 2; done; exit 1"
	// The loop can run ~75s; give execToOK headroom so it isn't cut off.
	_, err := execToOK(t, proxmox.LxcCmd(spec.LXc, probe), "agent-tools healthz", 120)
	return err
}
