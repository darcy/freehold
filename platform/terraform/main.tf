# freehold bootstrap substrate — Terraform plans per kind (C7, POC_CHUNK3 v5).
#
# Execution discipline (locked): terraform runs ON the provisioning box (the
# PVE host / librem) through the runner's exec (`scripts/tf.sh`); every
# credential arrives as a runner-injected env var (TF_VAR_*), NEVER via tfvars
# or provider literals. TF-generated secrets live in state, so state is
# sensitive: keep it under /srv/data at 0600 or in an encrypted backend.
# Post-apply guard: assert state contains no operator-supplied variable value.
#
# NOTE (2026-08-24): the bpg/proxmox container provider underwent a schema
# rewrite (template_file_id/network_device removed by 0.66.0; the new clone+
# network_interface form and a VM.Audit ACL quirk blocked import). Until the
# provider integration is re-solved (next session: pin a pre-rewrite release
# or adopt the clone-based flow), the substrate is managed EXEC-FIRST per the
# plan's remote-exec discipline: null_resource + local-exec provisioners call
# the proven pct/kubectl flows on the box; terraform provides real state,
# ordering, and destroy-timing. The provider block is retained (pinned 0.66.0)
# for the follow-up integration.

terraform {
  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "0.66.0"
    }
  }
  backend "local" {
    # STABLE path (survives code reships): /srv/data/freehold-tf/terraform.tfstate
    # at 0600 — the storage-tier rule for sensitive state.
    path = "../terraform.tfstate"
  }
}

variable "proxmox_api_url" {
  type    = string
  default = "https://127.0.0.1:8006"
}
variable "proxmox_api_token_id" {
  type    = string
  default = "freehold-tf@pve!bootstrap"
}
variable "proxmox_api_token" {
  type      = string
  sensitive = true
  default   = ""
}
variable "node_name" {
  type    = string
  default = "librem"
}
variable "template_id" {
  type    = string
  default = "local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst"
}
variable "k3s_vmid" {
  type    = number
  default = 107
}
variable "litellm_master_key" {
  type      = string
  sensitive = true
}
variable "litellm_provider_key" {
  type      = string
  sensitive = true
}

# ---- kind: k3s (the substrate node for C0) — exec-first --------------------
# Create: pct create with the exact live shape (static IP, rootfs, unprivileged,
# features nesting+keyctl), idempotent (skip if already present), then the
# k3s-bringup script (gotchas encoded). Destroy: pct stop && pct destroy.
resource "null_resource" "k3s_lxc" {
  triggers = {
    vmid        = var.k3s_vmid
    template_id = var.template_id
    node        = var.node_name
  }
  provisioner "local-exec" {
    command = "${path.module}/scripts/k3s-lxc.sh ${var.k3s_vmid} ${var.template_id} ${var.node_name} apply"
  }
  provisioner "local-exec" {
    when    = destroy
    command = "${path.module}/scripts/k3s-lxc.sh ${self.triggers.vmid} ${self.triggers.template_id} ${self.triggers.node} destroy"
  }
}

# ---- kind: litellm-kube (C0 — kube workloads for LiteLLM + Postgres) -------
# Create: kubectl apply of terraform/manifests + rollout + model registration
# (the deterministic C5 legs that depend only on the operator-provided keys).
# Destroy: kubectl delete namespace litellm (the kube layer is disposable; the
# durable PVC on /srv/data/k8s-volumes is recreated empty by the apply).
resource "null_resource" "litellm_kube" {
  depends_on = [null_resource.k3s_lxc]
  triggers = {
    manifests    = sha256(join("", [for f in fileset(path.module, "manifests/*.yaml") : file(f)]))
    master_key   = var.litellm_master_key
    provider_key = var.litellm_provider_key
    vmid         = var.k3s_vmid
  }
  provisioner "local-exec" {
    command = "${path.module}/scripts/kube-apply.sh ${var.k3s_vmid} ${var.litellm_master_key} ${var.litellm_provider_key}"
  }
  provisioner "local-exec" {
    when    = destroy
    command = "${path.module}/scripts/kube-destroy.sh ${self.triggers.vmid}"
  }
}
