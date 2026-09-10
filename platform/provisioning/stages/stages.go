// Package stages is the shared "what to run" library of the world bring-up —
// the pure stage shell-scripts, manifest builders, and coordinate helpers that
// were authored in the operator-box `internal/cli/rebuild.go` pipeline. It
// deliberately carries NO execution: it builds command strings only. Both the
// box executor (`freehold build`, through its local runner) and the CP executor
// (`freehold-agent-tools world_build`, through its co-located runner) invoke the
// SAME stages through whatever runner handle they own — so a stage moved onto
// the CP runs byte-identically to the box.
//
// Secrets discipline: operators' keys never appear in these command strings.
// CP-generated values (litellm master/postgres) are single-quoted shell
// literals; provider keys and cert private keys ride the runner package and are
// injected by name at exec time. Every script that travels inside a
// single-quoted `pct exec ... -- bash -c '%s'` wrapper is single-quote-free.
package stages

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// RelayComposeDir is where the relay's docker-compose stack lives INSIDE the
// relay LXC's durable deploy mount (/srv/data/relay/deploy/compose), shared by
// the deploy-relay stage and agent-membership pct exec commands.
const RelayComposeDir = "/srv/data/relay/deploy/compose"

// CaddyEdgeDurableDir is the durable (backup=1) mirror of a slot's edge cert
// under /srv/data/k8s-volumes/caddy-edge/<slot>, read by the cert reuse gate and
// written by the cert install, so a teardown+rebuild reuses the cert.
func CaddyEdgeDurableDir(slot string) string {
	return "/srv/data/k8s-volumes/caddy-edge/" + slot
}

// SlotCertKeyEnv is the runner-injected env var name that carries a slot's cert
// PRIVATE KEY (sealed secret) so it never crosses argv/logs.
func SlotCertKeyEnv(slot string) string {
	return "CERT_KEY_" + strings.ToUpper(slot)
}

// GenSecretHex mints a 32-byte random hex secret (master key / postgres pw).
// Returns "" on RNG failure (the caller rejects empty).
func GenSecretHex() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// K3sInstallScript is the in-guest k3s install script, verbatim from the Rust
// installer (single-quote-free: it travels inside a single-quoted bash -c
// through the runner; the unit heredoc is unquoted-safe).
//
// K3sVersion is PINNED (INSTALL_K3S_VERSION) so the install skips the
// update.k3s.io channel lookup entirely: that channel redirects through
// github.com, and a network that MITMs/blackholes it (a self-signed cert on
// update.k3s.io) made the version resolution fail and fall back to a literal
// `stable` tag (404). A pinned version is also deterministic — the same k3s on
// every bring-up.
const K3sVersion = "v1.36.4+k3s1"

const K3sInstallScript = `set -euo pipefail
export PATH=/usr/local/bin:/root/.cargo/bin:$PATH
DEBIAN_FRONTEND=noninteractive apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl jq
if ! command -v kubectl >/dev/null 2>&1; then
  curl -sfL https://get.k3s.io -o /tmp/k3s-install.sh
  INSTALL_K3S_VERSION=__K3S_VERSION__ INSTALL_K3S_EXEC="server --disable traefik --disable servicelb --kubelet-arg feature-gates=KubeletInUserNamespace=true" sh /tmp/k3s-install.sh
fi
if ! grep -q KubeletInUserNamespace /etc/systemd/system/k3s.service 2>/dev/null; then
cat > /etc/systemd/system/k3s.service <<UNIT
[Unit]
Description=Lightweight Kubernetes
Documentation=https://k3s.io
Wants=network-online.target
After=network-online.target
[Install]
WantedBy=multi-user.target
[Service]
Type=notify
EnvironmentFile=-/etc/default/%N
ExecStartPre=-/sbin/modprobe br_netfilter
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/local/bin/k3s server --disable traefik --disable servicelb --kubelet-arg feature-gates=KubeletInUserNamespace=true
KillMode=process
Delegate=yes
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
TimeoutStartSec=0
Restart=always
RestartSec=5s
UNIT
  systemctl daemon-reload
  systemctl restart k3s
fi
KUBECTL=$(command -v kubectl)
K="$KUBECTL --kubeconfig /etc/rancher/k3s/k3s.yaml"
for i in $(seq 1 30); do
  $K get nodes >/dev/null 2>&1 && break
  sleep 10
done
$K get nodes 2>&1 | tail -2 | head -1
mkdir -p /srv/data/k8s-volumes
`

