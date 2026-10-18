package agenttools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"freehold/contract/console"
	"freehold/contract/delegate"
	"freehold/contract/wire"
	"freehold/control-plane/api/agent"
)

// MentionWatcher constants (the hardening invariants — each guards a real
// failure mode; see docs/AI.md "Mention watch").
const (
	// mentionWatchTag marks the fallback wake ("t" tag) so the watcher's
	// traffic is identifiable in history.
	mentionWatchTag = "fh-mention-watch"
	// MentionWatchTick is the poll cadence.
	MentionWatchTick = 60 * time.Second
	// MentionKickMaxDuration is the task's max_duration_ms — the longest a
	// kicked turn may run before buzz-acp itself deadlines it.
	MentionKickMaxDuration = 15 * time.Minute
	// MentionExecTimeoutS is the exec timeout: the task cap plus dial/
	// process-start slack.
	MentionExecTimeoutS = uint64((MentionKickMaxDuration + 60*time.Second) / time.Second)
	// MentionReplyWindow is how long after a kick the agent's task comment
	// may still land and count as the delivery.
	MentionReplyWindow = 30 * time.Minute
	// MentionInitLookback is how far back a FIRST poll reads (a CP with a
	// fresh cursor re-discovers the recent window; the ledger dedups).
	MentionInitLookback = 48 * time.Hour
	// mentionCursorSkew re-reads a little before the cursor so a clock edge
	// between the relay and the CP cannot skip an event.
	mentionCursorSkew = 120 // seconds
	// MaxMentionKicksPerTick bounds one agent's new mentions per tick (a
	// burst of mentions cannot stack unbounded turns).
	MaxMentionKicksPerTick = 3
	// mentionStaleGrace is how long a kicked row may sit without a terminal
	// record before it is closed as lost (a CP restart mid-exec leaves
	// exactly this shape).
	mentionStaleGrace = MentionKickMaxDuration + 2*time.Minute
	// MentionPromptMax bounds each text block embedded in the kick prompt
	// (the buzz-acp task JSON's own cap is 1MB; this keeps a pasted-novel
	// mention from becoming a prompt bomb).
	MentionPromptMax = 16 << 10
)

// MentionKinds are the channel-less mention shapes: a NIP-01 note and a
// NIP-34 issue event (Buzz's Pulse + project surfaces).
var MentionKinds = []uint32{wire.TextNote, wire.GitIssue}

// MentionWatcher is the channel-less mention path. buzz-acp pods subscribe
// per-channel (#h is always in the REQ), so a task comment or assignment — a
// kind-1/1621 note with the agent's p tag and NO channel — can never wake
// one. The watcher polls the relay for exactly that shape, records every
// mention in the ledger (mentions.json — the CP is the record), and KICKS
// the agent directly: a buzz-acp `run --task` exec'd into the pod. The task
// session keeps the agent's memory and gets the mention + task reference in
// its prompt; the agent replies ON the task via the bridge's task_comment
// tool. A failed kick falls back to the ordinary mention path (one wake into
// the shared channel) so nothing is lost. The zero-value hooks are the real
// ones; tests inject fakes.
type MentionWatcher struct {
	// Mentions is the authoritative ledger (this process owns it).
	Mentions *MentionsStore
	// Agents resolves the managed agents fresh per tick (the registry).
	Agents func() ([]console.AgentInfo, error)
	// DialURL/AuthURL are the relay endpoints; Secret is the CONSOLE
	// identity — the poll (a NIP-98-authenticated query) and the fallback
	// wake sign as it.
	DialURL string
	AuthURL string
	Secret  []byte
	// WakeChannel receives the fallback mention wake when a kick fails
	// (the shared freehold channel — the ordinary path every pod watches).
	WakeChannel string
	// Kick execs one buzz-acp `run --task` into the agent's pod and returns
	// the terminal record from stdout (cpbuild's BuildMentionKicker).
	Kick func(pod, taskJSON string, timeoutS uint64) (string, error)

	// Now is the clock (tests inject).
	Now func() time.Time
	// Query reads the relay (tests inject; default = the NIP-98 query).
	Query func(delegate.TaggedFilter) ([]delegate.TaggedEvent, error)

	// Tick is the cadence (default 60s).
	Tick time.Duration
}

