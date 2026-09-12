#!/bin/bash
# kube-apply: apply the litellm + postgres kube workloads into the k3s LXC,
# reconciled EXACTLY to the CP's stages (LitellmManifestScript/LitellmGatewayManifest):
# secrets live in k8s Secrets, first-run-wins, NO secret value is ever written to
# a file (the manifests reference the Secrets; none contain credentials — the
# "plaintext never on disk" model). Values ride runner-injected env (LITELLM,
# PROVIDER_KEY), never argv. Runs on the PVE host via terraform local-exec.
set -euo pipefail
VMID="$1"
LITELLM="${LITELLM:?runner must inject LITELLM (the litellm gate secret)}"
PROVIDER_KEY="${PROVIDER_KEY:?runner must inject PROVIDER_KEY}"
KGUEST="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec $VMID -- sh -c"

# namespace + Secrets FIRST (first-run-wins: the canonical values are the first
# apply's — postgres inits PGDATA against them; a re-apply must never re-roll).
$EX "$KGUEST create ns litellm 2>/dev/null || true"
PW=$(openssl rand -hex 16)
$EX "$KGUEST get secret litellm-pg -n litellm >/dev/null 2>&1 || $KGUEST create secret generic litellm-pg -n litellm --from-literal=postgres-pw='$PW'"
$EX "$KGUEST get secret litellm-keys -n litellm >/dev/null 2>&1 || $KGUEST create secret generic litellm-keys -n litellm --from-literal=master-key='$LITELLM'"

mkdir -p /tmp/tf-kube
cat >/tmp/tf-kube/postgres.yaml <<'PG'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: litellm-pg-data
  namespace: litellm
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: local-path
  resources:
    requests:
      storage: 10Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: litellm
spec:
  replicas: 1
  selector:
    matchLabels: {app: postgres}
  template:
    metadata:
      labels: {app: postgres}
    spec:
      containers:
      - name: postgres
        image: postgres:16
        env:
        - {name: POSTGRES_DB, value: litellm}
        - {name: POSTGRES_USER, value: llmproxy}
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef: {name: litellm-pg, key: postgres-pw}
        - {name: PGDATA, value: /var/lib/postgresql/data/pgdata}
        volumeMounts:
        - {name: data, mountPath: /var/lib/postgresql/data}
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: litellm-pg-data
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: litellm
spec:
  selector: {app: postgres}
  ports:
  - {port: 5432}
PG
cat >/tmp/tf-kube/litellm.yaml <<'LL'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: litellm
  namespace: litellm
spec:
  replicas: 1
  selector:
    matchLabels: {app: litellm}
  template:
    metadata:
      labels: {app: litellm}
    spec:
      containers:
      - name: litellm
        image: docker.litellm.ai/berriai/litellm:main-stable
        ports:
        - {containerPort: 4000}
        env:
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef: {name: litellm-pg, key: postgres-pw}
        - {name: DATABASE_URL, value: "postgresql://llmproxy:$(POSTGRES_PASSWORD)@postgres.litellm:5432/litellm"}
        - {name: STORE_MODEL_IN_DB, value: "True"}
        - name: LITELLM_MASTER_KEY
          valueFrom:
            secretKeyRef: {name: litellm-keys, key: master-key}
        readinessProbe:
          httpGet:
            path: /health/liveliness
            port: 4000
          periodSeconds: 10
          failureThreshold: 6
      hostAliases:
      - ip: "35.207.52.96"
        hostnames: ["api.fireworks.ai"]
---
apiVersion: v1
kind: Service
metadata:
  name: litellm
  namespace: litellm
spec:
  type: NodePort
  selector: {app: litellm}
  ports:
  - {port: 4000, targetPort: 4000, nodePort: 31400}
LL
pct push "$VMID" /tmp/tf-kube/postgres.yaml /tmp/tf-kube/postgres.yaml
pct push "$VMID" /tmp/tf-kube/litellm.yaml /tmp/tf-kube/litellm.yaml
$EX "$KGUEST apply -f /tmp/tf-kube/postgres.yaml"
$EX "$KGUEST apply -f /tmp/tf-kube/litellm.yaml"
$EX "$KGUEST rollout status deploy/litellm -n litellm --timeout=300s"
# idempotent model registration (best-effort on re-apply: an existing model is
# left as-is; the provider key rides env, never argv/disk).
BODY=$(printf '{"model_name":"deepseek-v4-flash","litellm_params":{"model":"fireworks_ai/accounts/fireworks/models/deepseek-v4-flash-0731","api_key":"%%s"}}' "$PROVIDER_KEY")
curl -s -m 30 -X POST -H "Authorization: Bearer $LITELLM" -H "Content-Type: application/json" -d "$BODY" http://127.0.0.1:31400/model/new >/tmp/tf-kube/reg.out 2>&1 || true
head -c 100 /tmp/tf-kube/reg.out
echo; echo "litellm-kube applied"
