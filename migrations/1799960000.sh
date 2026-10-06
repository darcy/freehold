echo "Migrate pre-carve-out local-path PVs onto the durable plane"

# Worlds installed before the /srv/data/k8s-volumes carve-out keep their
# local-path PVs on the k3s rootfs: UNBACKED (no backup=1 mount covers the
# rootfs — snapshot/export/backup read the recorded mount list only), and a
# provisioner config change never moves existing volumes. A teardown destroys
# the k3s guest with them. This migrates the two known claims capture-then-
# swap, entirely through the k8s API from the CP (kubectl + the staged
# KUBECONFIG — no node exec, no new grants):
#
#   litellm-pg-data — pg_dump to the CP's durable plane, scale-0, PVC
#     delete/recreate (the provisioner lands the new dir under the carve-out),
#     scale back, restore, verify.
#   caddy-data — tar /data out, swap, scale-0, write the capture back through
#     a helper pod that mounts the claim, then bring the edge up. The TLS
#     material is the state that matters (the mirror at
#     /srv/data/k8s-volumes/caddy-edge is a copy; this is the live one).
#
# The captures stay on the CP's durable plane as the permanent safety net.
# Every step is retry-safe from ANY kill point (the queue's own timeout kills
# a slow-but-healthy run; the next bring-up resumes it):
#   - the capture re-runs while the claim is still on the rootfs (data
#     intact), bringing the deployment up first if a prior run left it down;
#   - a claim ABSENT with a capture present is a run that died between the
#     PVC delete and the recreate — the claim is re-applied at its
#     terraform-pinned size and the tail finishes from the capture;
#   - a durable claim finishes its interrupted tail from the surviving
#     capture: an empty db re-restores (--clean converges a partial), the
#     caddy capture re-extracts (tar overwrite is idempotent);
#   - a deployment left at 0 is scaled back up before anything needs a pod.
# Any OTHER claim stranded outside the carve-out is reported, not touched:
# it gets its own migration, not a surprise inside this one.

DURABLE=/srv/data/k8s-volumes
CAP="$STATE_DIR/pv-migration"

command -v kubectl >/dev/null 2>&1 || { echo "FATAL: no kubectl on the CP"; exit 1; }
[ -n "${KUBECONFIG:-}" ] && [ -f "$KUBECONFIG" ] || {
  echo "FATAL: no staged KUBECONFIG${FREEHOLD_KUBECONFIG_ERROR:+ ($FREEHOLD_KUBECONFIG_ERROR)}"
  exit 1
}
mkdir -p "$CAP"

# ---- bounded waits: sized so the healthy path sits inside the queue's kill
# ---- bound; a timeout exits non-zero, the queue stays unmarked, and the
# ---- retry resumes from wherever this run stopped.

wait_ready() { # ns selector -> pod name once its only container is ready
  local ns="$1" sel="$2" i=0 ok
  while [ "$i" -lt 180 ]; do
    ok=$(kubectl get pods -n "$ns" -l "$sel" -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null)
    [ "$ok" = "true" ] && kubectl get pods -n "$ns" -l "$sel" -o jsonpath='{.items[0].metadata.name}' && return 0
    sleep 2; i=$((i + 2))
  done
  echo "timeout waiting for a ready $ns pod ($sel)"; return 1
}

wait_gone() { # ns selector
  local ns="$1" sel="$2" i=0
  while [ "$i" -lt 90 ]; do
    [ -z "$(pod_names "$ns" "$sel")" ] && return 0
    sleep 2; i=$((i + 2))
  done
  echo "timeout waiting for $ns pods ($sel) to stop"; return 1
}

wait_pv_gone() { # pv name — a recreated same-name claim must NOT rebind the old PV
  local i=0
  while [ "$i" -lt 120 ]; do
    kubectl get pv "$1" >/dev/null 2>&1 || return 0
    sleep 2; i=$((i + 2))
  done
  echo "timeout waiting for PV $1 to be reclaimed"; return 1
}

# ---- claim helpers

claim_exists() { kubectl get pvc "$2" -n "$1" >/dev/null 2>&1; }
pv_of() { kubectl get pvc "$2" -n "$1" -o jsonpath='{.spec.volumeName}'; }
pv_path_of() { kubectl get pv "$1" -o jsonpath='{.spec.local.path}'; }
pod_names() { kubectl get pods -n "$1" -l "$2" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null; }
first_pod() { kubectl get pods -n "$1" -l "$2" -o jsonpath='{.items[0].metadata.name}'; }

