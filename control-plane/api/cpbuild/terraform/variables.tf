# Service-definition variables for the kubernetes-provider resources.
#
# SECRET discipline (option A, explicitly chosen): litellm_master_key and
# postgres_password are the two services' real credentials. They arrive as
# runner-injected env (TF_VAR_* set in tf.sh, NEVER argv) and RIDE the 0600
# state under /srv/data/freehold-tf — the same disk/permission class as the
# durable-plane secrets store. The k8s Secret DATA is first-run-wins
# (lifecycle { ignore_changes = [data] }) so a re-apply never rotates values
# Postgres already initialized PGDATA against.
variable "kubeconfig_path" {
  type    = string
  default = "/srv/data/freehold-tf/kubeconfig"
}
variable "kube_context" {
  type    = string
  default = "default"
}
variable "litellm_master_key" {
  type      = string
  default   = ""
  sensitive = true
}
variable "postgres_password" {
  type      = string
  default   = ""
  sensitive = true
}
variable "caddyfile_b64" {
  type        = string
  default     = ""
  description = "Base64 of the rendered Caddyfile (the caddy-caddyfile ConfigMap data); plain, not secret. base64 so the multi-line content rides a single -var safely."
}
