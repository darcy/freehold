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

// tfDir is the LEGACY shared tf root: one dir per PVE host, used by every
// world on that host until per-world roots shipped. A world adopting the
// legacy dir (same k3s IP in its staged kubeconfig) migrates it wholesale;
// otherwise it is left for the world that owns it.
const tfDir = "/srv/data/freehold-tf"

// tfRoot is THIS world's tf module + state root on the provisioning box —
// per-world, so multiple worlds on one host never clobber each other's state
// or staged kubeconfig (the shared-root design destroyed a world's services
// when another world's teardown ran terraform destroy against the shared
// state). The kubeconfig staged beside the state is the world's fingerprint:
// its server IP is the world's own k3s node.
func (s *Spec) tfRoot() string {
	return "/srv/data/freehold-tf-" + s.dashedDomain()
}

// dashedDomain mirrors planebase's backend root for a dotted relay host: the
// /freehold/<dashed> base + the `freehold-<dashed>-*` LV names adopt.
func (s *Spec) dashedDomain() string {
	return strings.ReplaceAll(s.RelayHost, ".", "-")
}

// tfLxcName resolves a guest's deterministic LXC hostname for a role. An
// invalid world name fails loudly rather than flowing an empty hostname into
// the terraform plan.
func (s *Spec) tfLxcName(role string) (string, error) {
	return bootstrap.LXCName(s.Name, s.RelayHost, role)
}

// stageDeployTf ships the embedded module onto the provisioning box at the
// world's own tf root (0700) via a single signed exec on the co-located
// runner — each file is base64'd (no secrets, no network), decoupled from the
// PVE host's reachability to this repo. Idempotent.
//
// Legacy adoption: a world built before per-world roots kept its state in the
// SHARED /srv/data/freehold-tf. Two shapes are recovered, both fingerprinted by
// THIS world's k3s node IP (each world has a unique one):
//   - the per-world root does not exist and the legacy dir's staged kubeconfig
//     names this world's k3s IP → move the legacy dir wholesale (module + state)
//     into the per-world root;
//   - the per-world root EXISTS but has no terraform.tfstate (staged by a prior
//     build that still pinned the shared backend) while the legacy state names
//     this world's k3s IP → move just the legacy STATE into the root.
//
// A legacy dir/state belonging to ANOTHER world (a different k3s IP) is left
// alone; this world then starts empty and its first apply fails loud on
// "already exists" (the release-test repair: delete the orphaned k8s objects
// once, rebuild — the state re-populates).
func (s *Spec) stageDeployTf() error {
	root := s.tfRoot()
	adopt := "true"
	if s.ProxyIP != "" {
		kip := config.StripCIDR(s.ProxyIP)
		adopt = fmt.Sprintf(
			`if [ ! -d %[1]q ]; then { [ -f %[2]q/kubeconfig ] && grep -q 'server: https://%[3]s:' %[2]q/kubeconfig && mv %[2]q %[1]q && echo adopted-legacy-tf-root; true; }; elif [ ! -f %[1]q/terraform.tfstate ] && [ -f %[2]q/terraform.tfstate ] && grep -qE '(^|[^0-9.])%[3]s([^0-9.]|$)' %[2]q/terraform.tfstate; then mv %[2]q/terraform.tfstate %[1]q/terraform.tfstate && { [ -f %[2]q/terraform.tfstate.backup ] && mv %[2]q/terraform.tfstate.backup %[1]q/terraform.tfstate.backup; true; } && echo adopted-legacy-tf-state; fi; true`,
			root, tfDir, kip)
	}
	var parts []string
	parts = append(parts,
		"umask 077",
		adopt,
		"mkdir -p "+root+"/scripts",
		"rm -f "+root+"/main.tf "+root+"/scripts/*")
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
				base64.StdEncoding.EncodeToString(b), root, rel))
		return nil
	})
	if err != nil {
		return fmt.Errorf("stage tf: walk: %w", err)
	}
	parts = append(parts, "chmod 755 "+root+"/scripts/*")
	return s.run(strings.Join(parts, " && "), 120)
}

// tfVars renders the non-secret -var arguments the CP driver passes tf.sh
// (vmids/hostnames/IPs/plane base). Callers resolve vmids FIRST so terraform
// ADOPTS the real guests (adopt-if-missing no-ops when present).
func (s *Spec) tfVars() ([]string, error) {
	hostCp, err := s.tfLxcName("cp")
	if err != nil {
		return nil, err
	}
	hostRelay, err := s.tfLxcName("relay")
	if err != nil {
		return nil, err
	}
	hostK3s, err := s.tfLxcName("k3s")
	if err != nil {
		return nil, err
	}
	return []string{
		"-var", "domain_dash=" + s.dashedDomain(),
		"-var", "vmid_cp=" + strconv.FormatUint(uint64(s.CpLxc), 10),
		"-var", "vmid_relay=" + strconv.FormatUint(uint64(s.RelayLxc), 10),
		"-var", "vmid_k3s=" + strconv.FormatUint(uint64(s.K3sVmid), 10),
		"-var", "host_cp=" + hostCp,
		"-var", "host_relay=" + hostRelay,
		"-var", "host_k3s=" + hostK3s,
		"-var", "k3s_ip=" + config.StripCIDR(s.ProxyIP),
		"-var", "k3s_gw=" + s.RelayGW,
		"-var", "thin_pool=" + s.ThinPool,
		// The provider's kubeconfig rides the WORLD's own tf root — the
		// variables.tf default is the LEGACY shared path.
		"-var", "kubeconfig_path=" + s.tfRoot() + "/kubeconfig",
	}, nil
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
	if err := s.resolveGuestVmids(); err != nil {
		return fmt.Errorf("tf %s: %w", action, err)
	}
	if err := s.stageDeployTf(); err != nil {
		return fmt.Errorf("tf: ship module: %w", err)
	}
	vars, err := s.tfVars()
	if err != nil {
		return fmt.Errorf("tf %s: %w", action, err)
	}
	args := append([]string{action}, vars...)
	args = append(args, extraVars...)
	for _, t := range targets {
		args = append(args, "-target", t)
	}
	script := s.tfRoot() + "/scripts/tf.sh"
	// TF_ROOT pins the world's own module+state root (tf.sh defaults to the
	// legacy shared dir); the env-prefix rides the runner's sh so the state and
	// the staged kubeconfig are this world's, never another world's.
	cmdline := "TF_ROOT=" + s.tfRoot() + " " + script + " " + strings.Join(args, " ")
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
// the provisioning box), and writes it at the world's tf root (0600) for the
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
	root := s.tfRoot()
	cmd := fmt.Sprintf("umask 077; mkdir -p %s && printf '%%s' '%s' | base64 -d > %s/kubeconfig && chmod 600 %s/kubeconfig",
		root, b64, root, root)
	if err := s.run(cmd, 30); err != nil {
		return fmt.Errorf("stage kubeconfig: %w", err)
	}
	return nil
}
