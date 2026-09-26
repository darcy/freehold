package cpbuild

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cert "freehold/platform/services/certificates/letsencrypt"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

// Capability runners: the unit of grant is the RUNNER — one runner per
// capability, named <target>-<protocol>-<identity> (target = the box/service
// it reaches, protocol = ssh|api|local matching the connector class,
// identity = the credential level). A department holds a grant = a roster
// membership on each runner its role needs; several departments share a
// runner only when the capability is identical (pve-ssh-root). Each runner is
// its own relay channel (the audit stream) and re-reads its relay-signed
// 39002 roster per call, so a grant/revoke lands live. A department's pod
// reaches its runners directly (signed as the agent's own nsec, audience =
// runner pubkey); the CPA and custom agents hold no runner coords and never
// see exec.
type capabilityRunner struct {
	// name is the runner/target/secret name (the provision convention ties
	// all three together) — capability-named, never consumer-named.
	name string
	// kind is the connector kind (TargetMeta.kind): ssh, or an api-class
	// kind (kubernetes/litellm/cloudflare) whose exec runs locally with the
	// credential + address injected as env, or local (the runner's own host).
	kind string
	// port is the runner's MCP bind port on the CP LXC (the pod dials
	// http://<cpIP>:<port>).
	port int
	// rosters are the departments granted onto this runner.
	rosters []string
	// kubernetes doors only: the SA-token Secret (declared in doors.tf) the
	// token is read back from, re-sealed every build (a k3s rebuild rotates
	// the CA, so a stale token would fail closed).
	tokenSecret string
	tokenNS     string
	// dns-provider doors only: the zone served + the stored provider env
	// (the credential fields seal as extra named secrets).
	dnsZone string
	dnsEnv  map[string]string
	// addr overrides the ssh target address (dynamic runners: the operator's
	// box, not the PVE host the co-located runner owns).
	addr string
	// dynamic marks an agent-provisioned runner (a state CapabilityRecord):
	// adopt-only on rebuild — its credential came from the operator, there is
	// no build-time source to re-seal from, and the ssh pubkey is installed on
	// the TARGET box (by the operator), never on the PVE host.
	dynamic bool
}

// kubernetesVersion is the kubectl build installed on the CP LXC (the kube
// doors' exec target) — matches the k3s pin in terraform/scripts/k3s-bringup.sh.
const kubernetesVersion = "v1.36.4"

// cloudflareAPIBase is the cloudflare API v4 root (the door's address env).
const cloudflareAPIBase = "https://api.cloudflare.com/client/v4"

// capabilityRunners is the static capability-runner table. Dynamic entries
// (one cloudflare-api-<domain> runner per stored DNS zone, and one runner per
// agent-provisioned CapabilityRecord) are appended from the CP's state at
// staging.
func capabilityRunners() []capabilityRunner {
	return []capabilityRunner{
		{name: "pve-ssh-root", kind: "ssh", port: 8791,
			rosters: []string{"network", "compute", "data"}},
		{name: "kube-api-root", kind: "kubernetes", port: 8792,
			rosters: []string{"compute"}, tokenSecret: "compute-door-token", tokenNS: "kube-system"},
		{name: "kube-api-caddysa", kind: "kubernetes", port: 8793,
			rosters: []string{"network"}, tokenSecret: "caddy-door-token", tokenNS: "caddy"},
		{name: "kube-api-litellmsa", kind: "kubernetes", port: 8794,
			rosters: []string{"ai"}, tokenSecret: "litellm-door-token", tokenNS: "litellm"},
		{name: "litellm-api-admin", kind: "litellm", port: 8795,
			rosters: []string{"ai"}},
		{name: "dnsmasq-local-root", kind: "local", port: 8796,
			rosters: []string{"network"}},
	}
}

// cloudflareRunnerPort is where the dynamic per-zone runners start, after the
// static table's highest port. Agent-provisioned capability records start
// above it (dynamicRunnerPortBase) so the two derivation paths never collide.
const cloudflareRunnerPort = 8797

