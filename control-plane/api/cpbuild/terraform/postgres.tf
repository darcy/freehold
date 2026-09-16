# postgres.tf — the litellm postgres database, declared as kubernetes_manifest
# (server-side apply) resources. SSA is used deliberately: kubernetes_persistent_
# volume_claim BLOCKS until the claim is Bound, but local-path uses
# WaitForFirstConsumer — it only binds when the deployment POD (the dependent)
# claims it, so the provider serializes PVC->Deployment into a deadlock that
# times out. kubernetes_manifest applies without waiting on status, the pod
# triggers provisioning, and everything converges at the k8s level.
#
# The postgres password arrives as TF_VAR_postgres_password (runner-injected env,
# never argv/tfvars) and rides the 0600 state (option A); re-applying the same
# value is a no-op (no rotation).

resource "kubernetes_manifest" "litellm_namespace" {
  depends_on = [null_resource.k3s_bringup]
  manifest = {
    apiVersion = "v1"
    kind       = "Namespace"
    metadata   = { name = "litellm" }
  }
}

resource "kubernetes_manifest" "postgres_pvc" {
  depends_on = [kubernetes_manifest.litellm_namespace]
  manifest = {
    apiVersion = "v1"
    kind       = "PersistentVolumeClaim"
    metadata   = { name = "litellm-pg-data", namespace = "litellm" }
    spec = {
      accessModes      = ["ReadWriteOnce"]
      storageClassName = "local-path"
      resources        = { requests = { storage = "10Gi" } }
    }
  }
}

resource "kubernetes_manifest" "postgres_secret" {
  depends_on = [kubernetes_manifest.litellm_namespace]
  manifest = {
    apiVersion = "v1"
    kind       = "Secret"
    metadata   = { name = "litellm-pg", namespace = "litellm" }
    type       = "Opaque"
    data       = { "postgres-pw" = base64encode(var.postgres_password) }
  }
}

resource "kubernetes_manifest" "postgres_deploy" {
  depends_on = [kubernetes_manifest.postgres_secret, kubernetes_manifest.postgres_pvc]
  manifest = {
    apiVersion = "apps/v1"
    kind       = "Deployment"
    metadata   = { name = "postgres", namespace = "litellm" }
    spec = {
      replicas = 1
      selector = { matchLabels = { app = "postgres" } }
      template = {
        metadata = { labels = { app = "postgres" } }
        spec = {
          containers = [{
            name  = "postgres"
            image = "postgres:16"
            env = [
              { name = "POSTGRES_DB", value = "litellm" },
              { name = "POSTGRES_USER", value = "llmproxy" },
              { name = "POSTGRES_PASSWORD", valueFrom = { secretKeyRef = { name = "litellm-pg", key = "postgres-pw" } } },
              { name = "PGDATA", value = "/var/lib/postgresql/data/pgdata" },
            ]
            volumeMounts = [{ name = "data", mountPath = "/var/lib/postgresql/data" }]
          }]
          volumes = [{ name = "data", persistentVolumeClaim = { claimName = "litellm-pg-data" } }]
        }
      }
    }
  }
}

resource "kubernetes_manifest" "postgres_service" {
  depends_on = [kubernetes_manifest.postgres_deploy]
  manifest = {
    apiVersion = "v1"
    kind       = "Service"
    metadata   = { name = "postgres", namespace = "litellm" }
    spec = {
      selector = { app = "postgres" }
      ports    = [{ port = 5432, targetPort = 5432 }]
    }
  }
}
