package cpdeploy

import (
	"fmt"
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
	if err := shipConsoleBins(t, spec); err != nil {
		return err
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

	// Copy the new release's migration scripts (do NOT mark them: update runs
	// pending scripts through world_migrate, then stamps the version last).
	if spec.MigrationsDir != nil && *spec.MigrationsDir != "" {
		if err := ShipMigrations(t, spec, *spec.MigrationsDir, false); err != nil {
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
	return nil
}
