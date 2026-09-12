# postgres.tf — the litellm postgres database. A declarative kubernetes-provider
# resource set (the deterministic static definition of the service), replacing
# the kube-apply.sh YAML here-doc. The postgres password arrives as
# TF_VAR_postgres_password (runner-injected env, never argv/tfvars) and rides
# the 0600 state; the k8s Secret is first-run-wins (ignore_changes = [data]) so
# a re-apply never rotates the password Postgres initialized PGDATA against.

resource "kubernetes_namespace" "litellm" {
  depends_on = [null_resource.k3s_bringup]
  metadata {
    name = "litellm"
  }
}

resource "kubernetes_secret" "litellm_pg" {
  metadata {
    name      = "litellm-pg"
    namespace = kubernetes_namespace.litellm.metadata[0].name
  }
  data = {
    "postgres-pw" = var.postgres_password
  }
  # First-run-wins: the first apply's value is authoritative (postgres inits
  # PGDATA against it); a re-apply must never re-roll it.
  lifecycle {
    ignore_changes = [data]
  }
}

resource "kubernetes_persistent_volume_claim" "litellm_pg_data" {
  metadata {
    name      = "litellm-pg-data"
    namespace = kubernetes_namespace.litellm.metadata[0].name
  }
  spec {
    access_modes       = ["ReadWriteOnce"]
    storage_class_name = "local-path"
    resources {
      requests = { storage = "10Gi" }
    }
  }
}

resource "kubernetes_deployment" "postgres" {
  metadata {
    name      = "postgres"
    namespace = kubernetes_namespace.litellm.metadata[0].name
  }
  spec {
    replicas = 1
    selector {
      match_labels = { app = "postgres" }
    }
    template {
      metadata {
        labels = { app = "postgres" }
      }
      spec {
        container {
          name  = "postgres"
          image = "postgres:16"
          env {
            name  = "POSTGRES_DB"
            value = "litellm"
          }
          env {
            name  = "POSTGRES_USER"
            value = "llmproxy"
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
            name  = "PGDATA"
            value = "/var/lib/postgresql/data/pgdata"
          }
          volume_mount {
            name       = "data"
            mount_path = "/var/lib/postgresql/data"
          }
        }
        volume {
          name = "data"
          persistent_volume_claim {
            claim_name = kubernetes_persistent_volume_claim.litellm_pg_data.metadata[0].name
          }
        }
      }
    }
  }
}

resource "kubernetes_service" "postgres" {
  metadata {
    name      = "postgres"
    namespace = kubernetes_namespace.litellm.metadata[0].name
  }
  spec {
    selector = { app = "postgres" }
    port {
      port        = 5432
      target_port = 5432
    }
  }
}
