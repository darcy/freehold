#!/bin/bash
# k3s bring-up executed ON the PVE host (via the runner's exec) after the LXC
# exists. Encodes the spike-verified gotchas (POC_CHUNK3 0.08):
#   - host kernel modules + conntrack (PVE-host side)
#   - the userns feature-gate flag placed AFTER `server` in the unit
#   - coredns public upstream + api.fireworks.ai hostAliases pin (the LAN
#     router's DNS answers that name with the tailnet proxy IP — a router oddity)
set -euo pipefail
VMID="$1"
GUEST_IP=192.168.30.243

# host side (this box)
modprobe br_netfilter 2>/dev/null || true
modprobe overlay 2>/dev/null || true
sysctl -w net.netfilter.nf_conntrack_max=131072 >/dev/null

pct exec "$VMID" -- bash -c '
  set -euo pipefail
  export PATH=/usr/local/bin:/root/.cargo/bin:$PATH
  K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
  # deps incl. the rust toolchain for later agent-free tooling
  DEBIAN_FRONTEND=noninteractive apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl jq
  if ! command -v k3s >/dev/null 2>&1; then
    curl -sfL https://get.k3s.io -o /tmp/k3s-install.sh
    INSTALL_K3S_VERSION=v1.36.4+k3s1 INSTALL_K3S_EXEC="server --kubelet-arg feature-gates=KubeletInUserNamespace=true" sh /tmp/k3s-install.sh
  fi
  # the unit MUST carry the flag AFTER the subcommand (k3s rejects it before)
  if ! grep -q KubeletInUserNamespace /etc/systemd/system/k3s.service; then
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
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/k3s server --kubelet-arg feature-gates=KubeletInUserNamespace=true
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
UNIT
    systemctl daemon-reload
    systemctl restart k3s
  fi
  # WAIT for the k3s API before any kubectl op (the install returns early)
  for i in $(seq 1 30); do
    $K get nodes >/dev/null 2>&1 && break
    sleep 10
  done
  $K get nodes 2>&1 | tail -2 | head -1
  # durable volume root: the locked carve-out (never the default local-path root)
  mkdir -p /srv/data/k8s-volumes
  # cluster DNS: public upstream (the LAN router hijacks some A records)
  $K get cm coredns -n kube-system -o jsonpath="{.data.Corefile}" > /tmp/corefile || true
  grep -q "1.1.1.1" /tmp/corefile 2>/dev/null || {
    sed -i "s|forward . /etc/resolv.conf|forward . 1.1.1.1 8.8.8.8|" /tmp/corefile
    $K create cm coredns -n kube-system --from-file=Corefile=/tmp/corefile --dry-run=client -o yaml | $K apply -f -
    $K rollout restart deploy/coredns -n kube-system
  }
  # local-path provisioner root -> the pinned carve-out
  cat > /tmp/lp.yaml <<LP
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-path-config
  namespace: kube-system
data:
  config.json: |-
    { "nodePathMap": [ { "node": "k3s", "paths": ["/srv/data/k8s-volumes"] } ] }
LP
  $K apply -f /tmp/lp.yaml || true
  $K rollout restart deploy/local-path-provisioner -n kube-system || true
  # the api.fireworks.ai pin (provider egress — the router DNS answers wrong)
  $K patch deploy litellm -n litellm --type=strategic -p "{\"spec\":{\"template\":{\"spec\":{\"hostAliases\":[{\"ip\":\"35.207.52.96\",\"hostnames\":[\"api.fireworks.ai\"]}]}}}}" 2>/dev/null || true
'
echo "WAIT_NODE_RUNS_LATER"
