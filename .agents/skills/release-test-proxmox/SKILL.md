---
name: release-test-proxmox
description: Use when validating a freehold pre-release on a real Proxmox host (e.g. "test the release", "run the Proxmox release tests"). Restores the pre-release's own downloaded assets and runs three envs on a real PVE host — Fresh (the full install→uninstall lifecycle on a disposable world), Rebuild (teardown→build on the persistent env, left running), and Live (`freehold update` against the always-running env) — then updates the release's test-status table rows for Proxmox.
metadata:
  version: 2.1.0
  author: freehold
  license: MIT
---

# Release-test-proxmox — run the live flows, fill the Proxmox table rows

Exercises a pre-release's **exact downloaded assets** through three live flows on a real
Proxmox host, then records the result in the release body's `## Test status` table — one
row per provider × env × test. When every row is ✅, it hands off to `release-publish`
(step 7) — the promotion itself is `release-publish`'s job.

## The three tests

| Test | Env (domains) | Flows | Left behind |
| --- | --- | --- | --- |
| **Fresh** | `relay`/`cp.fresh.freehold.technology` | install → CPA replies → teardown → all down → build → CPA replies → uninstall → all gone | nothing — fully destroyed |
| **Rebuild** | `relay`/`cp.rebuild.freehold.technology` | teardown → all down → build → CPA replies | the world, **left running** |
| **Live** | the always-running env | `freehold update --ref main` → world still healthy | the world, updated + running |

- **Fresh** proves a brand-new world works end to end from the release assets. Mint a NEW
  operator identity for the install — never the operator Rebuild/Live use.
- **Rebuild** proves `teardown` works on a world built by the **previous release** (the env
  persists between releases, so each run tears down old-version state) and `build` brings it
  back. Reuse the existing operator. **Never uninstall it** — the next release's test needs
  it running.
- **Live** proves `update` runs and does what is expected to a running env. Reuse the
  existing operator.

## When to use

After `release-prepare` published a pre-release, to validate it on Proxmox. Destructive on
the Fresh env (creates and destroys a real world) and mutating on Rebuild/Live — run only
against the disposable test host, with the operator's go-ahead.

## Preconditions

