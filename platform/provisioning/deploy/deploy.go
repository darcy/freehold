// Package deploy holds the generic deploy machinery shared by the platform's
// service deployers (relay, caddy, cp) — running commands inside an LXC through
// the runner's ONE exec primitive, and validating deploy paths. Service-specific
// deployers live with their service under platform/services/ and
// control-plane/cli/bootstrap-cp.
package deploy

import (
	"fmt"
	"strings"

	"freehold/platform/provisioning/bootstrap"
)

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
