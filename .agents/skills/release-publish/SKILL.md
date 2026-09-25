---
name: release-publish
description: Use when promoting a validated freehold pre-release to a full release (e.g. "publish v0.8.0", "promote the pre-release", "make it a release"). Reads the release's test-status table and publishes only when every row is ✅ Passed; otherwise it stops and reports what is unverified or failed. Never moves the tag.
metadata:
  version: 1.2.0
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

3. **Report + get the operator's go-ahead.** Show the table and the pass/fail summary.

4. **Promote (all rows ✅ + go-ahead only).**
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
- **Never create, move, or delete a tag.** Promotion is a metadata edit (`isPrerelease`
  + the Latest badge).
- **Never merge anything** — merging PRs is the operator's call (see `AGENTS.md`).
- The release notes were written and published at prepare time; do not edit them here.
- The body (including the table) is owned by the test skills; do not rewrite it here.
