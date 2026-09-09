// Package cpstate reads the Rust console's durable state.json + console
// identity from the CP state dir. The Go api/ server fronts the still-Rust
// console this phase, so its single-inventory reads (runners, DNS, agents) and
// its grant publishing (the console's channel-owner credential) come from the
// console's own on-disk store — the "console underneath". Read-only: the api
// server never writes the console's state (the running console process owns it
// in memory); it only reads it fresh per call.
package cpstate

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ConsoleDir is the subdir of the CP state dir holding the console's own
// identity (identity.json) — the channel-owner credential that signs runner
// roster writes.
const ConsoleDir = "console"

// Runner is one runner row from the console's state.json (the /api/overview
// source). The full RunnerRecord carries enc_pubkey/status/package_dir; the
// api server needs the name→nostr mapping for grants and the mcp_addr for
// readiness.
type Runner struct {
	Name       string `json:"name"`
	NostrPubkey string `json:"nostr_pubkey"`
	EncPubkey  string `json:"enc_pubkey"`
	Status     string `json:"status"`
	PackageDir string `json:"package_dir"`
	McpAddr    string `json:"mcp_addr,omitempty"`
	RiskLevel  string `json:"risk_level,omitempty"`
}

// Agent is one row of the console's own agents map (the registry.json one is
// authoritative; this is the pre-reconcile second store the migration folds).
type Agent struct {
	Name      string  `json:"name"`
	Pubkey    string  `json:"pubkey"`
	CreatedAt uint64  `json:"created_at"`
	Channel   *string `json:"channel,omitempty"`
}

// DnsRecord is one explicit DNS record the CP resolver serves.
type DnsRecord struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	Source string `json:"source"`
}

// State is the console's state.json subset the api/ front reads.
type State struct {
	Runners map[string]Runner `json:"runners"`
	DNS     map[string]DnsRecord `json:"dns"`
	Agents  map[string]Agent  `json:"agents"`
}

// Read loads the console's state.json from the CP state dir.
func Read(consoleStateDir string) (*State, error) {
	raw, err := os.ReadFile(filepath.Join(consoleStateDir, "state.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("read console state: %w", err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse console state: %w", err)
	}
	if st.Runners == nil {
		st.Runners = map[string]Runner{}
	}
	if st.DNS == nil {
		st.DNS = map[string]DnsRecord{}
	}
	if st.Agents == nil {
		st.Agents = map[string]Agent{}
	}
	// The console keys runners/dns/agents BY NAME; carry the key into each row
	// (JSON map keys don't populate a Name field on their own).
	for name, r := range st.Runners {
		r.Name = name
		st.Runners[name] = r
	}
	for name, d := range st.DNS {
		d.Name = name
		st.DNS[name] = d
	}
	for name, a := range st.Agents {
		a.Name = name
		st.Agents[name] = a
	}
	return &st, nil
}

// RunnerNostrPubkey resolves a runner name to its Nostr membership pubkey from
// the console's state (the key grants publish against).
func RunnerNostrPubkey(consoleStateDir, name string) (string, error) {
	st, err := Read(consoleStateDir)
	if err != nil {
		return "", err
	}
	r, ok := st.Runners[name]
	if !ok {
		return "", fmt.Errorf("runner %q is not provisioned (console state has %d runners)", name, len(st.Runners))
	}
	if r.NostrPubkey == "" {
		return "", fmt.Errorf("runner %q has no recorded nostr pubkey", name)
	}
	return r.NostrPubkey, nil
}

// ConsoleSecret loads the console's own Nostr secret from
// <stateDir>/console/identity.json — the channel-owner credential that signs
// runner roster writes (grant/revoke). 0600 on the durable plane; used
// in-process only, never returned beyond the bytes that sign a publish.
func ConsoleSecret(consoleStateDir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(consoleStateDir, ConsoleDir, "identity.json"))
	if err != nil {
		return nil, fmt.Errorf("console identity unreadable at %s/console: %w (deploy-cp mints it on the box)", consoleStateDir, err)
	}
	var id struct {
		NostrSecretHex string `json:"nostr_secret_hex"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, fmt.Errorf("parse console identity: %w", err)
	}
	if id.NostrSecretHex == "" {
		return nil, fmt.Errorf("console identity has no nostr secret")
	}
	sec, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil || len(sec) != 32 {
		return nil, fmt.Errorf("console identity secret not 32-byte hex")
	}
	return sec, nil
}