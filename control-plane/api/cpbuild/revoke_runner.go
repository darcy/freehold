package cpbuild

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"freehold/contract/relay"
	"freehold/contract/wire"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/api/cpstate"
	"freehold/control-plane/secret-management"
	"freehold/control-plane/state"
)

// revoke_runner — the CPA's take-away half of capability management, the
// counterpart of provision_runner. The operator says "the RTX3090 box is no
// longer yours to manage", the CPA calls this, and the capability is taken
// back: every grantee is dropped from the door's relay-signed roster (the same
// live write the re-provision's dropped-grantee path uses, so it lands without
// a restart), each affected pod's coords feed is cut and the pod re-applied,
// and — for a whole-door retirement — the channel is folded, the shipped
// credential erased, the unit stopped, and the record dropped under a
// retirement guard note.
//
// What the flow NEVER does is claim a teardown it could not check. Every leg
// reports its own verification: the roster is re-read from the relay (the runner
// reads that same signed 39002 per call, so it is the only definition of
// "revoked" that matters), the unit is asked whether it is still active and the
// door's port is dialled the same way the start path dials it, and the
// credential package is re-opened on disk. Anything unverifiable is named as
// such in the report: a revoked door that is still running is a known state,
// never a silent one.
//
// The channel is deliberately KEPT (folded, not deleted): the roster history and
// the door's kind-48001 audit stream ride it, and a revocation the auditor can
// no longer follow is not a revocation.

// revokeProbeWait is how long the door's port is given to stop answering after
// the unit stop, the mirror of startCapabilityRunner's readiness wait. Short on
// purpose: systemd tears the transient unit's cgroup down synchronously, so a
// listener still up after this is a real residue to report, not a race to
// paper over.
const revokeProbeWait = 3 * time.Second

