package drive

import (
	"freehold/contract/client"
	"freehold/platform/provisioning"
	"freehold/platform/provisioning/bootstrap"
)

// ExecFunc is the transport-free host-exec seam (see proxmox.ExecFunc).
type ExecFunc = provisioning.ExecFunc

// ClientExec adapts a runner MCP client + target to the host-exec seam.
func ClientExec(clientConn *client.McpClient, target string) ExecFunc {
	return func(cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
		return bootstrap.Exec(clientConn, target, cmd, timeoutS)
	}
}

// execToOK runs cmd on the host and asserts it succeeded.
func execToOK(exec ExecFunc, cmd, step string, timeoutS uint64) (*client.ExecOutcome, error) {
	out, err := exec(cmd, timeoutS)
	if err != nil {
		return nil, err
	}
	if err := bootstrap.ExpectOK(out, step); err != nil {
		return nil, err
	}
	return out, nil
}
