#!/bin/bash
# exec-first LXC management for the k3s node (C7): apply = create-if-missing +
# bring-up; destroy = stop + destroy. Runs ON the PVE host via the runner.
set -euo pipefail
VMID="$1"; TPL="$2"; NODE="$3"; ACTION="$4"
if [ "$ACTION" = "destroy" ]; then
  if pct status "$VMID" >/dev/null 2>&1; then
    pct stop "$VMID" 2>/dev/null || true
    timeout 180 pct destroy "$VMID" 2>&1 | tail -1 || pct destroy "$VMID" --force 2>&1 | tail -1
  fi
  echo "k3s LXC $VMID destroyed"
  exit 0
fi
# apply: idempotent — reuse an existing container, create only if absent
if ! pct status "$VMID" >/dev/null 2>&1; then
  pct create "$VMID" "$TPL" --hostname k3s --memory 4096 --cores 4 \
    --net0 name=eth0,bridge=vmbr0,ip=192.168.30.243/24,gw=192.168.30.1 \
    --rootfs local-lvm:20 --unprivileged 1 --features nesting=1,keyctl=1 \
    --start 1 2>&1 | tail -1
  sleep 15
fi
"$(dirname "$0")/k3s-bringup.sh" "$VMID"
echo "k3s LXC $VMID ready"