- A pre-release exists (`isPrerelease: true`, not a draft) with all six assets.
- **Secrets live in `@.env.test`** (repo root, gitignored, never committed): the DNS API
  token (`CLOUDFLARE_API_KEY`), the litellm provider key (`FIREWORKS_API_KEY`), and the
  operator npub (`OPERATOR_NPUB` — the persistent envs' operator), plus the PVE host and
  proxy IP. Source values from there; never paste them into logs, evidence, or the release
  body.
- **Operator identities:** the Fresh install uses a **new** operator (a new identity dir +
  pubkey — the installer's "Generate one for me" path, or a freshly minted keypair dir
  passed via `--operator-identity`); Rebuild and Live **reuse the existing operator**
  (`OPERATOR_NPUB` / the env profile's recorded one) — those worlds already know that
  pubkey.
- A real Proxmox host is reachable and each env's domains/proxy IP are known
  (operator-supplied or recorded in a profile). The Rebuild/Live profiles are listed
  under `test-proxmox-rebuild` / `test-proxmox-live` in `.envs.yml` (repo root).
- **Root access to the PVE host** (a door key already authorized, or a console/root
  password). A fresh profile mints a NEW door key, and `install --non-interactive` bails until that
  key is in the host's `/root/.ssh/authorized_keys` — see step 2.
- **DNS provider credentials** for the test zone. `build` owns DNS (`--manage-dns`) and
  needs them stored for the profile; see the field notes.
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

## The table this skill fills

Seeded by `release-prepare`; each row is ✅ only on its own evidence below.

```markdown
| Provider | Env | Test | Status |
| --- | --- | --- | --- |
| Proxmox | fresh.freehold.technology | Fresh - Install | ⚪ Unverified |
| Proxmox | fresh.freehold.technology | Fresh - Teardown | ⚪ Unverified |
| Proxmox | fresh.freehold.technology | Fresh - Build | ⚪ Unverified |
| Proxmox | fresh.freehold.technology | Fresh - Uninstall | ⚪ Unverified |
| Proxmox | rebuild.freehold.technology | Rebuild - Teardown | ⚪ Unverified |
| Proxmox | rebuild.freehold.technology | Rebuild - Build | ⚪ Unverified |
| Proxmox | live | Live - Update | ⚪ Unverified |
```

## Workflow

> **Order the tests around the CF gate.** The Fresh env's first build waits on DNS-01
> propagation — the long pole (~10–45 min: the provider API accepts the challenge TXT
> immediately while the authoritative NS keeps answering NXDOMAIN). Start Fresh's
> install + first build FIRST (detached, retrying — the resumable order reuses the
> challenge), and while the NS is still NXDOMAIN, move on to Rebuild and Live:
> neither needs the DNS gate (the rebuild env's cert is already issued; the live
> update issues nothing). Circle back to Fresh when the TXT resolves — re-run
> `build`, it installs the cert and continues — and finish the Fresh rows last.
> Never let the Fresh wait idle the whole run: the other tests are not blocked by it.
>
> **Never probe the challenge name through a caching resolver while it is negative.**
> An NXDOMAIN answer is cached (negative TTL — minutes to an hour) by 1.1.1.1/8.8.8.8
> and public DoH resolvers alike, and a cached no-hit can outlive the actual
> propagation — poisoning later checks and (in principle) the ACME validation path.
> While the record is negative, check either the provider's API (the record exists?)
> or the AUTHORITATIVE NS directly (`@<zone NS>`, uncached); the build's own
> propagation check already targets the authoritative NS. Hold the fresh build's
> next retry until the other tests' rows are done, so its first ACME-triggering
> attempt runs on a settled box instead of interleaved with the other builds'
> terraform work (the host-side tf dir locks — concurrent builds contest it).
>
> **Track the run as a todo list** — one item per prep step + per table row, in the
> order they run: assets restored → Fresh install → Fresh build (waiting on CF) →
> Rebuild - Teardown → Rebuild - Build → Live - Update → Fresh rows (cert → CPA
> reply → teardown → build → uninstall) → table updated → release-publish. Mark
> each in_progress when started and completed only on its own evidence, so the
> operator sees where the run is at any moment.

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
     install -m 755 "$dir/bin/$b" "$dir/release/$b"     # ResolveBins wants ../release/
   done
   # every sibling ResolveBins requires must exist, or `install` fails before it starts:
   test -x "$dir/bin/freehold-console" && test -x "$dir/bin/runner" \
     && test -x "$dir/release/freehold-console" && test -x "$dir/release/runner" \
     && test -x "$dir/release/freehold-agent-tools" && echo "sibling set ok"
   fh="$dir/bin/freehold"                                # the release CLI under test
   ```

2. **Fresh test — full lifecycle on a disposable world, with a NEW operator.**
   Domains: `relay.fresh.freehold.technology` / `cp.fresh.freehold.technology`. Fresh
   profile name, freshly minted operator identity (see Preconditions). Run `install`/`build`
   in the background (`nohup … > log 2>&1 &`) and poll the log — they run for minutes.
   ```bash
   name=<fresh-test-name>
   # a fresh profile mints a new door key; with --non-interactive the install prints it and bails.
   # Authorize it on the host, then re-run the SAME command:
   "$fh" install --name "$name" --host root@<pve-host> \
     --relay-domain relay.fresh.freehold.technology --cp-domain cp.fresh.freehold.technology \
     --proxy-ip <ip/cidr> --operator-pubkey <new-64-hex> \
     --operator-identity <new-dir> --litellm-provider-key <key> \
     --confirm-shared-pool --non-interactive
   #   (drop --confirm-shared-pool if the host's pool is empty; add --confirm-storage
   #    only when the host has no usable storage)
   # On the host: echo '<printed ssh-ed25519 line>' >> /root/.ssh/authorized_keys
   "$fh" build --config <profile-config> --manage-dns \
     --litellm-provider-key <key> --non-interactive     # install lands CP-only; build brings the world up
   ```
   `build` needs a DNS-01 credential stored *for the profile*. The shipped `dns-cred`
   writes to the **base** state dir (and seals to the base ops identity), not the profile's,
   so for a profile-scoped world it is not usable as-is — either run `build` once WITHOUT
   `--non-interactive` and let its prompt store the credential in the profile state dir, or place the
   sealed `dns-provider-{relay,cp}.json` under `<profile-state>/control-plane/`. See field
   notes.

   Per-row verdicts:
   - **Fresh - Install** ✅ iff install + build complete, the CP is healthy, and the CPA
     replies in the relay (post in `#freehold` with the CPA's pubkey in a `p` tag — see
     Field notes — wait for the reply, capture request + reply).
   - **Fresh - Teardown**: `"$fh" teardown --config <profile-config> --non-interactive`; ✅ iff every world guest is gone
     (relay + k3s LXC destroyed) and the CP is still healthy.
   - **Fresh - Build**: `"$fh" build --config <profile-config>`; ✅ iff the CPA replies again **and** the version
     ping (step 4) shows the expected version.
   - **Fresh - Uninstall**: `"$fh" uninstall --name "$name" --non-interactive`; ✅ iff the CP, runner,
     door, and every guest are removed (no `<name>-*` guests remain, the DOOR_SPEC key is
     gone).

3. **Rebuild test — teardown + build the persistent env; it stays running.**
   The env (`relay/cp.rebuild.freehold.technology`) is left running by the previous
   release's test run, so its world was built by an **old release** — this exercises
   teardown of old-version state. Reuse the existing operator and the env's own profile;
   drive everything through the release CLI. **Never uninstall this env.**
   ```bash
   "$fh" teardown --config <rebuild-profile-config> --non-interactive
   "$fh" build    --config <rebuild-profile-config> --non-interactive
   ```
   Per-row verdicts:
   - **Rebuild - Teardown** ✅ iff every world guest is down (relay + k3s LXC destroyed)
     and the CP is still healthy.
   - **Rebuild - Build** ✅ iff the world comes back, the CPA replies in the relay, **and**
     the version ping (step 4) shows the expected version.
   Leave the world running when done — the next release's test tears it down.

4. **Version ping — every row that ends with the world running gets one.**
   After Fresh - Build, Rebuild - Build, and Live - Update, ping the running CP and verify
   the version is the expected one:
   ```bash
   "$fh" update --check --config <profile-config>   # prints "CP version:" + pending migrations
   ```
   Expected = the release under test (unless the env is already on something newer). A
   world that comes back on the wrong version — or with unexpected pending migrations —
   fails its row even if the command exited 0.

5. **Live test — update the always-running env.**
   The live env is always running (left by the previous cycle); reuse its profile and the
   existing operator. This verifies `update` runs and does what is expected to a running
   world:
   ```bash
   "$fh" update --check   --config <live-profile-config>   # record the before-state
   "$fh" update --ref main --config <live-profile-config> --non-interactive
   "$fh" update --check   --config <live-profile-config>   # version + migrations after
   "$fh" status    --config <live-profile-config>
   ```
   `--ref main` sandbox-clones `main` and builds on the box. Pre-release candidates are
   plain `vX.Y.Z` tags marked prerelease — the update verb has no channel that selects
   them — so the Live row tests a **main build**, not the candidate's downloaded assets.
   The candidate is `main`'s tip at prepare time, so it is the same code the assets were
   cut from (this trade-off is accepted for now).
   - **Live - Update** ✅ iff the update completed, `update --check` shows the expected
     version with 0 unexpected pending migrations, `status` is healthy, and the CPA still
     replies in the relay. Already-on-target is fine: the run must report up-to-date
     cleanly — that still proves the flow.

6. **Update the release table** — change only the Proxmox rows' Status cells, leave every
   other row, the Env/Test columns, the title, assets, and the prerelease flag untouched:
   ```bash
   gh release view "$tag" --json body -q .body > /tmp/opencode/body.md
   # edit the Proxmox Status cells in /tmp/opencode/body.md (⚪/✅/❌), preserving the rest
   gh release edit "$tag" --notes-file /tmp/opencode/body.md
   gh release view "$tag" --json body -q .body | sed -n '/^## Test status/,$p'
   ```

7. **If every row you filled is ✅, run `release-publish`** — the next step of this
   release's flow, not an optional extra. Invoke the skill; it re-gates on the table, gets
   the operator's go-ahead, and flips the release to final (with the Latest badge). A
   fully-green table left unpromoted is how a release gets stranded behind a stale Latest
   badge — don't omit it. If any row is ⚪/❌ (or another provider's rows are still
   unverified), stop here and report the failing state instead.

