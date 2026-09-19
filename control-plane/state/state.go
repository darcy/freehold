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

// ControlPlaneState mirrors the Rust ControlPlaneState serde repr.
type ControlPlaneState struct {
	Runners map[string]RunnerRecord `json:"runners"`
	Secrets map[string]SecretRecord `json:"secrets"`
	DNS     map[string]DnsRecord    `json:"dns"`
	// Services is the world-services health registry (k3s/litellm/caddy coords),
	// recorded at build and served on /api/world for management boxes.
	Services         map[string]WorldService `json:"services,omitempty"`
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
