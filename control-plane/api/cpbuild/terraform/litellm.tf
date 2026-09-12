# litellm.tf — the litellm model-gateway proxy. Declarative kubernetes-provider
# resources (the deterministic static definition of the service), replacing the
# kube-apply.sh YAML here-doc. The gateway master key arrives as
# TF_VAR_litellm_master_key (runner-injected env, never argv/tfvars); the k8s
# Secret is first-run-wins (ignore_changes = [data]). Model registration is an
# event (a curl to the gateway API), kept as a null_resource local-exec that
# reads the runner-injected PROVIDER_KEY env directly — it never rides state.

resource "kubernetes_secret" "litellm_keys" {
  metadata {
    name      = "litellm-keys"
    namespace = kubernetes_namespace.litellm.metadata[0].name
  }
  data = {
    "master-key" = var.litellm_master_key
  }
  # First-run-wins: the gateway's master key is the deployed truth; a re-apply
  # must never rotate it (existing models/keys are minted against it).
  lifecycle {
    ignore_changes = [data]
  }
}

resource "kubernetes_deployment" "litellm" {
  depends_on = [kubernetes_deployment.postgres]
  metadata {
    name      = "litellm"
    namespace = kubernetes_namespace.litellm.metadata[0].name
  }
  spec {
    replicas = 1
    selector {
      match_labels = { app = "litellm" }
    }
    template {
      metadata {
        labels = { app = "litellm" }
      }
      spec {
        # Fireworks egress pin (deterministic; the node's resolvers must reach
        # api.fireworks.ai directly, not the LAN router).
        host_aliases {
          ip        = "35.207.52.96"
          hostnames = ["api.fireworks.ai"]
        }
        container {
          name  = "litellm"
          image = "docker.litellm.ai/berriai/litellm:main-stable"
          port {
            container_port = 4000
          }
          env {
            name = "POSTGRES_PASSWORD"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.litellm_pg.metadata[0].name
                key  = "postgres-pw"
              }
            }
          }
          env {
            name  = "DATABASE_URL"
            value = "postgresql://llmproxy:$(POSTGRES_PASSWORD)@postgres.litellm:5432/litellm"
          }
          env {
            name  = "STORE_MODEL_IN_DB"
            value = "True"
          }
          env {
            name = "LITELLM_MASTER_KEY"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.litellm_keys.metadata[0].name
                key  = "master-key"
              }
            }
          }
          readiness_probe {
            http_get {
              path = "/health/liveliness"
              port = 4000
            }
            period_seconds        = 10
            failure_threshold     = 6
            initial_delay_seconds = 5
          }
        }
      }
    }
  }
}

resource "kubernetes_service" "litellm" {
  metadata {
    name      = "litellm"
    namespace = kubernetes_namespace.litellm.metadata[0].name
  }
  spec {
    type     = "NodePort"
    selector = { app = "litellm" }
    port {
      port        = 4000
      target_port = 4000
      node_port   = 31400
    }
  }
}

# Model registration — an EVENT, not a resource: a curl to the gateway API
# mapping the CPA's model alias to the fireworks route. Idempotent on re-apply;
# the provider key rides the runner-injected env (PROVIDER_KEY) directly so it
# NEVER transits terraform state. Triggers on the deployment so a bare
# `terraform apply` re-registers only when the gateway changes.
resource "null_resource" "model_registration" {
  depends_on = [kubernetes_service.litellm]
  triggers = {
    deployment = kubernetes_deployment.litellm.id
  }
  provisioner "local-exec" {
    command = <<-EOT
      set -euo pipefail
      BODY=$(printf '{"model_name":"deepseek-v4-flash","litellm_params":{"model":"fireworks_ai/accounts/fireworks/models/deepseek-v4-flash-0731","api_key":"%s"}}' "$PROVIDER_KEY")
      curl -s -m 30 -X POST -H "Authorization: Bearer $LITELLM" -H "Content-Type: application/json" -d "$BODY" http://127.0.0.1:31400/model/new || true
      echo
    EOT
  }
}
