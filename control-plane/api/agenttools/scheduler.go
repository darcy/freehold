package agenttools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"freehold/contract/console"
	"freehold/contract/delegate"
	"freehold/contract/relay"
)

// Scheduler constants (the hardening invariants — each guards a real failure
// mode; see docs/AI.md "Scheduled jobs").
const (
	// fireTag marks every fire mention in the channel ("t" tag) so a job's
	// traffic is identifiable in history.
	fireTag = "fh-job"
	// runTimeout is how long a fired run may stay unanswered before it is
	// closed as a timeout (idle agents reply in seconds; long-but-active jobs
	// post SOMETHING in-channel well inside this — the reply check closes the
	// run on any agent message, so a chatty long run is never cut short).
	runTimeout = 10 * time.Minute
	// defaultTick is the scheduler cadence.
	defaultTick = 30 * time.Second
	// oneShotRetry is how long a skipped one-shot waits before re-firing
	// (the agent was busy at the moment it was due — a mini-queue, no machinery).
	oneShotRetry = 2 * time.Minute
)

// catchupWindow is how long past a missed slot a fire still goes out after a
// CP restart: half the period, clamped to [2m, 2h] (Hermes' invariant — past
// it the slot is recorded as missed, never silently dropped).
func catchupWindow(period time.Duration) time.Duration {
	w := period / 2
	const min = 2 * time.Minute
	const max = 2 * time.Hour
	if w < min {
		return min
	}
	if w > max {
		return max
	}
	return w
}

// ParseSchedule compiles a job's schedule: a standard 5-field cron expression
// (or an @every/@hourly descriptor) in the job's timezone — the TZ rides the
// spec as the CRON_TZ prefix the standard parser natively honors.
func ParseSchedule(cronExpr, tz string) (cron.Schedule, error) {
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return nil, fmt.Errorf("unknown timezone %q: %w", tz, err)
		}
		cronExpr = "CRON_TZ=" + tz + " " + cronExpr
	}
	sched, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return nil, fmt.Errorf("bad cron %q: %w", cronExpr, err)
	}
	return sched, nil
}

// nextAfter returns the first fire strictly after t for a recurring job.
func nextAfter(cronExpr, tz string, after time.Time) (time.Time, time.Duration, error) {
	sched, err := ParseSchedule(cronExpr, tz)
	if err != nil {
		return time.Time{}, 0, err
	}
	next := sched.Next(after)
	period := sched.Next(next).Sub(next)
	return next, period, nil
}

// Scheduler is the agent-tools process's tick loop. It fires due jobs as
// kind-9 mentions FROM THE CONSOLE IDENTITY into the world's scheduled-jobs
// channel (the pod wakes through its ordinary @mention path; the reply there
// is the delivery), watches each fired run for the agent's reply, and closes
// runs ok/timeout. Each job's fires thread under a per-job anchor, tagged
// t=fh-job. The zero-value clock/relay hooks are the real ones; tests inject
// fakes.
type Scheduler struct {
	// Jobs is the authoritative store (this process owns it).
	Jobs *JobsStore
	// Agents resolves an agent NAME to its current pubkey (the registry).
	Agents func() ([]console.AgentInfo, error)
	// DialURL/AuthURL are the relay endpoints; Secret is the CONSOLE identity
	// secret — the fire, the reply poll, and the failure note all sign as it.
	DialURL string
	AuthURL string
	Secret  []byte
	// JobsChannel is the world's scheduled-jobs channel id. Empty = legacy
	// mode (fires into the job's own Channel) — tests/older worlds.
	JobsChannel string
	// OperatorPub is the timeout-notice fallback for a legacy ownerless job.
	OperatorPub string
	// HeartbeatAgent is the agent that runs the permanent daily wake-path
	// canary (the CPA). Empty = no heartbeat (tests).
	HeartbeatAgent string

	// Now is the clock (tests inject).
	Now func() time.Time
	// Post fires one kind-9 message with extra tags (tests inject; default =
	// the relay).
	Post func(channel, mention string, extra [][]string, content string) error
	// PostID is Post that returns the published event id (the thread anchor).
	PostID func(channel, mention string, extra [][]string, content string) (string, error)
	// Poll reads the channel's messages newer than since (tests inject).
	Poll func(channel string, since int64) ([]delegate.PollResult, error)
	// ProfileName resolves a human pubkey's relay display name (tests inject).
	ProfileName func(pubkey string) string
	// EnsureChannel ensures the jobs channel (and operator membership) exists;
	// run once at startup (tests inject).
	EnsureChannel func() error

	// Tick is the cadence (default 30s).
	Tick time.Duration
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) tickInterval() time.Duration {
	if s.Tick > 0 {
		return s.Tick
	}
	return defaultTick
}

