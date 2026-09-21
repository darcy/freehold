---
name: pre-release
description: Use when cutting a versioned pre-release for freehold (e.g. "cut v0.8.0", "ship a release", "cut an rc"). Generates the CHANGELOG.md entry from git history since the previous release, lands it via a release PR, tags the merged main commit, and publishes a GitHub pre-release (marked prerelease) with the built assets and short, high-level notes. Promoting it to a full release is the publish-release skill's job.
metadata:
  version: 3.0.0
  author: freehold
  license: MIT
---

# Pre-release — generate the changelog, tag `main`, publish a GitHub pre-release

A version exists only when it is released, and every release is **three things together**:
a `CHANGELOG.md` entry, an annotated tag `vX.Y.Z` on `main`, and a GitHub Release with
**short, high-level** notes distilled from that entry. A version is cut as a **pre-release**
first: the GitHub Release is marked `prerelease` and carries the built assets. It becomes
final only after `publish-release` end-to-end tests those exact assets and promotes it —
same tag, same commit, same assets. This skill never promotes. There is no version bump per
merge or phase; this skill is the only thing that assigns a version.

The entry compares the **codebase** at the previous release to the current one: it describes
what is true now that wasn't then. It is **not** an exhaustive commit log — if something was
refactored and then refactored again, only the final shape is recorded; superseded or
reverted work is omitted.

## When to use

The operator asks to "tag a release", "ship a release", "cut an rc", or bump the version.
The version has **not** been recorded anywhere yet — you generate the changelog entry now,
from git history. Promotion of an already-cut pre-release is `publish-release`, not this.

## Workflow

1. **Pin the version + baseline.**
   ```bash
   git fetch --tags --quiet
   git log --oneline -1 main          # confirm main is checked out and synced
   git tag -l 'v*' | sort -V | tail -5
   ```
   Choose the next `vX.Y.Z` — the operator's number if given, else bump from the change
   nature (breaking/foundational → minor pre-1.0, feature → minor, fix → patch).
   Confirm it is NOT already tagged/released (`git tag -l vX.Y.Z`, `gh release view vX.Y.Z`);
   if it is, stop — never move a published tag. Iterating an already-cut candidate re-tags
   the next rc (`vX.Y.Z-rc.N`) instead.

2. **Generate the entry by comparing the codebase to the previous release.** The baseline
   is the commit that recorded the top `## [x.y.z]` entry (equivalently the previous `v*`
   tag):
   ```bash
   base=$(git log -1 --format=%H -- CHANGELOG.md)   # the previous release's entry commit
   git log --oneline "$base"..HEAD                   # the raw material, not the output
   git diff "$base"..HEAD -- <areas of interest>     # what actually differs now
   ```
   Describe the **net** difference between the released tree and the current one — what is
   true now that wasn't then. Do **not** enumerate commits/PRs chronologically: if work was
   refactored twice, record only the final shape; drop superseded, reverted, or
   intermediate work. Classify into `### Added / Changed / Fixed / Removed` and write the
   full entry as a new `## [X.Y.Z] — <one-line headline>` section. Group by outcome, not
   by commit; this is the only place the changelog grows.

3. **Get the draft entry approved.** Show the operator the full generated
   `CHANGELOG.md` section (and the distilled release notes) and **wait for their
   approval**. Do not commit, branch, or tag until they sign off; revise the draft
   as they ask.

4. **Land it via a release PR.**
   ```bash
   git checkout main && git pull --ff-only
   git checkout -b docs/changelog-vX.Y.Z   # a normal PR branch, not a release branch
   # prepend the new section to CHANGELOG.md
   git commit -am "docs(changelog): vX.Y.Z — <headline>"
   git push -u origin docs/changelog-vX.Y.Z
   gh pr create --base main --title "docs(changelog): vX.Y.Z" --body "<entry summary>"
   ```
   Poll `gh pr checks` until `check` + `bot-review` settle (see `AGENTS.md`
   "Pull requests"). The release commit/tag legitimately carries the version; ordinary
   work commits must not.

5. **Stop. The operator merges.** Never merge the release PR yourself (see `AGENTS.md`
   "Pull requests"). Wait for it to land on `main`.

6. **Tag the merged `main` commit — CI builds the assets.**
   ```bash
   git checkout main && git pull --ff-only
   git rev-parse HEAD           # must be the merged release commit
   git tag -a vX.Y.Z -m "vX.Y.Z — <one-line headline>"
   git push origin vX.Y.Z
   ```
   Every release is currently the tip of `main`, so the tag goes there (an rc too). The
   tag push triggers `.github/workflows/release.yml`, which builds the sibling set +
   `migrations.tar.gz` + `checksums.txt` and attaches them to a **draft** GitHub Release.
   Wait for that run to finish before publishing:
   ```bash
   gh run list --workflow=release.yml --limit 5   # until the vX.Y.Z run is completed
   ```
   If the run fails, fix forward with a new commit + a NEW tag (never move a
   published tag) or ask the operator — do not publish without assets.

7. **Publish the draft as a pre-release with curated notes + verify the assets.**
   ```bash
   gh release edit vX.Y.Z \
     --draft=false \
     --prerelease \
     --title "vX.Y.Z — <one-line headline>" \
     --notes-file /tmp/opencode/release_vX.Y.Z.md
   gh release view vX.Y.Z --json tagName,isDraft,isPrerelease,assets
   ```
   The notes file is the **distilled** entry (~5–10 headline bullets, never the
   changelog text verbatim). Confirm the tag is on the merged `main` commit, `isPrerelease`
   is `true`, and every asset (`freehold`, `freehold-console`, `runner`,
   `freehold-agent-tools`, `migrations.tar.gz`, `checksums.txt`) is present.

8. **Stop — do not promote.** Leave it a pre-release and tell the operator that
   `publish-release` promotes it after an end-to-end run on these assets. Promotion
   (`--prerelease=false`) is the one thing this skill must never do.

> **Future (when development continues past a release):** switch to trunk-first. `main`
> stays the trunk; cut a long-lived `release/vX.Y.Z` branch for the release and
> **backport** (cherry-pick) fixes onto it; the tag goes on the release branch head, not
> `main`, and the branch is not merged back. Not needed yet — every release is currently
> the tip of `main`.

## Hard rules

- **Get the draft changelog entry approved before committing anything** — the operator
  signs off on the generated section first (step 3).
- **Tag `main`'s merged release commit** (every release is the tip of `main`), not a
  feature/detached commit. (When release branches arrive, the tag moves to the release
  branch head instead — see the Future note.)
- **Always publish as a pre-release** (`--prerelease`). Never run `--prerelease=false` —
  promotion is `publish-release`'s job, gated on e2e.
- **Never** create, move, or delete a tag that already exists on `origin` without an
  explicit go-ahead.
- **Never merge the release PR** — merging is the operator's call (see `AGENTS.md`).
- The changelog entry and the tag+pre-release ship **together**; a changelog-only edit, or
  a tag/Release without the entry, is not a release.
- Release notes stay **shorter and higher-level** than the changelog entry.
- After creating the tag/Release, remind the operator to quit + restart opencode only if
  you also touched config — a plain pre-release needs no restart.
