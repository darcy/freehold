package agenttools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"freehold/contract/console"
	"freehold/contract/delegate"
)

func mkStore(t *testing.T) *JobsStore {
	t.Helper()
	s, err := OpenJobs(t.TempDir() + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mkSched(t *testing.T, store *JobsStore, agents map[string]string) (*Scheduler, *[][]string, *[]delegate.PollResult) {
	t.Helper()
	pks := map[string]string{}
	for name, pk := range agents {
		pks[name] = pk
	}
	reg := func() ([]console.AgentInfo, error) {
		out := []console.AgentInfo{}
		for name, pk := range pks {
			out = append(out, console.AgentInfo{Name: name, Pubkey: pk})
		}
		return out, nil
	}
	var fired [][]string
	polls := &[]delegate.PollResult{}
	s := &Scheduler{
		Jobs:   store,
		Agents: reg,
		Secret: make([]byte, 32),
		Now:    func() time.Time { return time.Unix(1_800_000_000, 0) },
		Post: func(channel, mention string, extra [][]string, content string) error {
			fired = append(fired, []string{channel, mention, content})
			return nil
		},
		Poll: func(channel string, since int64) ([]delegate.PollResult, error) { return *polls, nil },
	}
	return s, &fired, polls
}

func TestCatchupWindowClamps(t *testing.T) {
	for _, tc := range []struct {
		period, want time.Duration
	}{{time.Minute, 2 * time.Minute}, {time.Hour, 30 * time.Minute}, {48 * time.Hour, 2 * time.Hour}} {
		if got := catchupWindow(tc.period); got != tc.want {
			t.Errorf("catchupWindow(%v) = %v, want %v", tc.period, got, tc.want)
		}
	}
}

func TestParseScheduleTZ(t *testing.T) {
	sched, err := ParseSchedule("0 7 * * *", "America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-06-01 06:00 UTC == 01:00 Chicago; next 07:00 local = 12:00 UTC.
	next := sched.Next(time.Unix(1_780_308_000, 0).UTC())
	if next.UTC().Hour() != 12 {
		t.Errorf("next fire in Chicago = %v, want a 12:xx UTC hour (07:00 CDT)", next)
	}
	if _, err := ParseSchedule("0 7 * * *", "Not/AZone"); err == nil {
		t.Error("bad tz must refuse")
	}
	if _, err := ParseSchedule("99 7 * * *", ""); err == nil {
		t.Error("bad cron must refuse")
	}
}

func TestTickFiresDueJobAndAdvances(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	nt, _, err := nextAfter("*/5 * * * *", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(&Job{ID: "j1", Agent: "cpa", Channel: "ch1", Cron: "*/5 * * * *",
		Prompt: "do the thing", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"cpa": "aabb"})
	s.Now = func() time.Time { return now }
	s.TickNow()

	if len(*fired) != 1 {
		t.Fatalf("fired %d mentions, want 1", len(*fired))
	}
	if (*fired)[0][0] != "ch1" || (*fired)[0][1] != "aabb" {
		t.Errorf("fire = %v, want ch1/aabb", (*fired)[0])
	}
	j, _ := store.Get("j1")
	last := j.LastRun()
	if last == nil || last.Status != RunFired {
		t.Fatalf("last run = %+v, want fired", last)
	}
	if j.NextRunAt != uint64(nt.Unix()) {
		t.Errorf("NextRunAt = %d, want %d (advanced BEFORE dispatch)", j.NextRunAt, nt.Unix())
	}
}

func TestTickInFlightGateSkipsSecondJob(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	if err := store.Create(&Job{ID: "j1", Agent: "cpa", Channel: "ch1", Cron: "@daily", Prompt: "p1",
		NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(&Job{ID: "j2", Agent: "cpa", Channel: "ch1", Cron: "@daily", Prompt: "p2",
		NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"cpa": "aabb"})
	s.Now = func() time.Time { return now }
	s.TickNow()

	if len(*fired) != 1 {
		t.Fatalf("fired %d, want exactly 1 (in-flight gate)", len(*fired))
	}
	j2, _ := store.Get("j2")
	if last := j2.LastRun(); last == nil || last.Status != RunSkipped {
		t.Errorf("j2 last run = %+v, want skipped", last)
	}
	// The skipped slot was advanced, not dropped silently.
	if j2.NextRunAt <= uint64(now.Unix()) {
		t.Errorf("j2 NextRunAt = %d, want advanced past now", j2.NextRunAt)
	}
}

func TestTickClosesRunOnReplyAndTimesOutWithout(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	agentPK := "aabb"
	if err := store.Create(&Job{ID: "j1", Owner: "ccdd", Agent: "cpa", Channel: "ch1", Cron: "@daily",
		Prompt: "p", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, polls := mkSched(t, store, map[string]string{"cpa": agentPK})
	s.Now = func() time.Time { return now }
	s.TickNow()
	if len(*fired) != 1 {
		t.Fatal("job did not fire")
	}

	// The agent replies in-channel one tick later: the run closes ok.
	*polls = append(*polls, delegate.PollResult{CreatedAt: now.Unix() + 30, Author: agentPK, Content: "done"})
	s.Now = func() time.Time { return now.Add(60 * time.Second) }
	s.TickNow()
	j, _ := store.Get("j1")
	if last := j.LastRun(); last.Status != RunOK {
		t.Errorf("run = %s, want ok after the agent replied", last.Status)
	}

	// No reply within the timeout: the run closes timeout + the owner is notified.
	if err := store.Create(&Job{ID: "j2", Owner: "ccdd", Agent: "cpa", Channel: "ch1", Cron: "@daily",
		Prompt: "p", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	*fired = nil
	*polls = nil
	s.TickNow() // fires j2
	*polls = nil
	s.Now = func() time.Time { return now.Add(runTimeout + time.Minute) }
	s.TickNow()
	j2, _ := store.Get("j2")
	if last := j2.LastRun(); last.Status != RunTimeout {
		t.Errorf("run = %s, want timeout", last.Status)
	}
	// The failure note went to the owner (the 2nd post of this tick's history).
	if len(*fired) != 2 || (*fired)[1][1] != "ccdd" {
		t.Errorf("failure note = %v, want a mention to the owner", *fired)
	}
}

func TestTickMissedSlotRecordedNotFired(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	// @daily, last slot ~26h ago: outside the 2h-clamped window.
	if err := store.Create(&Job{ID: "j1", Agent: "cpa", Channel: "ch1", Cron: "@daily",
		Prompt: "p", NextRunAt: uint64(now.Add(-26 * time.Hour).Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"cpa": "aabb"})
	s.Now = func() time.Time { return now }
	s.TickNow()
	if len(*fired) != 0 {
		t.Fatalf("fired %d, want 0 (missed slot)", len(*fired))
	}
	j, _ := store.Get("j1")
	if last := j.LastRun(); last == nil || last.Status != RunMissed {
		t.Errorf("last run = %+v, want missed", last)
	}
	if j.NextRunAt <= uint64(now.Unix()) {
		t.Errorf("NextRunAt = %d, want advanced past now", j.NextRunAt)
	}
}

func TestTickOneShotRequeuesWhenBusyAndConsumes(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	// A job already in flight blocks the agent.
	if err := store.Create(&Job{ID: "busy", Agent: "cpa", Channel: "ch1", Cron: "@daily", Prompt: "p",
		NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(&Job{ID: "j1", Owner: "ccdd", Agent: "cpa", Channel: "ch1", At: uint64(now.Unix()),
		Prompt: "remind me"}); err != nil {
		t.Fatal(err)
	}
	s, fired, polls := mkSched(t, store, map[string]string{"cpa": "aabb"})
	s.Now = func() time.Time { return now }
	s.TickNow()
	if len(*fired) != 1 { // only the pre-open run's job state counts; the one-shot re-queued
		t.Fatalf("fired %d, want 1 (the one-shot re-queued, nothing new fired)", len(*fired))
	}
	j1, _ := store.Get("j1")
	if j1.At != uint64(now.Add(oneShotRetry).Unix()) {
		t.Errorf("one-shot At = %d, want re-queued +%v", j1.At, oneShotRetry)
	}
	if len(j1.Runs) != 0 {
		t.Errorf("re-queued one-shot recorded runs: %+v", j1.Runs)
	}

	// Free the agent (its open run closes on a reply), advance past the
	// re-queue delay; the one-shot fires once and consumes itself (At -> 0).
	*polls = append(*polls, delegate.PollResult{CreatedAt: now.Unix() + 30, Author: "aabb", Content: "done"})
	s.Now = func() time.Time { return now.Add(60 * time.Second) }
	s.TickNow() // closes busy's open run ok
	j1, _ = store.Get("j1")
	if j1.At == 0 {
		t.Fatal("one-shot fired while the agent was still busy")
	}
	*fired = nil
	s.Now = func() time.Time { return now.Add(3 * time.Minute) }
	s.TickNow()
	if len(*fired) != 1 {
		t.Fatalf("fired %d, want 1 (the one-shot)", len(*fired))
	}
	j1, _ = store.Get("j1")
	if j1.At != 0 {
		t.Errorf("one-shot At = %d, want 0 (consumed)", j1.At)
	}
}

func TestTickMissingAgentPausesJob(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	if err := store.Create(&Job{ID: "j1", Agent: "gone", Channel: "ch1", Cron: "@daily",
		Prompt: "p", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"cpa": "aabb"})
	s.Now = func() time.Time { return now }
	s.TickNow()
	if len(*fired) != 0 {
		t.Fatal("a job with no resolvable agent must not fire")
	}
	j, _ := store.Get("j1")
	if !j.Paused {
		t.Error("job should be paused with a recorded error")
	}
}

func TestTickRenameFollowsPubkey(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	// The job routes by pubkey; the registry later renames the agent.
	if err := store.Create(&Job{ID: "j1", Agent: "old-name", AgentPubkey: "aabb", Channel: "ch1",
		Cron: "@daily", Prompt: "p", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"new-name": "aabb"})
	s.Now = func() time.Time { return now }
	s.TickNow()
	if len(*fired) != 1 {
		t.Fatalf("fired %d, want 1 — a rename must not break the job", len(*fired))
	}
	j, _ := store.Get("j1")
	if j.Agent != "new-name" {
		t.Errorf("display name = %q, want refreshed to new-name", j.Agent)
	}
	if last := j.LastRun(); last == nil || last.Status != RunFired {
		t.Errorf("last run = %+v, want fired", last)
	}
}

func TestCreateValidation(t *testing.T) {
	store := mkStore(t)
	base := Job{ID: "x", Agent: "cpa", Channel: "ch", Prompt: "p", Cron: "@daily"}
	if err := store.Create(&base); err != nil {
		t.Fatalf("recurring create: %v", err)
	}
	for _, j := range []*Job{
		{ID: "a", Channel: "ch", Prompt: "p", Cron: "@daily"},
		{ID: "b", Agent: "cpa", Prompt: "p", Cron: "@daily"},
		{ID: "c", Agent: "cpa", Channel: "ch"},
	} {
		if err := store.Create(j); err == nil {
			t.Errorf("create %+v: want validation error", j)
		}
	}
	if err := store.Create(&Job{ID: "x", Agent: "cpa", Channel: "ch", Prompt: "p", Cron: "@daily"}); err == nil {
		t.Error("duplicate id must refuse")
	}
}

func TestRunLedgerCapped(t *testing.T) {
	store := mkStore(t)
	if err := store.Create(&Job{ID: "j", Agent: "cpa", Channel: "ch", Prompt: "p", Cron: "@daily"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxJobRuns+5; i++ {
		if err := store.recordRun("j", JobRun{FiredAt: uint64(i), Status: RunOK}); err != nil {
			t.Fatal(err)
		}
	}
	j, _ := store.Get("j")
	if len(j.Runs) != MaxJobRuns {
		t.Fatalf("runs = %d, want capped at %d", len(j.Runs), MaxJobRuns)
	}
	if j.Runs[0].FiredAt != uint64(5) {
		t.Errorf("oldest kept run = %d, want 5 (newest kept)", j.Runs[0].FiredAt)
	}
}

func TestLoadJobsReadOnlyMissingFile(t *testing.T) {
	jobs, err := LoadJobsReadOnly(t.TempDir() + "/nope.json")
	if err != nil || jobs != nil {
		t.Errorf("missing file = (%v, %v), want (nil, nil)", jobs, err)
	}
}

func TestSchedulerRunDisabledWithoutSecret(t *testing.T) {
	s := &Scheduler{Jobs: mkStore(t), DialURL: ""}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return")
	}
}

func TestTickOneShotFailedPostRequeuesAndPauses(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	if err := store.Create(&Job{ID: "j1", Owner: "ccdd", Agent: "cpa", Channel: "ch1",
		At: uint64(now.Unix()), Prompt: "remind me"}); err != nil {
		t.Fatal(err)
	}
	s, _, _ := mkSched(t, store, map[string]string{"cpa": "aabb"})
	clock := now
	s.Now = func() time.Time { return clock }
	s.Post = func(channel, mention string, extra [][]string, content string) error {
		return errors.New("event publish returned HTTP 400: refused")
	}
	for i := 0; i < 4; i++ {
		s.TickNow()
		j, _ := store.Get("j1")
		if j.Paused {
			t.Fatalf("job paused after %d failures, want re-queue until 5", i+1)
		}
		if j.At != uint64(clock.Add(oneShotRetry).Unix()) {
			t.Fatalf("attempt %d: At = %d, want re-queued +%v", i+1, j.At, oneShotRetry)
		}
		clock = clock.Add(oneShotRetry)
	}
	s.TickNow()
	j, _ := store.Get("j1")
	if !j.Paused {
		t.Error("5 consecutive failed posts must pause the job")
	}
	if last := j.LastRun(); last == nil || last.Status != RunError {
		t.Errorf("last run = %+v, want error", last)
	}
}

func TestTickMissingAgentPubkeyRoutedPauses(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	// A pubkey-routed job whose agent row was REMOVED (manage_agent remove):
	// the stored pubkey no longer resolves — the job must pause, not fire
	// into the channel (and ping the owner) every slot forever.
	if err := store.Create(&Job{ID: "j1", Owner: "ccdd", Agent: "freehold", AgentPubkey: "removed-pk",
		Channel: "ch1", Cron: "@daily", Prompt: "p", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"cpa": "aabb"})
	s.Now = func() time.Time { return now }
	s.TickNow()
	if len(*fired) != 0 {
		t.Fatal("a removed agent's job must not fire")
	}
	j, _ := store.Get("j1")
	if !j.Paused {
		t.Error("pubkey-routed job with a missing registry row must pause")
	}
}

func TestTickFiresIntoJobsChannelWithAttribution(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	human := "abababababababababababababababababababababababababababababababab"
	agentPK := "ccdd"
	if err := store.Create(&Job{ID: "j1", Owner: human, Agent: "cpa", Channel: "human-ch",
		Cron: "@daily", Label: "daily ask", Prompt: "brief me", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"cpa": agentPK})
	s.JobsChannel = "jobs-ch"
	s.ProfileName = func(string) string { return "Darcy" }
	s.Now = func() time.Time { return now }
	s.TickNow()

	var fire []string
	for _, f := range *fired {
		if f[1] == agentPK {
			fire = f
		}
	}
	if fire == nil {
		t.Fatalf("no fire mention to the agent: %v", *fired)
	}
	if fire[0] != "jobs-ch" {
		t.Errorf("fire channel = %q, want the jobs channel", fire[0])
	}
	for _, want := range []string{"Darcy", human, "human-ch", "daily ask", "brief me"} {
		if !strings.Contains(fire[2], want) {
			t.Errorf("fire content missing %q: %q", want, fire[2])
		}
	}
}

func TestTickThreadsFiresUnderAnchor(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	owner := "abababababababababababababababababababababababababababababababab"
	if err := store.Create(&Job{ID: "j1", Owner: owner, Agent: "cpa", Channel: "human",
		Cron: "@daily", Prompt: "p", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"cpa": "ccdd"})
	s.JobsChannel = "jobs"
	s.ProfileName = func(string) string { return "Op" }
	s.PostID = func(channel, mention string, extra [][]string, content string) (string, error) {
		return "anchor1", nil
	}
	var fireExtra [][]string
	s.Post = func(channel, mention string, extra [][]string, content string) error {
		fireExtra = extra
		*fired = append(*fired, []string{channel, mention, content})
		return nil
	}
	s.Now = func() time.Time { return now }
	s.TickNow()

	j, _ := store.Get("j1")
	if j.Anchor != "anchor1" {
		t.Errorf("job anchor = %q, want anchor1 (persisted for the next fire)", j.Anchor)
	}
	threaded := false
	for _, tag := range fireExtra {
		if len(tag) >= 2 && tag[0] == "e" && tag[1] == "anchor1" {
			threaded = true
		}
	}
	if !threaded {
		t.Errorf("fire tags = %v, want an e-root tag on the anchor", fireExtra)
	}
}

func TestTimeoutNoteLandsInJobsChannel(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	human := "abababababababababababababababababababababababababababababababab"
	if err := store.Create(&Job{ID: "j1", Owner: human, Agent: "cpa", Channel: "human",
		Cron: "@daily", Prompt: "p", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"cpa": "ccdd"})
	s.JobsChannel = "jobs"
	s.ProfileName = func(string) string { return "Op" }
	s.Now = func() time.Time { return now }
	s.TickNow()
	*fired = nil
	s.Now = func() time.Time { return now.Add(runTimeout + time.Minute) }
	s.TickNow()

	found := false
	for _, f := range *fired {
		if f[0] == "jobs" && f[1] == human && strings.Contains(f[2], "did not respond") {
			found = true
		}
	}
	if !found {
		t.Errorf("timeout note = %v, want a console-signed note to the owner in the jobs channel", *fired)
	}
}

func TestSelfInitiatedJobIsAttributed(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	agentPK := "ccdd"
	if err := store.Create(&Job{ID: "j1", Owner: agentPK, Agent: "cpa", Channel: "cpa-ch",
		Cron: "@daily", Label: "watch", Prompt: "check it", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"cpa": agentPK})
	s.JobsChannel = "jobs"
	s.Now = func() time.Time { return now }
	s.TickNow()

	var fire []string
	for _, f := range *fired {
		if f[1] == agentPK {
			fire = f
		}
	}
	if fire == nil {
		t.Fatalf("no fire mention: %v", *fired)
	}
	for _, want := range []string{"self-initiated", "watch", "check it"} {
		if !strings.Contains(fire[2], want) {
			t.Errorf("fire content missing %q: %q", want, fire[2])
		}
	}
}

func TestTimeoutNoteFallsBackToOperator(t *testing.T) {
	store := mkStore(t)
	now := time.Unix(1_800_000_000, 0)
	if err := store.Create(&Job{ID: "j1", Agent: "cpa", Channel: "human",
		Cron: "@daily", Prompt: "p", NextRunAt: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	s, fired, _ := mkSched(t, store, map[string]string{"cpa": "ccdd"})
	s.JobsChannel = "jobs"
	s.OperatorPub = "opopopopopopopopopopopopopopopopopopopopopopopopopopopopopopopop"
	s.Now = func() time.Time { return now }
	s.TickNow()
	*fired = nil
	s.Now = func() time.Time { return now.Add(runTimeout + time.Minute) }
	s.TickNow()

	found := false
	for _, f := range *fired {
		if f[0] == "jobs" && f[1] == s.OperatorPub && strings.Contains(f[2], "did not respond") {
			found = true
		}
	}
	if !found {
		t.Errorf("legacy ownerless timeout = %v, want the operator fallback ping", *fired)
	}
}

func TestSchedulerEnsuresHeartbeat(t *testing.T) {
	store := mkStore(t)
	s, _, _ := mkSched(t, store, map[string]string{"cpa": "ccdd"})
	s.JobsChannel = "jobs"
	s.HeartbeatAgent = "cpa"
	s.OperatorPub = "op"
	s.ensureHeartbeat(map[string]string{"cpa": "ccdd"})

	j, ok := store.Get("heartbeat")
	if !ok {
		t.Fatal("heartbeat job was not created")
	}
	if j.Agent != "cpa" || j.Channel != "jobs" || j.Cron != "@daily" || j.Owner != "op" {
		t.Errorf("heartbeat = %+v, want CPA-owned daily job in the jobs channel", j)
	}
	// Idempotent, and re-created if deleted (the permanent canary).
	s.ensureHeartbeat(map[string]string{"cpa": "ccdd"})
	if rows := store.Snapshot(); len(rows) != 1 {
		t.Fatalf("heartbeat ensure duplicated the job: %v", rows)
	}
	if err := store.Delete("heartbeat"); err != nil {
		t.Fatal(err)
	}
	s.ensureHeartbeat(map[string]string{"cpa": "ccdd"})
	if _, ok := store.Get("heartbeat"); !ok {
		t.Error("a deleted heartbeat must be re-created")
	}
}
