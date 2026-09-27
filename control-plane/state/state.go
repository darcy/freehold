// Package state reproduces the control-plane state store
// (control-plane/src/state.rs) — CP state.json: runners + secrets.
// The CP is a SECRET PROVISIONER: records hold public keys and ciphertext
// only, never plaintext or private keys.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// StateFile is the on-disk CP state file.
	StateFile = "state.json"
	// StateDirEnv overrides the CP state dir.
	StateDirEnv = "FREEHOLD_CP_STATE_DIR"
)

// RunnerStatus is the lowercased runner lifecycle status enum.
type RunnerStatus string

const (
	RunnerActive  RunnerStatus = "active"
	RunnerRevoked RunnerStatus = "revoked"
)

// RunnerRecord is a runner's public record (pubkeys, package path, status).
type RunnerRecord struct {
	NostrPubkey string       `json:"nostr_pubkey"`
	EncPubkey   string       `json:"enc_pubkey"`
	Status      RunnerStatus `json:"status"`
	PackageDir  string       `json:"package_dir"`
	CreatedAt   uint64       `json:"created_at"`
	McpAddr     *string      `json:"mcp_addr,omitempty"`
	RiskLevel   *string      `json:"risk_level,omitempty"`
}

// AgentRecord is a named AI agent's registry row.
type AgentRecord struct {
	Pubkey    string  `json:"pubkey"`
	CreatedAt uint64  `json:"created_at"`
	Channel   *string `json:"channel,omitempty"`
}

// DnsRecord is one explicit DNS record the CP resolver serves (name -> record).
type DnsRecord struct {
	IP        string `json:"ip"`
	Source    string `json:"source"`
	CreatedAt uint64 `json:"created_at"`
}

// WorldService is one of the deployed world's health-monitored services
// (k3s / litellm / caddy): coords the CP records at build and serves over
// /api/world so any logged-in management box renders the live world (instead of
// only the box that deployed it probing its own local coords). The CP probes
// them co-located from its own LXC. Only public coords — no secrets.
type WorldService struct {
	// Kind is the service kind: "k3s", "litellm" or "caddy".
	Kind string `json:"kind"`
	// URL is the probe target — https://<host>:6443 (k3s), the gateway health
	// endpoint (litellm), https://<edge-host> (caddy).
	URL string `json:"url"`
	// ReachHost is an optional host for a plain reachability probe (caddy
	// https edge) independent of the URL path.
	ReachHost string `json:"reach_host,omitempty"`
	CreatedAt uint64 `json:"created_at"`
}

// DnsWildcard is the resolver's wildcard apex (all subdomains of apex -> ip).
type DnsWildcard struct {
	Apex      string `json:"apex"`
	IP        string `json:"ip"`
	Source    string `json:"source"`
	CreatedAt uint64 `json:"created_at"`
}

// SecretRecord is a runner's secret (ciphertext only).
type SecretRecord struct {
	Runner        string  `json:"runner"`
	Kind          string  `json:"kind"`
	Address       string  `json:"address"`
	CiphertextHex string  `json:"ciphertext_hex"`
	CreatedAt     uint64  `json:"created_at"`
	RotatedAt     *uint64 `json:"rotated_at,omitempty"`
}

