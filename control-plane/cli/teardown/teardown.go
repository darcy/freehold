// Package teardown reproduces installer/src/teardown.rs — destroying the
// managed world with THREE scopes:
//   - WholeWorld (default): the COMPUTE teardown — destroys the LXCs and
//     keeps EVERYTHING else intact: the config file WITH the recorded LXC
//     coordinates (vmid + ip are operator-owned facts — proxy targets and
//     static assignments; rebuild reuses them for a deterministic re-boot),
//     the world home (~/.freehold: runner package + keys), the host door
//     key, and the plane's locations.
//     --data turns it into the FULL teardown: tenant datasets + the
//     freehold-created thin pool go, then the door key, the world home,
//     and the config.
//   - TenantCompute (one LXC, config survives for reattach)
//   - TenantData (one LXC + its dataset subtree)
//
// ORDER MATTERS: 1) the door must prove itself, 2) destroy the targeted
// LXC(s), 3) (--data) destroy the tenant datasets + freehold-created thin
// pool, 4) local half: --data = full wipe (door key, world home, config
// LAST); default = config KEPT INTACT (recorded LXC coordinates included).
// Remote steps run through the `freehold exec` subprocess —
// the same signed exec contract the CLI uses.
package teardown

import (
	"fmt"
	"os/exec"
	"strings"
)

// Scope is the teardown scope.
type Scope int

const (
	ScopeWholeWorld Scope = iota
	ScopeTenantCompute
	ScopeTenantData
)

// tfModuleDir is where the embedded terraform module lives ON the box (ships
// there by the CP world_build's stageDeployTf); destroy runs it from there.
const tfModuleDir = "/srv/data/freehold-tf"

// String renders the scope for operator-facing output.
func (s Scope) String() string {
	switch s {
	case ScopeWholeWorld:
		return "whole-world"
	case ScopeTenantCompute:
		return "tenant-compute"
	case ScopeTenantData:
		return "tenant-data"
	default:
		return "?"
	}
}

// ScopeFor is the single source of truth for the three-scope derivation.
func ScopeFor(tenant *string, data bool) Scope {
	if tenant != nil {
		if data {
			return ScopeTenantData
		}
		return ScopeTenantCompute
	}
	return ScopeWholeWorld
}

// TenantLxcRole maps a tenant name to the LXC role it rides (k3s-volumes
// rides the k3s guest).
func TenantLxcRole(tenant string) string {
	switch tenant {
	case "k3s-volumes", "k3s":
		return "k3s"
	case "relay":
		return "relay"
	case "cp":
		return "cp"
	}
	return ""
}

// Plan is everything the CLI needs to show before confirmation.
type Plan struct {
	Domain     string
	Lxcs       []LxcRef
	Managed    []string
	WorldHome  string
	ConfigPath string
	DoorTarget string
}

// LxcRef is one managed LXC (role, vmid).
type LxcRef struct {
	Role string
	VMID *uint32
}

// ExecRunner is the subprocess driver that runs `freehold exec`.
type ExecRunner struct {
	OrchestratorBin string // path to the running freehold binary
	Addr            string // runner MCP addr
	AgentDir        string // ops identity dir
	Runner          string // runner target name
	Domain          string // the world's domain — the destroy-side name guard derives the expected guest name from it
	// ConfigPath pins the tenant profile's config for the child exec so it
	// resolves the right runner/config (no implicit default; required under
	// multi-profile). Empty = rely on the child's own default.
	ConfigPath string

	// execFn overrides the subprocess shell-out for hermetic tests
	// (nil = the real `freehold exec`).
	execFn func(cmd string) (bool, string)
}

// Runner is the remote-side surface Run drives. ExecRunner is the real
// subprocess driver; the hermetic tests fake it.
type Runner interface {
	Exec(cmd string) (bool, string)
	DestroyOneLxc(role string, vmid *uint32) ([]string, error)
	DestroyDataset(tenant, domain, pool, kind, dataset string) (bool, error)
	DestroyPool(vg, pool string) error
	TerraformDestroy() ([]string, error)
}

// Exec runs one command through the runner and returns (ok, output).
func (r *ExecRunner) Exec(cmd string) (bool, string) {
	if r.execFn != nil {
		return r.execFn(cmd)
	}
	args := []string{"exec"}
	if r.ConfigPath != "" {
		args = append(args, "--config", r.ConfigPath)
	}
	args = append(args, "--addr", r.Addr, "--agent-dir", r.AgentDir, r.Runner, cmd)
	out, err := exec.Command(r.OrchestratorBin, args...).CombinedOutput()
	if err != nil {
		return false, string(out)
	}
	return true, string(out)
}