## Field notes (learned on the v0.7.0 first run)

### CPA-reply verification (the v0.7.2 run's traps)

- **buzz-acp triggers on a `#p` tag, not message text.** A kind-9 channel message spawns an
  agent session only when its tags carry the agent's pubkey as `p` (`[["h", channelID],
  ["p", <agent-pubkey>]]`); "@freehold …" in the CONTENT alone is never seen. Post with the
  mention tag, poll `kinds:9, "#h":<id>` for a reply whose author differs from yours.
- **The CPA pod subscribes with a since-filter — ping only AFTER it subscribes.** A ping
  sent before the pod's `subscribed to channel <#freehold>` log line is never delivered.
  Pods take ~5–10 min after a build to come up and subscribe; check
  `kubectl -n agents get pods` (guest 111-class) + the pod log, then ping. The CPA's pubkey
  comes from `freehold status` (the `freehold` agent row).
- **`dns-cred` always writes the BASE state dir** (even with `--config <profile>`; product
  gap confirmed) — and it CLOBBERS the base creds for the base world's domains (restore
  them by re-running `dns-cred` against the base config afterwards). For a profile-scoped
  build, seal the cred to the PROFILE's agent-ops identity yourself (the
  `cert.SaveCreds` shape: `{"provider", "sealed", "aad":"cert-dns-<slot>"}`, sealed with
  the profile identity's X25519 pubkey) and place it at
  `<profile-state>/control-plane/dns-provider-<slot>.json`.