// BuildRevokeRunner binds the revoke_runner flow to a Spec + the agent-tools
// registry. It mirrors BuildProvisionRunner's shape (same guards, same helpers,
// state-then-side-effects-then-report ordering) rather than sharing code with
// it: the two flows must agree exactly at the roster and the coords feed, and a
// revocation that quietly drifted from its sibling's helpers would drift
// precisely where the audit matters.
func BuildRevokeRunner(spec *Spec, reg *agenttools.Registry) agent.RevokeRunnerFn {
	create := BuildCreateAgentFn(spec)
	return func(args agent.RetireArgs) (string, error) {
		name := strings.TrimSpace(args.Name)
		if !runnerNameRe.MatchString(name) {
			return "", fmt.Errorf("revoke_runner: name must be kebab-case [a-z0-9-] (got %q) — <target>-<protocol>-<identity>", args.Name)
		}
		named := dedupNonBlank(args.RevokeFrom)

		// Name guards — the provision flow's, in the same words: the build's static
		// table and the per-zone DNS doors are operator-owned by construction, so
		// an agent may not reach them from here.
		for _, static := range capabilityRunners() {
			if static.name == name {
				return "", fmt.Errorf("revoke_runner %s: a build-time capability runner is operator-owned — it is retired by the build/teardown, never by an agent", name)
			}
		}
		if strings.HasPrefix(name, "cloudflare-api-") {
			return "", fmt.Errorf("revoke_runner %s: the cloudflare-api- prefix is reserved (per-zone DNS doors are operator-owned)", name)
		}

		cpState := spec.consoleStateDir()
		store, err := state.Open(cpState)
		if err != nil {
			return "", fmt.Errorf("open CP state: %w", err)
		}

		// The retirement guard is read FIRST, before the record's provenance: it is
		// the stronger statement about the name (a retired door's record is already
		// gone, so any check that leans on the record would answer "no record" for
		// what is actually "you took this away already"). An agent may not
		// re-revoke, and — checked the same way in provision_runner — may not
		// re-mint it either.
		if prior, retired := store.GetRetired(name); retired {
			return "", fmt.Errorf("revoke_runner %s: already retired (%s) — the name stays refused until the operator re-enables it by provisioning the door from the console", name, orUnknown(prior.RetiredBy))
		}

		// Ownership guards (the mirror of the provision side's): the agent surface
		// only ever takes away what the agent flow gave. A runner with no capability
		// record was provisioned by the console; a record with operator provenance
		// likewise. Both are the operator's to revoke.
		rec, hasRec := store.GetCapability(name)
		if !hasRec {
			return "", fmt.Errorf("revoke_runner %s: no capability record — a runner the build or the console provisioned is revoked by the operator (the console), and an agent may not revoke what it did not provision", name)
		}
		if !rec.AgentProvisioned() {
			return "", fmt.Errorf("revoke_runner %s: this door was provisioned by the operator — revoking it stays operator-scoped (the console)", name)
		}
		// The single-vs-whole decision reads the RAW argument, never the filtered
		// list. `revoke_from: [""]` filters down to nothing, and deciding on the
		// filtered list would read that as "no list given" — silently escalating a
		// malformed removal request into a WHOLE-DOOR RETIREMENT (roster cleared,
		// credential erased, record dropped, name refused to the agent surface).
		// A caller that asked to remove named agents must never get a teardown for
		// a blank entry, so an all-blank list is refused outright rather than
		// reinterpreted.
		single := len(args.RevokeFrom) > 0
		if single && len(named) == 0 {
			return "", fmt.Errorf("revoke_runner %s: revoke_from was given but names no agent (every entry is blank) — name the agents to take the door from, or pass an empty revoke_from to retire the door deliberately", name)
		}
		targets := []string{}
		if single {
			// A from-the-roster removal: only names actually on the roster are
			// targets. Naming nobody on it is a no-op that MUST NOT touch the door,
			// so it is reported as a no-op instead of falling through to the retire
			// legs (which would take the door down for a typo).
			on := map[string]bool{}
			for _, g := range rec.Rosters {
				on[g] = true
			}
			for _, g := range named {
				if on[g] {
					targets = append(targets, g)
				}
			}
			if len(targets) == 0 {
				return renderRevoke(revokeReport{
					name:         name,
					single:       true,
					noop:         true,
					grantees:     named,
					auditChannel: relay.RunnerChannelID(rpkOrEmpty(store, cpState, name)),
					outcomes: []revokeOutcome{{step: "roster", ok: true, detail: fmt.Sprintf(
						"nothing to take: [%s] is not on %s's roster (it holds [%s])", strings.Join(named, ", "), name, orNone(rec.Rosters))}},
					verdict: fmt.Sprintf("revoke_runner %s: the named agents hold no grant on this door — nothing was changed, and the door keeps serving [%s]", name, orNone(rec.Rosters)),
				}), nil
			}
		} else {
			targets = append(targets, rec.Rosters...)
		}

		// Resolve every target's pubkey BEFORE any write: a grantee whose registry
		// row is gone cannot be revoked by pubkey, and that has to surface as a loud
		// gap in the report rather than a silent skip (its exec would otherwise
		// linger unmentioned, which is exactly the hole this tool exists to close).
		pubkeys := map[string]string{}
		orphaned := []string{}
		for _, g := range targets {
			row, err := registryRow(reg, g)
			if err != nil || row.Pubkey == "" {
				orphaned = append(orphaned, g)
				continue
			}
			pubkeys[g] = row.Pubkey
		}

		runnerPK := rpkOrEmpty(store, cpState, name)
		rep := revokeReport{name: name, single: single, grantees: targets, auditChannel: relay.RunnerChannelID(runnerPK)}
		dial, auth := spec.relayDialURL(), spec.relaySignURL()
		secret, secErr := cpstate.ConsoleSecret(cpState)

		// 1) Roster — the LIVE revocation. Drop each pubkey from the door's
		//    channel, then read the roster back: the relay's signed 39002 is what
		//    the runner re-reads per call, so that read, not the call's intent, is
		//    what "revoked" means.
		switch {
		case runnerPK == "":
			rep.outcomes = append(rep.outcomes, revokeOutcome{step: "roster", detail: fmt.Sprintf(
				"the door has no recorded relay identity, so its roster could not be written — every grantee may still hold exec through it; report this as UNREVOKED to the operator")})
		case secErr != nil:
			rep.outcomes = append(rep.outcomes, revokeOutcome{step: "roster", detail: fmt.Sprintf(
				"the console's channel-owner credential is unavailable (%v) — only the channel owner may sign a remove-user, so the roster could not be written and every grantee may still hold exec", secErr)})
		default:
			var failed []string
			for _, g := range targets {
				// An unresolved grantee has no pubkey to remove: a remove-user
				// carrying "" is not a revocation this call can honestly issue (the
				// relay's reading of it is undefined — it may no-op or error), and a
				// silent no-op is precisely what would let the leg below print a
				// clean mark over a still-live grant. rosterOutcome marks the leg
				// UNVERIFIED for exactly these names.
				if pubkeys[g] == "" {
					continue
				}
				if err := provisioner.RemoveUserMembership(store, dial, auth, name, pubkeys[g], cpState); err != nil {
					failed = append(failed, fmt.Sprintf("%s: %v", g, err))
				}
			}
			have, qerr := relay.QueryChannelRoster(dial, spec.RelayPK, runnerPK, secret)
			rep.outcomes = append(rep.outcomes, rosterOutcome(single, targets, pubkeys, runnerPK, failed, have, qerr))
		}

		// The capability record's roster is the BUILD-TIME source of both the
		// grants and every pod's coords feed (dynamicRunners + agentRunnerCoords
		// read it), so a from-the-roster removal that only wrote the relay would be
		// silently undone by the next build: the named agents must leave the record
		// as well. A failure here is fatal rather than reported-and-continued — a
		// half-removal the next build quietly reverses is worse than an error.
		if single {
			updated := rec
			updated.Rosters = without(rec.Rosters, targets)
			if err := store.InsertCapability(name, updated); err != nil {
				// The relay removal has landed; the report of it must not be
				// dropped with the error, so failLoud renders it into the error text.
				return failLoud(rep, "revoke_runner %s: drop [%s] from the door's recorded roster FAILED (%v) — the relay removal landed; leaving the record as it was would re-grant them on the next build",
					name, strings.Join(targets, ", "), err)
			}
		}

		// 2) Coords — cut the door out of each affected pod's coords feed and
		//    re-apply the pod. The provision flow makes this process's map
		//    authoritative from state before re-applying so a pod carries every
		//    runner it should and not just the delta; the revocation does the same
		//    in the other direction.
		cut := []string{}
		unverified := []string{}
		for _, g := range targets {
			row, rerr := registryRow(reg, g)
			if rerr != nil {
				unverified = append(unverified, g)
				cut = append(cut, fmt.Sprintf("%s: no longer in the registry — its pod was NOT re-applied, so its coords feed may still name the door (the roster gate is what denies it); an operator must clear its row", g))
				continue
			}
			// Re-resolve the pod's whole coords feed FROM STATE, then cut the door
			// explicitly: the re-apply renders the department's entire
			// FREEHOLD_RUNNER_* set out of this map, so a feed that merely reflects
			// what this call touched would strip the department's other doors. The
			// map is lazily initialised because in the CP's serve process only a
			// build's stageDepartmentRunners populates it — skipping it when nil
			// would hand create() an empty feed, which is the bug. The explicit cut
			// stays: on the retire path the record is still present here (it leaves
			// only after the teardown legs), so the roster it mirrors is behind the
			// truth this call just wrote to the relay.
			if spec.DepartmentRunners == nil {
				spec.DepartmentRunners = map[string][]agent.RunnerCoords{}
			}
			spec.DepartmentRunners[g] = cutCoord(spec.agentRunnerCoords(store, g), name)
			channels, private := reconciledChannels(row)
			if _, err := create(g, row.Purpose, channels, private); err != nil {
				unverified = append(unverified, g)
				cut = append(cut, fmt.Sprintf("%s: pod re-apply FAILED (%v) — it may still carry the revoked door in its env until the next build", g, err))
				continue
			}
			cut = append(cut, fmt.Sprintf("%s: re-applied, coords feed now [%s]", g, orNone(coordTargets(spec, g))))
		}
		if len(cut) == 0 {
			cut = []string{fmt.Sprintf("nothing to cut — [%s] held no coords to lose", orNone(targets))}
		}
		sort.Strings(cut)
		rep.outcomes = append(rep.outcomes, revokeOutcome{step: "coords", ok: len(unverified) == 0, detail: strings.Join(cut, "; ")})

		if single {
			// A from-the-roster removal stops here BY DESIGN: the door itself keeps
			// serving whoever remains, so its unit, its channel and its credential
			// are none of this call's business.
			rep.outcomes = append(rep.outcomes, revokeOutcome{step: "door", ok: true, detail: fmt.Sprintf(
				"untouched by design: a from-the-roster removal leaves %s standing for its remaining grantees [%s]", name, orNone(without(rec.Rosters, targets)))})
		} else {
			// 3) Whole-door retirement: fold the channel, erase the credential, stop
			//    the unit, drop the record — each verified on its own terms.
			if _, hasRow := store.GetRunner(name); !hasRow {
				// A door recorded but never provisioned (a half-finished earlier
				// provision). The state says nothing was stood up, but the state is
				// not evidence: a package can exist under this name even with no
				// runner row, because rpkOrEmpty falls back to the on-disk CP state.
				// So the legs still RUN rather than being asserted clean — the fold is
				// attempted with whatever identity resolved, and the package is
				// checked on disk — and only a check that finds nothing is allowed to
				// report "nothing to fold / nothing to erase".
				if runnerPK == "" {
					rep.outcomes = append(rep.outcomes, revokeOutcome{step: "channel", ok: false, detail: fmt.Sprintf(
						"no runner row and no resolvable relay identity — nothing could be folded and it cannot be established that no channel %s was ever opened; an operator must check the relay", rep.auditChannel)})
				} else if secErr != nil {
					rep.outcomes = append(rep.outcomes, revokeOutcome{step: "channel", ok: false, detail: fmt.Sprintf(
						"no runner row and the console's channel-owner credential is unavailable (%v) — the fold of %s could not be attempted, so its existence is UNKNOWN", secErr, rep.auditChannel)})
				} else if err := provisioner.RevokeRunnerChannel(store, dial, auth, name, cpState); err != nil {
					rep.outcomes = append(rep.outcomes, revokeOutcome{step: "channel", ok: false, detail: fmt.Sprintf(
						"no runner row is recorded under this name, and the attempted fold of %s reported (%v) — treat a channel as possibly live and have an operator check it", rep.auditChannel, err)})
				} else {
					rep.outcomes = append(rep.outcomes, revokeOutcome{step: "channel", ok: true, detail: fmt.Sprintf(
						"folded from the identity the state still held: %s is off its own roster and its meta is republished as revoked; the channel is KEPT so the roster history and the door's audit stream stay readable", rep.auditChannel)})
				}
				rep.outcomes = append(rep.outcomes, credentialOutcome(runnerPackageDir(cpState, name)))
			} else {
				if err := provisioner.RevokeRunnerChannel(store, dial, auth, name, cpState); err != nil {
					rep.outcomes = append(rep.outcomes, revokeOutcome{step: "channel", ok: false, detail: fmt.Sprintf(
						"the channel fold did NOT complete (%v) — the door's roster write surface may still answer; the channel %s is kept either way", err, rep.auditChannel)})
				} else {
					rep.outcomes = append(rep.outcomes, revokeOutcome{step: "channel", ok: true, detail: fmt.Sprintf(
						"folded: the runner is off its own roster and its meta is republished as revoked; the channel %s is KEPT so the roster history and the door's audit stream stay readable", rep.auditChannel)})
				}
				// The credential: the audited revoke verb flips the status AND drops the
				// shipped secrets.json, so the CP no longer holds a copy the door could
				// serve. Re-opened below to prove it.
				if _, err := provisioner.RevokeRunner(store, name); err != nil {
					rep.outcomes = append(rep.outcomes, revokeOutcome{step: "credential", ok: false, detail: fmt.Sprintf(
						"the CP's revoke of the runner FAILED (%v) — treat its credential as still shippable", err)})
				} else {
					rep.outcomes = append(rep.outcomes, credentialOutcome(runnerPackageDir(cpState, name)))
				}
			}

			// The unit. Only where freehold hosted it — the same condition
			// stageDepartmentRunners requires to stand one up — is "it is down" a
			// claim rather than a hope, and a state-only call is a leg that verified
			// NOTHING, never a verified leg.
			if spec.CpLxc == 0 || spec.CpIP == "" || spec.RelayHost == "" || spec.RunnerTarget == "" || spec.RelayPK == "" {
				rep.outcomes = append(rep.outcomes, revokeOutcome{step: "unit", ok: false, detail: fmt.Sprintf(
					"state-only: this build has no substrate/relay wiring, so freehold never hosted this door's unit — no stop was made and NOTHING was verified about where it actually runs; wherever that is, the door is now unauthorized and MUST NOT be trusted to answer")})
			} else {
				rep.hosted = true
				// The department-named legacy unit is in play ONLY where this door's
				// own roster implicates it (retireRunner's second stop exists because
				// the data department's door was renamed): stopping it for an
				// unrelated door took down a capability nobody asked about.
				legacy := ""
				if containsRoster(rec.Rosters, legacyDeptName) {
					legacy = "freehold-runner-" + legacyDeptName
				}
				rep.outcomes = append(rep.outcomes, unitOutcomes(name, rec.Port, legacy)...)
			}

			if err := store.RemoveCapability(name); err != nil {
				// Side effects have already landed (the fold, the erase, the stop):
				// failLoud keeps their report in the error text instead of letting
				// the serve drop it.
				return failLoud(rep, "revoke_runner %s: drop the capability record FAILED (%v) — the fold, the credential erase and the unit stop already ran; read the legs below for what completed", name, err)
			}
			rep.outcomes = append(rep.outcomes, revokeOutcome{step: "record", ok: true, detail: fmt.Sprintf(
				"removed — %s is no longer re-staged by any build and no longer resolves into any agent's coords feed", name)})
			if err := store.InsertRetired(name, state.RetiredCapability{
				RevokedAt:  uint64(time.Now().Unix()),
				RetiredBy:  state.OriginAgent,
				LastRoster: append([]string(nil), rec.Rosters...),
			}); err != nil {
				// The record is already gone but the guard note did not land: the
				// name is re-mintable until this is fixed, so the report must say so.
				return failLoud(rep, "revoke_runner %s: the capability record is DROPPED and the retirement guard note did NOT land (%v) — the name is re-mintable until this is fixed", name, err)
			}
		}
		// 4) Follow-ups someone else owns, each addressed to whoever can do it.
		rep.notes = append(rep.notes, fmt.Sprintf(
			"the door's channel %s stays live and read-only: its roster history and its kind-48001 audit stream remain queryable, so the revocation stays auditable — do not delete it", rep.auditChannel))
		if len(orphaned) > 0 {
			rep.notes = append(rep.notes, fmt.Sprintf(
				"[%s] %s no longer in the agent registry, so their roster entries could not be revoked by pubkey — their exec may linger until an operator clears the roster from the console (the console signs the same remove-user). Ask the operator; never the agent itself",
				strings.Join(orphaned, ", "), isAre(orphaned)))
		}
		if !single {
			rep.notes = append(rep.notes, fmt.Sprintf(
				"the door's substrate credential stays authorized on its target (%s). Dropping it is a Compute-domain action — the same fold the build's teardown does (its authorized_keys line + the deploy/known_hosts entry); hand %s to Compute instead of reaching for it yourself",
				orNone([]string{rec.Address}), name))
		}
		if extra := stillHeld(store, targets, name); extra != "" {
			rep.notes = append(rep.notes, "other doors the affected agents legitimately keep: "+extra+" — revoked doors are gone from their coords feed; these remain theirs")
		}
		if !single && !rep.hosted {
			// The box-hosted / resident case: freehold never started this process, so
			// whoever hosts it closes it. It is NOT handed to the agent whose access
			// was just taken — asking that agent to tear down the door you just
			// removed is both a privilege it may no longer hold and an audit lie.
			rep.notes = append(rep.notes, fmt.Sprintf(
				"box-side close-out (this door was not hosted by freehold here — it runs on a box the build does not drive, e.g. a VPS): the OPERATOR on that box runs "+
					"(1) systemctl stop && disable freehold-runner-%s, then remove its unit file; "+
					"(2) rm -rf %s — the sealed package holds the door's runner keys; "+
					"(3) verify: systemctl is-active freehold-runner-%s reports inactive AND nothing answers on %s. "+
					"Report the result back: a revoked door that is still running is a known state, never a silent one. Do not ask the agent that just lost the door to run this",
				name, runnerPackageDir(cpState, name), name, fmt.Sprint(rec.Port)))
		}

		rep.verdict = revokeVerdict(name, single, targets, rep)
		return renderRevoke(rep), nil
	}
}

