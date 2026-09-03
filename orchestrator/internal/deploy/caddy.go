package deploy

import (
	"fmt"
	"strings"
)

// Caddyfile renders Caddy's runtime config for freehold's TLS fronting proxy.
//
// Caddy neither runs its own ACME client nor auto-HTTPS here: freehold issues
// the wildcard certificate through embedded lego (DNS-01) and presents it from
// the durable volume, so the pod's relay/CP vhosts serve the cert BY HOST and
// reverse-proxy to the plain-HTTP LXCs on the LAN.
//
// The certs live at /data/tls/{fullchain,key}.pem on the Caddy data PVC (the
// lego stage F3 writes them there; Caddy reloads on change). auto_https is off
// so Caddy never fights the operator's external nginx for 80/443 or reaches for
// ACME itself.
//
// RelayUpstream selects which host Caddy fronts today: only the relay LXC
// (192.168.30.8:3000) is an in-band TLS/D1 blocker. The CP console still binds
// loopback by default, so cp.<domain> is left for when that bind moves to the
// LAN — Caddy will pick up a CP vhost from the same Render func then.
func RenderCaddyfile(domain, relayUpstream string) string {
	return fmt.Sprintf(`{
	auto_https off
}

relay.%s {
	tls /data/tls/fullchain.pem /data/tls/key.pem
	reverse_proxy %s
}
`, domain, relayUpstream)
}

// CaddyManifest is the kube body applied inside the k3s LXC: a durable PVC for
// the certs + config, a ConfigMap holding the rendered Caddyfile, and a
// hostNetwork Deployment (so Caddy binds 80/443 on the node LAN IP — the
// split-horizon wildcard apex *.domain points here) behind a NodePort service.
// The lego stage writes the cert into $CADDY_TLS_PATH on the PVC.
//
// __CADDYFILE__ is substituted with the rendered Caddyfile (single-quoted for
// the shell embedding — it contains no single quotes).
func CaddyManifest(caddyfile string) string {
	caddyfile = yamlBlockIndent(caddyfile)
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: caddy-data
  namespace: caddy
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: local-path
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: caddy-caddyfile
  namespace: caddy
data:
  Caddyfile: |
    %s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: caddy
  namespace: caddy
spec:
  replicas: 1
  selector:
    matchLabels: {app: caddy}
  template:
    metadata:
      labels: {app: caddy}
    spec:
      hostNetwork: true
      containers:
      - name: caddy
        image: caddy:2.8
        ports:
        - {containerPort: 80, name: http}
        - {containerPort: 443, name: https}
        volumeMounts:
        - {name: caddyfile, mountPath: /etc/caddy/Caddyfile, subPath: Caddyfile, readOnly: true}
        - {name: data, mountPath: /data}
      volumes:
      - name: caddyfile
        configMap: {name: caddy-caddyfile}
      - name: data
        persistentVolumeClaim: {claimName: caddy-data}
---
apiVersion: v1
kind: Service
metadata:
  name: caddy
  namespace: caddy
spec:
  type: NodePort
  selector: {app: caddy}
  ports:
  - {port: 80, targetPort: 80, nodePort: 30080}
  - {port: 443, targetPort: 443, nodePort: 30443}
`, caddyfile)
}

// yamlBlockIndent prefixes every non-empty line with 4 spaces so the Caddyfile
// nests as a child of the `Caddyfile: |` block scalar. Pure.
func yamlBlockIndent(s string) string {
	out := ""
	first := true
	for _, line := range splitLines(s) {
		if !first {
			out += "\n"
		}
		first = false
		if line == "" {
			out += "    "
			continue
		}
		out += "    " + line
	}
	return out
}

// splitLines splits on '\n' (retains a trailing empty element so a trailing
// newline becomes an indented blank line — harmless in a block scalar).
func splitLines(s string) []string {
	if s == "" {
		return []string{""}
	}
	return strings.Split(s, "\n")
}
