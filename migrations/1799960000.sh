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
#   caddy-data — tar /data out, swap, scale back, write back, restart the
#     edge. The TLS material is the state that matters (the mirror at
#     /srv/data/k8s-volumes/caddy-edge is a copy; this is the live one).
#
# The captures stay on the CP's durable plane as the permanent safety net.
# Idempotent per claim: a claim already bound under the carve-out is skipped,
# so a retry after a partial run finishes the rest — and a failed restore is
# retried from the surviving capture (a durable-but-empty db re-restores).
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

# ---- bounded waits: a timeout exits non-zero, the queue stays unmarked and
# ---- the next bring-up retries the whole idempotent flow.

wait_ready() { # ns selector -> pod name once its only container is ready
  local ns="$1" sel="$2" i=0 ok
  while [ "$i" -lt 300 ]; do
    ok=$(kubectl get pods -n "$ns" -l "$sel" -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null)
    [ "$ok" = "true" ] && kubectl get pods -n "$ns" -l "$sel" -o jsonpath='{.items[0].metadata.name}' && return 0
    sleep 2; i=$((i + 2))
  done
  echo "timeout waiting for a ready $ns pod ($sel)"; return 1
}

wait_gone() { # ns selector
  local ns="$1" sel="$2" i=0
  while [ "$i" -lt 120 ]; do
    [ -z "$(kubectl get pods -n "$ns" -l "$sel" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)" ] && return 0
    sleep 2; i=$((i + 2))
  done
  echo "timeout waiting for $ns pods ($sel) to stop"; return 1
}

wait_pv_gone() { # pv name — a recreated same-name claim must NOT rebind the old PV
  local i=0
  while [ "$i" -lt 180 ]; do
    kubectl get pv "$1" >/dev/null 2>&1 || return 0
    sleep 2; i=$((i + 2))
  done
  echo "timeout waiting for PV $1 to be reclaimed"; return 1
}

# ---- claim helpers

claim_exists() { kubectl get pvc "$2" -n "$1" >/dev/null 2>&1; }
pv_of() { kubectl get pvc "$2" -n "$1" -o jsonpath='{.spec.volumeName}'; }
pv_path_of() { kubectl get pv "$1" -o jsonpath='{.spec.local.path}'; }

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

