# The backblaze skill — the off-site leg (restic) and its timer

This is Data's runbook for the freehold **backup verbs**: restic pushes of the whole
durable plane to an off-site repo (Backblaze B2, S3, sftp, …) and the host-side timers
that keep it nightly. The ask arrives as "set up backblaze backups", "did last night's
backup run?", or "prove the backup actually restores". Read-on-boot material: re-check
against the repo as the system evolves.

## The verbs run ON the control plane guest — through `cp-local-root`

Your pod routes **`exec` by target**: `cp-local-root` is local root exec on the CP
guest, where deploy-cp ships the verb surface. Every command below is that shape:

    exec("<cmd>", "cp-local-root")

with `<cmd>` always pinning both:

    --config /srv/data/cp/profile/config.toml --ssh-key /srv/data/cp/verb-ssh.key

## Recipes

**Run** (the whole plane → the repo; the first run is the expensive one, after that
incremental):

    exec("/srv/data/cp/bin/freehold backup run --config /srv/data/cp/profile/config.toml --ssh-key /srv/data/cp/verb-ssh.key", "cp-local-root")

- No deadline: the first push of a big plane can run long — a still-running exec is
  progress, not a hang. Re-running continues (restic is incremental).
- Quote what it prints: sources count, stored size, the repo URI.

**Verify** (the round-trip: does the repo answer and hold snapshots?):

    exec("/srv/data/cp/bin/freehold backup snapshots --config /srv/data/cp/profile/config.toml --ssh-key /srv/data/cp/verb-ssh.key", "cp-local-root")

**The timer** — install/re-render the host-side systemd timers (nightly
`freehold-backup.timer` at 04:00 host-local + a weekly `freehold-backup-verify.timer`
repo check, both Persistent):

    exec("/srv/data/cp/bin/freehold backup install-timer --config /srv/data/cp/profile/config.toml --ssh-key /srv/data/cp/verb-ssh.key", "cp-local-root")

- The timer is a RENDER of the backup verb's own restic line, never a fork — when the
  plane's mounts change (a new service's durable volume, a tenant added), **re-run
  install-timer** so the host's nightly scope follows. Check it whenever the plane
  grows.
- The scripts live on the host (`/srv/nobackup/freehold-backup*.sh`) and carry no
  credentials — they reference the 0600 files `backup init` pushed and **fail loudly
  with a named reason** until those exist. If the journal says "no
  /srv/nobackup/freehold-restic-password — run freehold backup init", the answer is
  the operator's box-side init, not anything you can do.

**Restore drill** — the proof the leg works. Read-only against the plane (restores to
a scratch dir on the host, then cleans up): via `pve-ssh-root`,

    exec("RESTIC_PASSWORD_FILE=/srv/nobackup/freehold-restic-password . /srv/nobackup/freehold-restic.env 2>/dev/null; restic -r <uri> restore latest --target /srv/nobackup/restore-drill --host <world>", "pve-ssh-root")

then check the planted proof landed, then `rm -rf /srv/nobackup/restore-drill`. A
backup leg that has never been restored is a rumor, not a backup — run the drill
when the operator asks for proof and after any repo/backend change.

## The rules

- **`backup init --env` never runs from you.** The credentials (B2 application key,
  AWS keys) arrive on the operator's box, typed by the operator, and ride to the host
  as 0600 files. Credentials never appear in chat, in a command you build, or in
  anything you print — the env file is a PATH you reference, like the cp-verb key.
- **Retention and prune are the operator's call.** `restic forget`/`prune` deletes
  off-site history — never agent-initiated. If the operator asks for a retention
  policy, propose it in chat and let them say the word.
- The password + env files exist in exactly two places (the operator's box profile
  dir + the host's `/srv/nobackup`, both 0600, both outside every backup scope). If
  BOTH are gone, the repo's key exists nowhere you can reach — say so plainly and
  early, because there is no recovery from that after the fact.
- Everything is relay-audited: the verbs ride `cp-local-root`, raw probes ride
  `pve-ssh-root`, and a restore drill is the only place raw restic is ever correct.
