package cpbuild

import (
	"fmt"
	"net"
	"os/exec"
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
		single := len(named) > 0
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
				if err := provisioner.RemoveUserMembership(store, dial, auth, name, pubkeys[g], cpState); err != nil {
					failed = append(failed, fmt.Sprintf("%s: %v", g, err))
				}
			}
			have, qerr := relay.QueryChannelRoster(dial, spec.RelayPK, runnerPK, secret)
			rep.outcomes = append(rep.outcomes, rosterOutcome(single, targets, pubkeys, failed, have, qerr))
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
				return "", fmt.Errorf("revoke_runner %s: drop [%s] from the door's recorded roster: %w (the relay removal landed; leaving the record as it was would re-grant them on the next build)", name, strings.Join(targets, ", "), err)
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
				// provision): there is nothing to fold, and saying so beats the
				// "runner not found" error the fold would return.
				rep.outcomes = append(rep.outcomes,
					revokeOutcome{step: "channel", ok: true, detail: fmt.Sprintf(
						"no runner was ever provisioned under this name, so no channel %s came up to fold — the door existed only as a capability record", rep.auditChannel)},
					revokeOutcome{step: "credential", ok: true, detail: fmt.Sprintf(
						"no sealed package exists at %s — the CP holds no credential copy of this door", runnerPackageDir(cpState, name))})
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
			// claim rather than a hope.
			if spec.CpLxc == 0 || spec.CpIP == "" || spec.RelayHost == "" || spec.RunnerTarget == "" || spec.RelayPK == "" {
				rep.outcomes = append(rep.outcomes, revokeOutcome{step: "unit", ok: true, detail: fmt.Sprintf(
					"state-only: this build has no substrate/relay wiring, so freehold never hosted this door's unit — wherever it runs it is now unauthorized and unserved, and MUST NOT be trusted to answer")})
			} else {
				rep.hosted = true
				rep.outcomes = append(rep.outcomes, unitOutcome(name, rec.Port))
			}

			if err := store.RemoveCapability(name); err != nil {
				return "", fmt.Errorf("revoke_runner %s: drop the capability record: %w", name, err)
			}
			rep.outcomes = append(rep.outcomes, revokeOutcome{step: "record", ok: true, detail: fmt.Sprintf(
				"removed — %s is no longer re-staged by any build and no longer resolves into any agent's coords feed", name)})
			if err := store.InsertRetired(name, state.RetiredCapability{
				RevokedAt:  uint64(time.Now().Unix()),
				RetiredBy:  state.OriginAgent,
				LastRoster: append([]string(nil), rec.Rosters...),
			}); err != nil {
				return "", fmt.Errorf("revoke_runner %s: record the retirement: %w", name, err)
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
	name         string
	single       bool
	grantees     []string
	outcomes     []revokeOutcome
	notes        []string
	auditChannel string
	hosted       bool
	verdict      string
}

// rosterOutcome judges the revocation against the roster the relay actually
// holds now. A read failure is NEVER folded into success.
func rosterOutcome(single bool, targets []string, pubkeys map[string]string, failed []string, have []string, qerr error) revokeOutcome {
	if qerr != nil {
		return revokeOutcome{step: "roster", ok: false, detail: fmt.Sprintf(
			"the removals were issued but the roster could NOT be read back to confirm them (%v) — do not claim the door is gone from [%s] until its roster is re-read%s",
			qerr, orNone(targets), joinFailures(failed))}
	}
	present := map[string]bool{}
	for _, pk := range have {
		present[pk] = true
	}
	var still []string
	for _, g := range targets {
		if pk := pubkeys[g]; pk != "" && present[pk] {
			still = append(still, g)
		}
	}
	if len(still) > 0 {
		return revokeOutcome{step: "roster", ok: false, detail: fmt.Sprintf(
			"the relay roster STILL lists [%s] (%d entr%s total) — the remove-user did not land; the door is NOT taken from them%s",
			strings.Join(still, ", "), len(have), pluralize(len(have)), joinFailures(failed))}
	}
	if len(failed) > 0 {
		return revokeOutcome{step: "roster", ok: false, detail: fmt.Sprintf(
			"the roster reads back without the targets (%d entr%s) but %d removal%s reported a failure — treat as UNCONFIRMED%s",
			len(have), pluralize(len(have)), len(failed), sIf(len(failed)), joinFailures(failed))}
	}
	if single {
		return revokeOutcome{step: "roster", ok: true, detail: fmt.Sprintf(
			"verified against the relay-signed roster: [%s] is off it (%d entr%s remain)", strings.Join(targets, ", "), len(have), pluralize(len(have)))}
	}
	return revokeOutcome{step: "roster", ok: true, detail: fmt.Sprintf(
		"verified against the relay-signed roster: the door's roster is empty (%d entr%s left) — every grantee is off it", len(have), pluralize(len(have)))}
}