# ensure_up: a partial run can leave the deployment at 0 (killed after
# scale-0, or after a wait_gone timeout). Terraform pins both deployments at
# one replica, so anything below that comes back before a pod is needed.
ensure_up() { # ns deploy sel -> pod name
  local ns="$1" deploy="$2" sel="$3" cur
  cur=$(kubectl get deploy "$deploy" -n "$ns" -o jsonpath='{.spec.replicas}')
  [ -n "$cur" ] && [ "$cur" -ge 1 ] || kubectl scale deploy "$deploy" -n "$ns" --replicas=1
  wait_ready "$ns" "$sel"
}

# swap_claim: delete + recreate the PVC so the provisioner lands a fresh dir
# under the carve-out. Caller has scaled the workload down first; the caller
# scales back up (local-path binds WaitForFirstConsumer — the pod triggers it).
swap_claim() { # ns claim
  local ns="$1" claim="$2" pv size
  pv=$(pv_of "$ns" "$claim")
  size=$(kubectl get pvc "$claim" -n "$ns" -o jsonpath='{.spec.resources.requests.storage}')
  # Reclaim Delete: the old dir goes with the PV even if the world ran Retain.
  kubectl patch pv "$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}' >/dev/null
  kubectl delete pvc "$claim" -n "$ns"
  wait_pv_gone "$pv"
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $claim
  namespace: $ns
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: local-path
  resources:
    requests:
      storage: $size
EOF
}

# recreate_claim: the known manifest for a claim a prior run deleted and never
# re-applied (sizes pinned in terraform's postgres.tf / caddy.tf).
recreate_claim() { # ns claim size
  kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $2
  namespace: $1
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: local-path
  resources:
    requests:
      storage: $3
EOF
}

