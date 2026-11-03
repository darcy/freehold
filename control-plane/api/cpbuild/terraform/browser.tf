# browser.tf — the browser pool: a shared headless-Chromium service agents
# drive through the audited browser door. Browserless (the same image the
# browser-pool box proved live) fetches/renders/automates pages on request;
# the pod is STATELESS (every request a fresh session — sealed-profile
# borrowing is a later design, not this). The shared token arrives as
# TF_VAR_browser_token (runner-injected env, never argv/tfvars) — the SAME
# value the browser door's credential carries, so the door and the pool never
# drift. The NodePort rides the k3s node's LAN address (the litellm 31400
# pattern — LAN-trusted, nothing at the edge); the AUDIT lives in the door:
# every agent exec through browser-pool names agent/session in the runner's
# audit log.

variable "browser_token" {
  type      = string
  sensitive = true
  default   = ""
}

resource "kubernetes_manifest" "browser_namespace" {
  manifest = {
    apiVersion = "v1"
    kind       = "Namespace"
    metadata   = { name = "browser" }
  }
}

resource "kubernetes_manifest" "browser_token_secret" {
  depends_on = [kubernetes_manifest.browser_namespace]
  manifest = {
    apiVersion = "v1"
    kind       = "Secret"
    metadata   = { name = "browser-token", namespace = "browser" }
    type       = "Opaque"
    data       = { "token" = base64encode(var.browser_token) }
  }
}

resource "kubernetes_manifest" "browser_deploy" {
  depends_on = [kubernetes_manifest.browser_token_secret]
  manifest = {
    apiVersion = "apps/v1"
    kind       = "Deployment"
    metadata   = { name = "browserless", namespace = "browser" }
    spec = {
      replicas = 1
      selector = { matchLabels = { app = "browserless" } }
      template = {
        metadata = { labels = { app = "browserless" } }
        spec = {
          containers = [{
            name  = "browserless"
            image = "ghcr.io/browserless/chromium:latest"
            ports = [{ containerPort = 3000 }]
            env = [
              # Browserless v2's TOKEN protects every route (the librem box's
              # proven posture); the door's credential carries the same value.
              { name = "TOKEN", valueFrom = { secretKeyRef = { name = "browser-token", key = "token" } } },
              # Workers shed on their own timeout instead of a hard mem kill.
              { name = "TIMEOUT", value = "60000" },
              { name = "CONCURRENT", value = "3" },
            ]
            resources = {
              requests = { memory = "512Mi", cpu = "250m" }
              # Sized for a 3-worker pile on heavy pages (~2.7Gi measured
              # live): the default k3s guest ships 4Gi (the pool is core —
              # the browser needs real room, not a squeeze), with the node's
              # remaining room for caddy/the agent pods. A smaller override
              # node must size this limit down with its own memory.
              limits   = { memory = "3Gi" }
            }
            readinessProbe = {
              # TCP, not HTTP: browserless v2 auth-protects every route (the
              # TOKEN), and a probe cannot carry the credential — an httpGet
              # would 401 forever. The REAL health signal is the door's verify
              # arm (GET /meta bearer 200), which knows the token.
              tcpSocket = { port = 3000 }
              periodSeconds       = 10
              failureThreshold    = 12
              initialDelaySeconds = 15
            }
          }]
        }
      }
    }
  }
}

resource "kubernetes_manifest" "browser_service" {
  depends_on = [kubernetes_manifest.browser_deploy]
  manifest = {
    apiVersion = "v1"
    kind       = "Service"
    metadata   = { name = "browserless", namespace = "browser" }
    spec = {
      type     = "NodePort"
      selector = { app = "browserless" }
      ports = [{
        port       = 3000
        targetPort = 3000
        nodePort   = 31401
      }]
    }
  }
}