func (w *MentionWatcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *MentionWatcher) tickInterval() time.Duration {
	if w.Tick > 0 {
		return w.Tick
	}
	return MentionWatchTick
}

func (w *MentionWatcher) query(f delegate.TaggedFilter) ([]delegate.TaggedEvent, error) {
	if w.Query != nil {
		return w.Query(f)
	}
	return delegate.QueryTaggedAuth(w.DialURL, w.authURL(), w.Secret, f)
}

func (w *MentionWatcher) authURL() string {
	if w.AuthURL != "" {
		return w.AuthURL
	}
	return w.DialURL
}

// wake posts the fallback mention into the shared channel (the console
// identity signs it — the same shape a scheduled-job fire takes).
func (w *MentionWatcher) wake(agentPK, content string) error {
	return delegate.PostTaggedMessageAuth(w.DialURL, w.authURL(), w.Secret, w.WakeChannel, agentPK,
		[][]string{{"t", mentionWatchTag}}, content)
}

// Run ticks until ctx is done. Disabled — loudly — without a relay, a
// console credential, a kick, or a wake channel (the same fail-closed shape
// the scheduler has).
func (w *MentionWatcher) Run(ctx context.Context) {
	if w.Secret == nil || w.DialURL == "" || w.Mentions == nil || w.Kick == nil || w.WakeChannel == "" {
		log.Print("mention watch: disabled (missing relay, credential, kick, or wake channel)")
		return
	}
	log.Print("mention watch: running")
	t := time.NewTicker(w.tickInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.TickNow()
		}
	}
}

// TickNow runs one pass: discover mentions, kick the free agents, settle
// replies. One agent gets at most one in-flight kick — a busy pod would
// drop a second task (the bot-not-responding failure mode).
func (w *MentionWatcher) TickNow() {
	now := w.now()
	byPK := map[string]string{} // pubkey → display name
	if w.Agents != nil {
		if agents, err := w.Agents(); err == nil {
			for _, a := range agents {
				if a.Pubkey != "" {
					byPK[a.Pubkey] = a.Name
				}
			}
		} else {
			log.Printf("mention watch: registry unreadable, no kicks this tick: %v", err)
		}
	}
	w.discover(now, byPK)
	w.kickOpen(now, byPK)
	w.settle(now)
}

// discover polls for new channel-less mentions and records them (status
// discovered). The cursor advances past every tick even with no events, so
// an idle relay polls the last minute, not the whole lookback, forever.
func (w *MentionWatcher) discover(now time.Time, byPK map[string]string) {
	if len(byPK) == 0 {
		return
	}
	st := w.Mentions.Snapshot()
	since := st.Cursor
	if since == 0 {
		since = uint64(now.Unix() - int64(MentionInitLookback.Seconds()))
	}
	pks := make([]string, 0, len(byPK))
	for pk := range byPK {
		pks = append(pks, pk)
	}
	sort.Strings(pks)
	events, err := w.query(delegate.TaggedFilter{
		Kinds: MentionKinds, P: pks, Since: int64(since - mentionCursorSkew), Limit: 500,
	})
	if err != nil {
		log.Printf("mention watch: poll failed: %v", err)
		return
	}
	// The skew re-reads a little before the cursor each tick, so an event
	// missed at the window edge is caught next tick; the ledger is the dedup.
	newest := now.Unix() - 60
	for _, ev := range events {
		if ev.CreatedAt > newest {
			newest = ev.CreatedAt
		}
	}
	var discovered []*delegate.TaggedEvent
	seen := map[string]bool{}
	for i := range events {
		ev := &events[i]
		if ev.HasChannel() || st.Rows[ev.ID] != nil || seen[ev.ID] {
			continue
		}
		if agentPked(ev, byPK) == "" {
			continue // a p tag for someone else's agent, or none managed
		}
		seen[ev.ID] = true
		discovered = append(discovered, ev)
	}
	if len(discovered) == 0 {
		_ = w.Mentions.Mutate(func(s *MentionState) bool { return advanceCursor(s, newest) })
		return
	}
	if err := w.Mentions.Mutate(func(s *MentionState) bool {
		changed := advanceCursor(s, newest)
		for _, ev := range discovered {
			if s.Rows[ev.ID] != nil {
				continue // a concurrent tick won the id
			}
			s.Rows[ev.ID] = &MentionRecord{
				ID: ev.ID, AgentPubkey: agentPked(ev, byPK), Agent: byPK[agentPked(ev, byPK)],
				Author: ev.Author, CreatedAt: uint64(ev.CreatedAt),
				Excerpt: mentionExcerpt(ev.Content), Root: ev.Root(),
				Status: MentionDiscovered,
			}
			changed = true
		}
		return changed
	}); err != nil {
		log.Printf("mention watch: record failed: %v", err)
		return
	}
	for _, ev := range discovered {
		log.Printf("mention watch: discovered %s for %s (root %s)", shortID(ev.ID), byPK[agentPked(ev, byPK)], shortID(ev.Root()))
	}
}