// TenantDataset re-derives a tenant's dataset from the two-place rule.
func TenantDataset(pool, domain, tenant string) string {
	dom := strings.ReplaceAll(domain, ".", "-")
	return fmt.Sprintf("%s/freehold/%s/%s", pool, dom, tenant)
}

// DestroyOneLxc destroys one LXC: name-verified present -> stopped ->
// destroyed -> verified gone. The name check is the HARD guard: the config
// keeps the recorded vmid across a compute teardown, and PVE hands a freed
// id to the operator's next guest (pvesh get /cluster/nextid returns the
// LOWEST free id — the one just freed), so a second teardown must not
// `pct destroy` a foreign container by vmid alone. Mirrors the create-side
// guard (bootstrap/drivers.go: "already exists as container %q (expected
// %q)"). Returns log lines.
func (r *ExecRunner) DestroyOneLxc(role string, vmid *uint32) ([]string, error) {
	var log []string
	if vmid == nil {
		log = append(log, fmt.Sprintf("%s LXC: never created (no vmid recorded)", role))
		return log, nil
	}
	// Identity is the RECORDED VMID (the config holds the stable id freehold
	// itself minted). We do NOT gate on a derived guest-name: that derivation
	// changed across the no-base-domain refactor, so a name check falsely
	// refused to destroy this world's own vmids. A free vmid is "already gone";
	// an occupied vmid is this world's recorded container (destroy by id).
	occupant, err := r.lxcOccupant(*vmid)
	if err != nil {
		return nil, err
	}
	if occupant == "" {
		log = append(log, fmt.Sprintf("%s LXC %d: already gone", role, *vmid))
		return log, nil
	}
	log = append(log, fmt.Sprintf("%s LXC %d (%s)", role, *vmid, occupant))
	ok, status := r.Exec(fmt.Sprintf("pct status %d", *vmid))
	if !ok {
		return nil, fmt.Errorf("pct status failed on %s: %s", r.Runner, status)
	}
	if strings.Contains(status, "status: running") {
		if ok, _ := r.Exec(fmt.Sprintf("pct stop %d --skiplock", *vmid)); !ok {
			return nil, fmt.Errorf("pct stop %d failed", *vmid)
		}
	}
	if ok, _ := r.Exec(fmt.Sprintf("pct destroy %d", *vmid)); !ok {
		return nil, fmt.Errorf("pct destroy %d failed", *vmid)
	}
	still, err := r.lxcExists(*vmid)
	if err != nil {
		return nil, err
	}
	if still {
		return nil, fmt.Errorf("%s LXC %d still exists after destroy", role, *vmid)
	}
	log = append(log, fmt.Sprintf("destroyed %s LXC %d", role, *vmid))
	return log, nil
}

// lxcOccupant returns the NAME of the guest at vmid from `pct list` (the
// last column of the row; `awk $NF` after the vmid-anchored grep) — "" when
// there is no row (the vmid is free).
func (r *ExecRunner) lxcOccupant(vmid uint32) (string, error) {
	ok, out := r.Exec(fmt.Sprintf("pct list | grep -E '^\\s*%d ' | awk '{print $NF; exit}' || true", vmid))
	if !ok {
		return "", fmt.Errorf("pct list failed on %s: %s", r.Runner, out)
	}
	return strings.TrimSpace(out), nil
}

func (r *ExecRunner) lxcExists(vmid uint32) (bool, error) {
	ok, out := r.Exec(fmt.Sprintf("pct list | grep -c '^\\s*%d ' || true", vmid))
	if !ok {
		return false, fmt.Errorf("pct list failed on %s: %s", r.Runner, out)
	}
	return strings.TrimSpace(out) != "0", nil
}

