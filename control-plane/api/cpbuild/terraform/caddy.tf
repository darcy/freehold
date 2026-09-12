# caddy.tf — the Caddy TLS edge: a hostNetwork Deployment fronting the relay +
# CP vhosts, its rendered Caddyfile in a ConfigMap, certs on the durable PVC.
# This is the STATIC shape. The certs themselves are EVENT-driven (issue/resume
# via DNS-01, install into the PVC) and stay in the CP's overlay — NOT a terraform
# resource — so the Caddyfile references /data/tls/* that the overlay writes.

resource "kubernetes_namespace" "caddy" {
  depends_on = [null_resource.k3s_bringup]
  metadata {
    name = "caddy"
  }
}

resource "kubernetes_persistent_volume_claim" "caddy_data" {
  metadata {
    name      = "caddy-data"
    namespace = kubernetes_namespace.caddy.metadata[0].name
  }
  spec {
    access_modes       = ["ReadWriteOnce"]
    storage_class_name = "local-path"
    resources {
      requests = { storage = "1Gi" }
    }
  }
}

resource "kubernetes_config_map" "caddyfile" {
  metadata {
    name      = "caddy-caddyfile"
    namespace = kubernetes_namespace.caddy.metadata[0].name
  }
  data = {
    Caddyfile = base64decode(var.caddyfile_b64)
  }
}

resource "kubernetes_deployment" "caddy" {
  metadata {
    name      = "caddy"
    namespace = kubernetes_namespace.caddy.metadata[0].name
  }
  spec {
    replicas = 1
    selector {
      match_labels = { app = "caddy" }
    }
    template {
      metadata {
        labels = { app = "caddy" }
      }
      spec {
        host_network = true
        container {
          name  = "caddy"
          image = "caddy:2.8"
          port {
            container_port = 80
            name           = "http"
          }
          port {
            container_port = 443
            name           = "https"
          }
          volume_mount {
            name       = "caddyfile"
            mount_path = "/etc/caddy/Caddyfile"
            sub_path   = "Caddyfile"
            read_only  = true
          }
          volume_mount {
            name       = "data"
            mount_path = "/data"
          }
        }
        volume {
          name = "caddyfile"
          config_map {
            name = kubernetes_config_map.caddyfile.metadata[0].name
          }
        }
        volume {
          name = "data"
          persistent_volume_claim {
            claim_name = kubernetes_persistent_volume_claim.caddy_data.metadata[0].name
          }
        }
      }
    }
  }
}

resource "kubernetes_service" "caddy" {
  metadata {
    name      = "caddy"
    namespace = kubernetes_namespace.caddy.metadata[0].name
  }
  spec {
    type     = "NodePort"
    selector = { app = "caddy" }
    port {
      name        = "http"
      port        = 80
      target_port = 80
      node_port   = 30080
    }
    port {
      name        = "https"
      port        = 443
      target_port = 443
      node_port   = 30443
    }
  }
}
