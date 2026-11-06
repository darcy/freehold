# litellm.tf — the litellm model-gateway proxy, declared as kubernetes_manifest
# (server-side apply) resources for the same reason as postgres.tf (the local-path
# PVC deadlock). The gateway master key arrives as TF_VAR_litellm_master_key
# (runner-injected env, never argv/tfvars). Model REGISTRATION is not here —
# stageLitellmAliases registers the alias set on the gateway straight from the
# CP's litellm store (provider choice + key), so no provider key ever rides
# terraform. The deployment's hostAliases pin api.fireworks.ai for the
# fireworks egress path (the freehold default provider); other providers
# resolve normally and the pin is inert for them.

resource "kubernetes_manifest" "litellm_config" {
  depends_on = [kubernetes_manifest.litellm_namespace]
  manifest = {
    apiVersion = "v1"
    kind       = "ConfigMap"
    metadata = {
      name      = "litellm-config"
      namespace = "litellm"
      labels    = { "app.kubernetes.io/managed-by" = "freehold" }
    }
    data = {
      # The MCP plane's default-deny posture (Phase 2): a key with no
      # explicit mcp_servers list reaches NO MCP server. Model registration
      # stays DB-side (STORE_MODEL_IN_DB env); the DB object types are
      # unaffected by pinning these two settings in config.
      "config.yaml" = <<-YAML
        general_settings:
          store_model_in_db: true
          require_key_mcp_access_defined: true
      YAML
    }
  }
}

resource "kubernetes_manifest" "litellm_secret" {
  depends_on = [kubernetes_manifest.litellm_namespace]
  manifest = {
    apiVersion = "v1"
    kind       = "Secret"
    metadata   = { name = "litellm-keys", namespace = "litellm" }
    type       = "Opaque"
    data       = { "master-key" = base64encode(var.litellm_master_key) }
  }
}

resource "kubernetes_manifest" "litellm_deploy" {
  depends_on = [kubernetes_manifest.litellm_secret, kubernetes_manifest.postgres_deploy, kubernetes_manifest.litellm_config]
  manifest = {
    apiVersion = "apps/v1"
    kind       = "Deployment"
    metadata   = { name = "litellm", namespace = "litellm" }
    spec = {
      replicas = 1
      selector = { matchLabels = { app = "litellm" } }
      template = {
        metadata = { labels = { app = "litellm" } }
        spec = {
          hostAliases = [{
            ip        = "35.207.52.96"
            hostnames = ["api.fireworks.ai"]
          }]
          containers = [{
            name  = "litellm"
            image = "docker.litellm.ai/berriai/litellm:main-stable"
            args  = ["--config", "/app/config.yaml", "--port", "4000"]
            ports = [{ containerPort = 4000 }]
            volumeMounts = [{
              name      = "config"
              mountPath = "/app/config.yaml"
              subPath   = "config.yaml"
              readOnly  = true
            }]
            env = [
              { name = "POSTGRES_PASSWORD", valueFrom = { secretKeyRef = { name = "litellm-pg", key = "postgres-pw" } } },
              { name = "DATABASE_URL", value = "postgresql://llmproxy:$(POSTGRES_PASSWORD)@postgres.litellm:5432/litellm" },
              { name = "STORE_MODEL_IN_DB", value = "True" },
              { name = "LITELLM_MASTER_KEY", valueFrom = { secretKeyRef = { name = "litellm-keys", key = "master-key" } } },
            ]
            readinessProbe = {
              httpGet             = { path = "/health/liveliness", port = 4000 }
              periodSeconds       = 10
              failureThreshold    = 6
              initialDelaySeconds = 5
            }
          }]
          volumes = [{
            name = "config"
            configMap = { name = "litellm-config" }
          }]
        }
      }
    }
  }
}

resource "kubernetes_manifest" "litellm_service" {
  depends_on = [kubernetes_manifest.litellm_deploy]
  manifest = {
    apiVersion = "v1"
    kind       = "Service"
    metadata   = { name = "litellm", namespace = "litellm" }
    spec = {
      type     = "NodePort"
      selector = { app = "litellm" }
      ports = [{
        port       = 4000
        targetPort = 4000
        nodePort   = 31400
      }]
    }
  }
}