// revokeOutcome is one leg's verified result. ok is true ONLY where the leg's own
// verification passed; an unverified leg is reported with ok false so neither the
// agent nor the operator can read intent as fact.
type revokeOutcome struct {
	step   string
	ok     bool
	detail string
}

// revokeReport is the structured truth of one call.
type revokeReport struct {
	name   string
	single bool
	// noop marks the nothing-to-take outcome: the header must then say UNCHANGED,
	// because the single header's "removed from the roster of" would open a report
	// whose every line says nothing was touched.
	noop bool
	// failed marks a mid-flight abort: the report is then rendered into the
	// error text (the serve drops the text half when an error comes back), and
	// its header must not read as a completed removal or retirement.
	failed       bool
	grantees     []string
	outcomes     []revokeOutcome
	notes        []string
	auditChannel string
	hosted       bool
	verdict      string
}

// failLoud aborts the flow with the report preserved: the serve path
// (textResult) discards the text half whenever an error is present, so a bare
// `return "", err` after irreversible legs have run (a roster removal, a
// channel fold, a credential erase, a unit stop) would leave the caller — and
// the audit — with only "it failed" and no record of what did change. The
// report rides inside the error instead.
func failLoud(rep revokeReport, format string, args ...any) (string, error) {
	rep.failed = true
	rep.verdict = fmt.Sprintf(format, args...)
	return "", fmt.Errorf("%s\n%s", rep.verdict, renderRevoke(rep))
}