// dynamicRunnerPortBase is where agent-provisioned capability records start
// (the per-zone DNS doors occupy at most the cp+relay slots above 8796).
const dynamicRunnerPortBase = 8800

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

// consoleStateDir is the CP's console state dir (the home of state.json, the
// runner records, and console/identity.json) — derived in either executor:
// agent-tools state `/srv/data/cp/agent-tools` and console state
// `/srv/data/cp/control-plane` both resolve to `/srv/data/cp/control-plane`.
func (s *Spec) consoleStateDir() string {
	_, dir := s.cpGuestDirs()
	return dir
}

// stageDepartmentRunners provisions/starts every capability runner and records
// the pod-facing coords per department. Idempotent + rebuild-safe: an existing
// runner package is adopted (identity preserved), ssh keys are re-authorized
// only when absent, rotating credentials (kube tokens, API keys) are re-sealed
// every build, and relay channels re-sync. A no-op without relay +
// co-located-runner wiring.
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
	runners := capabilityRunners()
	runners = append(runners, s.cloudflareRunners(store)...)
	runners = append(runners, dynamicRunners(store)...)
	needKube := false
	for _, r := range runners {
		if r.kind == "kubernetes" {
			needKube = true
		}
	}
	if needKube {
		if err := s.ensureKubectl(); err != nil {
			return fmt.Errorf("kubectl for the kube doors: %w", err)
		}
	}
	for _, r := range runners {
		if err := s.ensureCapabilityRunner(store, cpState, r, coloc.Address); err != nil {
			// The shared host runner must never silently die (Data's live
			// grant rides it); every other door waits loudly for its source
			// (a DNS credential not yet handed off, the services phase not
			// yet up) — the next build lights it.
			if r.name == "pve-ssh-root" {
				return fmt.Errorf("capability runner %s: %w", r.name, err)
			}
			fmt.Fprintf(os.Stderr, "capability runners: %s not staged: %v (the door waits for its source)\n", r.name, err)
			continue
		}
	}
	// Retired runners (renames keep the capability name, drop the
	// consumer-named one) die here — after the replacement is staged, so a
	// department never loses exec across the build.
	s.retireRunner(store, "data-pve")
	return nil
}

// cloudflareRunners derives one cloudflare-api-<domain> runner per DISTINCT
// DNS zone with a stored credential (the CP's own slots: cp, then relay).
// Skipped (loud, not fatal) when no credential has been handed off yet — the
// door waits for `freehold build` to hand it off. kind = the record's provider
// (a non-cloudflare provider probes red until its status arm exists). Ports
// skip anything an agent-provisioned capability record already holds.
func (s *Spec) cloudflareRunners(store *state.StateStore) []capabilityRunner {
	var out []capabilityRunner
	port := cloudflareRunnerPort
	occupied := occupiedPorts(store)
	seen := map[string]bool{}
	for _, slot := range []string{"cp", "relay"} {
		host := s.CpHost
		if slot == "relay" {
			host = s.RelayHost
		}
		zone := zoneOf(host)
		if zone == "" || seen[zone] {
			continue
		}
		provider, env, err := s.dnsCredFromStore(slot)
		if err != nil || len(env) == 0 {
			fmt.Fprintf(os.Stderr, "capability runners: no %s DNS credential yet — the %s door waits for hand-off\n", slot, zone)
			continue
		}
		seen[zone] = true
		for occupied[port] {
			port++
		}
		out = append(out, capabilityRunner{
			name:    "cloudflare-api-" + strings.ReplaceAll(zone, ".", "-"),
			kind:    strings.ToLower(provider),
			port:    port,
			rosters: []string{"network"},
			dnsZone: zone,
			dnsEnv:  env,
		})
		port++
	}
	return out
}

