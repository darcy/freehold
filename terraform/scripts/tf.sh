#!/bin/bash
# Runner-exec wrapper: run terraform on the provisioning box through the runner.
# Credentials arrive as runner-injected env ({SECRET} + {SECRET}_URL); nothing
# is written by hand. Usage (on the provisioning box via the runner's exec):
#   tf.sh plan|apply|destroy|import ...
set -euo pipefail
CMD="${1:-plan}"; shift || true
[ -n "${TF_VAR_proxmox_api_url:-}" ] || export TF_VAR_proxmox_api_url="https://127.0.0.1:8006"
[ -n "${TF_VAR_proxmox_api_token_id:-}" ] || export TF_VAR_proxmox_api_token_id="freehold-tf@pve!bootstrap"
# proxmox token from the runner-injected secret env (TF_VAR name matches)
export TF_VAR_proxmox_api_token="${PROXMOX_API_TOKEN:?runner must inject PROXMOX_API_TOKEN}"
export TF_VAR_litellm_master_key="${LITELLM_MASTER_KEY:?runner must inject LITELLM_MASTER_KEY}"
export TF_VAR_litellm_provider_key="${LITELLM_PROVIDER_KEY:?runner must inject LITELLM_PROVIDER_KEY}"
[ -x "$(command -v terraform)" ] || { echo "terraform missing on the provisioning box" >&2; exit 1; }
cd /srv/data/freehold-tf 2>/dev/null || cd "${TF_ROOT:-/opt/freehold-tf}"
exec terraform "$CMD" "$@"
