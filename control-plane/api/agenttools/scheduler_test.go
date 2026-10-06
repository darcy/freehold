package agenttools

import (
	"context"
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
		Jobs:    store,
		Agents:  reg,
		Secret:  make([]byte, 32),
		Now:     func() time.Time { return time.Unix(1_800_000_000, 0) },
		Post:    func(channel, mention, content string) error { fired = append(fired, []string{channel, mention, content}); return nil },
		Poll:    func(channel string, since int64) ([]delegate.PollResult, error) { return *polls, nil },
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
