# freehold bootstrap substrate — Terraform plans per kind (C7, POC_CHUNK3 v5).
#
# Execution discipline (locked): terraform runs ON the provisioning box (the PVE
# host / librem) through the runner's exec (`scripts/tf.sh`); every credential
# arrives as a runner-injected env var (TF_VAR_*), NEVER via tfvars or provider
# literals. TF-generated secrets (random_password etc.) live in state, so state
# is sensitive: keep it under /srv/data at 0600 or in an encrypted backend.
# Post-apply guard: assert state contains no operator-supplied variable value.
#
# Provider creds: a PVE API token minted ONCE through the runner exec
# (pveum user add freehold-tf@pve --password ... ; pveum aclmod / --user
# freehold-tf@pve --role Administrator) — the token value then rides the
# runner's secret plane, never the repo.

terraform {
  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "~> 0.66"
    }
  }
  backend "local" {
    # path defaults to ./terraform.tfstate — relocate to /srv/data/freehold-tf
    # (0600) on the provisioning box per the storage-tier rule.
  }
}

variable "proxmox_api_url" {
  type        = string
  description = "PVE API endpoint (e.g. https://127.0.0.1:8006 when run on the PVE host)"
}

variable "proxmox_api_token_id" {
  type        = string
  description = "PVE API token id, e.g. freehold-tf@pve!bootstrap"
}

variable "proxmox_api_token" {
  type        = string
  sensitive   = true
  description = "PVE API token secret — runner-injected TF_VAR, never committed"
}

variable "node_name" {
  type    = string
  default = "librem"
}

variable "template_id" {
  type    = string
  default = "local:vztmpl/debian-13-standard_13.6-1_amd64.tar.zst"
}

provider "proxmox" {
  endpoint = var.proxmox_api_url
  username = var.proxmox_api_token_id
  token    = var.proxmox_api_token
  insecure = true # self-signed PVE cert (LAN-only appliance) — fine for the POC
}

# ---- kind: k3s (the substrate node for C0) ---------------------------------
resource "proxmox_virtual_environment_container" "k3s" {
  vm_id           = 107
  node_name       = var.node_name
  template_file_id = var.template_id
  description     = "k3s cluster node (Chunk-3 C0 substrate) — managed by terraform"
  started         = true
  unprivileged    = true
  features {
    nesting = true
    keyctl  = true
  }
  memory {
    dedicated = 4096
  }
  cpu {
    cores = 4
  }
  disk {
    datastore_id = "local-lvm"
    size         = 20
  }
  network_device {
    bridge = "vmbr0"
    ipv4 = {
      address = "192.168.30.243/24"
      gateway = "192.168.30.1"
    }
  }
  # The one-time k3s bring-up — host/kernel prep happens ON the PVE host (this
  # box), the k3s install + unit fix happen in the guest. All through exec.
  provisioner "local-exec" {
    command = "${path.module}/scripts/k3s-bringup.sh ${self.vm_id}"
  }
}

# ---- kind: litellm-kube (C0 — the kube workloads for LiteLLM + Postgres) ----
# The kube manifests live in terraform/manifests/ (litellm + postgres with the
# PVC pinned to /srv/data/k8s-volumes); kubectl runs on the k3s node via the
# runner chain. Secret values (master key, provider key) are TF_VAR-injected
# env of the kubectl step, never files-in-repo.
resource "null_resource" "litellm_kube" {
  triggers = {
    manifests   = sha256(join("", [for f in fileset(path.module, "manifests/*.yaml") : file(f)]))
    master_key  = var.litellm_master_key
    provider_key = var.litellm_provider_key
  }
  provisioner "local-exec" {
    command = "${path.module}/scripts/kube-apply.sh ${var.k3s_vmid} ${var.litellm_master_key} ${var.litellm_provider_key}"
  }
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
