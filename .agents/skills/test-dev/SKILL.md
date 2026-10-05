---
name: test-dev
description: Use when deploying main to the dev world and manually verifying the changes (e.g. "update dev", "test on dev", "deploy to dev"). Diffs what's on dev against the tip of main, confirms with the operator what to manually test, deploys via the update flow, then runs those checks. release-test invokes this before any provider's e2e.
metadata:
  version: 1.0.0
  author: freehold
  license: MIT
---

# Test-dev — deploy main to dev, verify the changes by hand

The dev world (AGENTS.md "Environments" — build, test, sandbox) is where changes are proven
BEFORE the release e2e: deploy main to dev, manually exercise what changed, and only a green
dev lets `release-test` move on to the provider e2e envs.

## The dev world

- The dev profile is listed under `dev:` in `.envs.yml` (repo root, gitignored — local to the
  operator box). Resolve profile name → config path with `freehold profiles`.
- **The update flow is the only way changes reach dev**: `freehold update --ref main`. Doing a
  thing by hand on the box is for TESTING a hypothesis only — the fix ships through a PR.
- Dev is mutating but sandbox: migrations are one-way. Note pending migrations before
  deploying (`update --check` prints them) so an unexpected migration count is caught then,
  not after.

## Workflow

1. **Resolve the dev profile.** Read `.envs.yml`'s `dev:` key → profile name(s) →
   `freehold profiles` → the config path. More than one profile → ask the operator which
   (or test each).
2. **Diff dev vs main.** Current state: `freehold update --check --config <cfg>` (version +
   channel + pending migrations) and `freehold status --config <cfg>`. The deployed commit:
   the version string is the deployed tree's `git describe` — a plain `vX.Y.Z` means the tag;
   `vX.Y.Z-N-g<sha>` means `<sha>`. Then the changeset the deploy will apply:
   ```bash
   git fetch origin main --quiet
   git log --oneline <deployed>..origin/main
   git diff --stat <deployed>..origin/main | tail -5
   ```
   When the version string yields no sha, confirm the window with the operator instead of
   guessing.
3. **Confirm what to test with the operator.** Present the changeset and ask what to manually
   verify on dev. Anything the changeset claims (a fixed flow, a new surface, a prompt or
   behavior change) becomes a concrete check; the operator may add their own. Do not deploy
   before this list exists.
   **Set up each check BEFORE deploying, so it can fail.** For every confirmed check, figure
   out what the old build's behavior looks like and capture it first (a before-baseline: ask
   the agent the question, write the marker, note the setting) — then the post-deploy run
   proves the change instead of confirming a guess. Skip setup that can't exist yet (a
   feature's surface isn't on the old build — baseline what you can); don't go overboard —
   one honest baseline per check.
4. **Deploy (detached).** `update` runs minutes — never under a shell that can kill it
   (a killed update leaves half-provisioned state):
   ```bash
   setsid nohup freehold update --ref main --config <cfg> --non-interactive \
     > /tmp/opencode/dev-update.log 2>&1 & disown
   ```
   Poll the log to completion, then `freehold update --check --config <cfg>`: expected
   version, 0 unexpected pending migrations.
5. **Health.** `freehold status --config <cfg>` healthy; the CPA replies in the relay (post
   in `#freehold` with the CPA's pubkey in a `p` tag — mechanics in release-test-proxmox's
   field notes; pods take ~5–10 min after an update to come up and subscribe). For direct
   verification (pod env, kubectl, guest state) sign exec through the world's runner:
   `freehold exec <target> <cmd> --config <cfg>` — the target is the profile's runner
   target as `freehold status` shows it (e.g. `proxmox-box`), not a runner's display name.
6. **Run the confirmed checks, one at a time, recording pass/fail with evidence.** Talk to
   the agents in the relay for behavior changes; use the console and `freehold status` for
   surfaces. A check that can't run on dev is reported skipped, never passed. Agent pods
   are BARE pods — `kubectl delete pod` removes them until the next `freehold build`
   re-creates them; use that (the update flow's own path) when a check needs a fresh pod.
7. **Disposition.** A NEW defect (undocumented in AGENTS.md's "Known gaps") → report it and
   STOP: the release e2e does not start until it's fixed + merged, and the fix re-runs this
   skill. Already a known gap → record it, proceed. Operator-accepted → record the
   acceptance; it lands in "Known gaps".

## Hard rules

- Deploy through the update flow only (`update --ref main`); never hand-edit the dev world.
- Never deploy without the operator's confirmed test list — the point is the manual
  verification, not just the deploy.
- Report honestly: an unrun check is ⚪, a failure is ❌ with evidence, and a NEW defect stops
  the release flow.