// K3sLocalPathDurableScript re-points the local-path StorageClass's backing
// store at the DURABLE plane mount (/srv/data/k8s-volumes — backup=1, survives
// a compute teardown) instead of the default /var/lib/rancher/k3s/storage on the
// ephemeral rootfs, so k8s PVCs (Caddy's cert, litellm postgres) survive a
// compute teardown and a rebuild. The WHOLE ConfigMap is re-emitted (config.json
// + helperPod/setup/teardown) so `apply` replaces the k3s-managed default
// wholesale rather than dropping the helper-pod config. Re-asserted on every
// reconcile: the k3s local-storage Addon can reset the ConfigMap on a k3s
// restart. Idempotent.
//
// SINGLE-QUOTE-FREE (like K3sInstallScript): it travels inside a
// `pct exec ... -- bash -c '%s'` single-quote wrapper, so no ' may appear. The
// ConfigMap is written with an UNQUOTED heredoc; its setup/teardown shell keys
// use \$VOL_DIR so the writer heredoc emits a literal $VOL_DIR and the wrapping
// `set -u` never sees an unbound variable.
const K3sLocalPathDurableScript = `set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
mkdir -p /tmp/local-path
cat > /tmp/local-path/local-path-config.yaml <<YAMLEOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-path-config
  namespace: kube-system
data:
  config.json: |
    {
      "nodePathMap":[
      {
        "node":"DEFAULT_PATH_FOR_NON_LISTED_NODES",
        "paths":["/srv/data/k8s-volumes"]
      }
      ]
    }
  helperPod.yaml: |
    apiVersion: v1
    kind: Pod
    metadata:
      name: helper-pod
    spec:
      containers:
      - name: helper-pod
        image: "rancher/mirrored-library-busybox:1.37.0"
        imagePullPolicy: IfNotPresent
  setup: |
    #!/bin/sh
    set -eu
    mkdir -m 0777 -p "\$VOL_DIR"
    chmod 700 "\$VOL_DIR/.."
  teardown: |
    #!/bin/sh
    set -eu
    rm -rf "\$VOL_DIR"
YAMLEOF
$K -n kube-system apply -f /tmp/local-path/local-path-config.yaml
$K -n kube-system rollout restart deployment/local-path-provisioner >/dev/null 2>&1 || true
$K -n kube-system rollout status deployment/local-path-provisioner --timeout=120s >/dev/null 2>&1 || true
rm -rf /tmp/local-path
echo LOCALPATH_DURABLE_OK
`

// litellmPostgresManifest is the postgres Deployment on the durable plane
// (local-path -> /srv/data/k8s-volumes), password from the k8s Secret.
const litellmPostgresManifest = `apiVersion: v1
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
  - {port: 5432}`

// litellmGatewayManifest is the litellm proxy (master key from the k8s
// Secret, fireworks egress pinned, NodePort 31400 for the LAN/agents).
const litellmGatewayManifest = `apiVersion: apps/v1
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
  - {port: 4000, targetPort: 4000, nodePort: 31400}`