// credentialOutcome re-opens the sealed package to prove the CP no longer holds
// a copy of the credential.
func credentialOutcome(pkgDir string) revokeOutcome {
	if _, err := wire.Load(pkgDir); err == nil {
		return revokeOutcome{step: "credential", ok: false, detail: fmt.Sprintf(
			"the sealed package at %s is STILL READABLE after the revoke — treat the credential as live and close it by hand", pkgDir)}
	}
	return revokeOutcome{step: "credential", ok: true, detail: fmt.Sprintf(
		"the sealed package at %s is gone — the CP holds no copy of the credential any more", pkgDir)}
}

// unitOutcome stops the transient unit and VERIFIES the stop locally. The CP
// executor's own exec path is the CP guest — that is how startCapabilityRunner
// gets the unit up with systemctl/systemd-run — so the teardown asks the same
// box the same way, without routing a root exec through a runner: this flow is
// revoking exec, so it must not depend on holding any.
func unitOutcome(name string, port int) revokeOutcome {
	unit := "freehold-runner-" + name
	// The department-named legacy unit is stopped too, exactly as retireRunner does:
	// a world built before the runner-named convention can still hold it. The stop
	// runs on the box this process IS (the CP guest), the same way the start path
	// runs systemctl/systemd-run — no runner credential is borrowed to prove a
	// teardown.
	script := fmt.Sprintf("systemctl stop %s 2>/dev/null; systemctl reset-failed %s 2>/dev/null; "+
		"systemctl stop freehold-runner-data 2>/dev/null; systemctl reset-failed freehold-runner-data 2>/dev/null; true", unit, unit)
	stopOut, _ := exec.Command("sh", "-c", script).CombinedOutput()
	// systemd releases the transient unit's cgroup synchronously, so a short settle
	// is the whole grace the probe needs: longer than that is a real residue.
	time.Sleep(revokeProbeWait)
	activeRaw, _ := exec.Command("sh", "-c", fmt.Sprintf("systemctl is-active %s 2>/dev/null || true", unit)).CombinedOutput()
	active := strings.TrimSpace(string(activeRaw))
	if active == "" {
		active = "unknown"
	}
	if active != "inactive" {
		return revokeOutcome{step: "unit", ok: false, detail: fmt.Sprintf(
			"freehold-runner-%s reports %q after the stop (stop output: %s) — the door is revoked and unauthorized, which is the security answer, but a running unit is a known state, not a clean one",
			name, active, firstLine(string(stopOut)))}
	}
	// The listener is the second, independent witness: a process that outlived its
	// transient unit (a wedged child, a re-exec) still holds the port. Dialled
	// exactly the way startCapabilityRunner waits for it to come up.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if c, derr := net.DialTimeout("tcp", addr, 2*time.Second); derr == nil {
		_ = c.Close()
		return revokeOutcome{step: "unit", ok: false, detail: fmt.Sprintf(
			"freehold-runner-%s is inactive yet %s STILL ANSWERS — the door is unauthorized, NOT stopped-and-verified", name, addr)}
	}
	return revokeOutcome{step: "unit", ok: true, detail: fmt.Sprintf(
		"freehold-runner-%s is inactive and nothing answers on %s — verified down after the stop", name, addr)}
}

// renderRevoke turns the structured report into what the agent reads and relays.
// The order is fixed (steps, then follow-ups, then the verdict) so the operator's
// eye finds the verdict in the same place every time.
func renderRevoke(r revokeReport) string {
	var b strings.Builder
	if r.single {
		fmt.Fprintf(&b, "revoke_runner: %s removed from the roster of [%s]\n", r.name, orNone(r.grantees))
	} else {
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
	var bad []string
	for _, o := range r.outcomes {
		if !o.ok {
			bad = append(bad, o.step)
		}
	}
	who := orNone(targets)
	switch {
	case single && len(bad) == 0:
		return fmt.Sprintf("%s is out of [%s]'s hands (verified against the relay roster) and out of their pod's coords feed; the door itself stands for whoever remains on its roster", name, who)
	case single:
		return fmt.Sprintf("%s: the removal from [%s] was issued but is NOT fully verified (%s) — do not tell the operator it is gone until those legs report clean", name, who, strings.Join(bad, ", "))
	case len(bad) == 0:
		return fmt.Sprintf("%s is retired: its roster is empty and verified, [%s] is out of their hands, its credential is erased from the CP, its unit is verified down, and its name is refused to the agent surface until the operator re-enables it from the console", name, who)
	default:
		return fmt.Sprintf("%s is retired in state — the roster write, the record removal and the name guard all ran — but these did NOT verify: %s. A revoked door that is still running is a known state, never a silent one: say what is unverified and let the operator close it", name, strings.Join(bad, ", "))
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
