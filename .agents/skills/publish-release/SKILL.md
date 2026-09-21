---
name: publish-release
description: Use when promoting a validated freehold pre-release to a full release (e.g. "publish v0.8.0", "promote the pre-release", "make it a release"). Requires a green end-to-end run on the pre-release's own downloaded assets against real infrastructure; only then flips the GitHub pre-release to a full release. Never moves the tag.
metadata:
  version: 1.0.0
  author: freehold
  license: MIT
---

# Publish-release — e2e-test the pre-release assets, then promote to a full release

A version becomes final only after its **pre-release assets** — the exact binaries and
metadata attached to the pre-release git tag — pass an end-to-end run against real
infrastructure. This skill restores those assets, verifies them, runs the world against
them, and only on green (and with the operator's go-ahead) flips the GitHub pre-release to
a full release. The tag, commit, and assets never change: promotion is metadata only.

## When to use

After `pre-release` cut a version and the operator asks to "publish", "promote", or "make it
final". The target already exists as a GitHub pre-release (`isPrerelease: true`, not a
draft). Cutting/publishing a new candidate is `pre-release`, not this.

## Preconditions (stop if any is false)

```bash
tag=<vX.Y.Z>                                   # operator's tag, else newest pre-release
gh release view "$tag" --json tagName,isDraft,isPrerelease,assets \
  -q '{tag:.tagName,draft:.isDraft,prerelease:.isPrerelease,assets:[.assets[].name]}'
```

- The release exists, `isDraft` is `false`, `isPrerelease` is `true`.
- All six assets are present: `freehold`, `freehold-console`, `runner`,
  `freehold-agent-tools`, `migrations.tar.gz`, `checksums.txt`.
- The tag is on the merged `main` commit.

If an asset is missing, stop — fix forward with a NEW pre-release (`pre-release` skill);
never move or re-push a tag.

## Workflow

1. **Restore + verify the assets exactly as a consumer would** (not a local `just build` —
   what you test must be byte-identical to what ships):
   ```bash
   dir=/tmp/opencode/e2e-$tag && rm -rf "$dir" && mkdir -p "$dir"
   gh release download "$tag" -D "$dir"
   (cd "$dir" && sha256sum -c checksums.txt)      # every asset verifies
   tar -xzf "$dir/migrations.tar.gz" -C "$dir"
   ```

2. **Run the end-to-end acceptance on those assets against real infrastructure.** Use the
   downloaded `freehold` binary so the release-download + redeploy + migration path is the
   one under test. On a box with a world at the previous version:
   - `./freehold update --stable --yes` (or `--rc` for a `vX.Y.Z-rc.N` candidate — the
     channel selector keys off the tag name) → confirm it acquires + sha256-verifies the
     assets, redeploys the CP, runs pending migrations, and repins the version **last**.
   - `./freehold update --check` reports the tag and zero pending migrations.
   - `./freehold status` and the CP's `/healthz` report `$tag`; every world pillar is green.
   - `freehold build` brings the world up clean on the new version (or a fresh
     `install`/`login` if the release changed those surfaces).
   - The hermetic gates stay green: `just test` (rustfmt/build/test + six Go modules +
     the Chunk-1/2 acceptance gate).
   Capture the commands and their output (checksums, acquired version, migration log,
   status/health version strings) as evidence. If no real-world run is possible, say so and
   **stop** — do not promote on unit tests alone.

3. **Report the result to the operator.** If RED: leave the release a pre-release, report
   the failure, and stop — fix forward with a NEW pre-release tag. If GREEN: show the
   evidence and get the operator's go-ahead.

4. **Promote (green + go-ahead only).**
   ```bash
   gh release edit "$tag" --prerelease=false
   gh release view "$tag" --json tagName,isDraft,isPrerelease,assets
   ```
   Confirm `isPrerelease` is now `false` and the tag still points at the same commit with
   the same assets. Optionally append the e2e evidence to the release body.

## Hard rules

- **No promotion without a green e2e run on the assets downloaded from the pre-release.**
  A local `just build` or unit tests alone are not sufficient evidence.
- **Never create, move, or delete a tag.** Promotion is a metadata edit (`isPrerelease`).
- **Never merge anything** — merging PRs is the operator's call (see `AGENTS.md`).
- The changelog entry already landed when the pre-release was cut; do not edit it here.
- Release notes stay short and high-level; evidence may be linked or appended.
