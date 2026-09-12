# freehold bootstrap substrate + kube workloads — exec-first Terraform (A1).
#
# Discipline (locked): terraform runs ON the provisioning box (the PVE host)
# through the co-located runner's exec (`scripts/tf.sh`); the substrate idempotent
# pct/kubectl scripts are mirrored from the CP's planebase/drive/bootstrap Go
# drivers so terraform ADOPTS the same resources the CP creates, plan-clean.
# Every secret arrives as a runner-injected env var (TF_VAR_*, mapped in tf.sh) —
# NEVER as argv/-var/legacy tfvars/provider literals. State is sensitive: keep it
# under /srv/data/freehold-tf at 0600 (the storage-tier rule for sensitive state).
# Destroy: `terraform destroy` tears the substrate LXCs + kube workloads down
# (order: litellm_kube before k3s_bringup/lxc_k3s); the durable plane survives by
# design (teardown's --data path handles it separately).
terraform {
  backend "local" {
    path = "/srv/data/freehold-tf/terraform.tfstate"
  }
}

variable "node_name" {
  type    = string
  default = "librem"
}
variable "template" {
  type    = string
  default = "debian-13-standard_13.6-1_amd64.tar.zst"
}
variable "memory_mb" {
  type    = number
  default = 2048
}
variable "rootfs_gb" {
  type    = number
  default = 16
}
variable "pool" {
  type    = string
  default = "local-lvm"
}
variable "vg" {
  type    = string
  default = "pve"
}
variable "thin_pool" {
  type    = string
  default = ""
}
variable "lv_size_gb" {
  type    = number
  default = 8
}
variable "domain_dash" {
  type    = string # the DASHED relay-host domain, e.g. relay-librem-freehold-technology
  default = "relay"
}
variable "vmid_cp" {
  type    = number
  default = 0
}
variable "vmid_relay" {
  type    = number
  default = 0
}
variable "vmid_k3s" {
  type    = number
  default = 0
}
variable "host_cp" {
  type    = string
  default = "-"
}
variable "host_relay" {
  type    = string
  default = "-"
}
variable "host_k3s" {
  type    = string
  default = "-"
}
variable "k3s_ip" {
  type    = string
  default = "-" # bare; CIDR built here
}
variable "k3s_gw" {
  type    = string
  default = "-"
}

locals {
  plane_base = "/freehold/${var.domain_dash}"
  k3s_cidr   = var.k3s_ip == "" || var.k3s_ip == "-" ? "-" : "${var.k3s_ip}/24"
  mount_cp    = "--mp0=${local.plane_base}/cp,mp=/srv/data/cp,backup=1"
  mount_relay = "--mp0=${local.plane_base}/docker-root,mp=/var/lib/docker,backup=1 --mp1=${local.plane_base}/deploy,mp=/srv/data/relay,backup=1"
  mount_k3s   = "--mp0=${local.plane_base}/k3s-volumes,mp=/srv/data/k8s-volumes,backup=1"
}

# ---- durable volume plane ----------------------------------------------
resource "null_resource" "plane" {
  triggers = {
    base    = var.domain_dash
    vg      = var.vg
    pool    = var.thin_pool
    lv_size = var.lv_size_gb
  }
  provisioner "local-exec" {
    command = "${path.module}/scripts/plane.sh ${var.domain_dash} ${var.vg} ${var.thin_pool} ${var.lv_size_gb}"
  }
  # no destroy: the durable plane survives teardown by design (--data path).
}

# ---- substrate LXCs (cp/relay/k3s) — adopt-if-missing pct create ----------
resource "null_resource" "lxc_cp" {
  depends_on = [null_resource.plane]
  triggers = { vmid = var.vmid_cp, template = var.template, host = var.host_cp, mem = var.memory_mb, root = var.rootfs_gb }
  provisioner "local-exec" {
    command = "${path.module}/scripts/lxc.sh ${var.vmid_cp} ${var.template} ${var.node_name} ${var.host_cp} ${var.memory_mb} ${var.rootfs_gb} - - '${local.mount_cp}' apply"
  }
  provisioner "local-exec" {
    when = destroy
    command = "${path.module}/scripts/lxc.sh ${self.triggers.vmid} ${self.triggers.template} ${self.triggers.host} ${self.triggers.host} ${self.triggers.mem} ${self.triggers.root} - - '-' destroy"
  }
}

resource "null_resource" "lxc_relay" {
  depends_on = [null_resource.plane]
  triggers = { vmid = var.vmid_relay, template = var.template, host = var.host_relay, mem = var.memory_mb, root = var.rootfs_gb }
  provisioner "local-exec" {
    command = "${path.module}/scripts/lxc.sh ${var.vmid_relay} ${var.template} ${var.node_name} ${var.host_relay} ${var.memory_mb} ${var.rootfs_gb} - - '${local.mount_relay}' apply"
  }
  provisioner "local-exec" {
    when = destroy
    command = "${path.module}/scripts/lxc.sh ${self.triggers.vmid} ${self.triggers.template} ${self.triggers.host} ${self.triggers.host} ${self.triggers.mem} ${self.triggers.root} - - '-' destroy"
  }
}

resource "null_resource" "lxc_k3s" {
  depends_on = [null_resource.plane]
  triggers = { vmid = var.vmid_k3s, template = var.template, host = var.host_k3s, ip = local.k3s_cidr, gw = var.k3s_gw, mem = var.memory_mb, root = var.rootfs_gb }
  provisioner "local-exec" {
    command = "${path.module}/scripts/lxc.sh ${var.vmid_k3s} ${var.template} ${var.node_name} ${var.host_k3s} ${var.memory_mb} ${var.rootfs_gb} ${local.k3s_cidr} ${var.k3s_gw} '${local.mount_k3s}' apply"
  }
  provisioner "local-exec" {
    when = destroy
    command = "${path.module}/scripts/lxc.sh ${self.triggers.vmid} ${self.triggers.template} ${self.triggers.host} ${self.triggers.host} ${self.triggers.mem} ${self.triggers.root} ${self.triggers.ip} ${self.triggers.gw} '-' destroy"
  }
}

# ---- k3s bring-up (install + durable carve-outs) — dies with the LXC ---------
resource "null_resource" "k3s_bringup" {
  depends_on = [null_resource.lxc_k3s]
  triggers = { vmid = var.vmid_k3s, ip = local.k3s_cidr }
  provisioner "local-exec" {
    command = "${path.module}/scripts/k3s-bringup.sh ${var.vmid_k3s}"
  }
}

# ---- litellm + postgres kube workloads (kubectl, secrets via env) -----------
# Triggers on vmid ONLY: the secret VALUES must not ride terraform state
# (sensitive = true only redacts CLI output, not state storage) — kube-apply.sh
# reads them from runner-injected env and the k8s Secrets are first-run-wins.
resource "null_resource" "litellm_kube" {
  depends_on = [null_resource.k3s_bringup]
  triggers = {
    vmid = var.vmid_k3s
  }
  provisioner "local-exec" {
    command = "${path.module}/scripts/kube-apply.sh ${var.vmid_k3s}"
  }
  provisioner "local-exec" {
    when    = destroy
    command = "${path.module}/scripts/kube-destroy.sh ${self.triggers.vmid}"
  }
}
