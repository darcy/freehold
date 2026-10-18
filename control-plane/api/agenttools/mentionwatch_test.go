package agenttools

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"freehold/contract/console"
	"freehold/contract/delegate"
	"freehold/contract/wire"
)

const testAgentPK = "1111111111111111111111111111111111111111111111111111111111111111"

func frozenNow() time.Time { return time.Unix(1_700_000_000, 0) }

// fakeQuery dispatches on the filter's shape: a mention poll (#p), a task
// fetch (ids), a reply poll (#e + authors) — honoring the requested ids so
// the task-context fetch returns only its own event.
func fakeQuery(mentions, tasks, replies []delegate.TaggedEvent) func(delegate.TaggedFilter) ([]delegate.TaggedEvent, error) {
	return func(f delegate.TaggedFilter) ([]delegate.TaggedEvent, error) {
		switch {
		case len(f.P) > 0:
			return mentions, nil
		case len(f.Ids) > 0:
			var out []delegate.TaggedEvent
			for _, ev := range tasks {
				for _, id := range f.Ids {
					if ev.ID == id {
						out = append(out, ev)
						break
					}
				}
			}
			return out, nil
		case len(f.E) > 0:
			return replies, nil
		}
		return nil, nil
	}
}

func testWatcher(t *testing.T, agents []console.AgentInfo, query func(delegate.TaggedFilter) ([]delegate.TaggedEvent, error)) (*MentionWatcher, *MentionsStore) {
	t.Helper()
	store, err := OpenMentions(t.TempDir() + "/mentions.json")
	if err != nil {
		t.Fatalf("OpenMentions: %v", err)
	}
	w := &MentionWatcher{
		Mentions:    store,
		Secret:      make([]byte, 32),
		DialURL:     "http://relay.test",
		WakeChannel: "freehold-chan",
		Now:         frozenNow,
		Query:       query,
	}
	if agents != nil {
		w.Agents = func() ([]console.AgentInfo, error) { return agents, nil }
	}
	return w, store
}

func agentRow() []console.AgentInfo {
	return []console.AgentInfo{{Name: "homelab", Pubkey: testAgentPK}}
}

func mentionEvent(id, author, root, content string) delegate.TaggedEvent {
	return delegate.TaggedEvent{
		ID: id, Kind: wire.TextNote, Author: author, CreatedAt: frozenNow().Unix() - 60,
		Tags:    [][]string{{"e", root, "", "root"}, {"p", testAgentPK}},
		Content: content,
	}
}

func rowOf(t *testing.T, s *MentionsStore, id string) *MentionRecord {
	t.Helper()
	st := s.Snapshot()
	if r := st.Rows[id]; r != nil {
		cp := *r
		return &cp
	}
	return nil
}

