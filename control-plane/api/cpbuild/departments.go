package cpbuild

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

// Department capability runners: a dedicated runner per department, owned by
// that department's identity, carrying the raw grant for one capability. The
// first is Data's read of the Proxmox box (root@<host> over SSH) so it can
// verify every LXC/kube volume lands on a backed-up mount. A department's pod
// reaches its runner directly (signed as the agent's own nsec, audience =
// runner pubkey) — the runner re-reads its relay-signed 39002 roster per call,
// so a grant/revoke lands without a restart. The CPA and custom agents hold no
// runner coords and so never see exec.
type deptRunnerSpec struct {
	// dept is the department name (matches agents.DepartmentNames).
	dept string
	// name is the runner/target/secret name (the provision convention ties all
	// three together).
	name string
	// kind is the connector kind (ssh for the Proxmox root target).
	kind string
	// port is the runner's MCP bind port on the CP LXC (the pod dials
	// http://<cpIP>:<port>).
	port int
}

// departmentRunnerSpecs is the capability-runner table. Data first; other
// departments gain their own runners as their tooling lands.
func departmentRunnerSpecs() []deptRunnerSpec {
	return []deptRunnerSpec{
		{dept: "data", name: "data-pve", kind: "ssh", port: 8790},
	}
}

// relayDialURL is the relay origin to DIAL from inside the CP guest: the
// community hostname on :3000 (the CP resolver maps it to the relay guest), so
// the request Host matches the community. Falls back to the recorded RelayURL.
func (s *Spec) relayDialURL() string {
	if s.RelayHost != "" {
		return "http://" + s.RelayHost + ":3000"
	}
	return s.RelayURL
}

// relaySignURL is the relay's CANONICAL URL for NIP-98 signing (public https).
func (s *Spec) relaySignURL() string {
	if s.RelayAuthURL != "" {
		return s.RelayAuthURL
	}
	if s.RelayHost != "" {
		return "https://" + s.RelayHost
	}
	return s.RelayURL
}

func departmentRunnerByDept(dept string) (deptRunnerSpec, bool) {
	for _, s := range departmentRunnerSpecs() {
		if s.dept == dept {
			return s, true
		}
	}
	return deptRunnerSpec{}, false
}

// consoleStateDir is the CP's console state dir (the home of state.json, the
// runner records, and console/identity.json) — derived in either executor:
// agent-tools state `/srv/data/cp/agent-tools` and console state
// `/srv/data/cp/control-plane` both resolve to `/srv/data/cp/control-plane`.
func (s *Spec) consoleStateDir() string {
	_, dir := s.cpGuestDirs()
	return dir
}

// stageDepartmentRunners provisions/starts each department's capability runner
// and records the pod-facing coords on the Spec. Idempotent + rebuild-safe: an
// existing runner package is adopted (identity preserved), its host key
// re-authorized only when absent, and its relay channel re-synced. A no-op
// without relay + co-located-runner wiring.
func (s *Spec) stageDepartmentRunners() error {
	if s.CpLxc == 0 || s.CpIP == "" || s.RelayHost == "" || s.RunnerTarget == "" || s.RelayPK == "" {
		return nil
	}
	cpState := s.consoleStateDir()
	store, err := state.Open(cpState)
	if err != nil {
		return fmt.Errorf("open CP state: %w", err)
	}
	coloc, ok := store.GetSecret(s.RunnerTarget)
	if !ok || coloc.Address == "" {
		return fmt.Errorf("department runners: co-located runner %q address not recorded", s.RunnerTarget)
	}
	for _, spec := range departmentRunnerSpecs() {
		if err := s.ensureDepartmentRunner(store, cpState, spec, coloc.Address); err != nil {
			return fmt.Errorf("department runner %s: %w", spec.name, err)
		}
	}
	return nil
}