assert_durable() { # ns claim
  local path
  path=$(pv_path_of "$(pv_of "$1" "$2")")
  case "$path" in
    "$DURABLE"/*) echo "$2: bound at $path";;
    *) echo "FATAL: $2 bound at '$path' — still outside $DURABLE"; return 1;;
  esac
}

# ---- litellm-pg-data: pg_dump -> swap -> restore ---------------------------

NS=litellm; CLAIM=litellm-pg-data; SEL=app=postgres
CAP_PG="$CAP/litellm-pg-data.sql.gz"
if claim_exists "$NS" "$CLAIM"; then
  PPATH=$(pv_path_of "$(pv_of "$NS" "$CLAIM")")
  if [ -z "$PPATH" ]; then
    echo "litellm-pg-data: PV is not local-path — skipping"
  else
    case "$PPATH" in
      "$DURABLE"/*) echo "litellm-pg-data: already on $PPATH";;
      *)
        REPS=$(kubectl get deploy postgres -n "$NS" -o jsonpath='{.spec.replicas}')
        POD=$(kubectl get pods -n "$NS" -l "$SEL" -o jsonpath='{.items[0].metadata.name}')
        # capture FIRST — the dump on the CP's durable plane is the safety net
        kubectl exec -n "$NS" "$POD" -- pg_dump -U llmproxy -d litellm | gzip > "$CAP_PG"
        [ -s "$CAP_PG" ] && gzip -t "$CAP_PG" || { echo "FATAL: pg capture failed ($CAP_PG)"; exit 1; }
        echo "captured $(du -h "$CAP_PG" | cut -f1) -> $CAP_PG"
        kubectl scale deploy postgres -n "$NS" --replicas=0
        wait_gone "$NS" "$SEL"
        swap_claim "$NS" "$CLAIM"
        kubectl scale deploy postgres -n "$NS" --replicas="${REPS:-1}"
        POD=$(wait_ready "$NS" "$SEL")
        i=0
        until kubectl exec -n "$NS" "$POD" -- pg_isready -U llmproxy -q >/dev/null 2>&1; do
          sleep 2; i=$((i + 2))
          [ "$i" -lt 300 ] || { echo "FATAL: postgres never accepted connections"; exit 1; }
        done
        TABLES=$(kubectl exec -n "$NS" "$POD" -- psql -U llmproxy -d litellm -tAc "SELECT count(*) FROM pg_tables WHERE schemaname='public'")
        if [ "$TABLES" = "0" ] && [ -s "$CAP_PG" ]; then
          gunzip -c "$CAP_PG" | kubectl exec -i -n "$NS" "$POD" -- psql -U llmproxy -d litellm -v ON_ERROR_STOP=1 --quiet
          TABLES=$(kubectl exec -n "$NS" "$POD" -- psql -U llmproxy -d litellm -tAc "SELECT count(*) FROM pg_tables WHERE schemaname='public'")
        fi
        [ "$TABLES" != "0" ] || { echo "FATAL: litellm db empty after restore"; exit 1; }
        echo "litellm-pg-data: restored ($TABLES public tables)"
        ;;
    esac
    assert_durable "$NS" "$CLAIM"
  fi
fi

# ---- caddy-data: tar out -> swap -> write back -----------------------------

NS=caddy; CLAIM=caddy-data; SEL=app=caddy
CAP_CADDY="$CAP/caddy-data.tgz"
if claim_exists "$NS" "$CLAIM"; then
  PPATH=$(pv_path_of "$(pv_of "$NS" "$CLAIM")")
  if [ -z "$PPATH" ]; then
    echo "caddy-data: PV is not local-path — skipping"
  else
    case "$PPATH" in
      "$DURABLE"/*) echo "caddy-data: already on $PPATH";;
      *)
        REPS=$(kubectl get deploy caddy -n "$NS" -o jsonpath='{.spec.replicas}')
        POD=$(kubectl get pods -n "$NS" -l "$SEL" -o jsonpath='{.items[0].metadata.name}')
        kubectl exec -n "$NS" "$POD" -- tar -czf - -C /data . > "$CAP_CADDY"
        [ -s "$CAP_CADDY" ] && gzip -t "$CAP_CADDY" || { echo "FATAL: caddy capture failed ($CAP_CADDY)"; exit 1; }
        echo "captured $(du -h "$CAP_CADDY" | cut -f1) -> $CAP_CADDY"
        kubectl scale deploy caddy -n "$NS" --replicas=0
        wait_gone "$NS" "$SEL"
        swap_claim "$NS" "$CLAIM"
        kubectl scale deploy caddy -n "$NS" --replicas="${REPS:-1}"
        POD=$(wait_ready "$NS" "$SEL")
        # the fresh caddy writes its own storage dirs on start; /data/tls is
        # where the certs live — absent means the write-back is still owed
        if kubectl exec -n "$NS" "$POD" -- sh -c '[ ! -d /data/tls ]' && [ -s "$CAP_CADDY" ]; then
          kubectl exec -i -n "$NS" "$POD" -- sh -c 'tar -xzf - -C /data' < "$CAP_CADDY"
          kubectl -n "$NS" rollout restart deploy caddy >/dev/null
          echo "caddy-data: certs written back, edge restarted"
        fi
        ;;
    esac
    assert_durable "$NS" "$CLAIM"
  fi
fi

# ---- anything else stranded outside the carve-out is reported, not touched --

kubectl get pv -o jsonpath='{range .items[*]}{.spec.local.path}{"\t"}{.spec.claimRef.namespace}/{.spec.claimRef.name}{"\n"}{end}' |
while IFS="$(printf '\t')" read -r path ref; do
  [ -n "$path" ] || continue
  case "$path" in "$DURABLE"/*) continue;; esac
  echo "WARN: local-path PV at '$path' ($ref) is outside $DURABLE — unbacked; give it its own migration"
done

echo "durable-plane PV migration complete"
