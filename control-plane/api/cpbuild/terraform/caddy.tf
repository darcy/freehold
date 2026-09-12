# caddy.tf — the Caddy TLS edge (hostNetwork Deployment fronting the relay + CP
# vhosts, its rendered Caddyfile in a ConfigMap, certs on the durable PVC),
# declared as kubernetes_manifest (server-side apply) resources - same local-path
# rationale as postgres.tf. Certs are EVENT-driven (DNS-01 issue/install into the
# PVC, the CP's overlay) and reference /data/tls/* this Deployment serves.

resource "kubernetes_manifest" "caddy_namespace" {
  depends_on = [null_resource.k3s_bringup]
  manifest = {
    apiVersion = "v1"
    kind       = "Namespace"
    metadata   = { name = "caddy" }
  }
}

resource "kubernetes_manifest" "caddy_pvc" {
  depends_on = [kubernetes_manifest.caddy_namespace]
  manifest = {
    apiVersion = "v1"
    kind       = "PersistentVolumeClaim"
    metadata   = { name = "caddy-data", namespace = "caddy" }
    spec = {
      accessModes      = ["ReadWriteOnce"]
      storageClassName = "local-path"
      resources        = { requests = { storage = "1Gi" } }
    }
  }
}

resource "kubernetes_manifest" "caddy_configmap" {
  depends_on = [kubernetes_manifest.caddy_namespace]
  manifest = {
    apiVersion = "v1"
    kind       = "ConfigMap"
    metadata   = { name = "caddy-caddyfile", namespace = "caddy" }
    data       = { "Caddyfile" = base64decode(var.caddyfile_b64) }
  }
}

resource "kubernetes_manifest" "caddy_deploy" {
  depends_on = [kubernetes_manifest.caddy_pvc, kubernetes_manifest.caddy_configmap]
  manifest = {
    apiVersion = "apps/v1"
    kind       = "Deployment"
    metadata   = { name = "caddy", namespace = "caddy" }
    spec = {
      replicas = 1
      # hostNetwork binds 80/443 on the node - a RollingUpdate can never bring the
      # new pod up while the old one holds the ports, so the rollout wedges at
      # 0/1 available. RE-typed: delete the old pod first, then start the new.
      strategy = {
        type = "Recreate"
      }
      selector = { matchLabels = { app = "caddy" } }
      template = {
        metadata = { labels = { app = "caddy" } }
        spec = {
          hostNetwork = true
          containers = [{
            name  = "caddy"
            image = "caddy:2.8"
            ports = [
              { containerPort = 80, name = "http" },
              { containerPort = 443, name = "https" },
            ]
            volumeMounts = [
              { name = "caddyfile", mountPath = "/etc/caddy/Caddyfile", subPath = "Caddyfile", readOnly = true },
              { name = "data", mountPath = "/data" },
            ]
          }]
          volumes = [
            { name = "caddyfile", configMap = { name = "caddy-caddyfile" } },
            { name = "data", persistentVolumeClaim = { claimName = "caddy-data" } },
          ]
        }
      }
    }
  }
}

resource "kubernetes_manifest" "caddy_service" {
  depends_on = [kubernetes_manifest.caddy_deploy]
  manifest = {
    apiVersion = "v1"
    kind       = "Service"
    metadata   = { name = "caddy", namespace = "caddy" }
    spec = {
      type     = "NodePort"
      selector = { app = "caddy" }
      ports = [
        { name = "http", port = 80, targetPort = 80, nodePort = 30080 },
        { name = "https", port = 443, targetPort = 443, nodePort = 30443 },
      ]
    }
  }
}