// rosterOutcome judges the revocation against the roster the relay actually
// holds now. A read failure is NEVER folded into success, and a target whose
// pubkey could not be resolved NEVER counts as verified: it was not removable by
// this call, so its grant may still be live and the leg must say so rather than
// print a clean mark the note below then contradicts.
//
// The whole-door leg does not claim the roster is EMPTY: the runner itself is a
// member of its own channel (provisioner.SyncRunnerChannel members rpk twice, as
// channel owner and as member), and the fold that removes it runs later than this
// read-back — so emptiness is not establishable here, and claiming it beside a
// printed non-zero count was a self-contradiction. What IS establishable, and is
// what revocation means, is that no AGENT identity remains on it.
func rosterOutcome(single bool, targets []string, pubkeys map[string]string, runnerPK string, failed []string, have []string, qerr error) revokeOutcome {
	if qerr != nil {
		return revokeOutcome{step: "roster", ok: false, detail: fmt.Sprintf(
			"the removals were issued but the roster could NOT be read back to confirm them (%v) — do not claim the door is gone from [%s] until its roster is re-read%s",
			qerr, orNone(targets), joinFailures(failed))}
	}
	present := map[string]bool{}
	for _, pk := range have {
		present[pk] = true
	}
	// Every target had to resolve to a pubkey to be revocable at all; an
	// unresolved one is an open hole, counted separately so it can never be
	// absorbed into a verified reading.
	var unresolved []string
	for _, g := range targets {
		if pubkeys[g] == "" {
			unresolved = append(unresolved, g)
		}
	}
	var still []string
	for _, g := range targets {
		if pk := pubkeys[g]; pk != "" && present[pk] {
			still = append(still, g)
		}
	}
	// Who else is on the roster beyond the agents this call set out: the runner's
	// own membership is the expected one, anything else is a grant this flow did
	// not account for and must not be reported as a clean roster.
	unknown := []string{}
	for _, pk := range have {
		if pk == runnerPK || pk == "" {
			continue
		}
		known := false
		for _, g := range targets {
			if pubkeys[g] == pk {
				known = true
				break
			}
		}
		if !known {
			unknown = append(unknown, pk)
		}
	}
	switch {
	case len(still) > 0:
		return revokeOutcome{step: "roster", ok: false, detail: fmt.Sprintf(
			"the relay roster STILL lists [%s] (%d entr%s total) — the remove-user did not land; the door is NOT taken from them%s",
			strings.Join(still, ", "), len(have), pluralize(len(have)), joinFailures(failed))}
	case len(unresolved) > 0:
		return revokeOutcome{step: "roster", ok: false, detail: fmt.Sprintf(
			"[%s] could not be revoked by pubkey (their registry row is gone) — the roster reads back without the identities this call held, which says NOTHING about them: treat them as still holding exec until an operator clears the roster from the console%s",
			strings.Join(unresolved, ", "), joinFailures(failed))}
	case len(failed) > 0:
		return revokeOutcome{step: "roster", ok: false, detail: fmt.Sprintf(
			"the roster reads back without the targets (%d entr%s) but %d removal%s reported a failure — treat as UNCONFIRMED%s",
			len(have), pluralize(len(have)), len(failed), sIf(len(failed)), joinFailures(failed))}
	case single:
		return revokeOutcome{step: "roster", ok: true, detail: fmt.Sprintf(
			"verified against the relay-signed roster: [%s] is off it (%d entr%s remain, the door's other grantees and its own identity among them)",
			strings.Join(targets, ", "), len(have), pluralize(len(have)))}
	case len(unknown) > 0:
		// Whole-door ONLY: a from-the-roster removal leaves the door's other
		// grantees standing, so entries beyond the targets are its normal state —
		// but a RETIRED door's roster must hold nothing but its own runner
		// identity (which the fold below removes), and anything else is a grant
		// this record no longer accounts for.
		return revokeOutcome{step: "roster", ok: false, detail: fmt.Sprintf(
			"[%s] are off the roster, but it still lists %d entr%s this call did not account for (%s) — the roster is NOT clear, so the door keeps a holder the record does not name; have an operator read the roster and close them out",
			orNone(targets), len(unknown), pluralize(len(unknown)), strings.Join(unknown, ", "))}
	default:
		return revokeOutcome{step: "roster", ok: true, detail: fmt.Sprintf(
			"verified against the relay-signed roster: no agent identity is on it any more — every grantee is off it (%d entr%s left, the door's own membership)",
			len(have), pluralize(len(have)))}
	}
}

