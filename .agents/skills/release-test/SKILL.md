---
name: release-test
description: Use when testing a freehold pre-release for promotion (e.g. "test the release", "run the release tests"). The orchestrator, in LOCKED order — dev first (test-dev deploy + manual verification), then the per-provider e2e (release-test-proxmox today; release-test-vultr when it lands) — each provider skill fills its rows of the release's test-status table; release-publish promotes when every row passes.
metadata:
  version: 1.0.0
  author: freehold
  license: MIT
---

# Release-test — dev first, then the per-provider e2e

A pre-release is tested in two stages, in this order (LOCKED):

1. **Dev — run the `test-dev` skill.** Deploy main (the candidate's source) to the dev world
   and manually verify the changes with the operator. Dev is where behavior is reasoned about
   and exercised for real; a NEW defect found here STOPS the flow — fix on main, re-cut
   (release-prepare's re-cut path), and re-run test-dev. The e2e below never runs on a red or
   untested dev.
2. **Per-provider e2e — run the `release-test-<provider>` skill** (`release-test-proxmox`
   today; Vultr comes later). Each fills the release's test-status table rows for its
   provider, exercising the pre-release's own downloaded assets.

Invoke the stage skills — don't inline their workflows here. Track the whole run as a todo
list (dev deploy → dev checks → per-provider rows), one item per unit of work, completed only
on evidence.

## Preconditions

- A pre-release exists (release-prepare's output), not a draft.
- The stage skills' inputs: `.envs.yml` (repo root, gitignored) for the dev profile;
  `.env.test` (repo root, gitignored) for the e2e secrets. The dev stage needs no release
  assets — it deploys main; the e2e stage tests ONLY the downloaded assets.

## After both stages

When every row of the test-status table is ✅ and the dispositions are clean, hand off to
`release-publish`. Any ⚪/❌ left, or a red dev, stops here — report the failing state to the
operator.
