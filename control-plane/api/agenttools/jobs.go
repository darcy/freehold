package agenttools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// JobsFile is the durable scheduled-jobs store, kept in the agent-tools state
// dir beside registry.json. This process owns it in memory + on disk; the
// console folds it READ-ONLY for /api/jobs (LoadJobsReadOnly) — never a
// cross-process write, the same discipline the registry follows.
const JobsFile = "jobs.json"

// Job run statuses. "fired" is OPEN (the mention went out, no reply seen yet);
// everything else is terminal. "skipped" = the agent still had an open run;
// "missed" = the slot fell outside the catch-up window (recorded, never
// silently dropped); "error" = the fire itself failed.
const (
	RunFired   = "fired"
	RunOK      = "ok"
	RunTimeout = "timeout"
	RunSkipped = "skipped"
	RunMissed  = "missed"
	RunError   = "error"
)

// MaxJobRuns caps the per-job run ledger (newest kept, oldest dropped).
const MaxJobRuns = 20

// JobRun is one fire of a job.
type JobRun struct {
	FiredAt uint64 `json:"fired_at"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// Job is one scheduled job. The prompt is the owner's content: it rides this
// store (agent-tools' own 0600 durable dir) and is REDACTED at the console
// API for every non-owner viewer — the operator included.
type Job struct {
	ID string `json:"id"`
	// Owner is the job's human npub (64-hex) — the person who asked for it.
	// Empty = the agent's own job (a self-scheduled repo re-check): no human
	// gets failure notes, and the agent itself reads it via list_jobs.
	Owner string `json:"owner"`
	// AgentPubkey routes the fire — the agent's Nostr pubkey, stable across
	// renames (a rename keeps the pubkey and changes the name). Resolved from
	// the registry at create time; the row's Agent display name refreshes
	// from the registry at each fire. Legacy rows with an empty pubkey fall
	// back to resolving Agent by name.
	AgentPubkey string `json:"agent_pubkey,omitempty"`
	// Agent is the agent's display NAME (registry row), refreshed per fire.
	Agent string `json:"agent"`
	// Channel is the relay channel the fire lands in (the h-tag id). The
	// agent's in-channel reply is the delivery.
	Channel string `json:"channel"`
	// Cron is a standard 5-field schedule (or @every/@hourly descriptor)
	// evaluated in TZ. Empty with At != 0 = a one-shot reminder.
	Cron string `json:"cron,omitempty"`
	// At is a one-shot unix timestamp (recurring jobs leave it 0).
	At uint64 `json:"at,omitempty"`
	// TZ is the IANA zone the schedule runs in (empty = UTC).
	TZ string `json:"tz,omitempty"`
	// Label is the short human label (shown in fire/failure messages + UI).
	// Owner-adjacent content — redacted with the prompt.
	Label string `json:"label,omitempty"`
	// Prompt is what fires as the mention.
	Prompt string `json:"prompt"`
	// CreatedAt / NextRunAt are unix seconds. NextRunAt is advanced BEFORE
	// dispatch and persisted first (at-most-once across a mid-run crash).
	CreatedAt uint64 `json:"created_at"`
	NextRunAt uint64 `json:"next_run_at"`
	Paused    bool   `json:"paused,omitempty"`
	// Runs is the capped run ledger, oldest first.
	Runs []JobRun `json:"runs,omitempty"`
}

// LastRun returns the newest run (nil when the job never ran).
func (j *Job) LastRun() *JobRun {
	if len(j.Runs) == 0 {
		return nil
	}
	last := j.Runs[len(j.Runs)-1]
	return &last
}

// routeKey is the in-flight gate's key: the routing pubkey when known, else
// the stored name (legacy rows).
func (j *Job) routeKey() string {
	if j.AgentPubkey != "" {
		return j.AgentPubkey
	}
	return j.Agent
}

// OpenRun returns the run still awaiting its agent's reply (nil = none).
func (j *Job) OpenRun() *JobRun {
	if last := j.LastRun(); last != nil && last.Status == RunFired {
		return last
	}
	return nil
}

// JobsStore is the in-process owner of jobs.json (0600, atomic save).
type JobsStore struct {
	mu   sync.Mutex
	path string
	rows map[string]*Job
}

// OpenJobs loads (creating if needed) the jobs store at path.
func OpenJobs(path string) (*JobsStore, error) {
	s := &JobsStore{path: path, rows: map[string]*Job{}}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &s.rows); err != nil {
			return nil, fmt.Errorf("malformed jobs store %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *JobsStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.rows, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Create validates + records a new job, returning it with its NextRunAt set.
// Cron is parsed by the scheduler package's ParseSchedule (ParseStandard) so
// both sides agree on validity; a one-shot passes at > 0 and cron "".
func (s *JobsStore) Create(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j.ID == "" {
		return fmt.Errorf("job id required")
	}
	if _, taken := s.rows[j.ID]; taken {
		return fmt.Errorf("job id %s already exists", j.ID)
	}
	if j.Agent == "" {
		return fmt.Errorf("job needs an agent to run on")
	}
	if j.Channel == "" {
		return fmt.Errorf("job needs a channel to fire into")
	}
	if j.Prompt == "" {
		return fmt.Errorf("job needs a prompt")
	}
	if j.At == 0 && j.Cron == "" {
		return fmt.Errorf("job needs a cron schedule or a one-shot at")
	}
	if j.CreatedAt == 0 {
		j.CreatedAt = uint64(time.Now().Unix())
	}
	s.rows[j.ID] = j
	return s.saveLocked()
}

// Delete drops a job.
func (s *JobsStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[id]; !ok {
		return fmt.Errorf("no such job: %s", id)
	}
	delete(s.rows, id)
	return s.saveLocked()
}

// SetPaused flips a job's paused flag.
func (s *JobsStore) SetPaused(id string, paused bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.rows[id]
	if !ok {
		return fmt.Errorf("no such job: %s", id)
	}
	j.Paused = paused
	return s.saveLocked()
}

// Update mutates one job under the lock and saves. fn runs with the store
// locked; it receives the row (already verified present) and reports whether
// anything changed (no change = no save).
func (s *JobsStore) Update(id string, fn func(j *Job) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.rows[id]
	if !ok {
		return fmt.Errorf("no such job: %s", id)
	}
	if !fn(j) {
		return nil
	}
	return s.saveLocked()
}

// Snapshot returns sorted copies (by id).
func (s *JobsStore) Snapshot() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.rows))
	for id := range s.rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Job, 0, len(ids))
	for _, id := range ids {
		out = append(out, *s.rows[id])
	}
	return out
}

// Get returns a copy of one job.
func (s *JobsStore) Get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.rows[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// recordRun appends a run (capped) and saves. Used by the scheduler.
func (s *JobsStore) recordRun(id string, run JobRun) error {
	return s.Update(id, func(j *Job) bool {
		j.Runs = append(j.Runs, run)
		if len(j.Runs) > MaxJobRuns {
			j.Runs = j.Runs[len(j.Runs)-MaxJobRuns:]
		}
		return true
	})
}

// LoadJobsReadOnly reads the jobs file WITHOUT the owning process's lock —
// the console's read-only fold for /api/jobs. A missing file is an empty list
// (a world predating jobs), not an error.
func LoadJobsReadOnly(path string) ([]Job, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rows map[string]*Job
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("malformed jobs store %s: %w", path, err)
	}
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Job, 0, len(ids))
	for _, id := range ids {
		out = append(out, *rows[id])
	}
	return out, nil
}