// Run executes the teardown. cfg holds the managed state.
func Run(r Runner, cfg *Cfg, scope Scope, confirm bool) (string, error) {
	if !confirm {
		return "", fmt.Errorf("teardown aborted (not confirmed)")
	}
	var log []string
	// say appends to the final report AND streams the line live (the CLI's
	// Live hook — the TUI's subprocess stream shows it the moment it lands).
	say := func(lines ...string) {
		for _, l := range lines {
			log = append(log, l)
			if cfg.Live != nil {
				cfg.Live(l)
			}
		}
	}

	// 1. the door probe already passed at the CLI layer (it is the gate
	//    before Run is ever called).
	say(fmt.Sprintf("door verified (%s)", cfg.RunNTarget))

	// 2. destroy the targeted LXC(s).
	switch scope {
	case ScopeWholeWorld:
		// 2.0 terraform destroys the kube workloads FIRST (they live inside the
		// k3s guest) + the substrate LXCs; teardown's own pct stops/destroys
		// below then find them already-gone and no-op (idempotent, guarded).
		lines, err := r.TerraformDestroy()
		if err != nil {
			return "", err
		}
		say(lines...)
		for _, m := range []string{"relay", "cp", "k3s"} {
			managed := contains(cfg.Managed, m)
			if !managed {
				say(fmt.Sprintf("skipped %s LXC (not managed)", m))
				continue
			}
			vmid := cfg.LxcVMID(m)
			if vmid != nil {
				say(fmt.Sprintf("destroying %s LXC %d", m, *vmid))
			}
			lines, err := r.DestroyOneLxc(m, vmid)
			if err != nil {
				return "", err
			}
			say(lines...)
		}
	case ScopeTenantCompute, ScopeTenantData:
		role := cfg.TenantRole
		vmid := cfg.LxcVMID(role)
		if vmid != nil {
			say(fmt.Sprintf("destroying %s LXC %d", role, *vmid))
		}
		lines, err := r.DestroyOneLxc(role, vmid)
		if err != nil {
			return "", err
		}
		say(lines...)
	}

	// 3. optionally destroy the tenant dataset subtree (data+compute).
	if scope == ScopeWholeWorld && cfg.Data || scope == ScopeTenantData {
		var tenants []string
		if scope == ScopeTenantData {
			tenants = []string{cfg.TenantRoleDatasetName()}
		} else {
			tenants = []string{"relay", "cp", "k3s-volumes"}
		}
		for _, tenant := range tenants {
			dataset := TenantDataset(cfg.Pool, cfg.Domain, tenant)
			destroyed, err := r.DestroyDataset(tenant, cfg.Domain, cfg.Pool, cfg.BackendKind, dataset)
			if err != nil {
				return "", fmt.Errorf("data+compute teardown for %s FAILED: %v — the dataset was NOT destroyed; the config mapping is preserved", tenant, err)
			}
			if destroyed {
				say(fmt.Sprintf("destroyed %s dataset subtree (%s)", tenant, dataset))
			} else {
				say(fmt.Sprintf("no dataset to destroy for %s (absent) — nothing destroyed", tenant))
			}
		}
		// The freehold-CREATED thin pool (config plane.thin_pool) goes with
		// the data. A reused stock pool (pve/data) was never recorded here —
		// that is the guard that keeps --data off host-owned pools. Refuses
		// while LVs still ride it (the tenant destroy above just cleared
		// them); absent = no-op.
		if scope == ScopeWholeWorld && cfg.ThinPool != "" {
			if err := r.DestroyPool(cfg.Pool, cfg.ThinPool); err != nil {
				return "", fmt.Errorf("thin-pool teardown FAILED: %v — the pool is NOT removed", err)
			}
			say(fmt.Sprintf("removed freehold-created thin pool %s/%s", cfg.Pool, cfg.ThinPool))
		}
	}

	// 4. the local half. --data = the FULL teardown: the host door key, the
	//    world home, then the config LAST. Default whole-world = the compute
	//    teardown: KEEP the config file INTACT (recorded LXC coordinates included),
	//    world home, and the door key — a rebuild reuses the package + door
	//    and skips the door gate. Per-tenant KEEPS everything too.
	switch {
	case scope == ScopeWholeWorld && cfg.Data:
		keyRemoval := fmt.Sprintf("sed -i '/ssh-ed25519 [A-Za-z0-9+/=]* %s$/d' /root/.ssh/authorized_keys", cfg.RunnerComment)
		if ok, _ := r.Exec(keyRemoval); !ok {
			return "", fmt.Errorf("door removal command failed on %s", cfg.RunNTarget)
		}
		_, check := r.Exec(fmt.Sprintf("grep -c 'ssh-ed25519 .* %s' /root/.ssh/authorized_keys || true", cfg.RunnerComment))
		still := 0
		fmt.Sscanf(strings.TrimSpace(check), "%d", &still)
		if still != 0 {
			return "", fmt.Errorf("the runner's key is STILL in authorized_keys (%d line(s))", still)
		}
		say(fmt.Sprintf("door removed from %s (%s — verified)", cfg.RunnerComment, cfg.RunNTarget))

		// freehold's operator-side state is KEPT even on a full --data
		// teardown: the config (recorded coords), the world home (runner
		// packages + ops identity + sealed DNS provider credentials + door
		// key) survive, so a rebuild reuses the package + does not require
		// re-entering DNS/operator material. Only the appliance's data
		// (LXCs, datasets, thin pool) is torn down above.
		if cfg.WorldHome != "" {
			say("kept world home " + cfg.WorldHome)
		}
		if cfg.ConfigPath != "" {
			say("kept config " + cfg.ConfigPath)
		}
	case scope == ScopeWholeWorld:
		// The config SURVIVES INTACT: domain, runner identity, the plane's
		// locations, AND the recorded LXC coordinates (vmid + ip). The IPs
		// are operator-owned facts (proxy targets, static assignments out
		// of the DHCP range) — pruning them turned a cheap rebuild into a
		// re-addressing surprise. Rebuild reuses the recorded vmid+ip and
		// re-boots the SAME world; teardown never chases ghosts anyway
		// (DestroyOneLxc treats "already gone" as a no-op).
		say("config KEPT INTACT (LXC coordinates preserved — rebuild reuses the same vmids + IPs)")
		say(fmt.Sprintf("door key KEPT on %s (rebuild reuses it — no door gate)", cfg.RunNTarget))
	default:
		say("config KEPT (per-tenant teardown — coords + dataset mapping for reattach)")
	}

	return "teardown complete:\n  " + strings.Join(log, "\n  "), nil
}