// dynamicRunners maps the state store's capability records (the
// provision_runner flow's agent-provisioned runners) onto the staging table.
func dynamicRunners(store *state.StateStore) []capabilityRunner {
	var out []capabilityRunner
	for name, rec := range store.Capabilities() {
		rosters := append([]string(nil), rec.Rosters...)
		out = append(out, capabilityRunner{
			name: name, kind: rec.Kind, port: rec.Port,
			rosters: rosters, addr: rec.Address, dynamic: true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// occupiedPorts collects every port the capability-runner table can bind: the
// static runners, the agent-provisioned records (fixed at creation), and the
// per-zone DNS doors' derivation base. A new record allocates above all of
// them so pod env coords stay stable across rebuilds.
func occupiedPorts(store *state.StateStore) map[int]bool {
	occupied := map[int]bool{}
	for _, r := range capabilityRunners() {
		occupied[r.port] = true
	}
	for _, rec := range store.Capabilities() {
		occupied[rec.Port] = true
	}
	return occupied
}

// NextCapabilityPort returns the first free MCP port above
// dynamicRunnerPortBase, skipping every port the capability-runner table can
// bind: the static runners, the recorded capabilities, and the per-zone DNS
// doors (derived — with enough zones their ports climb toward the dynamic
// base). Shared by the agent flow AND the console's rosters path, so both
// allocators always agree.
func (s *Spec) NextCapabilityPort(store *state.StateStore) int {
	occupied := occupiedPorts(store)
	for _, r := range s.cloudflareRunners(store) {
		occupied[r.port] = true
	}
	port := dynamicRunnerPortBase
	for occupied[port] {
		port++
	}
	return port
}

// OccupiedCapabilityPorts exposes the recorded+static port set for surfaces
// without a build Spec (the console's fallback allocator).
func OccupiedCapabilityPorts(store *state.StateStore) map[int]bool {
	return occupiedPorts(store)
}

// zoneOf returns a host's registrable zone: everything after the first label
// ("cp.example.com" -> "example.com"). "" for a bare host.
func zoneOf(host string) string {
	if i := strings.Index(host, "."); i >= 0 && i < len(host)-1 {
		return host[i+1:]
	}
	return ""
}

// ensureCapabilityRunner brings one capability runner up and records its coords
// for every rostered department. hostAddr is `root@<host>` — the same PVE box
// the co-located runner owns.
func (s *Spec) ensureCapabilityRunner(store *state.StateStore, cpState string, r capabilityRunner, hostAddr string) error {
	pkgDir := filepath.Join(cpState, "runner", r.name)
	rec, exists := store.GetRunner(r.name)
	if !exists {
		if r.dynamic {
			// An agent-provisioned runner whose package vanished: its
			// credential came from the operator and cannot be re-derived —
			// fail loudly (the door waits) instead of silently re-keying.
			return fmt.Errorf("capability record %s has no runner package — re-provision it (the credential is not re-derivable)", r.name)
		}
		c, err := s.runnerCredential(r, hostAddr)
		if err != nil {
			return err
		}
		res, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
			Name: r.name, Kind: r.kind, Address: c.addr, Secret: c.secret, RunnerDir: pkgDir,
		})
		if err != nil {
			return fmt.Errorf("provision: %w", err)
		}
		rec = state.RunnerRecord{NostrPubkey: res.NostrPubkey, EncPubkey: res.EncPubkey, PackageDir: res.PackageDir}
		for _, name := range sortedNames(c.extras) {
			if _, err := provisioner.AddSecret(store, r.name, name, c.extras[name]); err != nil {
				return fmt.Errorf("seal extra %s: %w", name, err)
			}
		}
		// A dynamic ssh door's pubkey is installed on the TARGET box by the
		// operator (the provision flow hands the line back) — never appended to
		// the PVE host's authorized_keys.
		if r.kind == "ssh" && !r.dynamic {
			if err := s.authorizeHostKey(c.pubLine, r.name); err != nil {
				return err
			}
		}
	} else if r.dynamic {
		// Adopt-only: the record is the spec, the package holds the operator's
		// credential, and no build-time source exists to re-seal from. The
		// channel sync + (re)start below are the re-assert.
	} else {
		// ssh credentials are the runner's identity — stable; re-authorize the
		// SAME key if the host lost it. Everything else rotates (a k3s rebuild
		// rotates the CA; API keys are re-read from their source), so re-seal
		// every build — the runner restarts below and loads them. AddSecret
		// re-ships the whole package (targets + grants preserved).
		if r.kind == "ssh" {
			pubLine, err := departmentRunnerPubLine(pkgDir, r.name)
			if err != nil {
				return err
			}
			if err := s.authorizeHostKey(pubLine, r.name); err != nil {
				return err
			}
		} else {
			c, err := s.runnerCredential(r, hostAddr)
			if err != nil {
				return err
			}
			// Preserve the primary record's kind/address (AddSecret stamps
			// "extra"); the package is the truth, the record mirrors it.
			before, _ := store.GetSecret(r.name)
			if _, err := provisioner.AddSecret(store, r.name, r.name, c.secret); err != nil {
				return fmt.Errorf("re-seal credential: %w", err)
			}
			if rec, ok := store.GetSecret(r.name); ok {
				rec.Kind = before.Kind
				rec.Address = c.addr
				store.InsertSecret(r.name, rec)
				if err := store.Save(); err != nil {
					return err
				}
			}
			if c.addr != pkgTargetAddr(pkgDir, r.name) {
				if err := setTargetAddress(store, r.name, c.addr); err != nil {
					return fmt.Errorf("move target address: %w", err)
				}
			}
			for _, name := range sortedNames(c.extras) {
				if _, err := provisioner.AddSecret(store, r.name, name, c.extras[name]); err != nil {
					return fmt.Errorf("re-seal extra %s: %w", name, err)
				}
			}
		}
	}
	// The runner is a private NIP-29 channel; membership is the live grant.
	if err := provisioner.SyncRunnerChannel(store, s.relayDialURL(), s.relaySignURL(), r.name, cpState); err != nil {
		return fmt.Errorf("sync relay channel: %w", err)
	}
	// A relay-mode runner must be a relay COMMUNITY member to read its roster
	// (else the grant query 403s relay_membership_required and fails closed).
	if err := s.addRelayCommunityMember(rec.NostrPubkey); err != nil {
		return fmt.Errorf("relay community membership: %w", err)
	}
	if err := s.startCapabilityRunner(r, pkgDir); err != nil {
		return fmt.Errorf("start runner: %w", err)
	}
	if s.DepartmentRunners == nil {
		s.DepartmentRunners = map[string][]agent.RunnerCoords{}
	}
	coords := agent.RunnerCoords{
		URL:    fmt.Sprintf("http://%s:%d", s.CpIP, r.port),
		Pubkey: rec.NostrPubkey,
		Target: r.name,
		Secret: r.name,
	}
	for _, dept := range r.rosters {
		s.DepartmentRunners[dept] = append(s.DepartmentRunners[dept], coords)
	}
	return nil
}

// runnerCred is a capability runner's resolved credential material.
type runnerCred struct {
	addr    string
	secret  []byte
	extras  map[string][]byte
	pubLine string // ssh only: the authorized_keys line for the fresh keypair
}

// runnerCredential resolves a runner's target address + credential + extra
// named secrets for its kind, SOURCING them fresh on every call (the kube
// tokens and API keys are re-read from where they live; only the ssh keypair
// is minted once and then stable).
func (s *Spec) runnerCredential(r capabilityRunner, hostAddr string) (runnerCred, error) {
	switch r.kind {
	case "ssh":
		addr := hostAddr
		if r.addr != "" {
			addr = r.addr
		}
		// The authorized_keys comment carries the world so keys from several
		// worlds on one host stay tell-apart-able (freehold-<name>-<runner>).
		comment := r.name
		if s.Name != "" {
			comment = "freehold-" + s.Name + "-" + r.name
		}
		priv, pub, err := crypto.GenerateSSHKeypair(comment)
		if err != nil {
			return runnerCred{}, fmt.Errorf("generate ssh keypair: %w", err)
		}
		return runnerCred{addr: addr, secret: priv, pubLine: pub}, nil
	case "kubernetes":
		token, err := s.doorToken(r.tokenSecret, r.tokenNS)
		if err != nil {
			return runnerCred{}, err
		}
		return runnerCred{addr: "https://" + config.StripCIDR(s.ProxyIP) + ":6443", secret: token}, nil
	case "litellm":
		master, provider, err := s.litellmDoorKeys()
		if err != nil {
			return runnerCred{}, err
		}
		return runnerCred{
			addr:   strings.TrimSuffix(s.LitellmBaseURL, "/v1"),
			secret: master,
			extras: map[string][]byte{"provider-key": provider},
		}, nil
	case "local":
		// The credential is a placeholder by convention (secret name = runner
		// name); the door IS the runner's own host.
		return runnerCred{secret: []byte("local")}, nil
	default:
		// A DNS-provider door (cloudflare today): the API token is the target
		// credential (the runner's own self-check verifies it), the remaining
		// provider env + the zone seal as extra named secrets whose names
		// round-trip back to the same env names.
		if r.dnsZone == "" || len(r.dnsEnv) == 0 {
			return runnerCred{}, fmt.Errorf("dns door %s has no zone/env", r.name)
		}
		tokenKey := dnsTokenKey(r.dnsEnv)
		extras := map[string][]byte{"zone": []byte(r.dnsZone)}
		for env, value := range r.dnsEnv {
			if env == tokenKey {
				continue
			}
			extras[envToLower(env)] = []byte(value)
		}
		return runnerCred{addr: cloudflareAPIBase, secret: []byte(r.dnsEnv[tokenKey]), extras: extras}, nil
	}
}

// dnsTokenKey picks the credential-bearing env var of a stored DNS-provider
// credential (the runner's verify probe uses it as the bearer). Preference:
// the lego DNS-token names, then any *TOKEN, then the first key sorted.
func dnsTokenKey(env map[string]string) string {
	for _, preferred := range []string{"CF_DNS_API_TOKEN", "CF_API_TOKEN"} {
		if _, ok := env[preferred]; ok {
			return preferred
		}
	}
	for _, k := range sortedStrings(env) {
		if strings.HasSuffix(k, "TOKEN") {
			return k
		}
	}
	return sortedStrings(env)[0]
}

func sortedStrings(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// envToLower maps an env var name to a secret name ("CF_DNS_API_TOKEN" ->
// "cf-dns-api-token") so the runner's env_name() re-derives the same env.
func envToLower(env string) string {
	return strings.ToLower(strings.ReplaceAll(env, "_", "-"))
}

// sortedNames keeps the extra-seal order deterministic (tests + logs).
func sortedNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pkgTargetAddr reads a runner's shipped target address from its PACKAGE (the
// source of truth for target metadata; the state record mirrors it).
func pkgTargetAddr(pkgDir, name string) string {
	pkg, err := wire.Load(pkgDir)
	if err != nil {
		return ""
	}
	return pkg.Targets[name].Address
}

// setTargetAddress updates a runner's shipped TargetMeta address + its state
// record when the endpoint moved (e.g. a re-IPed k3s node). AddSecret cannot
// do this (it touches only secrets); a full rotate would drop the extras.
func setTargetAddress(store *state.StateStore, name, addr string) error {
	rec, ok := store.GetRunner(name)
	if !ok {
		return fmt.Errorf("runner %s not found", name)
	}
	pkg, err := wire.Load(rec.PackageDir)
	if err != nil {
		return err
	}
	meta, ok := pkg.Targets[name]
	if !ok {
		return fmt.Errorf("runner %s has no target metadata", name)
	}
	meta.Address = addr
	pkg.Targets[name] = meta
	if err := pkg.WriteToDir(rec.PackageDir); err != nil {
		return err
	}
	secretRec, _ := store.GetSecret(name)
	secretRec.Address = addr
	store.InsertSecret(name, secretRec)
	return store.Save()
}

// doorToken reads a door's SA token back from the k3s guest (kubectl on the
// guest through the co-located runner) and decodes it. The token passes
// through CP memory only — it is sealed into the runner's package, never
// stored or logged.
func (s *Spec) doorToken(secret, ns string) ([]byte, error) {
	out, err := s.execOut(fmt.Sprintf(
		`pct exec %d -- /usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get secret %s -n %s -o jsonpath='{.data.token}'`,
		s.K3sVmid, secret, ns), 60)
	if err != nil {
		return nil, fmt.Errorf("read door token %s/%s: %w", ns, secret, err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("decode door token %s/%s: %w", ns, secret, err)
	}
	return raw, nil
}

// litellmDoorKeys opens the CP's durable litellm store in memory and returns
// the gateway master key + the provider (fireworks) key. Plaintext lives in
// memory only long enough to seal into the door runner's package.
func (s *Spec) litellmDoorKeys() (master, provider []byte, err error) {
	path := filepath.Join(s.StateDir, "world-secrets", "litellm.json")
	if !cert.CredExists(path) {
		return nil, nil, fmt.Errorf("no CP litellm store at %s yet — the litellm-api-admin door waits for the services phase", path)
	}
	secret, err := s.consoleEncSecret()
	if err != nil {
		return nil, nil, err
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	_, env, err := cert.LoadCreds(path, open, secret)
	if err != nil {
		return nil, nil, fmt.Errorf("open CP litellm store: %w", err)
	}
	m, ok := env["master"]
	if !ok || m == "" {
		return nil, nil, fmt.Errorf("CP litellm store is missing the master key")
	}
	p, ok := env["provider"]
	if !ok || p == "" {
		return nil, nil, fmt.Errorf("CP litellm store is missing the provider key")
	}
	return []byte(m), []byte(p), nil
}

// ensureKubectl installs kubectl on the CP LXC (the kube doors' exec target)
// when absent — arch-aware, pinned to the k3s version. Runs locally: the build
// executor is this guest.
func (s *Spec) ensureKubectl() error {
	if _, err := exec.Command("sh", "-c", "command -v kubectl").Output(); err == nil {
		return nil
	}
	arch := "amd64"
	if out, err := exec.Command("uname", "-m").Output(); err == nil && strings.TrimSpace(string(out)) == "aarch64" {
		arch = "arm64"
	}
	script := fmt.Sprintf(
		"curl -fsSL --max-time 120 https://dl.k8s.io/release/%s/bin/linux/%s/kubectl -o /usr/local/bin/kubectl && chmod +x /usr/local/bin/kubectl",
		kubernetesVersion, arch)
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		return fmt.Errorf("install kubectl: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// grantDepartmentRunner adds a department agent's pubkey to every capability
// runner its role holds (kind-9000 put-user, signed by the console owner).
// Called after the department pod's identity exists so a rebuild re-asserts
// the grants.
func (s *Spec) grantDepartmentRunner(dept, pubkey string) error {
	if pubkey == "" {
		return nil
	}
	// The stage is the source of truth for whether a runner was stood up:
	// when its guard short-circuits (no relay / co-located runner / CP IP) the
	// coords are absent and there is nothing to grant. Skip rather than fail
	// the whole build with a "runner not found" from a path we deliberately
	// no-oped.
	if len(s.DepartmentRunners[dept]) == 0 {
		return nil
	}
	store, err := state.Open(s.consoleStateDir())
	if err != nil {
		return err
	}
	for _, rc := range s.DepartmentRunners[dept] {
		if rc.Pubkey == "" || rc.Target == "" {
			continue
		}
		if err := provisioner.PutUserMembership(store, s.relayDialURL(), s.relaySignURL(), rc.Target, pubkey, s.consoleStateDir()); err != nil {
			return err
		}
	}
	return nil
}

// retireRunner removes a runner retired by a rename (data-pve -> pve-ssh-root):
// stop its unit, revoke it (the state record flips + the shipped ciphertext is
// removed), and drop its authorized_keys line from the host. Idempotent; runs
// AFTER the replacement is staged so a department never loses exec across the
// build. Best-effort: a half-completed earlier retire (revoked, key line still
// on the host) cannot re-derive the key blob once the package is gone — the
// dead line is then a hygiene issue, not access (the identity is revoked).
func (s *Spec) retireRunner(store *state.StateStore, name string) {
	rec, ok := store.GetRunner(name)
	if !ok {
		return
	}
	// The old department-runner convention named the unit after the DEPARTMENT
	// ("freehold-runner-data"); the new one after the RUNNER. Stop both.
	script := "systemctl stop freehold-runner-" + name + " 2>/dev/null; " +
		"systemctl stop freehold-runner-data 2>/dev/null; " +
		"systemctl reset-failed freehold-runner-" + name + " 2>/dev/null; " +
		"systemctl reset-failed freehold-runner-data 2>/dev/null; true"
	_, _ = exec.Command("sh", "-c", script).CombinedOutput()
	if rec.Status != state.RunnerRevoked {
		pubLine, perr := departmentRunnerPubLine(rec.PackageDir, name)
		if _, rerr := provisioner.RevokeRunner(store, name); rerr != nil {
			fmt.Fprintf(os.Stderr, "retire %s: revoke failed: %v\n", name, rerr)
			return
		}
		if perr == nil {
			s.deauthorizeHostKey(pubLine, name)
		}
	}
}

// deauthorizeHostKey removes a retired runner's key line from the PVE host's
// root authorized_keys (idempotent). Runs on the host through the co-located
// runner; the key blob is base64 — it can never contain the '|' delimiter.
func (s *Spec) deauthorizeHostKey(pubLine, comment string) {
	fields := strings.Fields(pubLine)
	if len(fields) < 2 {
		return
	}
	cmd := fmt.Sprintf(`touch /root/.ssh/authorized_keys; sed -i '\|%s|d' /root/.ssh/authorized_keys`, fields[1])
	if _, err := s.execOut(cmd, 60); err != nil {
		fmt.Fprintf(os.Stderr, "retire %s: deauthorize on host: %v\n", comment, err)
	}
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

// startCapabilityRunner (re)starts the runner as a transient systemd unit on
// the CP LXC (systemctl/systemd-run run LOCALLY — the CP executor is this
// guest). Reloads the binary + package on every build.
func (s *Spec) startCapabilityRunner(r capabilityRunner, pkgDir string) error {
	binDir, _ := s.cpGuestDirs()
	bin := binDir + "/freehold-runner"
	// Always pass --relay-auth-url: the runner signs the CANONICAL URL while
	// dialing the LAN origin. relaySignURL() falls back to the dial URL when no
	// public origin is known (a no-op), but if it resolves to https://<host>
	// while the dial is http://<host>:3000, omitting it would make the runner
	// sign the LAN URL and the relay would reject it "URL mismatch". Always
	// threading it keeps the dial/auth split intact.
	flags := fmt.Sprintf("--state-dir %s --addr 0.0.0.0:%d --relay-url %s --relay-pubkey %s --relay-auth-url %s --allow-remote",
		pkgDir, r.port, s.relayDialURL(), s.RelayPK, s.relaySignURL())
	unit := "freehold-runner-" + r.name
	script := fmt.Sprintf(
		"systemctl stop %s 2>/dev/null; systemctl reset-failed %s 2>/dev/null; systemd-run --unit=%s --collect %s serve %s",
		unit, unit, unit, bin, flags)
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run %s: %v: %s", unit, err, strings.TrimSpace(string(out)))
	}
	// Wait for the port to listen so the next step never races a refused dial.
	addr := fmt.Sprintf("127.0.0.1:%d", r.port)
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
		if derr == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("capability runner %s did not listen on %s: %v", r.name, addr, derr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
