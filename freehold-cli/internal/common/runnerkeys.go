package common

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/platform/provisioning/box"
)

// HostExec is the minimal remote-exec surface the ref builders need.
type HostExec interface {
	Exec(cmd string) (bool, string)
}

// RunnerKeyRefs returns the exact substrate-key lines to remove: the box's own
// package line and the plane runner's rotated line (when the CP is reachable).
func RunnerKeyRefs(cfg *config.Config, r HostExec) []string {
	var refs []string
	seen := map[string]bool{}
	add := func(line string) {
		if line != "" && !seen[line] {
			seen[line] = true
			refs = append(refs, line)
		}
	}
	add(SubstratePubLine(cfg))
	add(PlaneSubstratePubLine(cfg, r))
	return refs
}

// PlaneSubstratePubLine reads the co-located runner package from the CP guest
// and returns its exact substrate authorized_keys line. "" when unreachable.
func PlaneSubstratePubLine(cfg *config.Config, r HostExec) string {
	if r == nil || cfg.Lxc.Cp.Vmid == nil || cfg.Runner.Target == "" {
		return ""
	}
	dir := "/srv/data/cp/control-plane/runner/" + cfg.Runner.Target
	cat := func(file string) string {
		ok, out := r.Exec(fmt.Sprintf("pct exec %d -- cat %s/%s 2>/dev/null", *cfg.Lxc.Cp.Vmid, dir, file))
		if !ok {
			return ""
		}
		return out
	}
	var id struct {
		EncSecretHex string `json:"enc_secret_hex"`
	}
	if err := json.Unmarshal([]byte(cat("identity.json")), &id); err != nil {
		return ""
	}
	encSecret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return ""
	}
	pkg := &wire.SecretPackage{}
	if err := json.Unmarshal([]byte(cat("secrets.json")), pkg); err != nil {
		return ""
	}
	for _, meta := range pkg.Targets {
		if meta.Kind != "ssh" {
			continue
		}
		ct, ok := pkg.Secrets[meta.Secret]
		if !ok {
			continue
		}
		sealed, err := hex.DecodeString(ct)
		if err != nil {
			continue
		}
		pem, err := crypto.Open(encSecret, []byte(meta.Secret), sealed)
		if err != nil {
			continue
		}
		line, err := crypto.ExtractED25519PublicKeyLine(pem)
		if err != nil {
			continue
		}
		return line
	}
	return ""
}

// SubstratePubLine derives the box's OWN substrate key line from its local
// runner package; "" when there is no local package (thin box).
func SubstratePubLine(cfg *config.Config) string {
	if cfg.Runner.Target == "" {
		return ""
	}
	runnerDir := filepath.Join(box.RunnerPkgs(), cfg.Runner.Target)
	pem, err := box.SubstrateKeyPEM(runnerDir, cfg.Runner.Target)
	if err != nil {
		return ""
	}
	line, err := crypto.ExtractED25519PublicKeyLine(pem)
	if err != nil {
		return ""
	}
	return line
}

// GuestExecAdapter adapts the transient provider to the host-exec surface, so
// key removal has ONE implementation.
type GuestExecAdapter struct {
	ExecFn func(cmd string, timeoutS uint64) (bool, string)
}

// Exec satisfies HostExec.
func (a GuestExecAdapter) Exec(cmd string) (bool, string) { return a.ExecFn(cmd, 60) }

// WarnOtherDoors lists other freehold operator doors still authorized on the
// host.
func WarnOtherDoors(r HostExec) {
	ok, out := r.Exec("grep -o 'freehold-door-[^ ]*' /root/.ssh/authorized_keys 2>/dev/null || true")
	if !ok {
		return
	}
	var others []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			others = append(others, l)
		}
	}
	if len(others) > 0 {
		fmt.Printf("  ! other freehold boxes' doors remain on the host: %s\n    (uninstall removes only THIS box's door — revoke the others with `freehold door revoke` from each)\n", strings.Join(others, ", "))
	}
}
