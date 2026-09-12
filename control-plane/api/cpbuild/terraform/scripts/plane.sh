#!/bin/bash
# exec-first durable volume plane for the CP-owned world. Mirrors the Go
# planebase/drive LVM-thin path (EnsureLvmLv + ChownGuestUid) byte-for-byte so
# terraform adopts the SAME /freehold/<dom> LVs the CP creates, plan-clean.
# Tenants:
#   relay-docker-root -> /freehold/<DOM>/docker-root  -> /var/lib/docker  (relay)
#   relay-deploy      -> /freehold/<DOM>/deploy       -> /srv/data/relay  (relay)
#   cp                -> /freehold/<DOM>/cp           -> /srv/data/cp     (cp)
#   k3s-volumes       -> /freehold/<DOM>/k3s-volumes  -> /srv/data/k8s-volumes (k3s)
#
# Usage: plane.sh <DOM> <VG> <THINPOOL> <LVSIZE_GB> <POOLSIZE_GB>
#   DOM  = the DASHED relay host's domain (e.g. relay-librem-freehold-technology)
#   creates/mounts/chowns each LV if absent; idempotent. No destroy action:
#   the durable plane survives teardown by design (the --data path is separate).
set -euo pipefail
DOM="$1"; VG="${2:-pve}"; POOL="${3:-}"; LVSIZE="${4:-8}"; POOLSIZE="${5:-40}"
BASE="/freehold/${DOM}"
[ -n "$POOL" ] && POOL_ARG="-T ${VG}/${POOL}" || POOL_ARG=""

ensure_lv() {  # name dir
  local lv="$1" dir="$2"; local dev="/dev/${VG}/${lv}" path="${BASE}/${dir}"
  # pool: named thinPool adopted when present, carved when not; empty reuses.
  if ! lvs --noheadings -o lv_name "${VG}/${lv}" >/dev/null 2>&1; then
    if [ -n "$POOL" ]; then
      lvs "${VG}/${POOL}" >/dev/null 2>&1 || lvcreate -L "${POOLSIZE}G" -T "${VG}/${POOL}"
      lvcreate -V "${LVSIZE}G" -T "${VG}/${POOL}" -n "$lv"
    else
      # reuse the VG's existing thin pool, else carve freehold-thin
      local P; P=$(lvs --noheadings -o pool_lv "${VG}" 2>/dev/null | tr -d ' ' | head -1)
      [ -n "$P" ] && P_ARG="-T ${VG}/${P}" || { P=freehold-thin; lvs "${VG}/${P}" >/dev/null 2>&1 || lvcreate -L "${POOLSIZE}G" -T "${VG}/${P}"; }
      lvcreate -V "${LVSIZE}G" -T "${VG}/${P}" -n "$lv"
    fi
  fi
  # mkfs gated on blkid (recovers a partial create); mount gated on mountpoint
  [ -n "$(blkid -s TYPE -o value "$dev" 2>/dev/null)" ] || { sleep 1; mkfs.ext4 -q "$dev"; }
  mkdir -p "$path"
  mountpoint -q "$path" || { sleep 1; mount "$dev" "$path"; }
  grep -qxF "$dev $path ext4 defaults 0 2" /etc/fstab || echo "$dev $path ext4 defaults 0 2" >> /etc/fstab
  # chown mount ROOT only (never -R) to the unprivileged-LXC shifted uid so the
  # guest can write it without re-rooting container-owned data below.
  chown 100000:100000 "$path"
}

ensure_lv "freehold-${DOM}-relay-docker-root" "docker-root"
ensure_lv "freehold-${DOM}-relay-deploy"      "deploy"
ensure_lv "freehold-${DOM}-cp"                "cp"
ensure_lv "freehold-${DOM}-k3s-volumes"       "k3s-volumes"
echo "durable plane ensured ($BASE)"
