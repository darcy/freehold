package proxmox

import (
	"fmt"
	"strconv"

	"freehold/contract/client"
	"freehold/platform/provisioning"
	"freehold/platform/provisioning/bootstrap"
)

// LxcCmd wraps a target command for execution inside an LXC via the host
// runner (single-quote-free payload). nil guest = run on the host directly.
func LxcCmd(lxc *uint32, cmd string) string {
	if lxc == nil {
		return cmd
	}
	return LxcExec(strconv.FormatUint(uint64(*lxc), 10), cmd)
}

// LxcExec is LxcCmd for a string guest handle ("" = host). The payload rides a
// single-quoted `sh -c` wrapper, so callers must pass single-quote-free
// commands (the repo-wide convention in platform/stages).
func LxcExec(guest, cmd string) string {
	if guest == "" {
		return cmd
	}
	return fmt.Sprintf("pct exec %s -- sh -c '%s'", guest, cmd)
}

// GuestExecFunc adapts a runner client into the platform guest-exec seam.
func GuestExecFunc(clientConn *client.McpClient, target string) provisioning.GuestExecFunc {
	return func(guest, cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
		return bootstrap.Exec(clientConn, target, LxcExec(guest, cmd), timeoutS)
	}
}
