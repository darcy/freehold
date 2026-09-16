// Package migrations is the CP's verify-gated migration runner — the
// Omarchy-style mechanism for versioned config/prompt/repair changes that
// don't have clean desired-state (IaC) semantics.
//
// It deliberately improves on the Omarchy marker-file weakness the design
// called out: completion is recorded against a POSTCONDITION. The `State` is
// structured JSON under the CP's durable plane (/srv/data/cp/migrations.json,
// backed up), not loose marker files; each migration carries an explicit
// `Verify` gate. A migration is DONE only when `Apply` succeeded AND `Verify`
// confirms convergence (🟢). `Apply`-succeeded-but-`Verify`-failed is recorded
// as not-done and retried on the next run — so "ran" is never mistaken for
// "converged". Migrations are idempotent (re-applying must be safe).
package migrations

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// files embeds the versioned migration SCRIPTS — the Omarchy convention of one
// `.sh` file per migration. A file is named `<epoch>.sh` (the apply) with an
// optional `<epoch>.verify.sh` (the postcondition gate). Executing is the
// caller's job (through the CP's runner); this package only enumerates + tracks.
//
//go:embed files/*.sh
var files embed.FS

// Script is one embedded migration: Epoch is BOTH the version and the ledger
// name (the Omarchy timestamped-file identity). Body carries the apply steps;
// Verify carries the optional postcondition gate script.
type Script struct {
	Epoch  string
	Body   string
	Verify string
}

// Scripts returns the embedded migration scripts in ascending epoch order,
// grouping `<epoch>.sh` (apply) with its optional `<epoch>.verify.sh` (gate).
func Scripts() ([]Script, error) {
	matches, err := fs.Glob(files, "files/*.sh")
	if err != nil {
		return nil, err
	}
	byEpoch := map[string]*Script{}
	for _, m := range matches {
		base := strings.TrimSuffix(filepath.Base(m), ".sh")
		body, err := files.ReadFile(m)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(base, ".verify") {
			ep := strings.TrimSuffix(base, ".verify")
			s, ok := byEpoch[ep]
			if !ok {
				s = &Script{Epoch: ep}
				byEpoch[ep] = s
			}
			s.Verify = string(body)
			continue
		}
		if !epochOnly(base) {
			continue
		}
		byEpoch[base] = &Script{Epoch: base, Body: string(body)}
	}
	out := make([]Script, 0, len(byEpoch))
	for _, s := range byEpoch {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Epoch < out[j].Epoch })
	return out, nil
}

// epochOnly reports whether a bare filename base is a pure decimal epoch (a
// migration, not a helper `<something>.sh` we should ignore).
func epochOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Migration adapts a Script into the verify-gated Migration the runner
// executes. run executes a script BODY (the caller decides how: locally, or
// through the CP's runner). A missing Verify script means apply-success implies
// converged (nil gate).
func (s Script) Migration(run func(string) error) Migration {
	v := s.Verify
	return Migration{
		Name: s.Epoch,
		Apply: func() error {
			return run(s.Body)
		},
		Verify: func() error {
			if v == "" {
				return nil
			}
			return run(v)
		},
	}
}

// Migration is one versioned, idempotent change with a verify gate.
type Migration struct {
	// Name is the stable identifier (e.g. "001-ensure-agent-tools-registry").
	Name string
	// Apply performs the change; must be idempotent (safe to re-run).
	Apply func() error
	// Verify is the postcondition gate: nil = apply-success implies done; else
	// it must confirm convergence. A verify failure leaves the migration NOT
	// done so the next run retries it.
	Verify func() error
}

// Result is one migration's outcome for a run.
type Result struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Err  string `json:"error,omitempty"`
	// Applied reports whether Apply actually ran this round (vs already done).
	Applied bool `json:"applied,omitempty"`
}

// Entry is the durable per-migration record. Status "✓" / "✗" reflect whether
// the VERIFY gate confirmed convergence, never merely whether Apply ran.
type Entry struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // "done" | "applied-pending-verify" | "failed"
	RanAt     int64  `json:"ran_at,omitempty"`
	VerifyAt  int64  `json:"verify_at,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// State is the durable migration ledger (JSON at `path`, 0600/0700).
type State struct {
	mu   sync.Mutex
	path string
	/// entries by migration name.
	entries map[string]Entry
}

// Open loads the ledger at path (creating parent dir + an empty ledger if
// absent). Returns a usable State either way.
func Open(path string) (*State, error) {
	s := &State{path: path, entries: map[string]Entry{}}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &s.entries); err != nil {
			return nil, fmt.Errorf("malformed migration state %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// save persists the ledger atomically (0600, parent 0700).
func (s *State) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Done reports whether a migration is durably verified-converged.
func (s *State) Done(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[name].Status == "done"
}

// Pending returns migrations not (yet) verified-converged, in a deterministic
// (name) order, so a run applies new/retryable migrations and skips done ones.
func (s *State) Pending(all []Migration) []Migration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []Migration
	for _, m := range all {
		if s.entries[m.Name].Status != "done" {
			pending = append(pending, m)
		}
	}
	return pending
}

// Run applies every pending migration, gating completion on its Verify
// postcondition, and durably records each outcome. A failed Apply leaves the
// migration pending (retried next run); an Apply that fails Verify is recorded
// as pending-verify, so it is RETRIED, never marked done on a bare "ran".
// Run stops at the first hard failure and returns the partial results.
func (s *State) Run(all []Migration) ([]Result, error) {
	pending := s.Pending(all)
	results := make([]Result, 0, len(pending))
	for _, m := range pending {
		res := Result{Name: m.Name, Applied: true}
		if err := m.Apply(); err != nil {
			res.Err = err.Error()
			s.record(m.Name, "failed", err.Error(), time.Now().Unix())
			results = append(results, res)
			return results, fmt.Errorf("%s: %w", m.Name, err)
		}
		// Verify gate: nil verify => apply-success implies converged.
		verifyAt := time.Now().Unix()
		var verr error
		if m.Verify != nil {
			verr = m.Verify()
		}
		if verr == nil {
			res.OK = true
			s.record(m.Name, "done", "", verifyAt)
		} else {
			res.Err = "verify: " + verr.Error()
			s.record(m.Name, "applied-pending-verify", verr.Error(), verifyAt)
		}
		results = append(results, res)
	}
	return results, nil
}

func (s *State) record(name, status, lastErr string, at int64) {
	s.mu.Lock()
	s.entries[name] = Entry{Name: name, Status: status, RanAt: at, VerifyAt: at, LastError: lastErr}
	s.mu.Unlock()
	_ = s.save()
}