// ensureDepartmentRunner brings one department runner up and records its coords.
// hostAddr is `root@<host>` — the same PVE box the co-located runner owns.
func (s *Spec) ensureDepartmentRunner(store *state.StateStore, cpState string, spec deptRunnerSpec, hostAddr string) error {
	pkgDir := filepath.Join(cpState, "runner", spec.name)
	rec, exists := store.GetRunner(spec.name)
	if !exists {
		priv, pub, err := crypto.GenerateSSHKeypair(spec.name)
		if err != nil {
			return fmt.Errorf("generate ssh keypair: %w", err)
		}
		res, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
			Name: spec.name, Kind: spec.kind, Address: hostAddr, Secret: priv, RunnerDir: pkgDir,
		})
		if err != nil {
			return fmt.Errorf("provision: %w", err)
		}
		rec = state.RunnerRecord{NostrPubkey: res.NostrPubkey, EncPubkey: res.EncPubkey, PackageDir: res.PackageDir}
		if err := s.authorizeHostKey(pub, spec.name); err != nil {
			return err
		}
	} else {
		pubLine, err := departmentRunnerPubLine(pkgDir, spec.name)
		if err != nil {
			return err
		}
		if err := s.authorizeHostKey(pubLine, spec.name); err != nil {
			return err
		}
	}
	// The runner is a private NIP-29 channel; membership is the live grant.
	if err := provisioner.SyncRunnerChannel(store, s.relayDialURL(), s.relaySignURL(), spec.name, cpState); err != nil {
		return fmt.Errorf("sync relay channel: %w", err)
	}
	// A relay-mode runner must be a relay COMMUNITY member to read its roster
	// (else the grant query 403s relay_membership_required and fails closed).
	if err := s.addRelayCommunityMember(rec.NostrPubkey); err != nil {
		return fmt.Errorf("relay community membership: %w", err)
	}
	if err := s.startDepartmentRunner(spec, pkgDir); err != nil {
		return fmt.Errorf("start runner: %w", err)
	}
	if s.DepartmentRunners == nil {
		s.DepartmentRunners = map[string]agent.RunnerCoords{}
	}
	s.DepartmentRunners[spec.dept] = agent.RunnerCoords{
		URL:    fmt.Sprintf("http://%s:%d", s.CpIP, spec.port),
		Pubkey: rec.NostrPubkey,
		Target: spec.name,
		Secret: spec.name,
	}
	return nil
}

// grantDepartmentRunner adds a department agent's pubkey to its capability
// runner's roster (kind-9000 put-user, signed by the console owner). Called
// after the department pod's identity exists so a rebuild re-asserts the grant.
func (s *Spec) grantDepartmentRunner(dept, pubkey string) error {
	spec, ok := departmentRunnerByDept(dept)
	if !ok || pubkey == "" {
		return nil
	}
	// The stage is the source of truth for whether the runner was stood up:
	// when its guard short-circuits (no relay / co-located runner / CP IP) the
	// coords are absent and there is nothing to grant. Skip rather than fail
	// the whole build with a "runner not found" from a path we deliberately
	// no-oped.
	if rc, staged := s.DepartmentRunners[dept]; !staged || rc.Pubkey == "" {
		return nil
	}
	cpState := s.consoleStateDir()
	store, err := state.Open(cpState)
	if err != nil {
		return err
	}
	return provisioner.PutUserMembership(store, s.relayDialURL(), s.relaySignURL(), spec.name, pubkey, cpState)
}

