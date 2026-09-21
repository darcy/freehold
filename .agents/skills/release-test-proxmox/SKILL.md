---
name: release-test-proxmox
description: Use when validating a freehold pre-release on a real Proxmox host (e.g. "test the release", "run the Proxmox release tests"). Restores the pre-release's own downloaded assets, runs the full lifecycle (install → agent replies in the relay → teardown → all down → rebuild → agent replies → uninstall → all gone) on a real PVE host, and updates the release's test-status table rows for Proxmox.
metadata:
  version: 1.0.0
  author: freehold
  license: MIT
---

# Release-test-proxmox — run the live lifecycle, fill the Proxmox table rows

Exercises a pre-release's **exact downloaded assets** through the full world lifecycle on a
real Proxmox host, then records the result in the release body's `## Test status` table. It
never promotes the release — `release-publish` does that once every row is ✅.

## When to use

After `release-prepare` published a pre-release, to validate it on Proxmox. Destructive: it
creates and destroys a real world, so run it only against a disposable test profile/host, with
the operator's go-ahead.

## Preconditions

- A pre-release exists (`isPrerelease: true`, not a draft) with all six assets.
- A real Proxmox host is reachable and a test world name/domains/proxy IP are known
  (operator-supplied or recorded in a profile).
- The hermetic gates are green (`just test`) — run them first so a live failure isn't a
  known-broken unit.

If the host or credentials aren't available, do **not** fake it: leave the Proxmox rows
`⚪ Unverified` and report that you couldn't test.

## Workflow

1. **Restore the assets into a `ResolveBins`-shaped layout** so the pre-release binaries are
   what gets installed (not a local `just build`). `ResolveBins` checks *paths*, not build
   profile: it wants `freehold-console` + `runner` beside the CLI, and the trio under
   `../release/`. The download already lands every binary in `bin/`, so only the `release/`
   copies are missing — stage them and confirm the full set:
   ```bash
   tag=<vX.Y.Z>
   dir=/tmp/opencode/pve-e2e-$tag && rm -rf "$dir" && mkdir -p "$dir/bin" "$dir/release"
   gh release download "$tag" -D "$dir/bin"   # puts freehold, freehold-console, runner,
                                              # freehold-agent-tools + migrations.tar.gz in bin/
   (cd "$dir/bin" && sha256sum -c checksums.txt)        # every asset verifies
   tar -xzf "$dir/bin/migrations.tar.gz" -C "$dir/bin"  # -> bin/migrations/
   for b in freehold-console runner freehold-agent-tools; do
     install -m 755 "$dir/bin/$b" "$dir/release/$b"      # ResolveBins wants ../release/
   done
   # every sibling ResolveBins requires must exist, or `install` fails before it starts:
   test -x "$dir/bin/freehold-console" && test -x "$dir/bin/runner" \
     && test -x "$dir/release/freehold-console" && test -x "$dir/release/runner" \
     && test -x "$dir/release/freehold-agent-tools" && echo "sibling set ok"
   fh="$dir/bin/freehold"                                # the release CLI under test
   ```

2. **Install flow — fresh world from the release assets, then verify the CPA replies in the
   relay.**
   ```bash
   "$fh" install --name <test-name> --host root@<pve-host> \
     --relay-domain <relay.example> --cp-domain <cp.example> \
     --proxy-ip <ip/cidr> --operator-pubkey <64-hex> \
     --operator-identity <dir> --litellm-provider-key <key> --yes
   "$fh" build            # if install lands CP-only, bring the world (relay/k3s/agents) up
   ```
   Verify the CPA replies in the relay: post a message in `#freehold` (Buzz, or the agent's
   message tools), wait for the CPA's reply, and capture the request + reply. This is the
   install half of the **Install/Uninstall** row.

3. **Teardown flow — the world goes down, the CP survives.**
   ```bash
   "$fh" teardown --yes
   "$fh" status
   ```
   Verify every world guest is gone (relay + k3s LXC destroyed) and the CP is still healthy.
   This is the teardown half of the **Rebuild/Teardown** row.

4. **Rebuild flow — the world comes back and the CPA replies again.**
   ```bash
   "$fh" build
   ```
   Verify the CPA replies in the relay again (post + confirm reply). This is the rebuild half
   of the **Rebuild/Teardown** row.

5. **Uninstall flow — nothing is left.**
   ```bash
   "$fh" uninstall --name <test-name> --yes             # add --remove-data to drop the plane too
   ```
   Verify the CP, runner, door, and every guest are removed (no `<test-name>-*` guests remain,
   the DOOR_SPEC key is gone). This is the uninstall half of the **Install/Uninstall** row.

6. **Compute the two row statuses** — a row is ✅ only if **both** its halves passed:
   - **Proxmox | Install/Uninstall** ✅ iff install → CPA replies **and** uninstall → all gone.
   - **Proxmox | Rebuild/Teardown** ✅ iff teardown → all down **and** rebuild → CPA replies.
   Mark a row ❌ Failed if it was attempted and failed; `⚪ Unverified` if it wasn't attempted
   (no host/creds). Never mark ✅ without the evidence above.

7. **Update the release table** — change only the Proxmox rows, leave every other row, the
   title, assets, and the prerelease flag untouched:
   ```bash
   gh release view "$tag" --json body -q .body > /tmp/opencode/body.md
   # edit the two Proxmox Status cells in /tmp/opencode/body.md (⚪/✅/❌), preserving the rest
   gh release edit "$tag" --notes-file /tmp/opencode/body.md
   gh release view "$tag" --json body -q .body | sed -n '/^## Test status/,$p'
   ```

## Hard rules

- **Test the downloaded pre-release assets**, never a local `just build`.
- **Destructive** — only a disposable test profile/host; get the operator's go-ahead first.
- **Never move/create/delete a tag**, never change assets, title, or `isPrerelease`; only the
  status-table cells are yours to edit. Never promote — that's `release-publish`.
- Keep the table format and icons exact (`⚪ Unverified` · `✅ Passed` · `❌ Failed`) so
  `release-publish` can parse it. Don't drop rows you didn't test.
- Record the evidence (checksums, install/teardown/rebuild/uninstall output, agent request +
  reply) when reporting to the operator.