// CapabilityRecord is an agent-provisioned capability runner's spec (the
// provision_runner flow): the dynamic half of the capability-runner table. A
// record makes an on-the-fly runner rebuild-safe — stageDepartmentRunners
// re-stages it every build, adopting the existing package (its credential came
// from the operator, so there is no build-time source to re-seal from). The
// record's presence IS the "the CPA provisioned this" marker: grants via the
// agent flow attach only to runners recorded here.
type CapabilityRecord struct {
	// Kind is the connector kind: ssh, or an api-class kind (unifi, ...).
	Kind string `json:"kind"`
	// Address is the target endpoint: user@host[:port] (ssh) or a base URL
	// (api-class).
	Address string `json:"address"`
	// Port is the runner's MCP bind port on the CP LXC — fixed at creation so
	// pod env coords stay stable across rebuilds.
	Port int `json:"port"`
	// Rosters are the agent names granted onto this runner (a department or a
	// custom agent the CPA provisioned it for); re-asserted on every rebuild.
	Rosters []string `json:"rosters"`
	// Origin is who provisioned it: "agent" (the CPA's provision_runner — the
	// only records the agent flow may re-provision/grant onto) or "operator"
	// (the console's rosters path — rebuild-safe but agent-untouchable). ""
	// reads as "agent" (records predate the field).
	Origin string `json:"origin,omitempty"`
	// Hosted is where the runner process lives: "" (the CP guest — the CP
	// stages and restarts its systemd unit) or "self" (resident on the
	// target: the runner was installed ON the target box — e.g. a sandbox
	// LXC — and enrolled with presented pubkeys; the CP never holds its
	// identity, ships no package, and starts nothing for it).
	Hosted string `json:"hosted,omitempty"`
	// Host is a self-hosted runner's dial target — the box's pinned name (a
	// bare host; the CP allocates the port) so a re-IPed guest keeps its
	// coords. Pod coords dial it instead of the CP IP.
	Host string `json:"host,omitempty"`
	// EnrollConfirmedAt is when the operator CONFIRMED the presented pubkeys
	// on the door page (against the guest's own `runner enroll` output /
	// Compute's audited report). Until then the console refuses the
	// credential fill — the barrier that keeps a compromised provisioning
	// agent from sealing to its own key.
	EnrollConfirmedAt *uint64 `json:"enroll_confirmed_at,omitempty"`
	CreatedAt         uint64  `json:"created_at"`
}

// Capability origins.
const (
	OriginAgent    = "agent"
	OriginOperator = "operator"
	// HostedSelf marks a runner resident on its own target (the runner-client
	// enroll flow); "" is the CP-guest hosting.
	HostedSelf = "self"
)

// AgentProvisioned reports whether the CPA's flow owns this record. Strict:
// only an explicit "agent" counts. An empty Origin reads as operator — the
// safe direction — because the console stamps "operator" and the agent flow
// stamps "agent", so a record WITHOUT one predates the field and its
// provenance is unprovable; an unset provenance must never widen what the
// agent surface may grant onto or take away. The cost lands on worlds with
// records from before the field existed: those doors are operator-owned now,
// and the console's ordinary re-provision re-enables them.
func (r CapabilityRecord) AgentProvisioned() bool {
	return r.Origin == OriginAgent
}

// RetiredCapability is the guard note a capability door leaves when it is
// retired: when it went away, who retired it, and who held the roster. It
// makes the retirement auditable AND keeps the name out of provision_runner's
// reach until an operator clears it — an agent may not re-mint a door it was
// just told to take away. It is deliberately NOT a permanent tombstone:
// re-provisioning the name clears the note, so a retired capability is
// re-enablable by the ordinary provision flow.
type RetiredCapability struct {
	// RevokedAt is when the retirement happened (unix seconds).
	RevokedAt uint64 `json:"revoked_at"`
	// RetiredBy is the provenance of the take-down: "agent" (the CPA's
	// revoke_runner) or "operator" (the console).
	RetiredBy string `json:"retired_by,omitempty"`
	// LastRoster is the roster the door carried at retirement — the record of
	// which agents the capability was taken away from.
	LastRoster []string `json:"last_roster,omitempty"`
}

// SelfHosted reports whether the runner process lives on the target itself
// (the CP holds no identity, ships no package, starts no unit for it).
func (r CapabilityRecord) SelfHosted() bool {
	return r.Hosted == HostedSelf
}

// ControlPlaneState mirrors the Rust ControlPlaneState serde repr.
type ControlPlaneState struct {
	Runners map[string]RunnerRecord `json:"runners"`
	Secrets map[string]SecretRecord `json:"secrets"`
	DNS     map[string]DnsRecord    `json:"dns"`
	// Services is the world-services health registry (k3s/litellm/caddy coords),
	// recorded at build and served on /api/world for management boxes.
	Services         map[string]WorldService `json:"services,omitempty"`
	// Capabilities is the dynamic capability-runner table (the provision_runner
	// flow's records; the static half lives in cpbuild.capabilityRunners).
	Capabilities     map[string]CapabilityRecord `json:"capabilities,omitempty"`
	// RetiredCapabilities holds the guard notes for doors the CPA's
	// revoke_runner retired (see RetiredCapability). The name stays refused by
	// provision_runner until an operator clears the note — by re-provisioning
	// the name, or by the console's "re-enable" action.
	RetiredCapabilities map[string]RetiredCapability `json:"retired_capabilities,omitempty"`
	// AgentGrants is the agent-grant mode: "confirm" (default — the CPA grants
	// when the operator's ask is in its own thread, else DMs for a yes), "auto"
	// (grants land unconfirmed), "off" (server-denies agent provisioning).
	AgentGrants      *string                 `json:"agent_grants,omitempty"`
	ResolverDomain   *string                 `json:"resolver_domain,omitempty"`
	ResolverWildcard *DnsWildcard            `json:"resolver_wildcard,omitempty"`
	Agents           map[string]AgentRecord  `json:"agents"`
	RelayHost        *string                 `json:"relay_host,omitempty"`
	Admins           []string                `json:"admins"`
	RelayURL         *string                 `json:"relay_url,omitempty"`
	RelayPubkey      *string                 `json:"relay_pubkey,omitempty"`
	AgentToolsURL    *string                 `json:"agent_tools_url,omitempty"`
	AgentToolsPubkey *string                 `json:"agent_tools_pubkey,omitempty"`
}