// credentialOutcome re-opens the sealed package to prove the CP no longer holds
// a copy of the credential. Absence is established from the FILESYSTEM, not from
// a load error: wire.Load fails for a truncated, corrupt, unparseable or
// unreadable file exactly as it fails for a missing one, and reading any error as
// "the credential is gone" would report a still-shippable secret as erased — the
// one mistake this leg cannot be allowed to make. A load that fails for a reason
// other than absence is reported UNVERIFIED with the error, because the bytes are
// plainly still there.
func credentialOutcome(pkgDir string) revokeOutcome {
	if _, err := os.Stat(pkgDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return revokeOutcome{step: "credential", ok: false, detail: fmt.Sprintf(
			"the package dir %s could not be inspected (%v) — its credential status is UNKNOWN, not erased", pkgDir, err)}
	}
	if _, err := os.Stat(filepath.Join(pkgDir, wire.SECRETS_FILE)); errors.Is(err, os.ErrNotExist) {
		return revokeOutcome{step: "credential", ok: true, detail: fmt.Sprintf(
			"no sealed package exists at %s (checked on disk) — the CP holds no copy of the credential for this door", pkgDir)}
	} else if err != nil {
		return revokeOutcome{step: "credential", ok: false, detail: fmt.Sprintf(
			"the sealed package at %s could not be inspected (%v) — its credential status is UNKNOWN, not erased", pkgDir, err)}
	}
	if _, err := wire.Load(pkgDir); err == nil {
		return revokeOutcome{step: "credential", ok: false, detail: fmt.Sprintf(
			"the sealed package at %s is STILL READABLE after the revoke — treat the credential as live and close it by hand", pkgDir)}
	} else if !errors.Is(err, os.ErrNotExist) {
		return revokeOutcome{step: "credential", ok: false, detail: fmt.Sprintf(
			"the sealed package at %s exists on disk but could not be opened (%v) — the file is PRESENT, so the credential must be treated as live and closed by hand", pkgDir, err)}
	}
	return revokeOutcome{step: "credential", ok: true, detail: fmt.Sprintf(
		"the sealed package at %s is gone — the CP holds no copy of the credential any more", pkgDir)}
}

