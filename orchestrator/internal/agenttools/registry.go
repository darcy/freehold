package agenttools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"freehold/orchestrator/internal/console"
)// Registry is the freehold-agent-tools server's durable agent registry: the CP
// holds it under the server's own state dir, so a compute-only teardown keeps
// every created agent's identity + row and a rebuild reconciles it. It satisfies
// agent.ConsoleOps with the local registry file — direct and in-process, never a
// console HTTP hop and never a cross-process write to the console's state.json
// (which the running console process owns in memory).
type Registry struct {
	mu    sync.Mutex
	path  string
	rows  map[string]console.AgentInfo
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
	r.rows[name] = console.AgentInfo{Name: name, Pubkey: pubkey, CreatedAt: uint64(time.Now().Unix())}
	if err := r.save(); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
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
// channel owner (the console), not a registry-file op. Honest "not wired" until
// the server holds that owner credential; the operator grants via the console.
func (r *Registry) Grant(runner, pubkey string) (json.RawMessage, error) {
	return nil, fmt.Errorf("grant_agent %s→%s: the runner's whitelist is its relay roster, owned by the console — not wired from the CP toolset yet (operator grants via the console)", pubkey, runner)
}