// LitellmManifestScript applies the postgres + litellm kube resources inside
// the k3s LXC: namespace, Secrets, PVC (local-path -> the durable plane),
// deployments, NodePort service.
//
// The k8s Secrets read their values from the RUNNER-INJECTED env (requested by
// name in the exec — $LITELLM = the gateway master, $POSTGRES_PW = the postgres
// password) — NEVER shell literals, so no credential crosses the audited
// command or the logs (the runner redacts injected env values from output; the
// audit carries only the $REF). The provider key is deliberately absent: it
// rides the runner package for the registration leg, and nothing in k8s reads
// it.
func LitellmManifestScript(k3sVmid uint32) string {
	script := strings.ReplaceAll(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec __VMID__ -- sh -c"
$EX "mkdir -p /tmp/litellm-manifests"
$EX "$K create ns litellm 2>/dev/null || true"
# Secrets come from the runner-injected env (by name), never literals. They
# are created ONLY when absent: the first run's values are the authoritative
# ones (postgres initializes PGDATA against them, and the reused runner package
# keeps them) — a re-run must never re-roll them.
$EX "$K get secret litellm-keys -n litellm >/dev/null 2>&1 || $K create secret generic litellm-keys -n litellm --from-literal=master-key=\"$LITELLM\""
$EX "$K get secret litellm-pg -n litellm >/dev/null 2>&1 || $K create secret generic litellm-pg -n litellm --from-literal=postgres-pw=\"$POSTGRES_PW\""
# Manifests: written HOST-side (this exec runs on the PVE host where pct
# lives), pushed INTO the guest, then applied with the full kubectl path.
mkdir -p /tmp/litellm-manifests
cat >/tmp/litellm-manifests/postgres.yaml <<'YAML'
__POSTGRES__
YAML
cat >/tmp/litellm-manifests/litellm.yaml <<'YAML'
__LITELLM__
YAML
pct push __VMID__ /tmp/litellm-manifests/postgres.yaml /tmp/litellm-manifests/postgres.yaml
pct push __VMID__ /tmp/litellm-manifests/litellm.yaml /tmp/litellm-manifests/litellm.yaml
$EX "$K apply -f /tmp/litellm-manifests/postgres.yaml"
$EX "$K apply -f /tmp/litellm-manifests/litellm.yaml"
$EX "$K rollout status deploy/litellm -n litellm --timeout=300s"
echo LEG1_OK`,
		"__VMID__", strconv.FormatUint(uint64(k3sVmid), 10),
	)
	script = strings.ReplaceAll(script, "__POSTGRES__", litellmPostgresManifest)
	script = strings.ReplaceAll(script, "__LITELLM__", litellmGatewayManifest)
	return script
}

// LitellmRegisterScript registers the model through the litellm runner: the
// runner injects LITELLM (master, Bearer) + PROVIDER_KEY (body) by name, and
// curl's the gateway's REAL URL (gwURL — never 127.0.0.1, which is dead on the
// runner host; the gateway is reached at its k3s-node NodePort). The model_name
// is the CPA's litellm alias (agent.CpaLiteLLMModel); litellm maps it to the
// deepseek route behind the scenes.
func LitellmRegisterScript(gwURL, modelName string) string {
	return fmt.Sprintf(`set -euo pipefail
BODY=$(printf '{"model_name":"%s","litellm_params":{"model":"fireworks_ai/accounts/fireworks/models/deepseek-v4-flash-0731","api_key":"%%s"}}' "$PROVIDER_KEY")
curl -s -m 30 -X POST -H "Authorization: Bearer $LITELLM" -H "Content-Type: application/json" -d "$BODY" "%s/model/new" | head -c 300
echo
echo LEG2_OK
`, modelName, gwURL)
}

// CaddyManifestScript applies the Caddy kube resources inside the k3s LXC:
// namespace, durable PVC, ConfigMap with the rendered Caddyfile, hostNetwork
// Deployment, NodePort service. No secrets in argv (the Caddyfile is plain).
// `caddyManifest` is the already-rendered kube YAML (deploy.CaddyManifest).
func CaddyManifestScript(k3sVmid uint32, caddyManifest string) string {
	script := strings.ReplaceAll(`set -euo pipefail
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
EX="pct exec __VMID__ -- sh -c"
$EX "$K create ns caddy 2>/dev/null || true"
# The Caddyfile is written HOST-side then pushed in + applied (same shape as
# the litellm/postgres manifests). The certs are NOT here: F3 writes them into
# the caddy-data PVC at /data/tls on each issuance.
mkdir -p /tmp/caddy-manifests
$EX "mkdir -p /tmp/caddy-manifests"
cat >/tmp/caddy-manifests/caddy.yaml <<'YAML'
__CADDY__
YAML
pct push __VMID__ /tmp/caddy-manifests/caddy.yaml /tmp/caddy-manifests/caddy.yaml
$EX "$K apply -f /tmp/caddy-manifests/caddy.yaml"
$EX "$K rollout status deploy/caddy -n caddy --timeout=120s || true"
echo CADDY_OK`,
		"__VMID__", strconv.FormatUint(uint64(k3sVmid), 10),
	)
	script = strings.ReplaceAll(script, "__CADDY__", caddyManifest)
	return script
}

// DnsRec is one explicit record the CP-owned resolver serves (split-horizon
// <name> -> <ip>, registered via `control-plane dns add` inside the CP LXC).
type DnsRec struct {
	Name   string
	IP     string
	Source string
}

// DnsRecords is the world's explicit resolver records, in register order: the
// guest bare names (relay/cp/proxy/k3s/litellm) plus the dotted public hosts
// (relayHost/cpHost) that must resolve to the PROXY (Caddy), never directly to
// a LXC. A caller drops a record by passing an empty value for its field.
func DnsRecords(relayHost, relayIP, cpHost, cpIP, proxyIP, litellmIP string) []DnsRec {
	var recs []DnsRec
	if relayIP != "" {
		recs = append(recs, DnsRec{Name: "relay", IP: relayIP, Source: "world-build relay"})
	}
	if cpIP != "" {
		recs = append(recs, DnsRec{Name: "cp", IP: cpIP, Source: "world-build cp"})
	}
	if proxyIP != "" {
		recs = append(recs, DnsRec{Name: "proxy", IP: proxyIP, Source: "world-build proxy-static"})
		recs = append(recs, DnsRec{Name: "k3s", IP: proxyIP, Source: "world-build proxy-static"})
	}
	if litellmIP != "" {
		recs = append(recs, DnsRec{Name: "litellm", IP: litellmIP, Source: "world-build litellm"})
	}
	if relayHost != "" && proxyIP != "" {
		recs = append(recs, DnsRec{Name: relayHost, IP: proxyIP, Source: "world-build relay-via-proxy"})
	}
	if cpHost != "" && proxyIP != "" {
		recs = append(recs, DnsRec{Name: cpHost, IP: proxyIP, Source: "world-build cp-via-proxy"})
	}
	return recs
}

// DnsAddCmd is the pct exec that runs `control-plane dns add <name> <ip>
// <source> [--domain <base>]` INSIDE the CP LXC (the dnsmasq resolver), the
// same shape stageDnsRegister uses. Single-quote-wrapped at the innermost
// level only; callers pass validated names/IPs.
func DnsAddCmd(cpLxc uint32, binDir, stateDir, name, ip, source, searchBase string) string {
	rest := []string{"'add'", shellSingleQuote(name), shellSingleQuote(ip), shellSingleQuote(source)}
	if searchBase != "" {
		rest = append(rest, "'--domain'", shellSingleQuote(searchBase))
	}
	inner := fmt.Sprintf("'%s/control-plane' 'dns' --state-dir '%s' %s",
		binDir, stateDir, strings.Join(rest, " "))
	return fmt.Sprintf("pct exec %d -- sh -c %s", cpLxc, shellSingleQuote(inner))
}

// DnsPointCmd builds the two commands that point ONE guest at the CP resolver:
// the PVE-managed `pct set --nameserver` (durable across guest reboots) and the
// immediate resolv.conf rewrite (pct only regenerates it at the NEXT boot).
// router is the resolver's upstream nameserver — passed only for the CP guest
// itself (it keeps the router as a secondary so dnsmasq still has upstream);
// empty for relay/k3s.
func DnsPointCmd(vmid uint32, resolver, router string, searchBase string) (pctSet, resolvConf string) {
	nsList := resolver
	if router != "" {
		nsList += " " + router
	}
	parts := []string{"'set'", "'" + strconv.FormatUint(uint64(vmid), 10) + "'", "'--nameserver'", shellSingleQuote(nsList)}
	if searchBase != "" {
		parts = append(parts, "'--searchdomain'", shellSingleQuote(searchBase))
	}
	pctSet = "pct " + strings.Join(parts, " ")
	body := "nameserver " + resolver + "\n"
	if router != "" {
		body += "nameserver " + router + "\n"
	}
	if searchBase != "" {
		body = "search " + searchBase + "\n" + body
	}
	resolvConf = fmt.Sprintf("pct exec %d -- sh -c \"printf '%s' > /etc/resolv.conf\"", vmid, body)
	return pctSet, resolvConf
}

// DnsVerifyCmd asks the CP resolver's OWN loopback for a record and fails
// unless it answers exactly wantIP — proving the whole chain (render -> write ->
// dnsmasq load) landed, not merely that tcp/53 is open.
func DnsVerifyCmd(cpLxc uint32, name, wantIP string) string {
	return fmt.Sprintf("pct exec %d -- sh -c \"dig +short +time=2 +tries=1 %s @127.0.0.1 2>/dev/null | grep -qx '%s'\"",
		cpLxc, name, wantIP)
}

// ParsePctGateway extracts the `net0` `gw=` value from a `pct config` dump.
// Static guests carry `gw=<router>`; DHCP guests (`ip=dhcp`) have NO gw= —
// an empty result routes the caller to the default-route fallback.
func ParsePctGateway(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "net0:") {
			continue
		}
		for _, kv := range strings.Split(l, ",") {
			v, found := strings.CutPrefix(kv, "gw=")
			if found {
				v = strings.TrimSpace(v)
				if v != "" && strings.ContainsAny(v, "0123456789") {
					return v
				}
			}
		}
	}
	return ""
}

