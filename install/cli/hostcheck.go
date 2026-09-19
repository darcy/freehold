package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"freehold/contract/client"
	"freehold/platform/provisioning"
	"freehold/platform/provisioning/box"
	"freehold/providers/proxmox"
)

// hostSideLiveCheck is the authoritative, profile-independent fail-if-live
// probe: using this box's deterministic DOOR_SPEC key, SSH directly to the
// host and look for `<name>-cp`. It runs BEFORE provisioning so a mint never
// re-deploys over a live world.
//
// It only has teeth when this box already holds an ops identity whose door is
// authorized (a prior login); a fresh box has no identity or no authorized
// key, so the probe degrades to a no-op rather than blocking the install.
func hostSideLiveCheck(name, host string) error {
	if name == "" || host == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(box.OpsDir(), "identity.json")); err != nil {
		return nil // fresh box — no identity to authorize with
	}
	pem, err := box.DoorKeyPEM()
	if err != nil {
		return nil
	}
	keyPath, cleanup, err := proxmox.WriteTempKey(pem)
	if err != nil {
		return err
	}
	defer cleanup()
	prov := proxmox.New(proxmox.SSHExec(strings.TrimPrefix(host, "root@"), keyPath))
	live, err := box.CPGuestLive(prov, name, "")
	if err != nil {
		return nil // host unreachable with this key — the door gate handles access
	}
	if live {
		return fmt.Errorf(
			"a live control plane for world %q already exists on %s (%s-cp):\n"+
				"  freehold build       bring up / reconcile the world\n"+
				"  freehold teardown    drop the world (the control plane stays)\n"+
				"  freehold uninstall   drop the control plane\n"+
				"  freehold login       join it from this box",
			name, host, name)
	}
	return nil
}

// sshTransport adapts the transient SSH provider to cpdeploy's host transport.
type sshTransport struct{ host, key string }

func (t sshTransport) Exec(cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
	return proxmox.SSHExec(t.host, t.key)(cmd, timeoutS)
}

func (t sshTransport) Upload(local, remote string, timeoutS uint64) (uint64, error) {
	return proxmox.SSHUpload(t.host, t.key, local, remote, timeoutS)
}

// transientKey writes the runner package's substrate SSH key to a 0600 temp
// file and returns its path + a cleanup func.
func transientKey(agentDir, target string) (string, func(), error) {
	runnerDir := filepath.Join(stateRootFor(agentDir), "runner", target)
	pem, err := box.SubstrateKeyPEM(runnerDir, target)
	if err != nil {
		return "", nil, fmt.Errorf("read substrate key from %s: %w", runnerDir, err)
	}
	return proxmox.WriteTempKey(pem)
}

// stageExec is the transport for a self-staged command: direct root SSH with
// the substrate key (--transient) or a served runner (the classic path).
func stageExec(addr, agentDir, target, host string, transient bool) (proxmox.ExecFunc, func(), error) {
	if transient {
		keyPath, cleanup, err := transientKey(agentDir, target)
		if err != nil {
			return nil, nil, err
		}
		return proxmox.SSHExec(strings.TrimPrefix(host, "root@"), keyPath), cleanup, nil
	}
	c, err := installConnect(addr, agentDir, target)
	if err != nil {
		return nil, nil, err
	}
	return proxmox.ClientExec(c, target), func() {}, nil
}

// transientFactory builds the direct-SSH provider once the runner package's
// substrate key exists (after stageProvision + the door gate): box swaps to it
// and runs host ops without a served runner. The caller must invoke cleanup.
func transientFactory(eng *box.Engine) func() (provisioning.Provider, func(), error) {
	return func() (provisioning.Provider, func(), error) {
		runnerDir := filepath.Join(box.RunnerPkgs(), eng.F.Target)
		pem, err := box.SubstrateKeyPEM(runnerDir, eng.F.Target)
		if err != nil {
			return nil, nil, fmt.Errorf("read substrate key from %s: %w", runnerDir, err)
		}
		keyPath, cleanup, err := proxmox.WriteTempKey(pem)
		if err != nil {
			return nil, nil, err
		}
		return proxmox.New(proxmox.SSHExec(strings.TrimPrefix(eng.F.Host, "root@"), keyPath)), cleanup, nil
	}
}
