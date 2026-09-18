package proxmox

import (
	"freehold/contract/client"
	"freehold/platform/provisioning"
	"freehold/platform/provisioning/bootstrap"
)

// Generic platform guards, delegated so the provider code reads unchanged.
var (
	ExpectOK        = bootstrap.ExpectOK
	Plain           = bootstrap.Plain
	PlainPath       = bootstrap.PlainPath
	TemplateVersion = bootstrap.TemplateVersion
)

// Exec runs a command on the substrate host via the runner's exec primitive.
func Exec(clientConn *client.McpClient, target, cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
	return bootstrap.Exec(clientConn, target, cmd, timeoutS)
}

// ExecToOK runs a command and asserts it succeeded.
func ExecToOK(clientConn *client.McpClient, target, cmd, step string, timeoutS uint64) (*client.ExecOutcome, error) {
	return bootstrap.ExecToOK(clientConn, target, cmd, step, timeoutS)
}

// ExecFunc is the transport-free host-exec seam platform code injects: it
// returns the same outcome shape as bootstrap.Exec. The provider owns command
// wrapping (pct exec); the caller owns the transport (McpClient today, direct
// SSH later).
type ExecFunc = provisioning.ExecFunc