func (s *Scheduler) post(channel, mention string, extra [][]string, content string) error {
	if s.Post != nil {
		return s.Post(channel, mention, extra, content)
	}
	return delegate.PostTaggedMessageAuth(s.DialURL, s.authURL(), s.Secret, channel, mention, extra, content)
}

// postID posts and returns the event id (the per-job thread anchor).
func (s *Scheduler) postID(channel, mention string, extra [][]string, content string) (string, error) {
	if s.PostID != nil {
		return s.PostID(channel, mention, extra, content)
	}
	if s.Post != nil {
		return "", s.Post(channel, mention, extra, content)
	}
	return delegate.PostTaggedMessageAuthID(s.DialURL, s.authURL(), s.Secret, channel, mention, extra, content)
}

// profileName resolves a human pubkey's relay display name (kind:0).
func (s *Scheduler) profileName(pubkey string) string {
	if s.ProfileName != nil {
		return s.ProfileName(pubkey)
	}
	evs, err := relay.QueryEventsAuth(s.DialURL, s.authURL(), s.Secret, []interface{}{map[string]interface{}{
		"kinds":   []interface{}{0},
		"authors": []interface{}{pubkey},
		"limit":   1,
	}})
	if err != nil || len(evs) == 0 {
		return ""
	}
	content, _ := evs[0]["content"].(string)
	var p struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if json.Unmarshal([]byte(content), &p) != nil {
		return ""
	}
	if strings.TrimSpace(p.DisplayName) != "" {
		return strings.TrimSpace(p.DisplayName)
	}
	return strings.TrimSpace(p.Name)
}

// ownerLabel resolves the job owner's display label and whether the job is
// self-initiated (owner is a registry agent).
func (s *Scheduler) ownerLabel(ownerPK string, byPK map[string]string) (label string, self bool) {
	if name, ok := byPK[ownerPK]; ok {
		return "@" + name, true
	}
	if name := s.profileName(ownerPK); name != "" {
		return name, false
	}
	return ShortHex(ownerPK), false
}

// fireChannel is where a job's fire lands: the world jobs channel, or the
// job's own channel on a legacy/unwired scheduler.
func (s *Scheduler) fireChannel(j Job) string {
	if s.JobsChannel != "" {
		return s.JobsChannel
	}
	return j.Channel
}