// unitOutcomes stops the transient unit(s) and VERIFIES each stop on its own leg.
// The CP executor's own exec path is the CP guest — that is how
// startCapabilityRunner gets the unit up with systemctl/systemd-run — so the
// teardown asks the same box the same way, without routing a root exec through a
// runner: this flow is revoking exec, so it must not depend on holding any.
//
// The department-named legacy unit (retireRunner's second stop, for worlds built
// before the runner-named convention) is stopped ONLY where this door's own roster
// implicates that department. Stopping `freehold-runner-data` on the way to
// retiring an unrelated GPU door took out a capability nobody asked about, and
// verified only the door-named unit while it did — a side effect that belonged in
// no report. When it IS in play it gets its own leg, so the stop is visible.
func unitOutcomes(name string, port int, legacyUnit string) []revokeOutcome {
	unit := "freehold-runner-" + name
	script := fmt.Sprintf("systemctl stop %s 2>/dev/null; systemctl reset-failed %s 2>/dev/null; true", unit, unit)
	stopOut, _ := exec.Command("sh", "-c", script).CombinedOutput()
	// systemd releases the transient unit's cgroup synchronously, so a short settle
	// is the whole grace the probe needs: longer than that is a real residue.
	time.Sleep(revokeProbeWait)
	active := systemctlActive(unit)
	if failedState(active) {
		return []revokeOutcome{{step: "unit", ok: false, detail: fmt.Sprintf(
			"%s reports %q after the stop (stop output: %s) — the door is revoked and unauthorized, which is the security answer, but a running unit is a known state, not a clean one",
			unit, active, firstLine(string(stopOut)))}}
	}
	// The listener is the second, independent witness: a process that outlived its
	// transient unit (a wedged child, a re-exec) still holds the port. Dialled
	// exactly the way startCapabilityRunner waits for it to come up.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if c, derr := net.DialTimeout("tcp", addr, 2*time.Second); derr == nil {
		_ = c.Close()
		return []revokeOutcome{{step: "unit", ok: false, detail: fmt.Sprintf(
			"%s is %s yet %s STILL ANSWERS — the door is unauthorized, NOT stopped-and-verified", unit, active, addr)}}
	}
	legs := []revokeOutcome{{step: "unit", ok: true, detail: fmt.Sprintf(
		"%s is %s and nothing answers on %s — verified down after the stop", unit, active, addr)}}
	if legacyUnit == "" {
		return legs
	}
	legacyOut, _ := exec.Command("sh", "-c", fmt.Sprintf(
		"systemctl stop %s 2>/dev/null; systemctl reset-failed %s 2>/dev/null; true", legacyUnit, legacyUnit)).CombinedOutput()
	legacyActive := systemctlActive(legacyUnit)
	switch {
	case failedState(legacyActive):
		legs = append(legs, revokeOutcome{step: "legacy unit", ok: false, detail: fmt.Sprintf(
			"%s (the department-named unit from before the runner-named convention) still reports %q after the stop (output: %s) — a second door of the %s department is up; close it by hand",
			legacyUnit, legacyActive, firstLine(string(legacyOut)), legacyDeptName)})
	case legacyActive == "unknown":
		// Observed absence, not an asserted one: is-active was asked and the unit is
		// simply not there on this box.
		legs = append(legs, revokeOutcome{step: "legacy unit", ok: true, detail: fmt.Sprintf(
			"%s is not present on this box (is-active reports %q) — the door is the only unit this flow had to close", legacyUnit, legacyActive)})
	default:
		legs = append(legs, revokeOutcome{step: "legacy unit", ok: true, detail: fmt.Sprintf(
			"%s stopped and now reports %s — the %s department's legacy unit is down too", legacyUnit, legacyActive, legacyDeptName)})
	}
	return legs
}

