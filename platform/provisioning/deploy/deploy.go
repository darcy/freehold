// Package deploy holds the generic deploy machinery shared by the platform's
// service deployers (relay, caddy, cp) — running commands inside an LXC through
// the runner's ONE exec primitive, and validating deploy paths. Service-specific
// deployers live with their service under platform/services/ and
// control-plane/cli/bootstrap-cp.
package deploy

import (
	"fmt"
	"strings"

	"freehold/contract/client"
	"freehold/platform/provisioning/bootstrap"
)

// LxcCmd wraps a target command for execution inside an LXC via the host
// runner (single-quote-free payload).
func LxcCmd(lxc *uint32, cmd string) string {
	if lxc != nil {
		return fmt.Sprintf("pct exec %d -- sh -c '%s'", *lxc, cmd)
	}
	return cmd
}

// SafeDeployDir validates a deploy dir: absolute, no '..', >= 2 components.
func SafeDeployDir(s string) error {
	if err := bootstrap.PlainPath(s); err != nil {
		return err
	}
	if !strings.HasPrefix(s, "/") {
		return fmt.Errorf("deploy dir must be an absolute path (got %q)", s)
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return fmt.Errorf("deploy dir must not contain '..' (got %q)", s)
		}
	}
	depth := 0
	for _, p := range strings.Split(s, "/") {
		if p != "" {
			depth++
		}
	}
	if depth < 2 {
		return fmt.Errorf("deploy dir must be at least two components deep (got %q)", s)
	}
	return nil
}

// CheckDocker passes the B1 gate: docker + compose must exist on the target
// (or inside its LXC).
func CheckDocker(clientConn *client.McpClient, target string, lxc *uint32) error {
	cmd := LxcCmd(lxc, "command -v docker && docker compose version")
	_, err := bootstrap.ExecToOK(clientConn, target, cmd, "docker gate", 60)
	return err
}