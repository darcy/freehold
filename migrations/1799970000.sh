echo "Migrate navidrome-data onto the durable plane"

# navidrome-data predates the /srv/data/k8s-volumes carve-out: its PV lives on
# the k3s rootfs, which no backup=1 mount covers. Capture it while the app is
# stopped, swap the claim so the provisioner lands a fresh dir under the
# carve-out, write the capture back, then bring Navidrome up. The capture stays
# on the CP's durable plane as the safety net; every step is retry-safe from
# any kill point (the capture re-runs while the claim is still on the rootfs,
# and a durable claim just reports already-moved).
#
# The move goes through a helper pod (busybox) that mounts the claim: a stopped
# Navidrome has no pod to exec into, and the helper gives a consistent SQLite
# snapshot rather than a live tar of a running database.

DURABLE=/srv/data/k8s-volumes
CAP="$STATE_DIR/pv-migration"
NS=navidrome
CLAIM=navidrome-data
DEPLOY=navidrome
SEL=app.kubernetes.io/name=navidrome
SIZE=5Gi
CAP_TGZ="$CAP/navidrome-data.tgz"
CAP_DONE="$CAP_TGZ.done"
HELPER=pv-migrate-navidrome

command -v kubectl >/dev/null 2>&1 || { echo "FATAL: no kubectl on the CP"; exit 1; }
[ -n "${KUBECONFIG:-}" ] && [ -f "$KUBECONFIG" ] || {
  echo "FATAL: no staged KUBECONFIG${FREEHOLD_KUBECONFIG_ERROR:+ ($FREEHOLD_KUBECONFIG_ERROR)}"
  exit 1
}
mkdir -p "$CAP"

claim_exists() { kubectl get pvc "$2" -n "$1" >/dev/null 2>&1; }
pv_of() { kubectl get pvc "$2" -n "$1" -o jsonpath='{.spec.volumeName}'; }
pv_path_of() { kubectl get pv "$1" -o jsonpath='{.spec.local.path}'; }
pod_names() { kubectl get pods -n "$1" -l "$2" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null; }
first_pod() { kubectl get pods -n "$1" -l "$2" -o jsonpath='{.items[0].metadata.name}'; }

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

capture_valid() { [ -s "$1" ] && tar -tzf "$1" >/dev/null 2>&1; }

provisioner_ok() {
  kubectl get cm local-path-config -n kube-system -o jsonpath='{.data.config\.json}' 2>/dev/null | grep -q "$DURABLE"
}

heal_provisioner() {
  provisioner_ok && return 0
  echo "the local-path carve-out is not in force — re-applying it"
  kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-path-config
  namespace: kube-system
data:
  config.json: |-
    { "nodePathMap": [ { "node": "DEFAULT_PATH_FOR_NON_LISTED_NODES", "paths": ["/srv/data/k8s-volumes"] } ] }
EOF
  kubectl rollout restart deploy/local-path-provisioner -n kube-system >/dev/null 2>&1 || true
  sleep 5
  provisioner_ok && return 0
  echo "FATAL: the carve-out is still not in force after a re-apply"; exit 1
}

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

swap_claim() { # ns claim
  local ns="$1" claim="$2" pv size
  pv=$(pv_of "$ns" "$claim")
  size=$(kubectl get pvc "$claim" -n "$ns" -o jsonpath='{.spec.resources.requests.storage}')
  kubectl patch pv "$pv" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}' >/dev/null
  kubectl delete pvc "$claim" -n "$ns"
  wait_pv_gone "$pv"
  recreate_claim "$ns" "$claim" "$size"
}