// legacyDeptName is the department whose pre-convention unit name has to be
// considered alongside the door's own (retireRunner's second stop).
const legacyDeptName = "data"

func systemctlActive(unit string) string {
	raw, _ := exec.Command("sh", "-c", fmt.Sprintf("systemctl is-active %s 2>/dev/null || true", unit)).CombinedOutput()
	active := strings.TrimSpace(string(raw))
	if active == "" {
		active = "unknown"
	}
	return active
}

// failedState is any state in which the unit is still a live thing on the box.
// "unknown" (no systemd, or a unit that was never loaded) is NOT one: nothing is
// running under it.
func failedState(active string) bool {
	switch active {
	case "active", "activating", "reloading":
		return true
	}
	return false
}

// renderRevoke turns the structured report into what the agent reads and relays.
// The order is fixed (steps, then follow-ups, then the verdict) so the operator's
// eye finds the verdict in the same place every time. The header is driven by what
// the call ACTUALLY did — a no-op must not open with a sentence claiming a removal,
// since a verbatim-relayed report whose first line is wrong is a misreport, not a
// summary.
func renderRevoke(r revokeReport) string {
	var b strings.Builder
	switch {
	case r.failed:
		fmt.Fprintf(&b, "revoke_runner: %s FAILED — the call stopped mid-flight; the legs below are what actually completed\n", r.name)
	case r.noop:
		fmt.Fprintf(&b, "revoke_runner: %s UNCHANGED — nothing was taken from [%s]\n", r.name, orNone(r.grantees))
	case r.single:
		// Subject and object: the AGENTS were removed from the DOOR's roster —
		// the door keeps serving whoever remains. The phrasing before read the
		// other way round, and the agent relays this line verbatim.
		fmt.Fprintf(&b, "revoke_runner: [%s] removed from the roster of %s\n", orNone(r.grantees), r.name)
	default:
		fmt.Fprintf(&b, "revoke_runner: capability door %s retired (its roster held [%s])\n", r.name, orNone(r.grantees))
	}
	for _, o := range r.outcomes {
		mark := "verified"
		if !o.ok {
			mark = "UNVERIFIED"
		}
		fmt.Fprintf(&b, "  [%s] %s: %s\n", mark, o.step, o.detail)
	}
	for _, n := range r.notes {
		fmt.Fprintf(&b, "  -> %s\n", n)
	}
	fmt.Fprintf(&b, "verdict: %s\n", r.verdict)
	return b.String()
}

