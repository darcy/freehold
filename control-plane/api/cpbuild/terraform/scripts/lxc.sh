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

NET="name=eth0,bridge=vmbr0,ip=dhcp,type=veth"
if [ "$IP" != "-" ] && [ "$GW" != "-" ]; then NET="name=eth0,bridge=vmbr0,ip=${IP},gw=${GW},type=veth"; fi
[ -n "$MP" ] && MP=" $MP" || MP=""
pct create "$VMID" "local:vztmpl/${TPL}" \
  --rootfs "local-lvm:${ROOTFS}" --memory "$MEM" --hostname "$HOSTNAME" \
  --unprivileged 1 --features fuse=1,keyctl=1,nesting=1 \
  --net0 "$NET"${MP} 2>&1 | tail -1
echo "lxc $VMID ($HOSTNAME) created"