// StateStore wraps the in-memory control-plane state with atomic-0600 save.
type StateStore struct {
	dir   string
	state ControlPlaneState
}

// Open opens (creating if needed) the CP state under dir (0700).
func Open(dir string) (*StateStore, error) {
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, StateFile)
	cp := defaultState()
	if _, err := os.Stat(path); err == nil {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read state: %w", err)
		}
		if err := json.Unmarshal(raw, &cp); err != nil {
			return nil, fmt.Errorf("malformed state json: %w", err)
		}
		ensureMaps(&cp)
	} else {
		// New store: persist immediately so a later save is not the first.
		_ = cp
	}
	return &StateStore{dir: dir, state: cp}, nil
}

func defaultState() ControlPlaneState {
	return ControlPlaneState{
		Runners:  map[string]RunnerRecord{},
		Secrets:  map[string]SecretRecord{},
		DNS:      map[string]DnsRecord{},
		Services: map[string]WorldService{},
		Agents:   map[string]AgentRecord{},
		Admins:   []string{},
	}
}

func ensureMaps(cp *ControlPlaneState) {
	if cp.Runners == nil {
		cp.Runners = map[string]RunnerRecord{}
	}
	if cp.Secrets == nil {
		cp.Secrets = map[string]SecretRecord{}
	}
	if cp.DNS == nil {
		cp.DNS = map[string]DnsRecord{}
	}
	if cp.Services == nil {
		cp.Services = map[string]WorldService{}
	}
	if cp.Capabilities == nil {
		cp.Capabilities = map[string]CapabilityRecord{}
	}
	if cp.RetiredCapabilities == nil {
		cp.RetiredCapabilities = map[string]RetiredCapability{}
	}
	if cp.Agents == nil {
		cp.Agents = map[string]AgentRecord{}
	}
	if cp.Admins == nil {
		cp.Admins = []string{}
	}
}

// Save persists atomically (0600).
func (s *StateStore) Save() error {
	data, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	return write0600Atomic(filepath.Join(s.dir, StateFile), data)
}

// Snapshot returns the current state (deep-ish copy).
func (s *StateStore) Snapshot() ControlPlaneState { return s.state }

