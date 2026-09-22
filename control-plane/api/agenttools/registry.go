package agenttools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"freehold/contract/console"
	"freehold/contract/relay"
	"freehold/control-plane/api/cpstate"
) // Registry is the freehold-agent-tools server's durable agent registry: the CP
// holds it under the server's own state dir, so a compute-only teardown keeps
// every created agent's identity + row and a rebuild reconciles it. It satisfies
// agent.ConsoleOps with the local registry file — direct and in-process, never a
// console HTTP hop and never a cross-process write to the console's state.json
// (which the running console process owns in memory).
type Registry struct {
	mu   sync.Mutex
	path string
	rows map[string]console.AgentInfo

	// Grant wiring (the console-owner credential): RelayURL is the relay to
	// publish roster writes to, ConsoleSecret is the console's OWN nostr secret
	// (its channel-owner key, loaded from the console state dir — 0600 durable,
	// in-process only), and ConsoleStateDir resolves runner names → their
	// nostr pubkeys. Empty RelayURL/ConsoleSecret = grants stay unwired.
	RelayURL        string
	ConsoleSecret   []byte
	ConsoleStateDir string
}

// OpenRegistry loads (creating if needed) the registry at path.
func OpenRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, rows: map[string]console.AgentInfo{}}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &r.rows); err != nil {
			return nil, fmt.Errorf("malformed registry %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return r, nil
}

func (r *Registry) save() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r.rows, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// RegisterAgent upserts a registry row.
func (r *Registry) RegisterAgent(name, pubkey, channel string) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[name] = console.AgentInfo{Name: name, Pubkey: pubkey, CreatedAt: uint64(time.Now().Unix()), Channel: channel}
	if err := r.save(); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

// SetPurpose records an agent's one-line purpose on its registry row (the tool
// knows it at create time; threading it separately keeps ConsoleOps mirroring
// the console client, which has no purpose field). Saved with the row so a
// rebuild reconciler can recreate the agent's system prompt verbatim.
func (r *Registry) SetPurpose(name, purpose string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[name]
	if !ok {
		return fmt.Errorf("register %s before setting its purpose", name)
	}
	row.Purpose = purpose
	r.rows[name] = row
	return r.save()
}

// SetChannels records the full channel list + private flag on an agent's
// registry row, so a rebuild rejoins every channel (not just the primary) and
// recreates the agent's own channel private. Returns an error when the row is
// absent (register before setting).
func (r *Registry) SetChannels(name string, channels []string, private bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[name]
	if !ok {
		return fmt.Errorf("register %s before setting its channels", name)
	}
	row.Channels = append([]string(nil), channels...)
	row.Private = private
	r.rows[name] = row
	return r.save()
}

// WithRegistryLocked runs fn while holding the registry's write lock, then
// re-reads the file into memory. It exists for the migration queue, which edits
// registry.json OUT-OF-BAND: the script shells out to a separate
// freehold-agent-tools process that opens its own handle and knows nothing about
// this serve's in-memory rows. Two hazards therefore have to be closed as ONE
// critical section: no concurrent roster write may land while the script holds
// the file (its save() would write this process's stale rows over what the script
// just wrote), and memory must be re-read afterwards, or the next save() clobbers
// the edit from the other direction. Doing only the second one leaves the first
// open, so the two are not separable by a caller.
//
// fn must not touch this Registry — it drives the out-of-band writer only, and
// re-entering a method would self-deadlock. fn's error is returned; the re-read
// happens either way, because a half-run queue still moved the durable file and
// memory has to match it.
func (r *Registry) WithRegistryLocked(fn func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := fn()
	if rerr := r.reloadLocked(); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// reloadLocked re-reads the registry file into memory. The caller holds mu. A
// file that has gone missing leaves the current rows in place rather than
// dropping them; a malformed file is an error for the same reason.
func (r *Registry) reloadLocked() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	rows := map[string]console.AgentInfo{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return fmt.Errorf("malformed registry %s: %w", r.path, err)
	}
	r.rows = rows
	return nil
}

// UnregisterAgent drops a registry row.
func (r *Registry) UnregisterAgent(name string) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rows, name)
	if err := r.save(); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

// Agents lists registry rows sorted by name.
func (r *Registry) Agents() ([]console.AgentInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]console.AgentInfo, 0, len(r.rows))
	for _, a := range r.rows {
		out = append(out, a)
	}
	if len(out) > 1 {
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	}
	return out, nil
}

// Grant binds agent pubkeys to a runner's whitelist — in the locked model that
// is a RELAY roster change on the runner's private channel, signed by the
// channel owner (the console). The unified api/ absorbed the console's owner
// credential, so this now publishes kind-9000 put-user to the runner's channel
// with the console's own identity: the runner re-reads its signed 39002 roster
// per call, so the grant lands without a restart. Fail-closed: no relay/console
// wiring means the grant refuses loudly, never silently succeeding.
func (r *Registry) Grant(runner, pubkey string) (json.RawMessage, error) {
	if r.RelayURL == "" || len(r.ConsoleSecret) == 0 {
		return nil, fmt.Errorf("grant_agent %s→%s: server holds no relay/console-owner wiring (deploy the agent-tools stage with --console-state-dir and a relay)", pubkey, runner)
	}
	runnerPK, err := cpstate.RunnerNostrPubkey(r.ConsoleStateDir, runner)
	if err != nil {
		return nil, fmt.Errorf("grant_agent %s→%s: %w", pubkey, runner, err)
	}
	if err := relay.PutUser(r.RelayURL, r.ConsoleSecret, runnerPK, pubkey); err != nil {
		return nil, fmt.Errorf("grant_agent %s→%s: publish roster change: %w", pubkey, runner, err)
	}
	return json.RawMessage(`{}`), nil
}
