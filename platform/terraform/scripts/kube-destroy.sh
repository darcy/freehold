#!/bin/bash
# destroy the litellm kube layer (namespace + workloads) on the k3s node.
set -euo pipefail
VMID="$1"
pct exec "$VMID" -- bash -c '
  set -euo pipefail
  K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
  $K delete namespace litellm --wait=true 2>/dev/null || true
  echo "litellm namespace gone"
'
echo "litellm-kube destroyed"