// Reload re-reads state.json from disk into the store. The build's world
// DNS/services/facts land via SEPARATE `freehold-console <dns|services>`
// processes writing state.json (not the running serve's in-memory state), so a
// long-lived console serve would otherwise serve a STALE snapshot (missing
// services/facts) on /api/world. Call before serving a snapshot.
func (s *StateStore) Reload() error {
	raw, err := os.ReadFile(filepath.Join(s.dir, StateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var st ControlPlaneState
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("malformed state json: %w", err)
	}
	s.state = st
	return nil
}

// GetRunner returns a runner record.
func (s *StateStore) GetRunner(name string) (RunnerRecord, bool) {
	r, ok := s.state.Runners[name]
	return r, ok
}

// GetSecret returns a secret record.
func (s *StateStore) GetSecret(name string) (SecretRecord, bool) {
	r, ok := s.state.Secrets[name]
	return r, ok
}

// InsertRunner records a runner.
func (s *StateStore) InsertRunner(name string, rec RunnerRecord) {
	s.state.Runners[name] = rec
}

// InsertSecret records a secret.
func (s *StateStore) InsertSecret(name string, rec SecretRecord) {
	s.state.Secrets[name] = rec
}

// Admins returns the admin whitelist.
func (s *StateStore) Admins() []string { return s.state.Admins }

// SetAdmins sets the admin whitelist.
func (s *StateStore) SetAdmins(admins []string) { s.state.Admins = admins }

// SetRunnerMcpAddr sets a runner's MCP address.
func (s *StateStore) SetRunnerMcpAddr(name string, addr *string) error {
	r, ok := s.state.Runners[name]
	if !ok {
		return fmt.Errorf("runner %s not found", name)
	}
	r.McpAddr = addr
	s.state.Runners[name] = r
	return nil
}

// RemoveAgent drops an agent's registry row.
func (s *StateStore) RemoveAgent(name string) { delete(s.state.Agents, name) }

// InsertAgent records an agent.
func (s *StateStore) InsertAgent(name string, rec AgentRecord) { s.state.Agents[name] = rec }

// RelayHost returns the relay community host.
func (s *StateStore) RelayHost() *string { return s.state.RelayHost }

// SetRelayHost sets the relay community host + saves.
func (s *StateStore) SetRelayHost(h *string) error {
	s.state.RelayHost = h
	return s.Save()
}

// SetRelayURL sets the relay URL + saves.
func (s *StateStore) SetRelayURL(u *string) error {
	s.state.RelayURL = u
	return s.Save()
}

// SetRelayPubkey sets the relay pubkey + saves.
func (s *StateStore) SetRelayPubkey(p *string) error {
	s.state.RelayPubkey = p
	return s.Save()
}

// GetDNS returns a DNS record.
func (s *StateStore) GetDNS(name string) (DnsRecord, bool) {
	r, ok := s.state.DNS[name]
	return r, ok
}

// InsertDNS records a DNS record.
func (s *StateStore) InsertDNS(name string, rec DnsRecord) { s.state.DNS[name] = rec }

// RemoveDNS deletes a DNS record.
func (s *StateStore) RemoveDNS(name string) { delete(s.state.DNS, name) }

// GetService returns a world-services record.
func (s *StateStore) GetService(name string) (WorldService, bool) {
	r, ok := s.state.Services[name]
	return r, ok
}

// InsertService records a world-services record.
func (s *StateStore) InsertService(name string, rec WorldService) { s.state.Services[name] = rec }

// RemoveService deletes a world-services record.
func (s *StateStore) RemoveService(name string) { delete(s.state.Services, name) }

// GetCapability returns a dynamic capability-runner record.
func (s *StateStore) GetCapability(name string) (CapabilityRecord, bool) {
	r, ok := s.state.Capabilities[name]
	return r, ok
}

// Capabilities returns the dynamic capability-runner table (sorted by name).
func (s *StateStore) Capabilities() map[string]CapabilityRecord {
	out := make(map[string]CapabilityRecord, len(s.state.Capabilities))
	for k, v := range s.state.Capabilities {
		out[k] = v
	}
	return out
}

// InsertCapability records a dynamic capability-runner spec + saves.
func (s *StateStore) InsertCapability(name string, rec CapabilityRecord) error {
	if s.state.Capabilities == nil {
		s.state.Capabilities = map[string]CapabilityRecord{}
	}
	s.state.Capabilities[name] = rec
	// Re-enabling the name (a re-provision of a retired door, by the operator's
	// console path) drops its retirement guard — see RetiredCapability.
	delete(s.state.RetiredCapabilities, name)
	return s.Save()
}

// RemoveCapability drops a dynamic capability-runner spec + saves.
func (s *StateStore) RemoveCapability(name string) error {
	delete(s.state.Capabilities, name)
	return s.Save()
}

// GetRetired returns a retired capability's guard note.
func (s *StateStore) GetRetired(name string) (RetiredCapability, bool) {
	r, ok := s.state.RetiredCapabilities[name]
	return r, ok
}

// InsertRetired records a retirement guard note + saves: from here the name is
// refused to the agent surface in BOTH directions (revoke_runner and
// provision_runner), until an operator's record write clears it.
func (s *StateStore) InsertRetired(name string, rec RetiredCapability) error {
	if s.state.RetiredCapabilities == nil {
		s.state.RetiredCapabilities = map[string]RetiredCapability{}
	}
	s.state.RetiredCapabilities[name] = rec
	return s.Save()
}

// ConfirmEnrollment stamps the operator's pubkey confirmation on a
// self-hosted capability record (the door page's confirm — the barrier that
// binds the credential fill to the guest's own `runner enroll` output).
func (s *StateStore) ConfirmEnrollment(name string, at uint64) error {
	rec, ok := s.state.Capabilities[name]
	if !ok {
		return fmt.Errorf("capability %s not found", name)
	}
	if !rec.SelfHosted() {
		return fmt.Errorf("capability %s is not self-hosted — nothing to confirm", name)
	}
	rec.EnrollConfirmedAt = &at
	s.state.Capabilities[name] = rec
	return s.Save()
}

// AgentGrantsMode returns the agent-grant mode ("confirm" when unset).
func (s *StateStore) AgentGrantsMode() string {
	if s.state.AgentGrants == nil || *s.state.AgentGrants == "" {
		return "confirm"
	}
	return *s.state.AgentGrants
}

// SetAgentGrantsMode sets the agent-grant mode + saves.
func (s *StateStore) SetAgentGrantsMode(mode string) error {
	s.state.AgentGrants = &mode
	return s.Save()
}

// ResolverDomain returns the resolver's world-domain suffix.
func (s *StateStore) ResolverDomain() *string { return s.state.ResolverDomain }

// SetResolverDomain sets the resolver domain + saves.
func (s *StateStore) SetResolverDomain(d *string) error {
	s.state.ResolverDomain = d
	return s.Save()
}

// ResolverWildcard returns the resolver wildcard apex.
func (s *StateStore) ResolverWildcard() *DnsWildcard { return s.state.ResolverWildcard }

// SetResolverWildcard sets the resolver wildcard + saves.
func (s *StateStore) SetResolverWildcard(w *DnsWildcard) error {
	s.state.ResolverWildcard = w
	return s.Save()
}

// AgentToolsURL returns the recorded agent-tools URL.
func (s *StateStore) AgentToolsURL() *string { return s.state.AgentToolsURL }

// SetAgentToolsURL sets the agent-tools URL + saves.
func (s *StateStore) SetAgentToolsURL(u *string) error {
	s.state.AgentToolsURL = u
	return s.Save()
}

// AgentToolsPubkey returns the recorded agent-tools pubkey.
func (s *StateStore) AgentToolsPubkey() *string { return s.state.AgentToolsPubkey }

// SetAgentToolsPubkey sets the agent-tools pubkey + saves.
func (s *StateStore) SetAgentToolsPubkey(p *string) error {
	s.state.AgentToolsPubkey = p
	return s.Save()
}

// Dir returns the state dir.
func (s *StateStore) Dir() string { return s.dir }

// SetRunnerStatus flips a runner's status.
func (s *StateStore) SetRunnerStatus(name string, status RunnerStatus) error {
	r, ok := s.state.Runners[name]
	if !ok {
		return fmt.Errorf("runner %s not found", name)
	}
	r.Status = status
	s.state.Runners[name] = r
	return nil
}

// RemoveRunner deletes a runner record.
func (s *StateStore) RemoveRunner(name string) { delete(s.state.Runners, name) }

// RemoveSecret deletes a secret record.
func (s *StateStore) RemoveSecret(name string) { delete(s.state.Secrets, name) }

// SetSecretCiphertext restores a secret's ciphertext + rotation stamp.
func (s *StateStore) SetSecretCiphertext(name, ciphertextHex string, rotatedAt *uint64) error {
	r, ok := s.state.Secrets[name]
	if !ok {
		return fmt.Errorf("secret %s not found", name)
	}
	r.CiphertextHex = ciphertextHex
	r.RotatedAt = rotatedAt
	s.state.Secrets[name] = r
	return nil
}

// RebuildFrom replaces the runner/secret views wholesale (rebuild fold).
func (s *StateStore) RebuildFrom(runners map[string]RunnerRecord, secrets map[string]SecretRecord) error {
	s.state.Runners = runners
	s.state.Secrets = secrets
	ensureMaps(&s.state)
	return s.Save()
}

// UpdateSecretCiphertext updates a secret's ciphertext + rotation stamp.
func (s *StateStore) UpdateSecretCiphertext(name, ciphertextHex string, rotatedAt uint64) error {
	r, ok := s.state.Secrets[name]
	if !ok {
		return fmt.Errorf("secret %s not found", name)
	}
	r.CiphertextHex = ciphertextHex
	r.RotatedAt = &rotatedAt
	s.state.Secrets[name] = r
	return nil
}

// RoverKeys returns the sorted runner names (BTreeMap ordering), for tests.
func (s *StateStore) Runners() []string {
	keys := make([]string, 0, len(s.state.Runners))
	for k := range s.state.Runners {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func nowSecs() uint64 {
	return uint64(time.Now().Unix())
}
