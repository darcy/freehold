# doors.tf — the capability doors: per-department ServiceAccounts + RBAC +
# long-lived SA tokens, the credentials the department runners carry. A door
# grants a SCOPED identity, never a shared admin one: Network's caddy door can
# only touch the caddy namespace, AI's litellm door only the litellm
# namespace; Compute holds the cluster-admin binding (root of the kube cluster
# — that IS its domain). Tokens are read back + re-sealed into the door
# runners' packages by the CP build every run (a k3s rebuild rotates the CA,
# so a token must always re-read).
#
# Naming: the door SA is named for <service>-door; the token Secret for
# <service>-door-token (the names the CP staging reads).

# ---- Network: the caddy namespace (ConfigMap edits + pod restart) ----------

resource "kubernetes_manifest" "caddy_door_sa" {
  depends_on = [kubernetes_manifest.caddy_namespace]
  manifest = {
    apiVersion = "v1"
    kind       = "ServiceAccount"
    metadata   = { name = "caddy-door", namespace = "caddy" }
  }
}

resource "kubernetes_manifest" "caddy_door_role" {
  depends_on = [kubernetes_manifest.caddy_namespace, kubernetes_manifest.caddy_door_sa]
  manifest = {
    apiVersion = "rbac.authorization.k8s.io/v1"
    kind       = "Role"
    metadata   = { name = "caddy-door", namespace = "caddy" }
    rules = [
      {
        apiGroups = [""]
        resources = ["configmaps"]
        verbs     = ["get", "list", "watch", "create", "update", "patch", "delete"]
      },
      {
        # hostNetwork: a config change needs the pod recreated (Recreate strategy)
        apiGroups = [""]
        resources = ["pods"]
        verbs     = ["get", "list", "watch", "delete"]
      },
      {
        apiGroups = ["apps"]
        resources = ["deployments"]
        verbs     = ["get", "list", "watch"]
      },
    ]
  }
}

resource "kubernetes_manifest" "caddy_door_binding" {
  depends_on = [kubernetes_manifest.caddy_namespace, kubernetes_manifest.caddy_door_role]
  manifest = {
    apiVersion = "rbac.authorization.k8s.io/v1"
    kind       = "RoleBinding"
    metadata   = { name = "caddy-door", namespace = "caddy" }
    roleRef = {
      apiGroup = "rbac.authorization.k8s.io"
      kind     = "Role"
      name     = "caddy-door"
    }
    subjects = [{
      kind      = "ServiceAccount"
      name      = "caddy-door"
      namespace = "caddy"
    }]
  }
}

resource "kubernetes_manifest" "caddy_door_token" {
  depends_on = [kubernetes_manifest.caddy_namespace, kubernetes_manifest.caddy_door_sa]
  manifest = {
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name        = "caddy-door-token"
      namespace   = "caddy"
      annotations = { "kubernetes.io/service-account.name" = "caddy-door" }
    }
    type = "kubernetes.io/service-account-token"
  }
}

# ---- AI: the litellm namespace (root of litellm's environment) -------------

resource "kubernetes_manifest" "litellm_door_sa" {
  depends_on = [kubernetes_manifest.litellm_namespace]
  manifest = {
    apiVersion = "v1"
    kind       = "ServiceAccount"
    metadata   = { name = "litellm-door", namespace = "litellm" }
  }
}

resource "kubernetes_manifest" "litellm_door_role" {
  depends_on = [kubernetes_manifest.litellm_namespace, kubernetes_manifest.litellm_door_sa]
  manifest = {
    apiVersion = "rbac.authorization.k8s.io/v1"
    kind       = "Role"
    metadata   = { name = "litellm-door", namespace = "litellm" }
    rules = [
      {
        # root of the litellm ENVIRONMENT (its workloads, config, and PVC-backed
        # state) — but NOT its Secrets: the gateway's master/provider keys ride
        # the litellm-api-admin runner's sealed package, and an agent reading
        # them off a kube Secret would be a second, ungoverned path to
        # plaintext. serviceaccounts/token minting is likewise excluded.
        apiGroups = ["", "apps"]
        resources = ["configmaps", "deployments", "pods", "pods/log", "persistentvolumeclaims", "services", "statefulsets"]
        verbs     = ["get", "list", "watch", "create", "update", "patch", "delete"]
      },
    ]
  }
}

resource "kubernetes_manifest" "litellm_door_binding" {
  depends_on = [kubernetes_manifest.litellm_namespace, kubernetes_manifest.litellm_door_role]
  manifest = {
    apiVersion = "rbac.authorization.k8s.io/v1"
    kind       = "RoleBinding"
    metadata   = { name = "litellm-door", namespace = "litellm" }
    roleRef = {
      apiGroup = "rbac.authorization.k8s.io"
      kind     = "Role"
      name     = "litellm-door"
    }
    subjects = [{
      kind      = "ServiceAccount"
      name      = "litellm-door"
      namespace = "litellm"
    }]
  }
}

resource "kubernetes_manifest" "litellm_door_token" {
  depends_on = [kubernetes_manifest.litellm_namespace, kubernetes_manifest.litellm_door_sa]
  manifest = {
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name        = "litellm-door-token"
      namespace   = "litellm"
      annotations = { "kubernetes.io/service-account.name" = "litellm-door" }
    }
    type = "kubernetes.io/service-account-token"
  }
}

# ---- Compute: root of the kube cluster (cluster-admin) ---------------------

resource "kubernetes_manifest" "compute_door_sa" {
  depends_on = [null_resource.k3s_bringup]
  manifest = {
    apiVersion = "v1"
    kind       = "ServiceAccount"
    metadata   = { name = "compute-door", namespace = "kube-system" }
  }
}

resource "kubernetes_manifest" "compute_door_binding" {
  depends_on = [kubernetes_manifest.compute_door_sa]
  manifest = {
    apiVersion = "rbac.authorization.k8s.io/v1"
    kind       = "ClusterRoleBinding"
    metadata   = { name = "freehold-compute-door" }
    roleRef = {
      apiGroup = "rbac.authorization.k8s.io"
      kind     = "ClusterRole"
      name     = "cluster-admin"
    }
    subjects = [{
      kind      = "ServiceAccount"
      name      = "compute-door"
      namespace = "kube-system"
    }]
  }
}

resource "kubernetes_manifest" "compute_door_token" {
  depends_on = [kubernetes_manifest.compute_door_sa]
  manifest = {
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name        = "compute-door-token"
      namespace   = "kube-system"
      annotations = { "kubernetes.io/service-account.name" = "compute-door" }
    }
    type = "kubernetes.io/service-account-token"
  }
}