func advanceCursor(s *MentionState, newest int64) bool {
	if newest <= 0 || uint64(newest) <= s.Cursor {
		return false
	}
	s.Cursor = uint64(newest)
	return true
}

// agentPked returns the managed pubkey the mention p-tags ("" when none).
func agentPked(ev *delegate.TaggedEvent, byPK map[string]string) string {
	for _, p := range ev.Tags {
		if len(p) > 1 && p[0] == "p" {
			if _, ok := byPK[p[1]]; ok {
				return p[1]
			}
		}
	}
	return ""
}

// kickOpen walks discovered records oldest-first and kicks one in-flight
// task per agent, up to MaxMentionKicksPerTick new kicks per agent per tick.
func (w *MentionWatcher) kickOpen(now time.Time, byPK map[string]string) {
	st := w.Mentions.Snapshot()
	ids := make([]string, 0, len(st.Rows))
	for id, r := range st.Rows {
		if r.Status == MentionDiscovered {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return st.Rows[ids[i]].CreatedAt < st.Rows[ids[j]].CreatedAt
	})
	inflight := map[string]bool{} // agent pubkey → a task is running
	budget := map[string]int{}
	for _, id := range ids {
		r := st.Rows[id]
		if inflight[r.AgentPubkey] {
			continue // the in-flight gate: one task per agent
		}
		if budget[r.AgentPubkey] >= MaxMentionKicksPerTick {
			continue
		}
		name := byPK[r.AgentPubkey]
		if name == "" {
			// The agent row is gone (removed): record it and stop retrying.
			_ = w.Mentions.Mutate(func(s *MentionState) bool {
				return MentionDetail(s, id, "agent is not in the registry — mention dropped")
			})
			continue
		}
		budget[r.AgentPubkey]++
		inflight[r.AgentPubkey] = true
		w.launchKick(id, *r, name, now)
	}
}

// launchKick flips the record to kicked (persisted FIRST — at-most-once),
// then runs the exec in a goroutine: a 15-minute task must not stall the
// tick loop's other agents (the settle pass runs regardless).
func (w *MentionWatcher) launchKick(id string, r MentionRecord, name string, now time.Time) {
	taskJSON, err := w.buildTask(id, r)
	if err != nil {
		_ = w.Mentions.Mutate(func(s *MentionState) bool {
			row, ok := s.Rows[id]
			if !ok {
				return false
			}
			row.Status = MentionFailed
			row.Detail = err.Error()
			return true
		})
		return
	}
	pod := agent.AgentPodName(name)
	kickedAt := uint64(now.Unix())
	if err := w.Mentions.Mutate(func(s *MentionState) bool {
		row, ok := s.Rows[id]
		if !ok || row.Status != MentionDiscovered {
			return false // a concurrent kick or a pruned row
		}
		row.Status = MentionKicked
		row.Agent = name
		row.KickedAt = kickedAt
		return true
	}); err != nil || !w.rowIs(id, MentionKicked) {
		return // persist failed (retried next tick) or a concurrent kick won
	}
	log.Printf("mention watch: kicked task for %s (mention %s, root %s)", name, shortID(id), shortID(r.Root))
	go func() {
		terminal, kerr := w.Kick(pod, taskJSON, MentionExecTimeoutS)
		status, detail := MentionCompleted, ""
		if kerr != nil {
			// The exec itself failed (pod gone, runner unreachable): fall
			// back to the ordinary mention path — one wake into the shared
			// channel, which every pod's subscription does see.
			detail = "kick failed: " + kerr.Error()
			status = MentionFailed
			if werr := w.wake(r.AgentPubkey, w.fallbackWake(r)); werr == nil {
				status = MentionFellBack
				detail += "; fallback wake sent to " + w.WakeChannel
			} else {
				detail += "; fallback wake also failed: " + werr.Error()
			}
		} else if ts := terminalStatus(terminal); ts != "" {
			switch ts {
			case "deadline", "cancelled":
				status = MentionTimedOut
			case "completed":
			default:
				status = MentionFailed
			}
			detail = "terminal: " + ts
		}
		if uerr := w.Mentions.Mutate(func(s *MentionState) bool {
			row, ok := s.Rows[id]
			if !ok || row.Status != MentionKicked {
				return false
			}
			row.Status = status
			row.Terminal = terminal
			row.Detail = detail
			return true
		}); uerr != nil {
			log.Printf("mention watch: terminal record for %s unrecorded: %v", name, uerr)
		}
		log.Printf("mention watch: task for %s finished: %s", name, status)
	}()
}