// departmentRunnerPubLine opens a runner package's sealed SSH credential with
// the runner's own encryption key and derives its authorized_keys line — so a
// rebuild re-authorizes the SAME key instead of minting a new identity.
func departmentRunnerPubLine(pkgDir, name string) (string, error) {
	var id struct {
		EncSecretHex string `json:"enc_secret_hex"`
	}
	raw, err := os.ReadFile(filepath.Join(pkgDir, "identity.json"))
	if err != nil {
		return "", fmt.Errorf("read runner identity: %w", err)
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", fmt.Errorf("parse runner identity: %w", err)
	}
	enc, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return "", err
	}
	pkg, err := wire.Load(pkgDir)
	if err != nil {
		return "", fmt.Errorf("load runner package: %w", err)
	}
	blob, err := hex.DecodeString(pkg.Secrets[name])
	if err != nil {
		return "", err
	}
	pem, err := crypto.Open(enc, []byte(name), blob)
	if err != nil {
		return "", fmt.Errorf("open runner credential: %w", err)
	}
	return crypto.ExtractED25519PublicKeyLine(pem)
}

// authorizeHostKey appends a public key line to the PVE host's root
// authorized_keys if its key blob is not already present (idempotent). Runs on
// the host through the co-located runner.
func (s *Spec) authorizeHostKey(pubLine, comment string) error {
	fields := strings.Fields(pubLine)
	if len(fields) < 2 {
		return fmt.Errorf("malformed public key for %s", comment)
	}
	blob := fields[1]
	cmd := fmt.Sprintf(
		`mkdir -p /root/.ssh; chmod 700 /root/.ssh; touch /root/.ssh/authorized_keys; grep -qF '%s' /root/.ssh/authorized_keys || echo '%s' >> /root/.ssh/authorized_keys`,
		blob, pubLine)
	if _, err := s.execOut(cmd, 60); err != nil {
		return fmt.Errorf("authorize %s on host: %w", comment, err)
	}
	return nil
}

// addRelayCommunityMember adds a runner pubkey to the relay community so it can
// read its signed roster (buzz-admin runs inside the relay LXC).
func (s *Spec) addRelayCommunityMember(pubkey string) error {
	relayLxc := s.RelayLxc
	if relayLxc == 0 {
		if err := s.resolveGuestVmids(); err != nil {
			return err
		}
		relayLxc = s.RelayLxc
	}
	if relayLxc == 0 || s.RelayCompose == "" {
		return fmt.Errorf("relay lxc/compose not recorded")
	}
	cmdLine := fmt.Sprintf("cd %s && docker compose exec -T relay buzz-admin add-member --pubkey %s", s.RelayCompose, pubkey)
	return s.run(fmt.Sprintf("pct exec %d -- sh -c '%s'", relayLxc, cmdLine), 120)
}

// startDepartmentRunner (re)starts the runner as a transient systemd unit on
// the CP LXC (systemctl/systemd-run run LOCALLY — the CP executor is this
// guest). Reloads the binary + package on every build.
func (s *Spec) startDepartmentRunner(spec deptRunnerSpec, pkgDir string) error {
	binDir, _ := s.cpGuestDirs()
	bin := binDir + "/freehold-runner"
	// Always pass --relay-auth-url: the runner signs the CANONICAL URL while
	// dialing the LAN origin. relaySignURL() falls back to the dial URL when no
	// public origin is known (a no-op), but if it resolves to https://<host>
	// while the dial is http://<host>:3000, omitting it would make the runner
	// sign the LAN URL and the relay would reject it "URL mismatch". Always
	// threading it keeps the dial/auth split intact.
	flags := fmt.Sprintf("--state-dir %s --addr 0.0.0.0:%d --relay-url %s --relay-pubkey %s --relay-auth-url %s --allow-remote",
		pkgDir, spec.port, s.relayDialURL(), s.RelayPK, s.relaySignURL())
	unit := "freehold-runner-" + spec.dept
	script := fmt.Sprintf(
		"systemctl stop %s 2>/dev/null; systemctl reset-failed %s 2>/dev/null; systemd-run --unit=%s --collect %s serve %s",
		unit, unit, unit, bin, flags)
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
	}
	// Wait for the port to listen so the next step never races a refused dial.
	addr := fmt.Sprintf("127.0.0.1:%d", spec.port)
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
		if derr == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("department runner %s did not listen on %s: %v", spec.name, addr, derr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
