#!/bin/bash
# Runner-exec wrapper: run terraform on the provisioning box through the runner.
# Credentials are NOT terraform variables — kube-apply.sh reads the runner-
# injected env (LITELLM, PROVIDER_KEY) directly, so no secret transits argv,
# tfvars, or terraform state (the module declares no secret variable; state
# carries only the adopt triggers). Everything non-secret rides -var argv.
# Usage (on the box via the runner):  tf.sh plan|apply|destroy [-var k=v ...]
set -euo pipefail
CMD="${1:-plan}"; shift || true
[ -x "$(command -v terraform)" ] || { echo "terraform missing on the provisioning box" >&2; exit 1; }
ROOT="${TF_ROOT:-/srv/data/freehold-tf}"
mkdir -p "$ROOT"
cd "$ROOT"
# exec-first: no provider download, so init is offline + safe
terraform init -input=false >/dev/null
case "$CMD" in apply|destroy) set -- "$@" -auto-approve;; esac
exec terraform "$CMD" -input=false "$@"
