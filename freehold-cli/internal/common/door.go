// DoorExec is the shared direct-to-host transport: this box's DOOR_SPEC key
// over root SSH, verified with a probe. The same path teardown's dead-CP flow
// rides, so host-side verbs (snapshot, export) work with the control plane
// DOWN. The key path comes back too — the pull/upload legs (scp) need it.
// Callers must invoke cleanup.
package common

import (
	"fmt"
	"strings"

	"freehold/contract/config"
	"freehold/platform/provisioning"
	"freehold/platform/provisioning/box"
	"freehold/providers/proxmox"
)

func DoorExec(cfg *config.Config) (exec provisioning.ExecFunc, keyPath string, cleanup func(), err error) {
	pem, err := box.DoorKeyPEM()
	if err != nil {
		return nil, "", nil, fmt.Errorf("no DOOR_SPEC key to reach the host (run `freehold login` first): %w", err)
	}
	keyPath, cleanup, err = proxmox.WriteTempKey(pem)
	if err != nil {
		return nil, "", nil, err
	}
	return doorExecWithKey(cfg, keyPath, cleanup)
}

// DoorExecWithKey is DoorExec with an explicit key: the CP-guest verbs pass
// the staged cp-verb key (/srv/data/cp/verb-ssh.key — the box never derives a
// door key there). The key file is the caller's — no temp, no cleanup.
func DoorExecWithKey(cfg *config.Config, sshKey string) (exec provisioning.ExecFunc, keyPath string, cleanup func(), err error) {
	if strings.TrimSpace(sshKey) != "" {
		return doorExecWithKey(cfg, sshKey, func() {})
	}
	return DoorExec(cfg)
}

func doorExecWithKey(cfg *config.Config, keyPath string, cleanup func()) (exec provisioning.ExecFunc, _ string, _ func(), err error) {
	exec = proxmox.SSHExec(strings.TrimPrefix(cfg.Host, "root@"), keyPath)
	out, err := exec("echo freehold-door-ok", 30)
	if err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("the host door can't be verified (is the door key authorized on %s?): %w", cfg.Host, err)
	}
	if !strings.Contains(out.Stdout, "freehold-door-ok") {
		// SSHExec reports exit-code failures IN the outcome (err is transport
		// only) — the probe's stderr is the real reason (Permission denied /
		// Connection refused / …).
		cleanup()
		return nil, "", nil, fmt.Errorf("the host door can't be verified (is the door key authorized on %s?): %s", cfg.Host, strings.TrimSpace(out.Stderr))
	}
	return exec, keyPath, cleanup, nil
}