// fireContent is the fire mention's body: the run metadata the woken agent
// needs to deliver + attribute its visible update, then the prompt.
func fireContent(j Job, ownerLabel string, self bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[scheduled job %q · %s]\n\n", jobLabel(j), j.ID)
	if self {
		fmt.Fprintf(&b, "Owner: %s (self-initiated)\n", ownerLabel)
		fmt.Fprintf(&b, "Visible update: this channel (system signal) — open with \"self-initiated: %s\"; the delivery reply here is enough unless the prompt names another channel.\n", jobLabel(j))
	} else {
		attribution := jobLabel(j)
		if j.Label == "" {
			attribution = "scheduled update"
		}
		fmt.Fprintf(&b, "Owner: %s (%s)\n", ownerLabel, j.Owner)
		if j.Channel == ScheduledJobsChannelID {
			fmt.Fprintf(&b, "Visible update: this channel (the delivery reply here is the visible update), opening with \"%s's %s:\".\n", ownerLabel, attribution)
		} else {
			fmt.Fprintf(&b, "Visible update: post in channel %s as yourself, opening with \"%s's %s:\"", j.Channel, ownerLabel, attribution)
			if j.ThreadRoot != "" {
				fmt.Fprintf(&b, " (thread under event %s)", j.ThreadRoot)
			}
			fmt.Fprintf(&b, ". If you cannot complete it, post a short failure note there instead.\n")
		}
	}
	fmt.Fprintf(&b, "Delivery: reply in this channel; your reply closes the run.\n\n%s", j.Prompt)
	return b.String()
}

// ensureHeartbeat keeps one daily heartbeat job alive — the wake-path canary.
// Its fire lands in the jobs channel and its visible update is the delivery
// reply there, so a world with zero other jobs still exercises mention-wake.
func (s *Scheduler) ensureHeartbeat(byName map[string]string) {
	if s.Jobs == nil || s.JobsChannel == "" || s.HeartbeatAgent == "" {
		return
	}
	if _, ok := s.Jobs.Get("heartbeat"); ok {
		return
	}
	pk := byName[s.HeartbeatAgent]
	if pk == "" {
		return
	}
	owner := s.OperatorPub
	if owner == "" {
		owner = pk
	}
	job := &Job{
		ID: "heartbeat", Owner: owner, Agent: s.HeartbeatAgent, AgentPubkey: pk,
		Channel: s.JobsChannel, Cron: "@daily", Label: "heartbeat",
		Prompt:    "Heartbeat: reply with a one-line status so the scheduled-job wake path stays exercised.",
		NextRunAt: nextRunFor("@daily", "", 0),
	}
	if err := s.Jobs.Create(job); err != nil {
		log.Printf("scheduled jobs: heartbeat ensure failed: %v", err)
	}
}

func (s *Scheduler) poll(channel string, since int64) ([]delegate.PollResult, error) {
	if s.Poll != nil {
		return s.Poll(channel, since)
	}
	return delegate.PollStreamAuth(s.DialURL, s.authURL(), s.Secret, channel, since)
}

func (s *Scheduler) authURL() string {
	if s.AuthURL != "" {
		return s.AuthURL
	}
	return s.DialURL
}

// Run ticks until ctx is done. Disabled (no-op) without a relay or a secret —
// the same fail-closed shape grant_agent has.
func (s *Scheduler) Run(ctx context.Context) {
	if s.Secret == nil || s.DialURL == "" || s.Jobs == nil {
		log.Print("scheduled jobs: disabled (no relay or console credential)")
		return
	}
	if s.EnsureChannel != nil {
		if err := s.EnsureChannel(); err != nil {
			log.Printf("scheduled jobs: jobs channel ensure failed (fires will report errors): %v", err)
		}
	}
	byName, _ := s.agentMaps()
	s.ensureHeartbeat(byName)
	log.Print("scheduled jobs: scheduler running")
	t := time.NewTicker(s.tickInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.TickNow()
		}
	}
}

// agentMaps resolves the registry fresh per tick into both directions:
// name→pubkey (legacy rows route by name) and pubkey→name (display names
// refresh, so a rename keeps the job working without a rewrite).
func (s *Scheduler) agentMaps() (byName, byPK map[string]string) {
	byName, byPK = map[string]string{}, map[string]string{}
	if s.Agents == nil {
		return byName, byPK
	}
	agents, err := s.Agents()
	if err != nil {
		log.Printf("scheduled jobs: registry unreadable, no fires this tick: %v", err)
		return byName, byPK
	}
	for _, a := range agents {
		byName[a.Name] = a.Pubkey
		if a.Pubkey != "" {
			byPK[a.Pubkey] = a.Name
		}
	}
	return byName, byPK
}