- **A thin pool at 100% puts guests into ext4 emergency-ro.** Writes fail (I/O error -5 →
  `dpkg was interrupted`, `docker: not found`, read-only `/var/lib`). Fix on the host:
  `lvextend -L +<n>G pve/<pool>` (the VG usually has free space), then reboot the affected
  guest and run `dpkg --configure -a` inside it before re-running `build`. Consider
  `thin_pool_autoextend_threshold` (<100) so the pool grows itself.
- **Never let a shell timeout kill a build.** A killed build leaves half-provisioned guests
  (interrupted dpkg). Run every long verb detached (`setsid nohup … & disown`, log + poll),
  so the calling shell vanishing can't kill it.

### Update-path notes (live test)

- **A re-install over a leftover durable plane records the WRONG runner pubkey.** When
  `install` says "reconnecting to your previous freehold data", the box-side runner package
  is minted FRESH while the CP adopts the plane's OLD co-located runner — the profile's
  `[runner] pubkey` (what the world-config carries as the signing audience) then names a
  runner that no longer exists, and every world-build exec dies with
  `unauthorized: signature does not verify`. Fix: compare the profile config's `[runner]`
  pubkey against the CP guest's `state.json` runner record (`pct exec <cp-vmid> -- …`) —
  when they differ, the world is half-old/half-new; `uninstall --remove-data` (local runner
  required) and re-install on the clean plane. Don't hand-patch the pubkey — the rest of
  the plane's state is the old world's too.
- **The door SAs raced the namespaces on the first services apply (fixed on main after
  v0.7.3):** the first build's services apply can 500 with
  `NotFound namespaces [caddy]`; the namespaces then exist, so a plain `build` retry
  passes. A released candidate whose assets predate the fix still tests fine with the
  retry, but a RE-CUT carries the fix.
- **The update verb has no rc channel.** Pre-release candidates are plain `vX.Y.Z` tags
  marked prerelease, and `update`'s release channels select tags by shape — the former
  `--rc` channel matched only `vX.Y.Z-rc.N` tags and could never see a candidate, so it
  is gone. The live env tracks `main` (`update --ref main`, a sandbox clone+build on the
  box); a stable-tracking env uses `--stable` or its recorded channel.
