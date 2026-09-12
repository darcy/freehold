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

// tfRun drives the terraform module on the provisioning box for `action`
// (plan|apply|destroy), optionally restricted to specific resources via
// `-target`. requiresSecrets toggles the tf.sh secret-gate: the SUBSTRATE phase
// (plane/LXCs/k3s) runs before k3s is up, so it needs neither the secret values
// nor a kubeconfig (TF_NO_SECRETS=1). The SERVICES phase requests the litellm
// master + postgres pw + provider key BY NAME so the runner injects + redacts
// them as env (LITELLM / POSTGRES_PW / PROVIDER_KEY); tf.sh maps the two
// service credentials to TF_VAR_* (env, never argv). Long timeout: apply rolls
// out litellm + waits for the API.
func (s *Spec) tfRun(action string, targets, extraVars []string, requiresSecrets bool) error {
	s.resolveGuestVmids()
	if err := s.stageDeployTf(); err != nil {
		return fmt.Errorf("tf: ship module: %w", err)
	}
	args := append([]string{action}, s.tfVars()...)
	args = append(args, extraVars...)
	for _, t := range targets {
		args = append(args, "-target", t)
	}
	script := tfDir + "/scripts/tf.sh"
	cmdline := script + " " + strings.Join(args, " ")
	if requiresSecrets {
		if err := s.runSecrets(cmdline, 900, "litellm", "postgres-pw", "provider-key"); err != nil {
			return fmt.Errorf("tf %s: %w", action, err)
		}
		return nil
	}
	if err := s.run("TF_NO_SECRETS=1 "+cmdline, 900); err != nil {
		return fmt.Errorf("tf %s (substrate): %w", action, err)
	}
	return nil
}

// stageKubeconfig fetches the k3s guest's kubeconfig, rewrites its `server` to
// the k3s NODE IP (the guest's kubeconfig points at 127.0.0.1, unreachable from
// the provisioning box), and writes it at <tfDir>/kubeconfig (0600) for the
// kubernetes provider. Runs only AFTER k3s is up (k3s_bringup applied).
func (s *Spec) stageKubeconfig() error {
	if s.K3sVmid == 0 {
		return fmt.Errorf("no k3s vmid to fetch the kubeconfig")
	}
	raw, err := s.runOut(fmt.Sprintf("pct exec %d -- cat /etc/rancher/k3s/k3s.yaml", s.K3sVmid), 60)
	if err != nil {
		return fmt.Errorf("fetch kubeconfig: %w", err)
	}
	kip := config.StripCIDR(s.ProxyIP)
	if kip == "" || kip == "-" {
		if out, err := s.runOut(fmt.Sprintf("pct exec %d -- ip -4 -o addr show eth0", s.K3sVmid), 30); err == nil {
			for _, f := range strings.Fields(out) {
				if strings.Contains(f, "/") && !strings.Contains(f, "127.0.0.1") {
					kip = config.StripCIDR(f)
					break
				}
			}
		}
	}
	if kip == "" || kip == "-" {
		return fmt.Errorf("no k3s node IP to rewrite the kubeconfig server")
	}
	rewritten := strings.Replace(raw, "https://127.0.0.1:6443", "https://"+kip+":6443", 1)
	if !strings.Contains(rewritten, "https://"+kip+":6443") {
		return fmt.Errorf("could not rewrite the kubeconfig server to https://%s:6443", kip)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(rewritten))
	cmd := fmt.Sprintf("umask 077; mkdir -p %s && printf '%%s' '%s' | base64 -d > %s/kubeconfig && chmod 600 %s/kubeconfig",
		tfDir, b64, tfDir, tfDir)
	if err := s.run(cmd, 30); err != nil {
		return fmt.Errorf("stage kubeconfig: %w", err)
	}
	return nil
}