assert_durable() { # ns claim
  local path
  path=$(pv_path_of "$(pv_of "$1" "$2")")
  case "$path" in
    "$DURABLE"/*) echo "$2: bound at $path";;
    *) echo "FATAL: $2 bound at '$path' — still outside $DURABLE"; return 1;;
  esac
}

delete_helper() {
  kubectl -n "$NS" delete pod "$HELPER" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
}

create_helper() { # claim
  delete_helper
  kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $HELPER
  namespace: $NS
spec:
  restartPolicy: Never
  containers:
  - name: mover
    image: rancher/mirrored-library-busybox:1.37.0
    command: ["sleep", "600"]
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: $1
EOF
  kubectl -n "$NS" wait --for=condition=Ready pod/"$HELPER" --timeout=180s >/dev/null
}

ensure_down() {
  local cur
  cur=$(kubectl get deploy "$DEPLOY" -n "$NS" -o jsonpath='{.spec.replicas}')
  [ -n "$cur" ] && [ "$cur" = "0" ] || kubectl scale deploy "$DEPLOY" -n "$NS" --replicas=0
  wait_gone "$NS" "$SEL"
}

ensure_up() {
  local cur
  cur=$(kubectl get deploy "$DEPLOY" -n "$NS" -o jsonpath='{.spec.replicas}')
  [ -n "$cur" ] && [ "$cur" -ge 1 ] || kubectl scale deploy "$DEPLOY" -n "$NS" --replicas=1
  wait_ready "$NS" "$SEL"
}

capture_old() { # PV
  create_helper "$CLAIM"
  if ! kubectl exec -n "$NS" "$HELPER" -- tar -czf - -C /data . > "$CAP_TGZ.tmp"; then
    rm -f "$CAP_TGZ.tmp"
    delete_helper
    echo "FATAL: navidrome-data capture failed — the claim was not touched"
    exit 1
  fi
  if ! capture_valid "$CAP_TGZ.tmp"; then
    rm -f "$CAP_TGZ.tmp"
    delete_helper
    echo "FATAL: navidrome-data capture failed validation — the claim was not touched"
    exit 1
  fi
  mv -f "$CAP_TGZ.tmp" "$CAP_TGZ"
  printf '%s\n' "$1" > "$CAP_TGZ.pv.tmp" && mv -f "$CAP_TGZ.pv.tmp" "$CAP_TGZ.pv"
  rm -f "$CAP_DONE"
  echo "captured $(du -h "$CAP_TGZ" | cut -f1) -> $CAP_TGZ (from $1)"
  delete_helper
}

write_back() {
  [ -s "$CAP_TGZ" ] || return 0
  create_helper "$CLAIM"
  if ! kubectl exec -i -n "$NS" "$HELPER" -- tar -xzf - -C /data < "$CAP_TGZ"; then
    delete_helper
    echo "FATAL: navidrome-data write-back failed"; exit 1
  fi
  delete_helper
  touch "$CAP_DONE"
  echo "navidrome-data: capture written back"
}

if claim_exists "$NS" "$CLAIM"; then
  PV=$(pv_of "$NS" "$CLAIM")
  PPATH=""
  [ -n "$PV" ] && PPATH=$(pv_path_of "$PV")
  if [ -z "$PPATH" ]; then
    echo "navidrome-data: claim unbound — binding it"
    heal_provisioner
    capture_valid "$CAP_TGZ" || {
      echo "FATAL: navidrome-data's claim is unbound but its capture is missing or not a complete archive — no recoverable copy exists"
      exit 1
    }
    write_back
    ensure_up
    assert_durable "$NS" "$CLAIM"
  else
    case "$PPATH" in
      "$DURABLE"/*)
        echo "navidrome-data: already on $PPATH"
        if capture_valid "$CAP_TGZ" && [ ! -f "$CAP_DONE" ]; then
          write_back
        fi
        ensure_up
        assert_durable "$NS" "$CLAIM"
        ;;
      *)
        ensure_down
        if capture_valid "$CAP_TGZ" && [ -f "$CAP_TGZ.pv" ] && [ "$(cat "$CAP_TGZ.pv")" != "$PV" ]; then
          echo "navidrome-data: capture survives from PV $(cat "$CAP_TGZ.pv") — not re-capturing the re-bound volume"
        else
          capture_old "$PV"
        fi
        heal_provisioner
        rm -f "$CAP_DONE"
        swap_claim "$NS" "$CLAIM"
        write_back
        ensure_up
        assert_durable "$NS" "$CLAIM"
        ;;
    esac
  fi
elif capture_valid "$CAP_TGZ"; then
  echo "navidrome-data: claim absent but a capture survives — recreating"
  heal_provisioner
  rm -f "$CAP_DONE"
  recreate_claim "$NS" "$CLAIM" "$SIZE"
  write_back
  ensure_up
  assert_durable "$NS" "$CLAIM"
elif [ -e "$CAP_TGZ" ]; then
  echo "FATAL: navidrome-data's claim is gone and its capture is not a complete archive — no recoverable copy exists"; exit 1
else
  echo "navidrome-data: claim absent and no capture — nothing to do"
fi
