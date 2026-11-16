package vultr

import (
	"strings"
	"testing"
)

// TestPVEInstallScriptPersistsHostsFix pins the /etc/hosts fix across boots:
// pmxcfs needs the node name to resolve to a non-loopback IP, but cloud-init
// rewrites /etc/hosts on every boot. The install writes a helper and a
// pve-cluster drop-in that runs it immediately before pmxcfs starts, and both
// must be installed before the already-PVE short-circuit so a re-adopt of an
// existing host applies the fix too.
func TestPVEInstallScriptPersistsHostsFix(t *testing.T) {
	script := PVEInstallScript()
	for _, want := range []string{
		`sed -i "s/^127\.0\.1\.1.*/$IP $HN/" /etc/hosts`,
		"/usr/local/sbin/freehold-fix-hosts",
		"pve-cluster.service.d/freehold-hosts.conf",
		"ExecStartPre=/usr/local/sbin/freehold-fix-hosts",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("PVE install script missing %q", want)
		}
	}
	fix := strings.Index(script, "/usr/local/sbin/freehold-fix-hosts")
	short := strings.Index(script, "if command -v pct")
	if fix < 0 || short < 0 || fix > short {
		t.Errorf("hosts fix must precede the already-PVE short-circuit (fix=%d short=%d)", fix, short)
	}
}