assert_durable() { # ns claim
  local path
  path=$(pv_path_of "$(pv_of "$1" "$2")")
  case "$path" in
    "$DURABLE"/*) echo "$2: bound at $path";;
    *) echo "FATAL: $2 bound at '$path' — still outside $DURABLE"; return 1;;
  esac
}

# ---- litellm-pg-data: pg_dump -> swap -> restore ---------------------------

NS=litellm; CLAIM=litellm-pg-data; SEL=app=postgres; DEPLOY=postgres; SIZE=10Gi
CAP_PG="$CAP/litellm-pg-data.sql.gz"

finish_pg() { # pod — restore from the capture if one is owed, verify the restore
  local pod="$1" tables
  local i=0
  until kubectl exec -n "$NS" "$pod" -- pg_isready -U llmproxy -q >/dev/null 2>&1; do
    sleep 2; i=$((i + 2))
    [ "$i" -lt 180 ] || { echo "FATAL: postgres never accepted connections"; exit 1; }
  done
  # A capture exists only if THIS migration made one (or a partial run did):
  # no capture means nothing was ever moved here, and a fresh world's empty
  # db is litellm's own business (its app migrations create the schema).
  # Table count alone cannot tell a restored db from a PARTIAL one, so the
  # verified marker — not the count — gates the re-restore; --clean makes the
  # re-restore over a partial converge, and the marker lands only after the
  # count verifies.
  if [ -s "$CAP_PG" ] && [ ! -f "$CAP_PG.done" ]; then
    echo "restoring litellm-pg-data from $CAP_PG"
    gunzip -c "$CAP_PG" | kubectl exec -i -n "$NS" "$pod" -- psql -U llmproxy -d litellm -v ON_ERROR_STOP=1 --quiet
    tables=$(kubectl exec -n "$NS" "$pod" -- psql -U llmproxy -d litellm -tAc "SELECT count(*) FROM pg_tables WHERE schemaname='public'")
    [ "$tables" != "0" ] || { echo "FATAL: restore left an empty db"; exit 1; }
    touch "$CAP_PG.done"
    echo "litellm-pg-data: restored ($tables public tables)"
  else
    echo "litellm-pg-data: no restore owed — db untouched"
  fi
}

if claim_exists "$NS" "$CLAIM"; then
  PPATH=$(pv_path_of "$(pv_of "$NS" "$CLAIM")")
  case "$PPATH" in
    "$DURABLE"/*) echo "litellm-pg-data: already on $PPATH";;
    *)
      # capture needs a pod; a prior run's wait_gone timeout may have left none
      if [ -z "$(pod_names "$NS" "$SEL")" ]; then
        echo "litellm-pg-data: no pod to capture from — bringing one up"
        ensure_up "$NS" "$DEPLOY" "$SEL" >/dev/null
      fi
      POD=$(first_pod "$NS" "$SEL")
      # capture FIRST — the dump on the CP's durable plane is the safety net.
      # --clean: a re-restore over a PARTIAL prior restore drops and rebuilds
      # instead of erroring, so the retry converges instead of trusting half a
      # database.
      kubectl exec -n "$NS" "$POD" -- pg_dump --clean --if-exists -U llmproxy -d litellm | gzip > "$CAP_PG"
      [ -s "$CAP_PG" ] && gzip -t "$CAP_PG" || { echo "FATAL: pg capture failed ($CAP_PG)"; exit 1; }
      echo "captured $(du -h "$CAP_PG" | cut -f1) -> $CAP_PG"
      kubectl scale deploy "$DEPLOY" -n "$NS" --replicas=0
      wait_gone "$NS" "$SEL"
      swap_claim "$NS" "$CLAIM"
      ;;
  esac
  POD=$(ensure_up "$NS" "$DEPLOY" "$SEL")
  finish_pg "$POD"
  assert_durable "$NS" "$CLAIM"
elif [ -s "$CAP_PG" ]; then
  echo "litellm-pg-data: claim absent but a capture survives — a prior run died mid-swap; recreating"
  recreate_claim "$NS" "$CLAIM" "$SIZE"
  POD=$(ensure_up "$NS" "$DEPLOY" "$SEL")
  finish_pg "$POD"
  assert_durable "$NS" "$CLAIM"
else
  echo "litellm-pg-data: claim absent and no capture — nothing to do"
fi

# ---- caddy-data: tar out -> swap -> helper-pod write back -> edge up -------

NS=caddy; CLAIM=caddy-data; SEL=app=caddy; DEPLOY=caddy; SIZE=1Gi
CAP_CADDY="$CAP/caddy-data.tgz"

# The write-back goes through a helper pod that mounts the claim: the shipped
# Caddyfile statically requires /data/tls/*, so an emptied PVC cannot boot
# the edge and there may be no healthy caddy pod to exec into (and no
# readinessProbe to trust one by). caddy:2.8 is already on the node — no new
# image. Tar overwrites are idempotent, so every retry re-extracts the whole
# capture and a mid-stream kill converges.
write_back_caddy() {
  [ -s "$CAP_CADDY" ] || return 0
  kubectl -n "$NS" delete pod pv-migrate-caddy --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
  kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: pv-migrate-caddy
  namespace: $NS
spec:
  restartPolicy: Never
  containers:
  - name: mover
    image: caddy:2.8
    command: ["sleep", "600"]
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: $CLAIM
EOF
  kubectl -n "$NS" wait --for=condition=Ready pod/pv-migrate-caddy --timeout=180s >/dev/null
  kubectl exec -i -n "$NS" pv-migrate-caddy -- sh -c 'tar -xzf - -C /data' < "$CAP_CADDY"
  kubectl -n "$NS" delete pod pv-migrate-caddy --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
  echo "caddy-data: capture written back via helper pod"
}

if claim_exists "$NS" "$CLAIM"; then
  PPATH=$(pv_path_of "$(pv_of "$NS" "$CLAIM")")
  case "$PPATH" in
    "$DURABLE"/*) echo "caddy-data: already on $PPATH";;
    *)
      if [ -z "$(pod_names "$NS" "$SEL")" ]; then
        echo "caddy-data: no pod to capture from — bringing one up"
        ensure_up "$NS" "$DEPLOY" "$SEL" >/dev/null
      fi
      POD=$(first_pod "$NS" "$SEL")
      kubectl exec -n "$NS" "$POD" -- tar -czf - -C /data . > "$CAP_CADDY"
      [ -s "$CAP_CADDY" ] && gzip -t "$CAP_CADDY" || { echo "FATAL: caddy capture failed ($CAP_CADDY)"; exit 1; }
      echo "captured $(du -h "$CAP_CADDY" | cut -f1) -> $CAP_CADDY"
      kubectl scale deploy "$DEPLOY" -n "$NS" --replicas=0
      wait_gone "$NS" "$SEL"
      swap_claim "$NS" "$CLAIM"
      ;;
  esac
  write_back_caddy
  ensure_up "$NS" "$DEPLOY" "$SEL" >/dev/null
  assert_durable "$NS" "$CLAIM"
elif [ -s "$CAP_CADDY" ]; then
  echo "caddy-data: claim absent but a capture survives — a prior run died mid-swap; recreating"
  recreate_claim "$NS" "$CLAIM" "$SIZE"
  write_back_caddy
  ensure_up "$NS" "$DEPLOY" "$SEL" >/dev/null
  assert_durable "$NS" "$CLAIM"
else
  echo "caddy-data: claim absent and no capture — nothing to do"
fi

# ---- anything else stranded outside the carve-out is reported, not touched --

kubectl get pv -o jsonpath='{range .items[*]}{.spec.local.path}{"\t"}{.spec.claimRef.namespace}/{.spec.claimRef.name}{"\n"}{end}' |
while IFS="$(printf '\t')" read -r path ref; do
  [ -n "$path" ] || continue
  case "$path" in "$DURABLE"/*) continue;; esac
  echo "WARN: local-path PV at '$path' ($ref) is outside $DURABLE — unbacked; give it its own migration"
done

echo "durable-plane PV migration complete"
