#!/bin/bash
# Runner-exec wrapper: run terraform on the provisioning box through the runner.
# The kubernetes provider is downloaded ONCE at `init` (online allowed — the
# cert flow needs network anyway); the kubeconfig cpbuild staged (rewritten to
# the k3s node IP) lets the provider reach the k3s API.
#
# SECRET mapping (option A): the runner-injected env gives LITELLM (gateway
# master) + POSTGRES_PW at these names (secret-name -> UPPER_ env). This script
# maps them to TF_VAR_* IN ENV (never argv/tfvars), so they ride 0600 state.
# PROVIDER_KEY is intentionally NOT mapped: the model-registration local-exec
# reads it directly, so the provider key never transits terraform state.
# Everything non-secret rides -var argv.
#
# Destroy needs neither the secret VALUES (they live in state) nor an early
# kubeconfig gate (the provider connects only when k8s resources are present),
# so those requirements are `plan`/`apply`-only — a substrate-only destroy still
# works without injected secrets.
set -euo pipefail
CMD="${1:-plan}"; shift || true
[ -x "$(command -v terraform)" ] || { echo "terraform missing on the provisioning box" >&2; exit 1; }
ROOT="${TF_ROOT:-/srv/data/freehold-tf}"
mkdir -p "$ROOT"
cd "$ROOT"

if [ "$CMD" != "destroy" ] && [ "${TF_NO_SECRETS:-0}" != "1" ]; then
  : "${LITELLM:?runner must inject LITELLM (the litellm gate secret)}"
  : "${POSTGRES_PW:?runner must inject POSTGRES_PW}"
  [ -f "$ROOT/kubeconfig" ] || { echo "no kubeconfig at $ROOT/kubeconfig (cpbuild must stage the rewritten k3s kubeconfig before apply)" >&2; exit 1; }
fi

# Keep a stale staged kubeconfig from a prior build from satisfying a validate
# against a k3s that was torn down: always point the provider at the durable one.
export KUBECONFIG="${KUBECONFIG:-$ROOT/kubeconfig}"

export TF_VAR_litellm_master_key="${LITELLM:-}"
export TF_VAR_postgres_password="${POSTGRES_PW:-}"

terraform init -input=false >/dev/null
case "$CMD" in apply|destroy) set -- "$@" -auto-approve;; esac
exec terraform "$CMD" -input=false "$@"
