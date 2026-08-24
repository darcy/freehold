#!/bin/bash
# kubectl apply of the C0 manifests (kube-apply: litellm + postgres). Runs on
# the PVE host through the runner's exec; kubectl executes on the k3s node via
# pct exec. Secret values are injected into the templated manifests at apply
# time — the files in terraform/manifests carry __MASTER_KEY__/__PROVIDER_KEY__
# placeholders and nothing else.
set -euo pipefail
VMID="$1"; MASTER="$2"; PROVIDER="$3"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
pct exec "$VMID" -- bash -s -- <<OUTER
  set -euo pipefail
  K=/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml
  mkdir -p /tmp/kube-apply
  for f in ${ROOT}/manifests/*.yaml; do
    sed "s|__MASTER_KEY__|${MASTER}|g; s|__PROVIDER_KEY__|${PROVIDER}|g" "\$f" > "/tmp/kube-apply/\$(basename \$f)"
  done
  \$K apply -f /tmp/kube-apply
  \$K rollout status deploy/litellm -n litellm --timeout=300s
  \$K get pods -n litellm | tail -2
OUTER
echo "litellm-kube applied"
