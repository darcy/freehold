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
# Self-install: terraform is a single static binary; a fresh CP guest does not
# have it and nothing else ships it. Pinned version; the kubernetes provider
# download at init is already an accepted online step. The resolved path (not
# `terraform`) below: the runner's non-login sh PATH often lacks
# /usr/local/bin.
TF_BIN="$(command -v terraform || true)"
if [ -z "$TF_BIN" ]; then
  TF_VERSION="${TF_VERSION:-1.9.8}"
  echo "terraform missing on the provisioning box — installing v$TF_VERSION"
  apt-get install -y -qq unzip >/dev/null 2>&1 || true
  curl -fsSL --retry 3 -o /tmp/terraform.zip "https://releases.hashicorp.com/terraform/${TF_VERSION}/terraform_${TF_VERSION}_linux_amd64.zip"
  unzip -oq /tmp/terraform.zip -d /tmp && install -m 0755 /tmp/terraform /usr/local/bin/terraform && rm -f /tmp/terraform.zip /tmp/terraform
  TF_BIN=/usr/local/bin/terraform
  [ -x "$TF_BIN" ] || { echo "terraform self-install failed" >&2; exit 1; }
fi
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
# The kubernetes provider reads var.kubeconfig_path (passed as -var on apply);
# DESTROY passes no vars, so default the var to the world's own root here or the
# provider falls back to the legacy shared default and destroy dies with
# "cannot create discovery client: no client config". A -var on apply still wins.
export TF_VAR_kubeconfig_path="${TF_VAR_kubeconfig_path:-$ROOT/kubeconfig}"

export TF_VAR_litellm_master_key="${LITELLM:-}"
export TF_VAR_postgres_password="${POSTGRES_PW:-}"

# Drop any backend record left by the old pinned shared-backend config: TF 1.9's
# `-reconfigure` does NOT handle UNSETTING a backend, so init would otherwise
# refuse the apply ("Backend initialization required"). With the record gone the
# default local backend takes effect and state lives in $ROOT (per world).
rm -f "$ROOT/.terraform/terraform.tfstate"
"$TF_BIN" init -input=false >/dev/null
case "$CMD" in apply|destroy) set -- "$@" -auto-approve;; esac
exec "$TF_BIN" "$CMD" -input=false "$@"
