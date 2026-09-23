---
name: release-prepare
description: Use when cutting a versioned pre-release for freehold (e.g. "cut v0.8.0", "ship a release", "cut an rc"). Generates the release notes from git history since the previous release, gets the operator's approval, tags the main commit, and publishes a GitHub pre-release (marked prerelease) with the built assets, short high-level notes, and a test-status table. release-test-proxmox fills the table; release-publish promotes it when every row passes.
metadata:
  version: 5.1.0
  author: freehold
  license: MIT
---

# Release-prepare — generate the notes, tag `main`, publish a GitHub pre-release

A version exists only when it is released, and every release is **two things together**: an
annotated tag `vX.Y.Z` on `main` and a GitHub Release with **short, high-level** notes. There
is **no changelog file** — the GitHub Release is the record. A version is cut as a
**pre-release** first: the GitHub Release is marked `prerelease`, carries the built assets,
and ends with a **test-status table**. `release-test-proxmox` fills that table by running the
live flows; `release-publish` promotes the release to final only once every row passes — same
tag, same commit, same assets. This skill never promotes. There is no version bump per merge
or phase; this skill is the only thing that assigns a version.

The notes compare the **codebase** at the previous release to the current one: they describe
what is true now that wasn't then. They are **not** an exhaustive commit log — if something
was refactored and then refactored again, only the final shape is recorded; superseded or
reverted work is omitted.

## When to use

The operator asks to "tag a release", "ship a release", "cut an rc", or bump the version.
The version has **not** been recorded anywhere yet — you generate the notes now, from git
history. Promotion of an already-cut pre-release is `release-publish`, not this.

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

   The baseline is the **previous release tag** — the newest tag reachable from `HEAD`:
   ```bash
   base=$(git describe --tags --abbrev=0)   # the previous release tag
   ```

2. **Generate the notes by comparing the codebase to the previous release.**
   ```bash
   git log --oneline "$base"..HEAD                   # the raw material, not the output
   git diff "$base"..HEAD -- <areas of interest>     # what actually differs now
   ```
   Describe the **net** difference between the released tree and the current one — what is
   true now that wasn't then. Do **not** enumerate commits/PRs chronologically: if work was
   refactored twice, record only the final shape; drop superseded, reverted, or intermediate
   work. Classify into `### Added / Changed / Fixed / Removed`. These notes **are** the
   release body — there is no file to commit and no release PR.

3. **Get the draft notes approved.** Show the operator the full generated notes (the ~5–10
   headline bullets) and **wait for their approval**. Do not tag or publish until they sign
   off; revise the draft as they ask. Write the approved notes to
   `/tmp/opencode/release_vX.Y.Z.md` for the publish step.

4. **Tag `main`'s tip — CI builds the assets.**
   ```bash
   git checkout main && git pull --ff-only
   git rev-parse HEAD           # must be the branch tip being released
   git tag -a vX.Y.Z -m "vX.Y.Z — <one-line headline>"
   git push origin vX.Y.Z
   ```
   Every release is currently the tip of `main`, so the tag goes there (an rc too). Tag with
   the normal current timestamp: a release's `created_at` is derived from the tag date, so
   the tag date is what orders the Releases page. The tag push triggers
   `.github/workflows/release.yml`, which builds the sibling set + `migrations.tar.gz` +
   `checksums.txt` and attaches them to a **draft** GitHub Release. Wait for that run to
   finish before publishing:
   ```bash
   gh run list --workflow=release.yml --limit 5   # until the vX.Y.Z run is completed
   ```
   If the run fails, fix forward with a new commit + a NEW tag (never move a published tag)
   or ask the operator — do not publish without assets.

5. **Publish the draft as a pre-release with the approved notes + verify the assets.**
   ```bash
   gh release edit vX.Y.Z \
     --draft=false \
     --prerelease \
     --title "vX.Y.Z — <one-line headline>" \
     --notes-file /tmp/opencode/release_vX.Y.Z.md
   gh release view vX.Y.Z --json tagName,isDraft,isPrerelease,assets
   ```
   The notes file is the **short, high-level** set of bullets (~5–10 headline items, never an
   exhaustive log) followed by the test-status table. Do **not** add an H1 headline — the
   release title already renders as the page heading, so a leading `# vX.Y.Z — …` would
   duplicate it. Confirm the tag is on the released `main` commit, `isPrerelease` is `true`,
   and every asset (`freehold`, `freehold-console`, `runner`, `freehold-agent-tools`,
   `migrations.tar.gz`, `checksums.txt`) is present.

   The notes file ends with the table, seeded as unverified — one row per provider × env ×
   test (the envs `release-test-proxmox` runs):
   ```markdown
   ## Test status

   | Provider | Env | Test | Status |
   | --- | --- | --- | --- |
   | Proxmox | fresh.freehold.technology | Fresh - Install | ⚪ Unverified |
   | Proxmox | fresh.freehold.technology | Fresh - Teardown | ⚪ Unverified |
   | Proxmox | fresh.freehold.technology | Fresh - Build | ⚪ Unverified |
   | Proxmox | fresh.freehold.technology | Fresh - Uninstall | ⚪ Unverified |
   | Proxmox | rebuild.freehold.technology | Rebuild - Teardown | ⚪ Unverified |
   | Proxmox | rebuild.freehold.technology | Rebuild - Build | ⚪ Unverified |
   | Proxmox | live | Live - Update | ⚪ Unverified |

   Legend: ⚪ Unverified · ✅ Passed · ❌ Failed — `release-test-proxmox` updates this
   table; `release-publish` requires every row ✅.
   ```

6. **Stop — do not promote.** Leave it a pre-release and tell the operator that
   `release-test-proxmox` fills the status table and `release-publish` promotes it once
   every row passes. Promotion (`--prerelease=false`) is the one thing this skill must
   never do.

> **Future (when development continues past a release):** switch to trunk-first. `main`
> stays the trunk; cut a long-lived `release/vX.Y.Z` branch for the release and
> **backport** (cherry-pick) fixes onto it; the tag goes on the release branch head, not
> `main`, and the branch is not merged back. Not needed yet — every release is currently
> the tip of `main`.

## Hard rules

- **Get the draft notes approved before tagging anything** — the operator signs off on the
  generated notes first (step 3).
- **Tag `main`'s tip** (every release is the tip of `main`), not a feature/detached commit.
  (When release branches arrive, the tag moves to the release branch head instead — see the
  Future note.)
- **Always publish as a pre-release** (`--prerelease`). Never run `--prerelease=false` —
  promotion is `release-publish`'s job, gated on the test-status table.
- **No H1 in the release body** — the release title is the heading; a leading `#` duplicates
  it on the release page.
- **Never** create, move, or delete a tag that already exists on `origin` without an
  explicit go-ahead.
- **There is no changelog file** — do not create or update a `CHANGELOG.md`. The tag and its
  GitHub Release **are** the release; a tag without its Release, or a Release without the tag
  it names, is not one.
- Release notes stay **short and high-level** — headline outcomes, not a commit log.
- After creating the tag/Release, remind the operator to quit + restart opencode only if
  you also touched config — a plain pre-release needs no restart.
