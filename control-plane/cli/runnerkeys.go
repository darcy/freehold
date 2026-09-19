package cli

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

// hostExec is the minimal remote-exec surface the ref builders need.
type hostExec interface {
	Exec(cmd string) (bool, string)
}

// runnerKeyRefs returns the substrate-key references to remove on uninstall:
// the box's own package line (exact body), the plane runner's rotated line
// (exact body, when the CP is reachable), and — only when neither package is
// readable — the bare runner TARGET (matched exactly on the last field).
// A target-name fallback can touch a line shared by another world with the same
// target on this host (the default `proxmox-box` is shared); it is used only
// when no exact body can be derived.
func runnerKeyRefs(cfg *config.Config, r hostExec) []string {
	var refs []string
	seen := map[string]bool{}
	add := func(line string) {
		if line != "" && !seen[line] {
			seen[line] = true
			refs = append(refs, line)
		}
	}
	add(substratePubLine(cfg))
	add(planeSubstratePubLine(cfg, r))
	if len(refs) == 0 {
		add(cfg.Runner.Target)
	}
	return refs
}

// planeSubstratePubLine reads the co-located runner package from the CP guest
// (its identity.json is plaintext 0600 root-readable) and returns its exact
// substrate authorized_keys line. "" when the CP is unreachable or the package
// is unreadable.
func planeSubstratePubLine(cfg *config.Config, r hostExec) string {
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

// substratePubLine derives the box's OWN substrate key line from its local
// runner package; "" when there is no local package (thin box).
func substratePubLine(cfg *config.Config) string {
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

// guestExecAdapter adapts the transient provider to teardown's host-exec
// surface, so key removal has ONE implementation.
type guestExecAdapter struct {
	exec func(cmd string, timeoutS uint64) (bool, string)
}

func (a guestExecAdapter) Exec(cmd string) (bool, string) { return a.exec(cmd, 60) }

// warnOtherDoors lists other freehold operator doors still authorized on the
// host (the runner substrate key is removed, not warned).
func warnOtherDoors(r hostExec) {
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