// rowIs confirms a record's status (a concurrent tick's Mutate could have
// raced the flip — only the winner launches the exec).
func (w *MentionWatcher) rowIs(id, status string) bool {
	st := w.Mentions.Snapshot()
	return st.Rows[id] != nil && st.Rows[id].Status == status
}

// buildTask composes the buzz-acp version-1 task JSON: the mention, the
// task reference, and the fetched task context, framed as instructions.
func (w *MentionWatcher) buildTask(id string, r MentionRecord) (string, error) {
	if len(r.AgentPubkey) != 64 {
		return "", fmt.Errorf("mention %s: bad agent pubkey", shortID(id))
	}
	taskID := "fh-mention-" + id
	if len(taskID) > 256 {
		taskID = taskID[:256]
	}
	// Re-fetch the FULL mention text (the excerpt is capped) — plus the task
	// itself when the mention references one, so an assignment (which often
	// carries no text of its own) still gives the agent something to work.
	text := r.Excerpt
	if evs, err := w.query(delegate.TaggedFilter{Kinds: MentionKinds, Ids: []string{id}}); err == nil && len(evs) > 0 {
		text = evs[0].Content
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Mention outside any channel — delivered by the control plane's mention watch]\n\n")
	fmt.Fprintf(&b, "From: %s\n", r.Author)
	fmt.Fprintf(&b, "Mention:\n%s\n", clip(text, MentionPromptMax))
	if r.Root != "" {
		fmt.Fprintf(&b, "\nTask reference: %s\n", r.Root)
		if task, terr := w.taskContext(r.Root); terr == nil && task != "" {
			fmt.Fprintf(&b, "\nTask:\n%s\n", clip(task, MentionPromptMax))
		}
		fmt.Fprintf(&b, "\nRespond ON the task: call the task_comment tool (issue_id %s) with your reply as content — that is how your answer reaches the task. If task_comment is missing from your tools, DM the author instead (their pubkey is above). Work it now.\n", r.Root)
	} else {
		fmt.Fprintf(&b, "\nThere is no task reference: reply by DM to the author (their pubkey is above). Work it now.\n")
	}
	task := map[string]interface{}{
		"version":         1,
		"task_id":         taskID,
		"agent_pubkey":    strings.ToLower(r.AgentPubkey),
		"prompt":          b.String(),
		"max_duration_ms": uint64(MentionKickMaxDuration / time.Millisecond),
	}
	raw, err := json.Marshal(task)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// taskContext fetches the task event itself (title + body) so the prompt
// carries something to work even for a bare assignment. Best-effort: an
// unreadable task still kicks with the mention text alone.
func (w *MentionWatcher) taskContext(root string) (string, error) {
	evs, err := w.query(delegate.TaggedFilter{Kinds: []uint32{wire.GitIssue, wire.TextNote}, Ids: []string{root}})
	if err != nil {
		return "", err
	}
	if len(evs) == 0 {
		return "", nil
	}
	var b strings.Builder
	if title := evs[0].Tag("title"); title != "" {
		fmt.Fprintf(&b, "%s\n\n", title)
	}
	fmt.Fprint(&b, evs[0].Content)
	return strings.TrimSpace(b.String()), nil
}

// settle closes the loop: the agent's own kind-1 with the task as its root
// e tag is the delivery (detected by an author-filtered query, so it works
// across CP restarts); stale kicks are closed as lost; expired windows are
// noted once.
func (w *MentionWatcher) settle(now time.Time) {
	st := w.Mentions.Snapshot()
	nowUnix := uint64(now.Unix())

	// Stale kicks first (a CP restart mid-exec leaves a row with no terminal).
	var stale []string
	for id, r := range st.Rows {
		if r.Status == MentionKicked && r.KickedAt > 0 &&
			nowUnix-r.KickedAt > uint64(mentionStaleGrace.Seconds()) {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		_ = w.Mentions.Mutate(func(s *MentionState) bool {
			row, ok := s.Rows[id]
			if !ok || row.Status != MentionKicked {
				return false
			}
			row.Status = MentionTimedOut
			row.Detail = "no terminal record (kick lost across a restart?)"
			return true
		})
	}

	// Reply detection: one query per agent with outstanding tasks (kinds
	// [1], #e = the tasks, authors = the agent), applied per row against
	// that row's own kick time (an agent's OLDER comment on the same task
	// must not count — the assignment may exist because they commented).
	type open struct {
		id   string
		root string
		at   uint64
	}
	byAgent := map[string][]open{}
	for id, r := range st.Rows {
		if r.Root == "" || r.Replied || r.KickedAt == 0 || r.Status == MentionDiscovered {
			continue
		}
		if nowUnix-r.KickedAt > uint64(MentionReplyWindow.Seconds()) {
			_ = w.Mentions.Mutate(func(s *MentionState) bool {
				return MentionDetail(s, id, "no reply on the task within the window")
			})
			continue
		}
		byAgent[r.AgentPubkey] = append(byAgent[r.AgentPubkey], open{id, r.Root, r.KickedAt})
	}
	for agentPK, opens := range byAgent {
		ids, roots := make([]string, 0, len(opens)), make([]string, 0, len(opens))
		since := nowUnix
		for _, o := range opens {
			ids = append(ids, o.id)
			roots = append(roots, o.root)
			if o.at < since {
				since = o.at
			}
		}
		events, err := w.query(delegate.TaggedFilter{
			Kinds: []uint32{wire.TextNote}, E: roots, Authors: []string{agentPK}, Since: int64(since),
		})
		if err != nil {
			log.Printf("mention watch: reply poll failed: %v", err)
			continue // transient; the next tick re-checks
		}
		for _, ev := range events {
			if ev.Author != agentPK {
				continue
			}
			for _, t := range ev.Tags {
				if len(t) < 4 || t[0] != "e" || t[3] != "root" {
					continue
				}
				for _, o := range opens {
					if t[1] == o.root && ev.CreatedAt >= int64(o.at) {
						_ = w.Mentions.Mutate(func(s *MentionState) bool {
							return MentionReplied(s, o.id, ev.ID)
						})
					}
				}
			}
		}
	}
}

// fallbackWake is the ordinary-path mention that stands in when the direct
// kick cannot run: the agent's #freehold subscription wakes it, and the
// prompt points at the task + the reply paths that work there.
func (w *MentionWatcher) fallbackWake(r MentionRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mention watch] You were @mentioned outside any channel (a task/comment the channel path cannot deliver).\n")
	fmt.Fprintf(&b, "From: %s | Task: %s\n\n%s\n\n", r.Author, orDash(r.Root), r.Excerpt)
	fmt.Fprintf(&b, "Fetch the task and respond on it: task_comment (issue_id %s) if your bridge has it, otherwise a DM to the author.", orDash(r.Root))
	return b.String()
}

// orDash renders an empty string as a dash (log/message polish).
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// terminalStatus extracts the status field from buzz-acp's version-1
// terminal record (one JSON object on stdout).
func terminalStatus(out string) string {
	var t struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &t); err != nil {
		return ""
	}
	return t.Status
}

// clip bounds a string for prompt embedding.
func clip(s string, max int) string {
	if len(s) > max {
		return s[:max] + "\n…(truncated)"
	}
	return s
}

// shortID keeps logs glanceable.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}