// TickNow runs one pass: fire due jobs, resolve open runs, record misses.
func (s *Scheduler) TickNow() {
	now := s.now()
	nowUnix := uint64(now.Unix())
	byName, byPK := s.agentMaps()
	s.ensureHeartbeat(byName)

	// The in-flight gate: an agent with an open run gets no second mention —
	// a busy pod would just drop it (the bot-not-responding failure mode).
	// Seeded from open runs, then updated AS this tick fires (two jobs for the
	// same agent, both due now, fire one). Keyed by ROUTING key (pubkey when
	// known, name otherwise) so a renamed agent keeps its gate.
	inflight := map[string]bool{}
	for _, j := range s.Jobs.Snapshot() {
		if open := j.OpenRun(); open != nil {
			inflight[j.routeKey()] = true
		}
	}

	for _, j := range s.Jobs.Snapshot() {
		if j.Paused {
			continue
		}
		// Resolve the agent: by stored pubkey (rename-proof) or, on legacy
		// rows, by stored name. Either way the row's display name refreshes.
		pk := j.AgentPubkey
		if pk == "" {
			pk = byName[j.Agent]
		} else if name, ok := byPK[pk]; ok {
			if name != j.Agent {
				_ = s.Jobs.Update(j.ID, func(j *Job) bool { j.Agent = name; return true })
				j.Agent = name
			}
		} else {
			pk = "" // pubkey-routed but the row is GONE (the agent was removed)
		}
		if pk == "" {
			// The agent row is gone (removed, not renamed): pause the job with
			// a recorded error instead of erroring every tick forever.
			_ = s.Jobs.recordRun(j.ID, JobRun{FiredAt: nowUnix, Status: RunError,
				Detail: "agent " + j.Agent + " is not in the registry — job paused"})
			_ = s.Jobs.SetPaused(j.ID, true)
			continue
		}
		route := pk
		if j.AgentPubkey == "" {
			route = j.Agent // legacy row: the gate keys on the name it stored
		}

		dueAt := j.NextRunAt
		if j.At != 0 {
			dueAt = j.At
		}
		if dueAt == 0 || dueAt > nowUnix {
			continue
		}

		// Catch-up window: a slot older than half the period (clamped) is a
		// recorded miss, not a stale fire — except one-shots, which re-queue.
		if j.At == 0 {
			_, period, perr := nextAfter(j.Cron, j.TZ, now.Add(-2*time.Second))
			if perr != nil {
				_ = s.Jobs.recordRun(j.ID, JobRun{FiredAt: nowUnix, Status: RunError, Detail: perr.Error()})
				_ = s.Jobs.SetPaused(j.ID, true)
				continue
			}
			window := int64(catchupWindow(period).Seconds())
			if int64(dueAt)+window < now.Unix() {
				s.advanceRecurring(j.ID, j.Cron, j.TZ, now)
				_ = s.Jobs.recordRun(j.ID, JobRun{FiredAt: dueAt, Status: RunMissed,
					Detail: "slot outside the catch-up window — skipped (never silently dropped)"})
				continue
			}
		}

		if inflight[route] {
			if j.At != 0 {
				// One-shot + busy agent: re-queue shortly rather than lose it.
				_ = s.Jobs.Update(j.ID, func(j *Job) bool {
					j.At = nowUnix + uint64(oneShotRetry.Seconds())
					return true
				})
				continue
			}
			_ = s.Jobs.Update(j.ID, func(j *Job) bool {
				s.advanceJobLocked(j, now)
				return true
			})
			_ = s.Jobs.recordRun(j.ID, JobRun{FiredAt: nowUnix, Status: RunSkipped,
				Detail: "agent " + j.Agent + " still has an open run — slot skipped"})
			continue
		}

		// Advance-before-dispatch: persist the new schedule + the open run
		// FIRST, so a crash between here and the post is at-most-once.
		next := uint64(0)
		if j.At == 0 {
			nt, _, err := nextAfter(j.Cron, j.TZ, now)
			if err != nil {
				_ = s.Jobs.recordRun(j.ID, JobRun{FiredAt: nowUnix, Status: RunError, Detail: err.Error()})
				_ = s.Jobs.SetPaused(j.ID, true)
				continue
			}
			next = uint64(nt.Unix())
		}
		oneShotDone := j.At != 0
		if err := s.Jobs.Update(j.ID, func(j *Job) bool {
			j.NextRunAt = next
			if oneShotDone {
				j.At = 0
			}
			j.Runs = append(j.Runs, JobRun{FiredAt: nowUnix, Status: RunFired})
			if len(j.Runs) > MaxJobRuns {
				j.Runs = j.Runs[len(j.Runs)-MaxJobRuns:]
			}
			return true
		}); err != nil {
			log.Printf("scheduled jobs: persist %s before dispatch failed: %v", j.ID, err)
			continue
		}
		inflight[route] = true

		// The fire lands in the world jobs channel; the job's own Channel is
		// where the agent cross-posts its visible update.
		jobsChannel := s.fireChannel(j)
		ownerName, self := s.ownerLabel(j.Owner, byPK)
		extra := [][]string{{"t", fireTag}}

		// Per-job anchor: each job's fires thread under one top-level message
		// so the channel reads as one thread per job. Created lazily on the
		// first fire; a failed anchor is logged and the fire goes top-level.
		anchor := j.Anchor
		if s.JobsChannel != "" && anchor == "" {
			ownerDesc := ownerName
			if self {
				ownerDesc += " (self-initiated)"
			}
			anchorContent := fmt.Sprintf("[scheduled job %q · %s]\n\nOwner: %s. Fires thread here.",
				jobLabel(j), j.ID, ownerDesc)
			anchorID, aerr := s.postID(jobsChannel, "", [][]string{{"t", fireTag}}, anchorContent)
			if aerr != nil {
				log.Printf("scheduled jobs: anchor for %s failed (fire goes top-level): %v", j.ID, aerr)
			} else if anchorID != "" {
				if uerr := s.Jobs.Update(j.ID, func(jj *Job) bool { jj.Anchor = anchorID; return true }); uerr != nil {
					log.Printf("scheduled jobs: persist anchor for %s failed: %v", j.ID, uerr)
				}
				anchor = anchorID
			}
		}
		if anchor != "" {
			extra = append(extra, []string{"e", anchor, "", "root"})
		}

		content := fmt.Sprintf("[scheduled job %q]\n\n%s", jobLabel(j), j.Prompt)
		if s.JobsChannel != "" {
			content = fireContent(j, ownerName, self)
		}
		if err := s.post(jobsChannel, pk, extra, content); err != nil {
			_ = s.Jobs.recordRun(j.ID, JobRun{FiredAt: nowUnix, Status: RunError,
				Detail: "fire failed: " + err.Error()})
			if oneShotDone {
				// A failed post provably never went out — re-queue the one-shot
				// instead of consuming it; five consecutive failures pause the
				// job (a permanently refusing channel must not hot-loop).
				_ = s.Jobs.Update(j.ID, func(j *Job) bool {
					j.Retry++
					if j.Retry >= 5 {
						j.Paused = true
						return true
					}
					j.At = nowUnix + uint64(oneShotRetry.Seconds())
					return true
				})
			}
			continue
		}
		log.Printf("scheduled jobs: fired %s (%s) at agent %s in %s", j.ID, jobLabel(j), j.Agent, jobsChannel)
	}

	s.resolveOpenRuns(now, byPK)
}

