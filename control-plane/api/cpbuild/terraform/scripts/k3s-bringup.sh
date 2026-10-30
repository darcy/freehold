#!/bin/bash
# k3s bring-up executed ON the PVE host (via the runner's exec) into an
# already-created k3s LXC. Reconciled source of the hardened install (deps,
# k3s pinned version, the userns feature-gate AFTER `server`, wait-for-API,
# durable local-path carve-out, cluster-DNS public upstream, fireworks pin).
# idempotent: only installs k3s if absent; re-asserts the durable carve-outs.
#
# The local-path carve-out is a SOURCE manifest (terraform/manifests/
# freehold-local-path.yaml), not an in-place patch of k3s's bundled addon: the
# bundled local-storage addon is disabled so a guest boot or host reset cannot
# re-render the stock rootfs path over it.
set -euo pipefail
VMID="$1"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
MANIFEST="$SCRIPT_DIR/../manifests/freehold-local-path.yaml"

[ -f "$MANIFEST" ] || {
  echo "FATAL: missing freehold-local-path source manifest at $MANIFEST"
  exit 1
}

# host side (this box)
modprobe br_netfilter 2>/dev/null || true
modprobe overlay 2>/dev/null || true
sysctl -w net.netfilter.nf_conntrack_max=131072 >/dev/null || true

# Stage the source manifest in the guest before the install so the in-guest
# step can install it after k3s is up (the manifests dir may not exist yet).
pct push "$VMID" "$MANIFEST" /tmp/freehold-local-storage.yaml --perms 600

pct exec "$VMID" -- bash -c '
  set -euo pipefail
  export PATH=/usr/local/bin:/root/.cargo/bin:$PATH
  # The fresh guest resolv.conf points at the LAN router (DHCP/PVE default),
  # which on home labs does not resolve - apt/curl then die with "Temporary
  # failure resolving". Pin public resolvers before fetching - but NEVER
  # clobber the CP resolver: this script re-runs on every build (build_epoch),
  # and the services-phase re-run lands AFTER worldDNS has pointed this guest
  # at the CP resolver (the subnet .12 convention) - a public rewrite there
  # undoes the point, and the pods CoreDNS forwards through ride the node
  # resolver for their answer to the relay host. A --cp-ip override outside
  # the subnet falls back to today behavior (the point is re-applied next
  # build, then clobbered again).
  CPDNS=$(ip -4 addr show eth0 | awk "/inet /{split(\$2,a,\"/\"); print a[1]}" | head -1 | cut -d. -f1-3).12
  if ! grep -qs "^nameserver $CPDNS" /etc/resolv.conf; then
    echo nameserver 1.1.1.1 > /etc/resolv.conf
    echo nameserver 8.8.8.8 >> /etc/resolv.conf
  fi
  K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
  DEBIAN_FRONTEND=noninteractive apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl jq
  if ! command -v k3s >/dev/null 2>&1; then
    curl -sfL https://get.k3s.io -o /tmp/k3s-install.sh
    INSTALL_K3S_VERSION=v1.36.4+k3s1 INSTALL_K3S_EXEC="server --disable traefik --disable servicelb --disable local-storage --kubelet-arg feature-gates=KubeletInUserNamespace=true" sh /tmp/k3s-install.sh
  fi
  # The unit MUST carry the flags AFTER the subcommand (k3s rejects them
  # before). local-storage is disabled so k3s never owns the provisioner
  # manifest; the source manifest is installed below.
  if ! grep -q -- "--disable local-storage" /etc/systemd/system/k3s.service \
     || ! grep -q KubeletInUserNamespace /etc/systemd/system/k3s.service; then
    cat > /etc/systemd/system/k3s.service <<UNIT
