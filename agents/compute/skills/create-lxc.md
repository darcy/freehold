# The create-lxc skill — standing up a guest slot on the PVE host

This is Compute's runbook for the most common ask: "create an LXC called X" —
from you (a user or freehold talking to you directly) or relayed through the
CPA. It binds the recipe so every guest lands the same way: named, on DHCP,
findable by name, with a sudo-capable account and a door handoff. It is
read-on-boot material: re-check it against the repo as the system evolves.

## The name is required

No name, no create. The name must be a valid hostname — lowercase
`[a-z0-9-]`, no leading/trailing dash (the same shape the control plane's DNS
records enforce) — because it becomes the `pct` hostname, the resolver record,
and the door's target label in one stroke. Refuse a request without one; a
guest nobody can name is a guest nobody can find.

## The recipe

Everything here runs through **`pve-ssh-root`** (root on the PVE host). Say
the vmid you actually got (`pct list` for the next free one); never invent one.

1. **Template** — the newest Debian standard template matching the host arch
   (`uname -m` → amd64/arm64; an arm64 guest cannot spawn on x86_64). Reuse
   first: `pvesm list local` — pick the newest `debian-*standard_<arch>.tar.*`
   already in the store. If none: `pveam update`, then
   `pveam available --section system` — pick the newest
   `debian-…standard_<arch>` match and `pveam download local <name>`.
2. **Create** — defaults 2 cores / 2048 MB / 8 GB rootfs (the caller may ask
   for more):

       pct create <vmid> local:vztmpl/<tpl> --rootfs local-lvm:8 --memory 2048 --cores 2 \
         --hostname <name> --unprivileged 1 --features fuse=1,keyctl=1,nesting=1 \
         --net0 name=eth0,bridge=vmbr0,ip=dhcp,type=veth

   **No root password.** The root account stays locked; the ways into this
   guest are host-side `pct exec` and the door's own key (below). Nothing
   else — a password that never exists cannot leak.
3. **Boot + address** — `pct start <vmid>`, then
   `pct exec <vmid> -- hostname -I` for the DHCP address the guest actually
   got (first entry, eth0).
4. **The account** — `lxcadmin` with passwordless sudo, and no password:

       pct exec <vmid> -- useradd -m -s /bin/bash lxcadmin
       pct exec <vmid> -- sh -c 'echo "lxcadmin ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/lxcadmin; chmod 0440 /etc/sudoers.d/lxcadmin; visudo -cf /etc/sudoers.d/lxcadmin'

   The account is key-only by design (its password field stays locked). If
   `visudo -c` reports a parse error, fix it before moving on — a broken
   sudoers drop can take sudo down for the whole guest.
5. **The pin** — register `<name> → <ip>` in the control plane's resolver so
   every other agent and guest can find the box by name. On the CP guest
   (via `pct exec <cp-vmid>` through `pve-ssh-root`):

       freehold-console --state-dir /srv/data/cp/control-plane dns add <name> <ip> "compute create-lxc"

   This is the state-backed record — the same mechanism the build uses to
   name guests: it renders into the dnsmasq addn-hosts, reloads the resolver,
   and survives a re-converge. Do NOT hand-edit dnsmasq files instead — a
   build's render would not know the record, and the pin would be drift.

## The door (handoff to freehold)

The guest is up but has no runner yet — capability is provisioned through the
CPA's `provision_runner`, never by you (you hold no CP toolset, and grants
are not yours to make). Report in the thread — name, vmid, IP, pinned — and
ask freehold to stand the door up:

- `provision_runner(name=<name>-ssh-lxcadmin, kind=ssh,
  address=lxcadmin@<name>, grant_to=<the agent who will work the box>)`
- The runner name is `<target>-<protocol>-<identity>` — never named for the
  consumer (the granting skill's rule). `lxcadmin` IS the identity level.
- The address is the PINNED NAME, not the raw IP — that is why the pin runs
  before the door: a re-IPed guest keeps its door address.

The tool returns the door's own **public** key line — the door mints its own
keypair; no password and no operator key is ever involved. Install it on the
guest yourself: for boxes you created, YOU are the installer (the granting
skill's "operator installs it" step collapses to zero hops here):

    pct exec <vmid> -- sh -c 'install -d -m 700 -o lxcadmin -g lxcadmin /home/lxcadmin/.ssh; grep -qF "<key-blob>" /home/lxcadmin/.ssh/authorized_keys || echo "<pubkey line>" >> /home/lxcadmin/.ssh/authorized_keys'

(The idempotent grep-guard matches how the build authorizes its own doors.)

Then verify before claiming: an exec probe through the door — `whoami` says
`lxcadmin`, `sudo -n true` works, `hostname` says `<name>` — and the door's
self-check goes 🟢. If freehold's confirm discipline is waiting on the
operator's yes, say the door is WAITING, not that it works.

## The check-ins

A new guest is new surface. Data's question fires on your report: "should
this be backed up?" — name it in the thread; "no" is a valid, final answer; a
silent gap is a failure. Exposure is Network's lane and you have not touched
it: a DHCP guest with a resolver record is internal-only. If the caller wants
the guest reachable from outside, say so plainly and hand the ask to Network.

## Tone

State the vmid, the IP, and the pinned name you actually got; never claim a
guest or a door you did not verify. Reference secrets by name only — this
runbook never touches one.
