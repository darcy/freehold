#!/bin/bash
# kubectl apply of the C0 manifests (litellm + postgres), self-contained.
# Secret values are injected at apply time only. Runs on the PVE host via the
# runner's exec; kubectl executes on the k3s node via pct exec.
set -euo pipefail
VMID="$1"; MASTER="$2"; PROVIDER="$3"

POSTGRES="$(cat <<'PG'
apiVersion: v1
kind: Namespace
metadata:
  name: litellm
---
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
        - {name: POSTGRES_PASSWORD, value: llmproxy-db-pass}
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
)"

LITELLM_TEMPLATE="$(cat <<'LL'
apiVersion: v1
kind: Secret
metadata:
  name: litellm-keys
  namespace: litellm
stringData:
  master-key: "__MASTER_KEY__"
  provider-key: "__PROVIDER_KEY__"
---
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
        - {name: DATABASE_URL, value: "postgresql://llmproxy:llmproxy-db-pass@postgres.litellm:5432/litellm"}
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
)"

LITELLM="$(printf '%s' "$LITELLM_TEMPLATE" | sed "s|__MASTER_KEY__|${MASTER}|g; s|__PROVIDER_KEY__|${PROVIDER}|g")"

pct exec "$VMID" -- bash -s -- "$POSTGRES" "$LITELLM" "$PROVIDER" <<'OUTER'
set -euo pipefail
POSTGRES="$1"; LITELLM="$2"; PROVIDER="$3"
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
mkdir -p /tmp/kube-apply
printf '%s' "$POSTGRES" > /tmp/kube-apply/postgres.yaml
printf '%s' "$LITELLM" > /tmp/kube-apply/litellm.yaml
for f in postgres.yaml litellm.yaml; do $K apply -f /tmp/kube-apply/$f || exit 1; done
$K rollout status deploy/litellm -n litellm --timeout=300s
# deterministic C5 leg: register the model (NodePort from inside the node)
M=$($K get secret litellm-keys -n litellm -o jsonpath='{.data.master-key}' | base64 -d)
BODY=$(printf '{"model_name":"deepseek-v4-flash","litellm_params":{"model":"fireworks_ai/accounts/fireworks/models/deepseek-v4-flash-0731","api_key":"%s"}}' "$PROVIDER")
curl -s -m 30 -X POST -H "Authorization: Bearer $M" -H "Content-Type: application/json" -d "$BODY" http://127.0.0.1:31400/model/new | head -c 100
echo
OUTER
echo "litellm-kube applied"