[Unit]
Description=Lightweight Kubernetes
Documentation=https://k3s.io
Wants=network-online.target
After=network-online.target
[Install]
WantedBy=multi-user.target
[Service]
Type=notify
EnvironmentFile=-/etc/default/%N
EnvironmentFile=-/etc/sysconfig/%N
EnvironmentFile=-/etc/systemd/system/k3s.service.env
KillMode=process
Delegate=yes
User=root
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/k3s server --disable traefik --disable servicelb --disable local-storage --kubelet-arg feature-gates=KubeletInUserNamespace=true
UNIT
    systemctl daemon-reload
    systemctl restart k3s
  fi
  # WAIT for the k3s API before any kubectl op (the install returns early)
  for i in $(seq 1 30); do
    $K get nodes >/dev/null 2>&1 && break
    sleep 10
  done
  # durable volume root: the locked carve-out (never the default local-path root)
  mkdir -p /srv/data/k8s-volumes
  # cluster DNS: forward through the NODE resolver (worldDNS points it at the
  # CP resolver) - the split-horizon answer (the relay/cp public hosts ->
  # the edge IP) is what every pod wss dial rides; a public upstream answers
  # the relay host with the public IP, and behind a hosted gateway (Vultr)
  # that hairpin DNATs back into the dialing guest and never answers.
  # Convergence needs TWO independent legs: the configmap rewrite (content
  # guard - idempotent, skipped once the Corefile carries the node-resolver
  # forward) and the RESTART (resolver guard - the coredns pod snapshots the
  # node resolv.conf at START, so every build where the CP point is in force
  # re-reads it; skipping the restart leaves the pods on the start-time
  # public pin, the exact failure this exists to fix).
  $K get cm coredns -n kube-system -o jsonpath="{.data.Corefile}" > /tmp/corefile || true
  if [ -f /tmp/corefile ] && ! grep -q "forward . /etc/resolv.conf" /tmp/corefile; then
    sed -i -E "s|forward \..*|forward . /etc/resolv.conf|" /tmp/corefile
    $K create cm coredns -n kube-system --from-file=Corefile=/tmp/corefile --dry-run=client -o yaml | $K apply -f -
  fi
  if grep -qs "^nameserver $CPDNS" /etc/resolv.conf; then
    $K rollout restart deploy/coredns -n kube-system 2>/dev/null || true
  fi
  # The bundled local-storage addon is disabled above, so this source manifest
  # is the sole owner of the local-path provisioner and survives guest boots.
  # It uses a DIFFERENT addon name (freehold-local-path, not local-storage):
  # the disabled local-storage controller deletes its own manifest on boot and
  # would delete ours too if it shared that name.
  # The node is the k3s wildcard (DEFAULT_PATH_FOR_NON_LISTED_NODES), NOT "k3s"
  # - the node is named by hostname, and a wrong node name makes the
  # provisioner fail with "no node was specified" (nothing binds).
  cp /tmp/freehold-local-storage.yaml /var/lib/rancher/k3s/server/manifests/freehold-local-path.yaml
  chmod 600 /var/lib/rancher/k3s/server/manifests/freehold-local-path.yaml
  rm -f /tmp/freehold-local-storage.yaml
  $K rollout restart deploy/local-path-provisioner -n kube-system 2>/dev/null || true
  # VERIFY the active config so a silently-broken carve-out is a LOUD bring-up
  # failure, not unbacked PVs discovered by a later migration. The settle check
  # outlasts the disabled local-storage addon cleanup, which can delete
  # same-named resources just after the first apply.
  for i in $(seq 1 60); do
    if $K get cm local-path-config -n kube-system -o jsonpath="{.data.config\.json}" 2>/dev/null | grep -q /srv/data/k8s-volumes; then
      sleep 5
      $K get cm local-path-config -n kube-system -o jsonpath="{.data.config\.json}" 2>/dev/null | grep -q /srv/data/k8s-volumes \
        && [ -f /var/lib/rancher/k3s/server/manifests/freehold-local-path.yaml ] \
        && $K -n kube-system get addons.k3s.cattle.io freehold-local-path >/dev/null 2>&1 \
        && $K get deploy local-path-provisioner -n kube-system -o jsonpath="{.status.readyReplicas}" 2>/dev/null | grep -qx 1 \
        && exit 0
    fi
    sleep 5
  done
  echo "FATAL: the active local-path provisioner config does not pin /srv/data/k8s-volumes - PVs would land unbacked on the rootfs"
  exit 1
'
echo "k3s bring-up reconciled"
