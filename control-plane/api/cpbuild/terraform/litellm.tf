# litellm.tf — the litellm model-gateway proxy, declared as kubernetes_manifest
# (server-side apply) resources for the same reason as postgres.tf (the local-path
# PVC deadlock). The gateway master key arrives as TF_VAR_litellm_master_key
# (runner-injected env, never argv/tfvars). Model registration is an EVENT (a
# curl to the gateway API, idempotent on re-apply) kept as a null_resource
# local-exec that reads the runner-injected PROVIDER_KEY directly - it never
# rides terraform state.

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
  depends_on = [kubernetes_manifest.litellm_secret, kubernetes_manifest.postgres_deploy]
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
            ports = [{ containerPort = 4000 }]
            env = [
              { name = "POSTGRES_PASSWORD", valueFrom = { secretKeyRef = { name = "litellm-pg", key = "postgres-pw" } } },
              { name = "DATABASE_URL", value = "postgresql://llmproxy:$(POSTGRES_PASSWORD)@postgres.litellm:5432/litellm" },
              { name = "STORE_MODEL_IN_DB", value = "True" },
              # ControlPlaneAgent is an ALIAS (the name the CPA harness requests),
              # not a registered model - litellm rewrites it to deepseek-v4-flash
              # at request time, so the registered model and its upstream stay the
              # single source of truth (see model_registration below).
              { name = "LITELLM_MODEL_ALIAS_MAP", value = "{\"ControlPlaneAgent\":\"deepseek-v4-flash\"}" },
              { name = "LITELLM_MASTER_KEY", valueFrom = { secretKeyRef = { name = "litellm-keys", key = "master-key" } } },
            ]
            readinessProbe = {
              httpGet             = { path = "/health/liveliness", port = 4000 }
              periodSeconds       = 10
              failureThreshold    = 6
              initialDelaySeconds = 5
            }
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

# Model registration - an EVENT, not a resource: idempotent on re-apply; the
# provider key rides the runner-injected env (PROVIDER_KEY) directly so it NEVER
# transits terraform state. Triggers on the service so a bare apply re-registers
# only when the gateway changes. This registers the REAL model (deepseek-v4-flash,
# its upstream + api key); the CPA's ControlPlaneAgent is a server-level alias to
# it (LITELLM_MODEL_ALIAS_MAP above), not a separate registration. The gateway is
# reached at the k3s NODE IP (k3s_ip), NOT 127.0.0.1 - terraform drives this via
# local-exec on the provisioning box, a DIFFERENT LXC from where litellm runs. A
# non-zero curl exit (e.g. connection refused / gateway not yet ready) now FAILS
# the apply instead of being swallowed, so a lost registration surfaces instead
# of silently leaving the CPA with no model. The litellm Deployment gates only on
# readiness; model_registration waits for /health/liveliness before registering.
resource "null_resource" "model_registration" {
  depends_on = [kubernetes_manifest.litellm_service]
  triggers = {
    deploy = yamlencode(kubernetes_manifest.litellm_deploy.object)
  }
  provisioner "local-exec" {
    command = <<-EOT
      set -euo pipefail
      # Wait for the freshly-rolled gateway to listen (its pod is created just
      # now; a Service apply does not imply the NodePort answers yet).
      for i in $(seq 1 30); do
        curl -fsS -m 5 "http://${var.k3s_ip}:31400/health/liveliness" >/dev/null 2>&1 && break
        sleep 2
      done
      BODY=$(printf '{"model_name":"deepseek-v4-flash","litellm_params":{"model":"fireworks_ai/accounts/fireworks/models/deepseek-v4-flash-0731","api_key":"%s"}}' "$PROVIDER_KEY")
      curl -fsS -m 30 -X POST -H "Authorization: Bearer $LITELLM" -H "Content-Type: application/json" -d "$BODY" "http://${var.k3s_ip}:31400/model/new"
      echo
    EOT
  }
}
