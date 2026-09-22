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
- **Root access to the PVE host** (a door key already authorized, or a console/root
  password). A fresh profile mints a NEW door key, and `install --yes` bails until that
  key is in the host's `/root/.ssh/authorized_keys` — see step 2.
- **DNS provider credentials** for the test zone (e.g. `CLOUDFLARE_DNS_API_TOKEN`). `build`
  owns DNS (`--manage-dns`) and needs them stored for the profile; see the field notes.
- **Consent to share the thin pool** when the host already runs another world
  (`--confirm-shared-pool`), and **`--confirm-storage`** only when the host has no usable
  storage at all.
- The hermetic gates are green (`just test`) — run them first so a live failure isn't a
  known-broken unit.
- Time: a fresh world's DNS-01 cert issuance can take a long time (observed ~45 min on a
  Cloudflare zone); budget for it. Run long commands in the background with a log and poll
  (a coding-agent shell often caps a single command at ~2 min).

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
   chmod +x "$dir/bin"/{freehold,freehold-console,runner,freehold-agent-tools}  # gh drops the exec bit
   (cd "$dir/bin" && sha256sum -c checksums.txt)        # every asset verifies
   tar -xzf "$dir/bin/migrations.tar.gz" -C "$dir/bin"  # -> bin/migrations/ (ResolveMigrationsDir wants ./migrations)
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
   name=<test-name>
   # a fresh profile mints a new door key; with --yes the install prints it and bails.
   # Authorize it on the host, then re-run the SAME command:
   "$fh" install --name "$name" --host root@<pve-host> \
     --relay-domain <relay.example> --cp-domain <cp.example> \
     --proxy-ip <ip/cidr> --operator-pubkey <64-hex> \
     --operator-identity <dir> --litellm-provider-key <key> \
     --confirm-shared-pool --yes
   #   (drop --confirm-shared-pool if the host's pool is empty; add --confirm-storage
   #    only when the host has no usable storage)
   # On the host: echo '<printed ssh-ed25519 line>' >> /root/.ssh/authorized_keys
   "$fh" build --config <profile-config> --manage-dns \
     --litellm-provider-key <key> --yes     # install lands CP-only; build brings the world up
   ```
   `build` needs a DNS-01 credential stored *for the profile*. The shipped `dns-cred`
   writes to the **base** state dir (and seals to the base ops identity), not the profile's,
   so for a profile-scoped world it is not usable as-is — either run `build` once WITHOUT
   `--yes` and let its prompt store the credential in the profile state dir, or place the
   sealed `dns-provider-{relay,cp}.json` under `<profile-state>/control-plane/`. See field
   notes.

   Run `install`/`build` in the background (`nohup … > log 2>&1 &`) and poll the log —
   they run for minutes.

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

## Field notes (learned on the v0.7.0 first run)

- **`gh release download` drops the execute bit** — `chmod +x` the binaries before the
  `test -x` check, or step 1 fails for the wrong reason.
- **A fresh profile needs a new host door.** `install --yes` refuses until the key it prints
  is in the host's `authorized_keys`; authorize it and re-run. If this box already has an
  authorized door for a prior profile, you can derive it
  (`platform/provisioning/box.DoorKeyPEM`) and use it for the direct SSH.
- **DNS creds are profile-scoped but `dns-cred` is not.** `freehold dns-cred` seals the
  credential to `box.OpsDir()` and writes it under `box.StateDir()` with no profile
  negotiation, so it targets the base `~/.freehold`, not `profiles/<name>/`. `build` (which
  negotiates the profile) then can't find it. Seed the profile-scoped cred via `build`'s
  own prompt or place the sealed file directly — this is a product gap worth filing.
- **Sharing a pool needs `--confirm-shared-pool`.** `install` bails on a host whose thin
  pool already holds live volumes (e.g. other worlds) unless you pass it.
- **DNS-01 propagation is the long pole — expect a retry.** A fresh build can fail
  `world-build cert: … dns-01 … did not propagate: … NXDOMAIN` even though the challenge
  TXT is already in the provider's API: on the test zone the authoritative NS kept
  answering `NXDOMAIN` for the record for ~10–30 min (the provider API accepted it
  immediately). Diagnose by comparing the provider API record against
  `dig TXT _acme-challenge.<host> @<authoritative-ns>`; when it starts answering, re-run
  `build` — the resumable order (`world-secrets/cert-pending-<slot>.json`) reuses the same
  challenge, so the retry resolves and installs. Don't call the cert broken on the first
  NXDOMAIN, and don't re-order/clean up by hand.
- **Fresh-world resolver ordering (fixed; was v0.7.0's first blocker).** `cpbuild`'s
  `pointGuestsAtResolver` ran *before* the CP's dnsmasq was installed (`worldDNS`), so on a
  brand-new world guests pointed at a resolver-less CP and litellm/caddy pulls died
  (`lookup registry-1.docker.io: Try again` → `FailedCreatePodSandBox`), 500ing
  `world_build`; rebuilds hid it. Fixed in `74b0875` (install the resolver before pointing
  guests at it). If a fresh build 500s at `terraform services`, check
  `ss -lunp | grep :53` inside the CP LXC (empty = not installed).
- **The PVE host's terraform workdir is shared host state, not world state.**
  `/srv/data/freehold-tf` keeps `terraform.tfstate` across a world's life. If you destroy
  the world out-of-band (manual `pct destroy`, a killed install) and re-build, terraform
  still believes `k3s_bringup` etc. ran and SKIPS them — the rebuild then dies at
  `tf kubeconfig: cat /etc/rancher/k3s/k3s.yaml: No such file`. Clear that dir (or run the
  product's own `teardown`) before a fresh build.
- **Thin-box lifecycle asymmetries.** From a thin box with no local runner:
  `teardown` works (the CP drives its own runner); `uninstall` does **not** — it needs a
  local runner, and its transient fallback runs `pct destroy` on a still-running guest and
  fails. Start the release's own `runner serve --state-dir <profile-state>/runner/<target>
  --addr 127.0.0.1:8787` to give uninstall its local runner.
- **Uninstall can skip relay/k3s after a teardown→rebuild.** `teardown` clears the recorded
  relay/k3s vmids ("the next build re-creates them"), but `build` does not re-record them in
  the operator config, so a later `uninstall` reports "never created (no vmid recorded)" and
  leaves those LXCs running. Verify with `pct list | grep <name>` after uninstall — the
  Install/Uninstall row fails if guests remain. (Product gap to file.)
- **A brand-new `v*` hub: re-cutting a failed pre-release.** If the candidate fails and
  `main` is fixed, the operator may say to move the pre-release tag to the new `main` tip and
  rebuild assets. That means (only on that explicit go-ahead): `gh release delete <tag>
  --yes` (tag survives), `git tag -f -a <tag> <main-sha>`, `git push -f origin <tag>` (CI
  `release.yml` builds a fresh draft), then re-publish it as a pre-release with the same
  notes + table (`gh release edit --draft=false --prerelease …`).
- **Running the test itself.** Put long installs/builds in the background with a log and
  poll; the operator box usually can't resolve the world's public DNS until the CP's records
  land, so talk to guests over the host or the LAN IP when needed.

## Hard rules

- **Test the downloaded pre-release assets**, never a local `just build`.
- **Destructive** — only a disposable test profile/host; get the operator's go-ahead first.
- **Never move/create/delete a tag, never change assets, title, or `isPrerelease`; only the
  status-table cells are yours to edit.** The one exception is an explicit operator go-ahead
  to re-cut a failed candidate onto the fixed `main` tip (see the field notes). Never promote
  — that's `release-publish`.
- Keep the table format and icons exact (`⚪ Unverified` · `✅ Passed` · `❌ Failed`) so
  `release-publish` can parse it. Don't drop rows you didn't test.
- Record the evidence (checksums, install/teardown/rebuild/uninstall output, agent request +
  reply) when reporting to the operator.