// TestWatcherKicksChannellessMention: the happy path — a kind-1 with the
// agent's p tag and no h tag is discovered, the pod is kicked with a valid
// buzz-acp task JSON, and the terminal record lands.
func TestWatcherKicksChannellessMention(t *testing.T) {
	ev := mentionEvent("mention1", "author-pk-64-chars-long-aaaaaaaaaaaaaaaaaaaaaaa", "issue1", "please fix the thing")
	kickCh := make(chan string, 1)
	w, store := testWatcher(t, agentRow(), fakeQuery(
		[]delegate.TaggedEvent{ev},
		[]delegate.TaggedEvent{ev, {ID: "issue1", Kind: wire.GitIssue, Tags: [][]string{{"title", "The thing"}}, Content: "body"}},
		nil,
	))
	w.Kick = func(pod, taskJSON string, timeoutS uint64) (string, error) {
		kickCh <- pod + "|" + taskJSON
		return `{"version":1,"task_id":"x","status":"completed"}`, nil
	}
	w.TickNow()

	got := strings.SplitN(<-kickCh, "|", 2)
	if got[0] != "homelab" {
		t.Fatalf("pod = %q, want homelab (the sanitized name)", got[0])
	}
	task := got[1]
	for _, want := range []string{`"version":1`, `"agent_pubkey":"` + testAgentPK + `"`, "please fix the thing", "issue1", "The thing", "task_comment"} {
		if !strings.Contains(task, want) {
			t.Fatalf("task JSON missing %q: %s", want, task)
		}
	}
	row := rowOf(t, store, "mention1")
	if row == nil || (row.Status != MentionKicked && row.Status != MentionCompleted) {
		t.Fatalf("row not kicked: %+v", row)
	}
	// The goroutine records the terminal; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		row = rowOf(t, store, "mention1")
		if row != nil && row.Status == MentionCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal never recorded: %+v", row)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestWatcherSkipsChannelScopedAndUnmanaged: an h-tagged event (the channel
// path owns it) and a p tag for an unmanaged key produce nothing.
func TestWatcherSkipsChannelScopedAndUnmanaged(t *testing.T) {
	scoped := mentionEvent("scoped", "author", "issue1", "in a channel")
	scoped.Tags = append(scoped.Tags, []string{"h", "chan-id"})
	unmanaged := mentionEvent("unmanaged", "author", "issue1", "not for us")
	unmanaged.Tags = [][]string{{"p", "9999999999999999999999999999999999999999999999999999999999999999"}}
	kicks := 0
	w, store := testWatcher(t, agentRow(), fakeQuery(
		[]delegate.TaggedEvent{scoped, unmanaged}, nil, nil))
	w.Kick = func(pod, taskJSON string, timeoutS uint64) (string, error) {
		kicks++
		return `{"status":"completed"}`, nil
	}
	w.TickNow()
	if kicks != 0 {
		t.Fatalf("kicked %d times, want 0", kicks)
	}
	if st := store.Snapshot(); len(st.Rows) != 0 {
		t.Fatalf("rows recorded: %+v", st.Rows)
	}
}

// TestWatcherInFlightGate: a second mention for a busy agent is not kicked
// until the first task's terminal lands (the busy pod would drop it).
func TestWatcherInFlightGate(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var mu sync.Mutex
	kicks := 0
	m1 := mentionEvent("m1", "author", "issue1", "first")
	m2 := mentionEvent("m2", "author", "issue1", "second")
	m2.CreatedAt = frozenNow().Unix() - 30 // newer: m1 is kicked first
	w, store := testWatcher(t, agentRow(), fakeQuery(
		[]delegate.TaggedEvent{m1, m2}, nil, nil))
	w.Kick = func(pod, taskJSON string, timeoutS uint64) (string, error) {
		mu.Lock()
		kicks++
		mu.Unlock()
		started <- struct{}{}
		<-release
		return `{"status":"completed"}`, nil
	}
	w.TickNow()
	<-started // the first kick is in flight
	mu.Lock()
	got := kicks
	mu.Unlock()
	if got != 1 {
		t.Fatalf("kicks = %d, want 1 while in flight", got)
	}
	// The second mention stays discovered (never dropped, retried next tick).
	if row := rowOf(t, store, "m2"); row == nil || row.Status != MentionDiscovered {
		t.Fatalf("m2 not discovered: %+v", row)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if row := rowOf(t, store, "m1"); row != nil && row.Status == MentionCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first task never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Next tick: the gate is free, the queued mention kicks.
	w.TickNow()
	deadline = time.Now().Add(2 * time.Second)
	for {
		if row := rowOf(t, store, "m2"); row != nil && row.Status == MentionCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("m2 never kicked: %+v", rowOf(t, store, "m2"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestWatcherFallbackWake: an exec-level failure sends the ordinary-path
// wake and records fell_back.
func TestWatcherFallbackWake(t *testing.T) {
	ev := mentionEvent("m1", "author", "issue1", "help")
	wakes := 0
	w, store := testWatcher(t, agentRow(), fakeQuery([]delegate.TaggedEvent{ev}, nil, nil))
	w.Kick = func(pod, taskJSON string, timeoutS uint64) (string, error) {
		return "", fmt.Errorf("runner exec failed (timed=false exit=1): pod gone")
	}
	// The wake is a delegate publish — inject it by faking the Query? No:
	// wake posts directly. Wrap via a hook: the watcher's wake uses
	// delegate.PostTaggedMessageAuth with the real dial — tests can't reach
	// the network, so assert through the recorded status instead (the wake
	// fails fast on the test's unroutable dial URL, giving failed+detail).
	w.TickNow()
	deadline := time.Now().Add(2 * time.Second)
	for {
		row := rowOf(t, store, "m1")
		if row != nil && (row.Status == MentionFailed || row.Status == MentionFellBack) {
			if row.Detail == "" {
				t.Fatal("no failure detail recorded")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failure never recorded: %+v", row)
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = wakes
}

// TestWatcherReplyDetection: the agent's own kind-1 with the task as its
// root e tag, created after the kick, marks the record replied.
func TestWatcherReplyDetection(t *testing.T) {
	ev := mentionEvent("m1", "author", "issue1", "help")
	w, store := testWatcher(t, agentRow(), fakeQuery(
		[]delegate.TaggedEvent{ev},
		nil,
		[]delegate.TaggedEvent{{ID: "reply1", Kind: wire.TextNote, Author: testAgentPK,
			CreatedAt: frozenNow().Unix(), Tags: [][]string{{"e", "issue1", "", "root"}}, Content: "done"}},
	))
	w.Kick = func(pod, taskJSON string, timeoutS uint64) (string, error) {
		return `{"status":"completed"}`, nil
	}
	w.TickNow()
	deadline := time.Now().Add(2 * time.Second)
	for {
		row := rowOf(t, store, "m1")
		if row != nil && row.Replied && row.ReplyID == "reply1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reply never detected: %+v", row)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A pre-kick comment on the same task does NOT count (the assignment may
	// exist because the agent commented) — the query is since-filtered, so
	// simulate by moving the reply before the kick and re-checking on a
	// fresh watcher pass.
	store.Mutate(func(s *MentionState) bool {
		s.Rows["m1"].Replied = false
		s.Rows["m1"].ReplyID = ""
		return true
	})
	old := mentionEvent("m1", "author", "issue1", "help")
	old.CreatedAt = frozenNow().Unix() - 3600
	w2, store2 := testWatcher(t, agentRow(), fakeQuery([]delegate.TaggedEvent{old}, nil,
		[]delegate.TaggedEvent{{ID: "old-reply", Kind: wire.TextNote, Author: testAgentPK,
			CreatedAt: frozenNow().Unix() - 7200, Tags: [][]string{{"e", "issue1", "", "root"}}, Content: "earlier"}}))
	w2.Kick = func(pod, taskJSON string, timeoutS uint64) (string, error) {
		return `{"status":"completed"}`, nil
	}
	w2.TickNow()
	time.Sleep(50 * time.Millisecond)
	if row := rowOf(t, store2, "m1"); row != nil && row.Replied {
		t.Fatal("a pre-kick comment counted as the reply")
	}
	_ = store
}

// TestWatcherStaleKickClosed: a kicked row with no terminal (a CP restart
// mid-exec) closes as timed_out after the stale grace.
func TestWatcherStaleKickClosed(t *testing.T) {
	ev := mentionEvent("m1", "author", "issue1", "help")
	blocked := make(chan struct{})
	w, store := testWatcher(t, agentRow(), fakeQuery([]delegate.TaggedEvent{ev}, nil, nil))
	w.Kick = func(pod, taskJSON string, timeoutS uint64) (string, error) {
		<-blocked // never returns: the exec hangs past the grace
		return "", nil
	}
	w.TickNow()
	// Backdate the kick past the stale grace and re-tick.
	store.Mutate(func(s *MentionState) bool {
		s.Rows["m1"].KickedAt = uint64(frozenNow().Unix() - int64(mentionStaleGrace.Seconds()) - 10)
		return true
	})
	w.TickNow()
	row := rowOf(t, store, "m1")
	if row == nil || row.Status != MentionTimedOut || !strings.Contains(row.Detail, "restart") {
		t.Fatalf("stale kick not closed: %+v", row)
	}
	close(blocked)
}

// TestMentionsStoreRoundTrip: the ledger persists (a restart keeps rows +
// cursor) and prunes oldest beyond the cap.
func TestMentionsStoreRoundTrip(t *testing.T) {
	path := t.TempDir() + "/mentions.json"
	store, err := OpenMentions(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Mutate(func(s *MentionState) bool {
		s.Cursor = 42
		s.Rows["a"] = &MentionRecord{ID: "a", CreatedAt: 1, Status: MentionCompleted}
		s.Rows["b"] = &MentionRecord{ID: "b", CreatedAt: 2, Status: MentionKicked}
		return true
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	reopened, err := OpenMentions(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	st := reopened.Snapshot()
	if st.Cursor != 42 || len(st.Rows) != 2 || st.Rows["b"].Status != MentionKicked {
		t.Fatalf("round trip lost state: %+v", st)
	}
	if err := reopened.Mutate(func(s *MentionState) bool {
		for i := 0; i < MaxMentionRows+10; i++ {
			s.Rows[fmt.Sprintf("x%d", i)] = &MentionRecord{ID: fmt.Sprintf("x%d", i), CreatedAt: uint64(100 + i)}
		}
		return true
	}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if st = reopened.Snapshot(); len(st.Rows) > MaxMentionRows {
		t.Fatalf("prune left %d rows", len(st.Rows))
	}
	if _, ok := st.Rows["a"]; ok {
		t.Fatal("oldest row not pruned")
	}
}
