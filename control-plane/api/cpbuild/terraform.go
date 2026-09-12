package cpbuild

import (
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"freehold/contract/config"
	"freehold/platform/provisioning/bootstrap"
)

// TerraformFS embeds the exec-first Terraform A1 module (substrate LXCs +
// durable plane + k3s bring-up + litellm/postgres kube workloads). It ships to
// the provisioning box and is driven through the co-located runner's tf.sh —
// adopt-if-missing, plan-clean against the world the CP already created.
//
//go:embed terraform/*.tf terraform/scripts/*.sh
var terraformFS embed.FS

// tfDir is where the module lives ON the provisioning box (the storage-tier
// rule for sensitive terraform state: /srv/data at 0700).
const tfDir = "/srv/data/freehold-tf"

// dashedDomain mirrors planebase's backend root for a dotted relay host: the
// /freehold/<dashed> base + the `freehold-<dashed>-*` LV names adopt.
func (s *Spec) dashedDomain() string {
	return strings.ReplaceAll(s.RelayHost, ".", "-")
}

// tfLxcName resolves a guest's deterministic LXC hostname for a role.
func (s *Spec) tfLxcName(role string) string {
	name, _ := bootstrap.DomainLXCName(s.RelayHost, role)
	return name
}

// stageDeployTf ships the embedded module onto the provisioning box at
// /srv/data/freehold-tf (0700) via a single signed exec on the co-located
// runner — each file is base64'd (no secrets, no network), decoupled from the
// PVE host's reachability to this repo. Idempotent.
func (s *Spec) stageDeployTf() error {
	var parts []string
	parts = append(parts, "umask 077; mkdir -p "+tfDir+"/scripts; rm -f "+tfDir+"/main.tf "+tfDir+"/scripts/*")
	err := fs.WalkDir(terraformFS, "terraform", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := terraformFS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "terraform/")
		parts = append(parts,
			fmt.Sprintf("printf '%%s' '%s' | base64 -d > %s/%s",
				base64.StdEncoding.EncodeToString(b), tfDir, rel))
		return nil
	})
	if err != nil {
		return fmt.Errorf("stage tf: walk: %w", err)
	}
	parts = append(parts, "chmod 755 "+tfDir+"/scripts/*")
	return s.run(strings.Join(parts, " && "), 120)
}

// tfVars renders the non-secret -var arguments the CP driver passes tf.sh
// (vmids/hostnames/IPs/plane base). Callers resolve vmids FIRST so terraform
// ADOPTS the real guests (adopt-if-missing no-ops when present).
func (s *Spec) tfVars() []string {
	return []string{
		"-var", "domain_dash=" + s.dashedDomain(),
		"-var", "vmid_cp=" + strconv.FormatUint(uint64(s.CpLxc), 10),
		"-var", "vmid_relay=" + strconv.FormatUint(uint64(s.RelayLxc), 10),
		"-var", "vmid_k3s=" + strconv.FormatUint(uint64(s.K3sVmid), 10),
		"-var", "host_cp=" + s.tfLxcName("cp"),
		"-var", "host_relay=" + s.tfLxcName("relay"),
		"-var", "host_k3s=" + s.tfLxcName("k3s"),
		"-var", "k3s_ip=" + config.StripCIDR(s.ProxyIP),
		"-var", "k3s_gw=" + s.RelayGW,
		"-var", "thin_pool=" + s.ThinPool,
	}
}

// worldTerraform drives the terraform module on the provisioning box through
// the co-located runner for `action` (plan|apply|destroy). Secrets (the litellm
// master + provider key) are requested BY NAME so the runner injects + redacts
// them as env (LITELLM / PROVIDER_KEY) — kube-apply.sh reads them directly, so
// nothing transits argv, -var, tfvars, or terraform state. Long timeout: apply
// boots k3s + waits for the API + rolls out litellm, which takes minutes.
func (s *Spec) worldTerraform(action string) error {
	s.resolveGuestVmids()
	if err := s.stageDeployTf(); err != nil {
		return fmt.Errorf("tf: ship module: %w", err)
	}
	args := strings.Join(append([]string{action}, s.tfVars()...), " ")
	if err := s.runSecrets(tfDir+"/scripts/tf.sh "+args, 900, "litellm", "provider-key"); err != nil {
		return fmt.Errorf("tf %s: %w", action, err)
	}
	return nil
}
