---
name: release-publish
description: Use when promoting a validated freehold pre-release to a full release (e.g. "publish v0.8.0", "promote the pre-release", "make it a release"). Reads the release's test-status table and publishes only when every row is ✅ Passed; otherwise it stops and reports what is unverified or failed. Never moves the tag.
metadata:
  version: 1.3.0
  author: freehold
  license: MIT
---

# Release-publish — gate on the test-status table, then promote to a full release

A version becomes final only after every test in the release's **test-status table** passes.
The table lives at the bottom of the pre-release body: one row per provider × env × test —
starting with the Proxmox Fresh/Rebuild/Live rows — each marked ⚪ Unverified, ✅ Passed, or
❌ Failed. `release-test-proxmox` (and future provider skills) fill it. This skill reads the
table, refuses to publish while any row is not ✅, and otherwise flips `isPrerelease=false`.
The tag, commit, and assets never change: promotion is metadata only.

## When to use

After `release-prepare` cut a pre-release and its rows have been filled by the test skills,
when the operator asks to "publish", "promote", or "make it final". Cutting a new candidate is
`release-prepare`; running the live flows is `release-test-proxmox`.

## Workflow

1. **Resolve the target** (the operator's tag, else the newest pre-release):
   ```bash
   tag=<vX.Y.Z>
   gh release view "$tag" --json tagName,isDraft,isPrerelease,body
   ```
   Stop if the release is missing, a draft, or already `isPrerelease: false` (already published).

2. **Parse the test-status table and check every row.** Read `## Test status` from the body;
   for each data row, require its Status cell to be `✅ Passed`. The set of rows grows over
   time (more providers, envs, tests) — iterate whatever is there; do not hardcode the row
   set.
   ```bash
   body=$(gh release view "$tag" --json body -q .body)
   printf '%s\n' "$body" | sed -n '/^## Test status/,$p'   # inspect the rows
   ```
   Stop and report the offending rows if any is `⚪ Unverified` or `❌ Failed`, or if the table
   is missing entirely.

3. **Be CRITICAL on gold: the release must be CLEAN except for known issues.** A green table
   is necessary, not sufficient. Before asking for the go-ahead, run the disposition check:
   - Collect every defect/finding the test run surfaced (from the test skill's report, the
     run's logs, the release body's findings section if present).
   - Classify each: **known issue** (already recorded in AGENTS.md's "Known gaps" — or the
     release notes name it), **operator-accepted** (the operator explicitly dispositioned it
     during the run), or **undocumented defect**.
   - **Any undocumented defect blocks promotion.** The fix lands on `main`, and the release
     is RE-CUT (per release-prepare's re-cut path: delete the candidate, re-tag the fixed
     tip, re-run the tests) — a gold release carrying an undocumented defect is how known
     gaps rot into user-facing breakage. Never argue a defect into "followup" on your own;
     that is the operator's call, made explicitly.
   - Known issues ride along: they are already recorded, and promotion does not bless them
     as fixed.

4. **Report + get the operator's go-ahead.** Show the table, the pass/fail summary, AND the
   findings disposition list (known issue / accepted / blocked). Promotion proceeds only on
   the operator's explicit yes WITH the disposition clean.

5. **Promote (all rows ✅ + dispositions clean + go-ahead only).**
   ```bash
   gh release edit "$tag" --prerelease=false --latest
   gh release view "$tag" --json tagName,isDraft,isPrerelease,assets
   gh api "$(gh repo view --json nameWithOwner -q .nameWithOwner | sed 's|^|repos/|')/releases/latest" --jq .tag_name
   ```
   Confirm `isPrerelease` is now `false`, the `releases/latest` call returns this tag
   (GitHub does NOT recompute the Latest badge on a prerelease→full edit — `--latest`
   moves it; without it a full release sits stranded behind a stale badge), and the tag
   still points at the same commit with the same assets.

## Hard rules

- **No promotion unless every row in the test-status table is ✅ Passed.** A missing table, an
  ⚪ Unverified row, or a ❌ Failed row blocks publication — never publish on a partial table.
- **No promotion with an undocumented defect.** The disposition check (step 3) blocks gold
  when the test run surfaced a defect that is neither a recorded known issue nor explicitly
  operator-accepted. Fix forward and re-cut instead.
- **Never create, move, or delete a tag.** Promotion is a metadata edit (`isPrerelease`
  + the Latest badge). (The re-cut of a failed candidate is release-prepare's path, on an
  explicit operator go-ahead.)
- **Never merge anything** — merging PRs is the operator's call (see `AGENTS.md`).
- The release notes were written and published at prepare time; do not edit them here.
- The body (including the table) is owned by the test skills; do not rewrite it here.
