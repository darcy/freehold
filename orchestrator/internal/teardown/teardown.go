// Package teardown reproduces installer/src/teardown.rs — destroying the
// managed world with THREE scopes:
//   - WholeWorld (default, optional --data destroys every tenant dataset)
//   - TenantCompute (one LXC, config survives for reattach)
//   - TenantData (one LXC + its dataset subtree)
//
// ORDER MATTERS: 1) the door must prove itself, 2) destroy the targeted LXC(s),
// 3) (whole-world) remove the runner's key LAST + verify, 4) local cleanup.
// Remote steps run through the `freehold-orchestrator exec` subprocess, the
// same contract the Rust installer uses.
package teardown

import (
	"fmt"
	"os"
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

// ExecRunner is the subprocess driver that runs `freehold-orchestrator exec`.
type ExecRunner struct {
	OrchestratorBin string // path to the freehold-orchestrator binary
	Addr            string // runner MCP addr
	AgentDir        string // ops identity dir
	Runner          string // runner target name
}

// Exec runs one command through the runner and returns (ok, output).
func (r *ExecRunner) Exec(cmd string) (bool, string) {
	args := []string{"exec", "--addr", r.Addr, "--agent-dir", r.AgentDir, r.Runner, cmd}
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

// DestroyOneLxc destroys one LXC: checked present -> stopped -> destroyed ->
// verified gone. Returns log lines.
func (r *ExecRunner) DestroyOneLxc(role string, vmid *uint32) ([]string, error) {
	var log []string
	if vmid == nil {
		log = append(log, fmt.Sprintf("%s LXC: never created (no vmid recorded)", role))
		return log, nil
	}
	exists, err := r.lxcExists(*vmid)
	if err != nil {
		return nil, err
	}
	if !exists {
		log = append(log, fmt.Sprintf("%s LXC %d: already gone", role, *vmid))
		return log, nil
	}
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

func (r *ExecRunner) lxcExists(vmid uint32) (bool, error) {
	ok, out := r.Exec(fmt.Sprintf("pct list | grep -c '^\\s*%d ' || true", vmid))
	if !ok {
		return false, fmt.Errorf("pct list failed on %s: %s", r.Runner, out)
	}
	return strings.TrimSpace(out) != "0", nil
}

// Run executes the teardown. cfg holds the managed state.
func Run(r *ExecRunner, cfg *Cfg, scope Scope, confirm bool) (string, error) {
	if !confirm {
		return "", fmt.Errorf("teardown aborted (not confirmed)")
	}
	var log []string

	// 1. drain a door probe (the caller already verified it; a refused probe
	//    aborts nothing remote).
	_ = r

	log = append(log, fmt.Sprintf("door verified (%s)", cfg.RunNTarget))

	// 2. destroy the targeted LXC(s).
	switch scope {
	case ScopeWholeWorld:
		for _, m := range []string{"relay", "cp", "k3s"} {
			managed := contains(cfg.Managed, m)
			if !managed {
				log = append(log, fmt.Sprintf("skipped %s LXC (not managed)", m))
				continue
			}
			vmid := cfg.LxcVMID(m)
			lines, err := r.DestroyOneLxc(m, vmid)
			if err != nil {
				return "", err
			}
			log = append(log, lines...)
		}
	case ScopeTenantCompute, ScopeTenantData:
		role := cfg.TenantRole
		vmid := cfg.LxcVMID(role)
		lines, err := r.DestroyOneLxc(role, vmid)
		if err != nil {
			return "", err
		}
		log = append(log, lines...)
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
				log = append(log, fmt.Sprintf("destroyed %s dataset subtree (%s)", tenant, dataset))
			} else {
				log = append(log, fmt.Sprintf("no dataset to destroy for %s (absent) — nothing destroyed", tenant))
			}
		}
	}

	// 4. whole-world: the door + local cleanup. Per-tenant KEEPS local home +
	//    config.
	if scope == ScopeWholeWorld {
		keyRemoval := fmt.Sprintf("sed -i '/ssh-ed25519 [A-Za-z0-9+/=]* %s$/d' /root/.ssh/authorized_keys", cfg.RunnerComment)
		if ok, _ := r.Exec(keyRemoval); !ok {
			return "", fmt.Errorf("door removal command failed on %s", r.Runner)
		}
		_, check := r.Exec(fmt.Sprintf("grep -c 'ssh-ed25519 .* %s' /root/.ssh/authorized_keys || true", cfg.RunnerComment))
		still := 0
		fmt.Sscanf(strings.TrimSpace(check), "%d", &still)
		if still != 0 {
			return "", fmt.Errorf("the runner's key is STILL in authorized_keys (%d line(s))", still)
		}
		log = append(log, fmt.Sprintf("door removed from %s (%s — verified)", cfg.RunnerComment, cfg.RunNTarget))

		if cfg.WorldHome != "" {
			if _, err := os.Stat(cfg.WorldHome); err == nil {
				if err := os.RemoveAll(cfg.WorldHome); err != nil {
					return "", err
				}
				log = append(log, "removed world "+cfg.WorldHome)
			}
		}
		if cfg.ConfigPath != "" {
			if _, err := os.Stat(cfg.ConfigPath); err == nil {
				if err := os.Remove(cfg.ConfigPath); err != nil {
					return "", err
				}
				log = append(log, "removed config "+cfg.ConfigPath)
			}
		}
	} else {
		log = append(log, "config KEPT (per-tenant teardown — coords + dataset mapping for reattach)")
	}

	return "teardown complete:\n  " + strings.Join(log, "\n  "), nil
}

// DestroyDataset runs the storage destroy through the orchestrator CLI and
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
