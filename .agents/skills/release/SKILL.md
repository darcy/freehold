---
name: release
description: Use when cutting a tagged GitHub release from main for freehold (e.g. "tag v0.4.0", "ship a release", "cut a release for the current phase"). Creates an annotated git tag on main and a high-level, short GitHub Release — do NOT dump the CHANGELOG verbatim into the release notes.
metadata:
  version: 1.0.0
  author: freehold
  license: MIT
---

# Release — tag `main` + publish a GitHub Release

This repo follows **one `0.x.y` per phase**: each phase that lands on `main`
gets an annotated tag `v0.x.y` **and** a GitHub Release whose notes are a
**short, high-level summary** distilled from `CHANGELOG.md` — never the full
changelog text.

## When to use

The operator asks to tag `main`, "cut/tag a release", or "ship a release" for a
version that has already merged to `main`. If they only want to *tag*, still
create the Release too (the tag and the Release always ship together).

## Workflow

1. **Pin the version + target.**
   ```bash
   cd /home/darcy/Work/freehold
   git fetch --tags --quiet
   git log --oneline -1 main          # confirm main is checked out and synced
   git tag -l 'v*'                     # ensure the target v0.x.y is NOT already tagged
   ```
   If the tag already exists on `main`, stop and tell the operator — do not
   move a published tag.

2. **Write the release notes (short, high-level).** Read the matching
   `## [0.x.y] — …` section of `CHANGELOG.md` and **distill** it into ~5–10
   bullets covering only the headline changes. Keep it scannable: group into
   `### Added / Fixed / Removed` only if it helps; a few one-liners is ideal.
   Do **not** copy the changelog paragraphs verbatim.

3. **Tag `main` and create the Release** (tag must point at `main`):
   ```bash
   git tag -a v0.x.y -m "v0.x.y — <one-line headline>" main
   git push origin v0.x.y
   ```
   Then publish:
   ```bash
   gh release create v0.x.y \
     --title "v0.x.y — <one-line headline>" \
     --notes-file /tmp/opencode/release_v0.x.y.md \
     --target main
   ```
   (Use `--target main` even when already on `main`, and `--generate-notes`
   only if you skipped step 2.)

4. **Verify** the release URL prints and that `gh release view v0.x.y` shows the
   correct tag target.

## Hard rules

- **Tag `main`**, not the current (possibly detached/feature) commit, unless
  the operator explicitly says otherwise.
- **Never** create, move, or delete a tag that already exists on `origin`
  without an explicit go-ahead.
- **Never** merge a PR as part of releasing — tagging/releasing is separate
  from merging (merging is the operator's call; see `AGENTS.md`).
- Release notes stay **shorter and higher-level** than the changelog entry.
- After creating the tag/Release, remind the operator to quit + restart opencode
  only if you also touched config — a plain tag+Release needs no restart.