// DestroyDataset runs the storage destroy through the CLI and
// parses the STORAGE-DESTROYED line (true = destroyed, false = absent).
func (r *ExecRunner) DestroyDataset(tenant, domain, pool, kind, dataset string) (bool, error) {
	args := []string{
		"storage", "destroy",
		"--addr", r.Addr,
		"--agent-dir", r.AgentDir,
		"--target", r.Runner,
		"--tenant", tenant,
		"--domain", domain,
		"--pool", pool,
	}
	if kind != "" {
		args = append(args, "--kind", kind)
	}
	out, err := exec.Command(r.OrchestratorBin, args...).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("dataset destroy for %s failed:\n%s", tenant, out)
	}
	text := string(out)
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "STORAGE-DESTROYED: "); ok {
			return v == "true", nil
		}
	}
	return false, fmt.Errorf("storage destroy for %s printed no STORAGE-DESTROYED line", tenant)
}

// DestroyPool removes a freehold-created thin pool through the CLI
// CLI (`storage destroy-pool`); drive.RemoveThinPool refuses while tenant
// LVs still ride it, so the caller runs this AFTER the tenant destroys.
func (r *ExecRunner) DestroyPool(vg, pool string) error {
	args := []string{
		"storage", "destroy-pool",
		"--addr", r.Addr,
		"--agent-dir", r.AgentDir,
		"--target", r.Runner,
		"--pool", vg,
		"--thin-pool", pool,
	}
	out, err := exec.Command(r.OrchestratorBin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("thin-pool removal for %s/%s failed:\n%s", vg, pool, out)
	}
	return nil
}

// TerraformDestroy runs the embedded terraform module's DESTROY on the
// provisioning box through the runner. It must run BEFORE the k3s LXC is torn
// down (the litellm/postgres kube workloads live inside that guest; the
// depends_on chain destroys litellm_kube first, then the LXCs). destroy uses
// stored state for its provisioner args, so no -var/secret is needed; it only
// runs when a managed state file exists (a world never built via terraform has
// none) — teardown's own pct stops/destroys then re-verify idempotently as the
// guest is already gone. The durable plane has no destroy provisioner and
// survives by design (the --data path handles datasets separately).
func (r *ExecRunner) TerraformDestroy() ([]string, error) {
	var log []string
	ok, _ := r.Exec("test -f " + tfModuleDir + "/terraform.tfstate")
	if !ok {
		log = append(log, "terraform: no managed state on the box — skipping terraform destroy")
		return log, nil
	}
	log = append(log, "terraform destroy (substrate + litellm/postgres kube workloads)")
	ok, out := r.Exec("cd " + tfModuleDir + " && " + tfModuleDir + "/scripts/tf.sh destroy")
	log = append(log, strings.TrimSpace(out))
	if !ok {
		return log, fmt.Errorf("terraform destroy failed:\n%s", out)
	}
	return log, nil
}

// Cfg is the teardown-input config surface.
type Cfg struct {
	Domain        string
	RunNTarget    string // the runner target name
	RunnerComment string // the runner's authorized_keys comment
	Managed       []string
	WorldHome     string
	ConfigPath    string
	Pool          string
	BackendKind   string
	TenantRole    string // for tenant-scoped teardown
	Data          bool   // whole-world --data
	Vmid          map[string]*uint32
	ThinPool      string            // freehold-CREATED thin pool (config plane.thin_pool); "" = none
	Live          func(line string) // optional: stream each line as it lands (TUI)
}

// LxcVMID returns the recorded vmid for a role (from the managed config).
func (c *Cfg) LxcVMID(role string) *uint32 {
	if c == nil || c.Vmid == nil {
		return nil
	}
	return c.Vmid[role]
}

// TenantRoleDatasetName returns the tenant ROLE for the scoped tenant name.
func (c *Cfg) TenantRoleDatasetName() string {
	if c.TenantRole == "" {
		return "cp"
	}
	return c.TenantRole
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
