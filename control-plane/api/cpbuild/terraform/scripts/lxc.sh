#!/bin/bash
# exec-first LXC management, adopting the SAME pct create shape BootstrapProxmoxLxc
# produces (unprivileged, fuse+keyctl+nesting, static-IP-or-dhcp net0, `--mpN`
# durable mounts with backup=1). apply = create-if-missing (idempotent adopt);
# destroy = stop + destroy. Runs ON the PVE host via the runner.
#
# Usage: lxc.sh <VMID> <TEMPLATE> <NODE> <HOSTNAME> <MEMORY_MB> <ROOTFS_GB>
#               <IP_CIDR|-> <GW|-> <MOUNT_ARGS|-> <ACTION>
#   MOUNT_ARGS = space-separated `--mpN=...` joined into the create ("" = none).
set -euo pipefail
VMID="$1"; TPL="$2"; NODE="$3"; HOSTNAME="$4"; MEM="${5:-2048}"; ROOTFS="${6:-16}"; IP="${7:--}"; GW="${8:--}"; MP="${9:-}"; ACTION="${10:-apply}"

if [ "$ACTION" = "destroy" ]; then
  if pct status "$VMID" >/dev/null 2>&1; then
    pct stop "$VMID" --skiplock 2>/dev/null || true
    timeout 180 pct destroy "$VMID" --skiplock 2>&1 | tail -1 || pct destroy "$VMID" --force 2>&1 | tail -1
  fi
  echo "lxc $VMID ($HOSTNAME) destroyed"; exit 0
fi

if pct status "$VMID" >/dev/null 2>&1; then
  echo "lxc $VMID ($HOSTNAME) already exists — adopted"; exit 0
fi

# An empty template resolves to the newest Debian standard template ALREADY in
# the store — the same rule the box's own births use (EnsureDebianTemplate), so
# a tf-created guest rides the SAME distro as its siblings on any host (a
# hard-coded ref dies when the host's PVE predates it: 8.4.5 cannot create
# trixie guests). Arch-filtered: the store mixes amd64/arm64 rows and an arm64
# guest cannot spawn on x86_64.
ARCH=$(dpkg --print-architecture 2>/dev/null || uname -m)
[ "$ARCH" = "x86_64" ] && ARCH=amd64
[ "$ARCH" = "aarch64" ] && ARCH=arm64
if [ -z "$TPL" ]; then
  TPL=$(pvesm list local 2>/dev/null | awk -v a="_${ARCH}.tar." '$1 ~ /local:vztmpl\/debian-/ && $0 ~ /standard_/ && index($1, a) {print $1}' | sed 's|local:vztmpl/||' | sort -V | tail -1)
  [ -n "$TPL" ] || { echo "no debian-${ARCH} template in the local store — pveam download one" >&2; exit 1; }
fi

NET="name=eth0,bridge=vmbr0,ip=dhcp,type=veth"
if [ "$IP" != "-" ] && [ "$GW" != "-" ]; then NET="name=eth0,bridge=vmbr0,ip=${IP},gw=${GW},type=veth"; fi
[ -n "$MP" ] && MP=" $MP" || MP=""
pct create "$VMID" "local:vztmpl/${TPL}" \
  --rootfs "local-lvm:${ROOTFS}" --memory "$MEM" --hostname "$HOSTNAME" \
  --unprivileged 1 --features fuse=1,keyctl=1,nesting=1 \
  --net0 "$NET"${MP} 2>&1 | tail -1
echo "lxc $VMID ($HOSTNAME) created"
