package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