// advanceRecurring moves a recurring job's NextRunAt strictly past now
// (repeatedly, so a long outage advances through every stale slot at once).
func (s *Scheduler) advanceRecurring(id, cronExpr, tz string, now time.Time) {
	_ = s.Jobs.Update(id, func(j *Job) bool {
		s.advanceJobLocked(j, now)
		return true
	})
}

// advanceJobLocked computes the next occurrence (caller holds no lock; the
// JobsStore.Update provides it).
func (s *Scheduler) advanceJobLocked(j *Job, now time.Time) {
	nt, _, err := nextAfter(j.Cron, j.TZ, now)
	if err != nil {
		return // leave the slot; the next tick re-records the parse error
	}
	j.NextRunAt = uint64(nt.Unix())
}

// resolveOpenRuns closes fired runs: any agent message in the job's channel
// after the fire closes it ok; past the timeout it closes + notifies the owner.
// byPK is the fresh registry pubkey→name map — the reply check matches the
// job's stored routing pubkey (falling back to the stored name on legacy rows).
func (s *Scheduler) resolveOpenRuns(now time.Time, byPK map[string]string) {
	nowUnix := now.Unix()
	for _, j := range s.Jobs.Snapshot() {
		open := j.OpenRun()
		if open == nil {
			continue
		}
		firedAt := int64(open.FiredAt)
		agentPK := j.AgentPubkey
		if agentPK == "" {
			agentPK = byNameOf(byPK, j.Agent)
		}
		if nowUnix-firedAt < int64(runTimeout.Seconds()) {
			// Still inside the window — but an early reply closes it now.
			msgs, err := s.poll(s.fireChannel(j), firedAt)
			if err != nil {
				log.Printf("scheduled jobs: reply poll for %s failed: %v", j.ID, err)
				continue // transient; the next tick re-checks
			}
			if agentReplied(msgs, agentPK, firedAt) {
				_ = s.Jobs.Update(j.ID, func(j *Job) bool {
					if last := j.LastRun(); last != nil && last.Status == RunFired && int64(last.FiredAt) == firedAt {
						last.Status = RunOK
						j.Runs[len(j.Runs)-1] = *last
						return true
					}
					return false
				})
			}
			continue
		}
		// Timed out: close + best-effort failure note to the owner in-channel.
		_ = s.Jobs.Update(j.ID, func(j *Job) bool {
			if last := j.LastRun(); last != nil && last.Status == RunFired && int64(last.FiredAt) == firedAt {
				last.Status = RunTimeout
				last.Detail = fmt.Sprintf("no reply from %s within %s", j.Agent, runTimeout)
				j.Runs[len(j.Runs)-1] = *last
				return true
			}
			return false
		})
		// Never-woke notice: the scheduler detected the miss, so it is
		// console-signed into the jobs channel and pings the owner there (they
		// hold membership; the home channel is not where the console can
		// write). Legacy ownerless rows fall back to the operator.
		owner := j.Owner
		if owner == "" {
			owner = s.OperatorPub
		}
		if owner != "" {
			note := fmt.Sprintf("⚠️ Scheduled job %q — %s did not respond within %s (run timed out). The job stays scheduled; ask an agent to delete_job or pause_job %s if you want it stopped.",
				jobLabel(j), j.Agent, runTimeout, j.ID)
			if err := s.post(s.fireChannel(j), owner, [][]string{{"t", fireTag}}, note); err != nil {
				log.Printf("scheduled jobs: failure note for %s undeliverable: %v", j.ID, err)
			}
		}
	}
}

// agentReplied reports whether the agent pk posted in the channel after t.
func agentReplied(msgs []delegate.PollResult, agentPK string, after int64) bool {
	if agentPK == "" {
		return false
	}
	for _, m := range msgs {
		if m.Author == agentPK && m.CreatedAt > after {
			return true
		}
	}
	return false
}

// byNameOf is a reverse lookup on the registry's pubkey→name map.
func byNameOf(byPK map[string]string, name string) string {
	for pk, n := range byPK {
		if n == name {
			return pk
		}
	}
	return ""
}

func jobLabel(j Job) string {
	if j.Label != "" {
		return j.Label
	}
	return j.ID
}