// CaddyCertInstallScript writes a slot's issued fullchain + the runner-injected
// private key ($CERT_KEY_<SLOT>) into the caddy-data PVC /data/tls/<slot> AND the
// durable mirror /srv/data/k8s-volumes/caddy-edge/<slot>, then rolls caddy. The
// private key is injected by the runner as an env var — never argv/logs.
func CaddyCertInstallScript(k3sVmid uint32, slot string, fullchain []byte) string {
	fcB64 := base64.StdEncoding.EncodeToString(fullchain)
	keyEnv := SlotCertKeyEnv(slot)
	return fmt.Sprintf(`set -e
printf '%%s' "${%s}" > /tmp/fh-key.pem
printf %s | base64 -d > /tmp/fh-fc.pem
pct push %d /tmp/fh-fc.pem /tmp/fc-%s.pem
pct push %d /tmp/fh-key.pem /tmp/key-%s.pem
pct exec %d -- sh -c '
set -e
K="/usr/local/bin/kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml"
PV=$($K get pvc caddy-data -n caddy -o jsonpath={.spec.volumeName})
PDIR=$($K get pv $PV -o jsonpath={.spec.local.path})
DIR=$PDIR/tls/%s
DUR=%s
mkdir -p "$DIR" "$DUR"
cp /tmp/fc-%s.pem "$DIR/fullchain.pem"
cp /tmp/key-%s.pem "$DIR/key.pem"
chmod 600 "$DIR/key.pem"
cp /tmp/fc-%s.pem "$DUR/fullchain.pem"
cp /tmp/key-%s.pem "$DUR/key.pem"
chmod 600 "$DUR/key.pem"
$K -n caddy rollout restart deploy/caddy >/dev/null 2>&1 || true
rm -f /tmp/fc-%s.pem /tmp/key-%s.pem
'
rm -f /tmp/fh-key.pem /tmp/fh-fc.pem
`,
		keyEnv, shellSingleQuote(fcB64), k3sVmid, slot, k3sVmid, slot, k3sVmid, // 1-7
		slot, CaddyEdgeDurableDir(slot), // 8-9 (DIR tls/slot, DUR)
		slot, slot, slot, slot, slot, slot) // 10-15
}

func shellSingleQuote(s string) string { return "'" + s + "'" }
