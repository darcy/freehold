package agenttools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// MentionsFile is the durable mention-watch ledger, kept in the agent-tools
// state dir beside jobs.json. This process owns it in memory + on disk (0600,
// atomic save) — the CP is the record of every channel-less mention it saw,
// kicked, and settled.
const MentionsFile = "mentions.json"

// Mention statuses. "discovered" is OPEN (seen, not yet kicked — it re-queues
// every tick until the agent is free); "kicked" is IN FLIGHT (the task exec
// is running); the rest are terminal. "fell_back" = the kick failed and the
// ordinary #freehold mention wake was sent instead.
const (
	MentionDiscovered = "discovered"
	MentionKicked     = "kicked"
	MentionCompleted  = "completed"
	MentionFailed     = "failed"
	MentionTimedOut   = "timed_out"
	MentionFellBack   = "fell_back"
)

// MaxMentionRows caps the ledger (newest by mention timestamp kept, oldest
// dropped — a bounded record, not an unbounded history).
const MaxMentionRows = 500

// MentionRecord is one channel-less mention the watcher handled.
type MentionRecord struct {
	// ID is the mention event's id (the dedup key across restarts).
	ID string `json:"id"`
	// AgentPubkey routes the kick (the task JSON's agent_pubkey) and matches
	// the reply poll's author filter; Agent is the display name, refreshed
	// from the registry.
	AgentPubkey string `json:"agent_pubkey"`
	Agent       string `json:"agent,omitempty"`
	// Author is who wrote the mention (64-hex).
	Author string `json:"author"`
	// CreatedAt is the mention event's timestamp (unix seconds).
	CreatedAt uint64 `json:"created_at"`
	// Excerpt is a bounded prefix of the mention text (the full text rides
	// the kick prompt).
	Excerpt string `json:"excerpt"`
	// Root is the task/issue event id from the mention's root e tag (empty
	// = a bare mention with no task reference).
	Root string `json:"root,omitempty"`
	// Status is the lifecycle state (see the constants above).
	Status string `json:"status"`
	// KickedAt is when the kick went out (unix seconds).
	KickedAt uint64 `json:"kicked_at,omitempty"`
	// Terminal is the status field of buzz-acp's version-1 terminal record
	// (completed/failed/cancelled/...) when one was captured.
	Terminal string `json:"terminal,omitempty"`
	// Replied/ReplyID record the agent's own kind-1 with this task as its
	// root e tag — the actual delivery, detected by the reply poll.
	Replied bool   `json:"replied,omitempty"`
	ReplyID string `json:"reply_id,omitempty"`
	// Detail carries the failure/fallback diagnostic.
	Detail string `json:"detail,omitempty"`
}

// MentionState is the persisted watcher state: the poll cursor plus the
// record ledger (the records ARE the dedup — an id with a row is handled).
type MentionState struct {
	// Cursor is the last seen mention timestamp (unix seconds); the poll
	// reads from Cursor - skew so a clock edge cannot skip an event.
	Cursor uint64                    `json:"cursor"`
	Rows   map[string]*MentionRecord `json:"rows"`
}

// MentionsStore is the in-process owner of mentions.json.
type MentionsStore struct {
	mu sync.Mutex
	// snap is the loaded state, re-embedded on every save.
	snap MentionState
	path string
}

// OpenMentions loads (creating if needed) the mention ledger at path.
func OpenMentions(path string) (*MentionsStore, error) {
	s := &MentionsStore{path: path, snap: MentionState{Rows: map[string]*MentionRecord{}}}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &s.snap); err != nil {
			return nil, fmt.Errorf("malformed mentions store %s: %w", path, err)
		}
		if s.snap.Rows == nil {
			s.snap.Rows = map[string]*MentionRecord{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *MentionsStore) saveLocked() error {
	// The cap holds no matter which path wrote — prune before persisting.
	PruneMentions(&s.snap)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Snapshot returns a deep-ish copy (rows copied) of the current state.
func (s *MentionsStore) Snapshot() MentionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make(map[string]*MentionRecord, len(s.snap.Rows))
	for id, r := range s.snap.Rows {
		cp := *r
		rows[id] = &cp
	}
	return MentionState{Cursor: s.snap.Cursor, Rows: rows}
}

// Mutate runs fn under the lock with the live state and saves when it
// reports a change. fn must not retain the state pointer.
func (s *MentionsStore) Mutate(fn func(st *MentionState) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !fn(&s.snap) {
		return nil
	}
	return s.saveLocked()
}

// Prune drops the oldest rows beyond MaxMentionRows (by mention timestamp).
func PruneMentions(st *MentionState) bool {
	if len(st.Rows) <= MaxMentionRows {
		return false
	}
	ids := make([]string, 0, len(st.Rows))
	for id := range st.Rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return st.Rows[ids[i]].CreatedAt < st.Rows[ids[j]].CreatedAt
	})
	for _, id := range ids[:len(ids)-MaxMentionRows] {
		delete(st.Rows, id)
	}
	return true
}

// mentionExcerpt is a bounded single-line prefix of the mention text (the
// full text rides the kick prompt; the excerpt is the ledger's glanceable form).
func mentionExcerpt(content string) string {
	const max = 160
	if len(content) > max {
		content = content[:max] + "…"
	}
	return content
}

// MentionReplied marks a record's reply (idempotent no-op when already set).
func MentionReplied(st *MentionState, id, replyID string) bool {
	r, ok := st.Rows[id]
	if !ok || r.Replied {
		return false
	}
	r.Replied = true
	r.ReplyID = replyID
	return true
}

// MentionDetail appends a diagnostic once (a marker prefix keeps it from
// stacking every tick).
func MentionDetail(st *MentionState, id, detail string) bool {
	r, ok := st.Rows[id]
	if !ok || r.Detail == detail {
		return false
	}
	r.Detail = detail
	return true
}
