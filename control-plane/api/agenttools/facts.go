package agenttools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"freehold/contract/worldfacts"
)

// The world-inventory wire format lives in the shared leaf so the local CLI can
// read it without importing this server package; aliased here for the server.
type (
	WorldFacts      = worldfacts.WorldFacts
	WorldDomains    = worldfacts.WorldDomains
	WorldPlane      = worldfacts.WorldPlane
	WorldPlaneMount = worldfacts.WorldPlaneMount
	WorldCert       = worldfacts.WorldCert
)

// FactsStore is the agent-tools server's durable world-facts store (facts.json
// under the state dir — survives compute-only teardown, like registry.json).
type FactsStore struct {
	mu    sync.Mutex
	path  string
	facts WorldFacts
}

// OpenFacts loads (creating if needed) the facts store at path.
func OpenFacts(path string) (*FactsStore, error) {
	f := &FactsStore{path: path}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &f.facts); err != nil {
			return nil, fmt.Errorf("malformed facts %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return f, nil
}

func (f *FactsStore) save() error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f.facts, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

// Register replaces the recorded world facts (the build's idempotent
// register-at-build — same input → same state).
func (f *FactsStore) Register(facts WorldFacts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.facts = facts
	return f.save()
}

// Facts returns the recorded world facts (an empty value when unregistered).
func (f *FactsStore) Facts() WorldFacts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.facts
}