func revokeVerdict(name string, single bool, targets []string, r revokeReport) string {
	bad := map[string]bool{}
	var badList []string
	for _, o := range r.outcomes {
		if !o.ok {
			if !bad[o.step] {
				bad[o.step] = true
				badList = append(badList, o.step)
			}
		}
	}
	sort.Strings(badList)
	who := orNone(targets)
	switch {
	case single && len(badList) == 0:
		return fmt.Sprintf("%s is out of [%s]'s hands (verified against the relay roster) and out of their pod's coords feed; the door itself stands for whoever remains on its roster", name, who)
	case single:
		return fmt.Sprintf("%s: the removal from [%s] was issued but is NOT fully verified (%s) — do not tell the operator it is gone until those legs report clean", name, who, strings.Join(badList, ", "))
	case len(badList) == 0:
		// Every sentence here has a leg that checked it. The unit is claimed down
		// ONLY where this build hosted it (the state-only leg ran no stop, asked no
		// is-active, dialled no port), so the clean verdict must not say otherwise.
		unit := "its unit is verified down"
		if !r.hosted {
			unit = "its unit was not hosted by this build, so no stop was made or verified (see the unit leg)"
		}
		return fmt.Sprintf("%s is retired: its roster is verified clear of every grantee, [%s] is out of their hands, its credential is erased from the CP, %s, and its name is refused to the agent surface until the operator re-enables it from the console", name, who, unit)
	default:
		// Only claim what ran: the record removal and the name guard are the last
		// two writes and this branch is reached only after them, but the roster WRITE
		// may have been impossible (no relay identity / no console credential), and
		// a report that claims a write its own leg just said could not be made is a
		// self-contradiction the agent would relay verbatim.
		ran := "the record removal and the name guard ran"
		if !bad["roster"] {
			ran = "the roster write, " + ran
		}
		return fmt.Sprintf("%s is retired in state — %s — but these did NOT verify: %s. A revoked door that is still running is a known state, never a silent one: say what is unverified and let the operator close it", name, ran, strings.Join(badList, ", "))
	}
}

// cutCoord drops one target's entry from a pod's coord list. The bridge routes
// exec by Target, so the name is the key to match.
func cutCoord(coords []agent.RunnerCoords, target string) []agent.RunnerCoords {
	out := coords[:0]
	for _, c := range coords {
		if c.Target == target {
			continue
		}
		out = append(out, c)
	}
	return out
}

// coordTargets names what a pod's coords feed holds now, for the report.
func coordTargets(spec *Spec, name string) []string {
	if spec.DepartmentRunners == nil {
		return nil
	}
	out := []string{}
	for _, c := range spec.DepartmentRunners[name] {
		out = append(out, c.Target)
	}
	return out
}

// stillHeld names the OTHER recorded capability doors the affected agents keep,
// so the report can never be read as "they have nothing". The capability table is
// the authoritative "what is a door" set and each record's rosters say who holds
// it; the static build-time table is deliberately not named here because those
// doors' grants are not agent-issued and listing them would imply otherwise.
func stillHeld(store *state.StateStore, targets []string, retired string) string {
	if len(targets) == 0 {
		return ""
	}
	held := []string{}
	for nm, rec := range store.Capabilities() {
		if nm == retired {
			continue
		}
		for _, g := range targets {
			if containsRoster(rec.Rosters, g) {
				held = append(held, nm)
				break
			}
		}
	}
	sort.Strings(held)
	return strings.Join(held, ", ")
}

// rpkOrEmpty resolves a runner's relay (Nostr) pubkey without failing the call:
// the report says plainly when it is missing, because that is precisely the case
// where the revocation could not be performed.
func rpkOrEmpty(store *state.StateStore, cpState, name string) string {
	if r, ok := store.GetRunner(name); ok && r.NostrPubkey != "" {
		return r.NostrPubkey
	}
	if pk, err := cpstate.RunnerNostrPubkey(cpState, name); err == nil {
		return pk
	}
	return ""
}

func without(all, drop []string) []string {
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	out := []string{}
	for _, a := range all {
		if !skip[a] {
			out = append(out, a)
		}
	}
	return out
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "(none)"
	}
	return strings.Join(xs, ", ")
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "an earlier retirement"
	}
	return "by " + s
}

func joinFailures(failed []string) string {
	if len(failed) == 0 {
		return ""
	}
	return "; failures: " + strings.Join(failed, "; ")
}

func pluralize(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func sIf(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func isAre(xs []string) string {
	if len(xs) == 1 {
		return "is"
	}
	return "are"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
