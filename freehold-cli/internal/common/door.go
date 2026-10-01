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
	exec = proxmox.SSHExec(strings.TrimPrefix(cfg.Host, "root@"), keyPath)
	out, err := exec("echo freehold-door-ok", 30)
	if err != nil {
		cleanup()
		return nil, "", nil, fmt.Errorf("the host door can't be verified (is this box's DOOR_SPEC key authorized on %s?): %w", cfg.Host, err)
	}
	if !strings.Contains(out.Stdout, "freehold-door-ok") {
		// SSHExec reports exit-code failures IN the outcome (err is transport
		// only) — the probe's stderr is the real reason (Permission denied /
		// Connection refused / …).
		cleanup()
		return nil, "", nil, fmt.Errorf("the host door can't be verified (is this box's DOOR_SPEC key authorized on %s?): %s", cfg.Host, strings.TrimSpace(out.Stderr))
	}
	return exec, keyPath, cleanup, nil
}