- **A CP state missing `agent_tools` coords fails every update.** Envs bootstrapped before
  the coords were recorded report `agent_tools_pubkey=""` in the world summary, and the
  update's migration window needs them (`no freehold-agent-tools coords recorded`). Fix per
  env: print the server's pubkey (`freehold-agent-tools identity --state-dir
  /srv/data/cp/agent-tools` inside the CP LXC), put
  `agent_tools_pubkey = '<64-hex>'` in the profile config's ROOT section (TOML: a key
  appended after a `[section]` header belongs to that section and silently doesn't parse as
  root), and re-run the update. Filing the adoption asymmetry (the URL is guarded against
  empty overwrite, the pubkey is not a hazard — but the state staying empty forever on old
  envs is the gap) is a named follow-up.
- **The version ping is `freehold update --check`** — `CP version:` + pending migrations;
  run it after every step that leaves the world running.

### Earlier notes (v0.7.0)

- **`gh release download` drops the execute bit** — `chmod +x` the binaries before the
  `test -x` check, or step 1 fails for the wrong reason.
- **A fresh profile needs a new host door.** `install --non-interactive` refuses until the key it prints
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
  product's own `teardown`) before a fresh build. The rebuild env's teardown → build is the
  product's own teardown, so its state stays coherent.
- **Thin-box lifecycle asymmetries.** From a thin box with no local runner:
  `teardown` works (the CP drives its own runner); `uninstall` does **not** — it needs a
  local runner, and its transient fallback runs `pct destroy` on a still-running guest and
  fails. Start the release's own `runner serve --state-dir <profile-state>/runner/<target>
  --addr 127.0.0.1:8787` to give uninstall its local runner.
- **Uninstall can skip relay/k3s after a teardown→rebuild.** `teardown` clears the recorded
  relay/k3s vmids ("the next build re-creates them"), but `build` does not re-record them in
  the operator config, so a later `uninstall` reports "never created (no vmid recorded)" and
  leaves those LXCs running. Verify with `pct list | grep <name>` after uninstall — the
  Fresh - Uninstall row fails if guests remain. (Product gap to file.)
- **A brand-new `v*` hub: re-cutting a failed pre-release.** If the candidate fails and
  `main` is fixed, the operator may say to move the pre-release tag to the new `main` tip and
  rebuild assets. That means (only on that explicit go-ahead): `gh release delete <tag>
  --yes` (tag survives — this `--yes` is gh's own flag), `git tag -f -a <tag> <main-sha>`, `git push -f origin <tag>` (CI
  `release.yml` builds a fresh draft), then re-publish it as a pre-release with the same
  notes + table (`gh release edit --draft=false --prerelease …`).
- **Running the test itself.** Put long installs/builds in the background with a log and
  poll; the operator box usually can't resolve the world's public DNS until the CP's records
  land, so talk to guests over the host or the LAN IP when needed.

## Hard rules

- **Test the downloaded pre-release assets**, never a local `just build`.
- **Destructive on Fresh; persistent on Rebuild/Live.** Only the Fresh world is disposable —
  never uninstall or destroy the rebuild or live envs. Get the operator's go-ahead first.
- **A NEW operator for the Fresh install; the existing operator for Rebuild and Live.**
- **Version-check every row that ends running** (Fresh - Build, Rebuild - Build, Live -
  Update) — a row is ✅ only when the running world reports the expected version.
- **`.env.test` never leaves the box and is never committed** — it is gitignored; never
  echo its values into logs, evidence, or the release body.
- **Never move/create/delete a tag, never change assets, title, or `isPrerelease`; only the
  status-table cells are yours to edit.** The one exception is an explicit operator go-ahead
  to re-cut a failed candidate onto the fixed `main` tip (see the field notes).
- **Never promote directly — promotion is `release-publish`'s job, gated on the
  test-status table and the operator's go-ahead.** When every row you filled is ✅, hand
  off to `release-publish` (step 7) rather than leaving the table unpublished.
- Keep the table format and icons exact (`⚪ Unverified` · `✅ Passed` · `❌ Failed`) so
  `release-publish` can parse it. Don't drop rows you didn't test.
- Record the evidence (checksums, per-step output, agent request + reply, version pings)
  when reporting to the operator.
