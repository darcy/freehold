package vultr

import (
	"context"
	"fmt"
	"strings"

	"freehold/platform/provisioning"
)

// The Pangolin VPS: a created host whose payload is the Pangolin stack
// (docker compose) instead of PVE-on-Debian — the public path's edge
// (docs/NETWORK.md). It is NOT a world: no CP, no relay, no guests — one
// instance, the stack, and nothing else; the world's CP drives it over the
// integration API (the root key sealed to the console identity). The
// instance is a bill: the profile records it ([pangolin_host]) and
// `freehold uninstall --destroy-host` destroys it.
//
// The bootstrap flow below is the live-verified drive (the chunk-3 spike):
// signup → session login → root API key → the actions grant. The key prints
// ONCE (PANGOLIN_ROOT_KEY=…) for the caller to seal CP-side.

// The compose's API ports (3000/3001/3003) bind 127.0.0.1 on the VPS: the
// world's CP reaches the integration API through an SSH forward (the
// CP-minted tunnel key), never through the public NIC — the Bearer key
// never rides cleartext internet. Only 80/443 (the edge) and 51820/udp
// (the Newt WireGuard listener) publish.

// PangolinInstallScript turns the fresh Debian instance into the world's
// Pangolin edge: docker, the three-container stack (pangolin + gerbil +
// traefik) rendered for the world domain, the integration API enabled, the
// admin bootstrapped, and the ROOT INTEGRATION KEY printed once. No
// apostrophes (the script rides a single-quoted sh -c). Idempotent: an
// already-running stack short-circuits (a re-adopt re-ensures cheaply),
// re-printing the persisted key. The admin credentials persist root-only
// (/srv/pangolin/admin.env), so a lost key is recoverable: the re-run's
// bootstrap re-logs in with the persisted admin and mints a fresh key.
func PangolinInstallScript(worldDomain, email string) string {
	compose := pangolinCompose(worldDomain, email)
	conf := pangolinConfig(worldDomain)
	traefik := pangolinTraefikConf(email)
	dynamic := pangolinDynamicConf()
	return fmt.Sprintf(`set -e
if [ -f /srv/pangolin/root-key.env ] && docker ps --format "{{.Names}}" 2>/dev/null | grep -q pangolin; then
  echo "PANGOLIN_ROOT_KEY=$(cat /srv/pangolin/root-key.env)"
  echo pangolin-install-ok
  exit 0
fi
apt-get update -qq >/dev/null 2>&1 || true
apt-get install -y -qq ca-certificates curl openssl >/dev/null 2>&1
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo $VERSION_CODENAME) stable" > /etc/apt/sources.list.d/docker.list
apt-get update -qq >/dev/null 2>&1 || true
apt-get install -y -qq docker-ce docker-compose-plugin >/dev/null 2>&1
systemctl enable --now docker >/dev/null 2>&1 || true
mkdir -p /srv/pangolin/config/traefik /srv/pangolin/config/letsencrypt
if [ ! -f /srv/pangolin/config/config.yml ]; then
cat > /srv/pangolin/docker-compose.yaml <<COMPOSE
%sCOMPOSE
cat > /srv/pangolin/config/config.yml <<CONF
%sCONF
cat > /srv/pangolin/config/traefik/traefik_config.yml <<TRK
%sTRK
cat > /srv/pangolin/config/traefik/dynamic_config.yml <<DYN
%sDYN
fi
cd /srv/pangolin && docker compose up -d 2>/dev/null || docker-compose up -d
for i in $(seq 1 60); do
  curl -sf http://localhost:3000/api/v1/ >/dev/null 2>&1 && break
  sleep 2
done
curl -sf http://localhost:3000/api/v1/ >/dev/null 2>&1 || { echo pangolin-api-did-not-come-up; exit 1; }
if [ -f /srv/pangolin/root-key.env ]; then
  echo "PANGOLIN_ROOT_KEY=$(cat /srv/pangolin/root-key.env)"
  echo pangolin-install-ok
  exit 0
fi
if [ -f /srv/pangolin/admin.env ]; then
  . /srv/pangolin/admin.env
else
  ADMIN_EMAIL=%s
  ADMIN_PW=$(openssl rand -hex 16)
  printf "ADMIN_EMAIL=%%s\nADMIN_PW=%%s\n" "$ADMIN_EMAIL" "$ADMIN_PW" > /srv/pangolin/admin.env
  chmod 600 /srv/pangolin/admin.env
fi
curl -s -X PUT http://localhost:3000/api/v1/auth/signup -H "Content-Type: application/json" -H "X-CSRF-Token: x-csrf-protection" -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PW\"}" >/dev/null
curl -s -c /srv/pangolin/admin.cookies -X POST http://localhost:3000/api/v1/auth/login -H "Content-Type: application/json" -H "X-CSRF-Token: x-csrf-protection" -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PW\"}" >/dev/null
KEY_JSON=$(curl -s -b /srv/pangolin/admin.cookies -X PUT http://localhost:3000/api/v1/api-key -H "Content-Type: application/json" -H "X-CSRF-Token: x-csrf-protection" -d "{\"name\":\"freehold\"}")
KEY_ID=$(echo "$KEY_JSON" | tr ",{}" "\n" | grep apiKeyId | sed "s/.*://; s/\"//g")
KEY_SECRET=$(echo "$KEY_JSON" | tr ",{}" "\n" | grep -w apiKey | sed "s/.*://; s/\"//g")
if [ -z "$KEY_ID" ] || [ -z "$KEY_SECRET" ]; then echo "PANGOLIN_KEY_JSON=$KEY_JSON"; echo pangolin-key-mint-failed; exit 1; fi
printf "%%s\n" "$KEY_ID.$KEY_SECRET" > /srv/pangolin/root-key.env
chmod 600 /srv/pangolin/root-key.env
ORG_JSON=$(curl -s -X PUT http://localhost:3003/v1/org -H "Authorization: Bearer $KEY_ID.$KEY_SECRET" -H "Content-Type: application/json" -d "{\"orgId\":\"freehold\",\"name\":\"freehold\",\"subnet\":\"100.90.0.0/16\",\"utilitySubnet\":\"100.91.0.0/24\"}")
echo "$ORG_JSON" | grep -q orgId || { echo "PANGOLIN_ORG_JSON=$ORG_JSON"; echo pangolin-org-create-failed; exit 1; }
GRANT=$(curl -sf -b /srv/pangolin/admin.cookies -X POST http://localhost:3000/api/v1/api-key/$KEY_ID/actions -H "Content-Type: application/json" -H "X-CSRF-Token: x-csrf-protection" -d "{\"actionIds\":[\"createOrg\",\"createSite\",\"deleteSite\",\"createResource\",\"deleteResource\",\"createTarget\",\"deleteTarget\",\"getSite\",\"getResource\",\"listSites\",\"listResources\",\"listTargets\",\"getDomain\",\"listOrgDomains\",\"generateAccessToken\",\"updateResource\"]}") || { echo pangolin-actions-grant-failed; exit 1; }
echo "$GRANT" | grep -q "\"success\":true" || { echo "PANGOLIN_GRANT_JSON=$GRANT"; echo pangolin-actions-grant-failed; exit 1; }
echo "PANGOLIN_ROOT_KEY=$KEY_ID.$KEY_SECRET"
rm -f /srv/pangolin/admin.cookies
echo pangolin-install-ok
`, compose, conf, traefik, dynamic, email)
}

// PreparePangolin mints or re-adopts the world's Pangolin VPS: the same
// created-host lifecycle as the world host (the shared createInstance core,
// the door key born-with, the billed handle recorded first), the pangolin
// payload installed instead of PVE. worldDomain is the base domain the
// stack serves (the world's own domain — the app siblings ride it); email
// is the ACME contact. The root integration key prints ONCE on the
// session's output.
func PreparePangolin(ctx context.Context, s *provisioning.HostSession, existingID, worldDomain, email string) (*provisioning.Host, error) {
	if s.ExecOnHost == nil {
		return nil, fmt.Errorf("the vultr provider needs the session's host transport (ExecOnHost)")
	}
	c, err := clientFor(s)
	if err != nil {
		return nil, err
	}
	var hostID, hostIP string
	if existingID != "" {
		status, ip, gerr := c.Instance(ctx, existingID)
		switch {
		case gerr == nil && status == "active" && ip != "" && ip != "0.0.0.0":
			s.Host = "root@" + ip
			s.Print("  vultr pangolin instance %s alive at %s — re-adopting it\n", existingID, ip)
			hostID, hostIP = existingID, ip
		case gerr != nil && strings.Contains(gerr.Error(), "HTTP 404"):
			s.Print("  vultr pangolin instance %s is gone — re-creating it\n", existingID)
		case gerr != nil:
			return nil, fmt.Errorf("cannot verify the recorded vultr instance %s: %w — refusing to mint a second one beside it; retry when the API answers", existingID, gerr)
		default:
			return nil, fmt.Errorf("vultr instance %s exists but has no address yet (status %q, still settling) — retry in a minute", existingID, status)
		}
	}
	if hostID == "" {
		id, ip, err := createInstance(ctx, s, c, "freehold-"+s.World+"-pangolin")
		if err != nil {
			return nil, err
		}
		hostID, hostIP = id, ip
	}
	s.Print("  instance live at %s — waiting for sshd, then installing the Pangolin stack (docker; this takes minutes)…\n", hostIP)
	if err := waitSSH(ctx, s, sshdWaitTimeout); err != nil {
		return nil, fmt.Errorf("instance %s: %w", hostID, err)
	}
	if err := s.ExecOnHost(PangolinInstallScript(worldDomain, email), 1800); err != nil {
		return nil, fmt.Errorf("pangolin install on instance %s: %w", hostID, err)
	}
	return &provisioning.Host{ID: hostID, IP: hostIP}, nil
}

// pangolinCompose renders the stack's compose file. The ports publish on
// the instance's NIC (the public path): web (traefik), the Newt WireGuard
// listener, the external/integration API ports (the world's CP dials the
// integration one), and the internal API for the bootstrap curls.
func pangolinCompose(worldDomain, email string) string {
	return `name: pangolin
services:
  pangolin:
    image: fosrl/pangolin:1.24.0
    container_name: pangolin
    restart: unless-stopped
    ports:
      - "127.0.0.1:3000:3000"
      - "127.0.0.1:3001:3001"
      - "127.0.0.1:3003:3003"
    volumes:
      - ./config:/app/config
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3001/api/v1/"]
      interval: "5s"
      timeout: "3s"
      retries: 20
  gerbil:
    image: fosrl/gerbil:latest
    container_name: gerbil
    restart: unless-stopped
    depends_on:
      pangolin:
        condition: service_healthy
    command:
      - --reachableAt=http://gerbil:3004
      - --generateAndSaveKeyTo=/var/config/key
      - --remoteConfig=http://pangolin:3001/api/v1/
    volumes:
      - ./config/:/var/config
    cap_add:
      - NET_ADMIN
      - SYS_MODULE
    ports:
      - 51820:51820/udp
      - 21820:21820/udp
      - 443:443
      - 80:80
  traefik:
    image: traefik:v3.7
    container_name: traefik
    restart: unless-stopped
    network_mode: service:gerbil
    depends_on:
      pangolin:
        condition: service_healthy
    command:
      - --configFile=/etc/traefik/traefik_config.yml
    volumes:
      - ./config/traefik:/etc/traefik:ro
      - ./config/letsencrypt:/letsencrypt
`
}

// pangolinConfig renders the stack's config.yml: the world domain is the
// base domain (the app siblings ride it), the integration API on (the
// world's CP drives it), signup open for the bootstrap (the admin exists
// after the script; locking signup is the operator's hardening act).
func pangolinConfig(worldDomain string) string {
	dash := "pangolin." + worldDomain
	return fmt.Sprintf(`gerbil:
    start_port: 51820
    base_endpoint: "%s"
app:
    dashboard_url: "https://%s"
    log_level: "info"
    telemetry:
        anonymous_usage: false
domains:
    domain1:
        base_domain: "%s"
server:
    integration_port: 3003
    secret: "$(openssl rand -hex 32)"
    cors:
        origins: ["https://%s"]
        methods: ["GET", "POST", "PUT", "DELETE", "PATCH"]
        allowed_headers: ["X-CSRF-Token", "Content-Type"]
        credentials: false
flags:
    enable_integration_api: true
    require_email_verification: false
    disable_signup_without_invite: false
    disable_user_create_org: false
    allow_raw_resources: true
`, dash, dash, worldDomain, worldDomain)
}

// pangolinTraefikConf renders the traefik static config: the ACME resolver
// with the operator's contact (HTTP-01 — the VPS is the public edge; it
// never holds the DNS provider's credentials) and Pangolin's own config
// provider.
func pangolinTraefikConf(email string) string {
	return fmt.Sprintf(`api:
  insecure: false
  dashboard: false
providers:
  http:
    endpoint: http://pangolin:3001/api/v1/traefik-config
    pollInterval: 5s
  file:
    filename: /etc/traefik/dynamic_config.yml
experimental:
  plugins:
    badger:
      moduleName: github.com/fosrl/badger
      version: v1.4.1
log:
  level: INFO
  format: common
certificatesResolvers:
  letsencrypt:
    acme:
      httpChallenge:
        entryPoint: web
      email: "%s"
      storage: /letsencrypt/acme.json
      caServer: https://acme-v02.api.letsencrypt.org/directory
entryPoints:
  web:
    address: ":80"
  websecure:
    address: ":443"
    transport:
      respondingTimeouts:
        readTimeout: 30m
    http:
      tls:
        certResolver: letsencrypt
      encodedCharacters:
        allowEncodedSlash: true
        allowEncodedQuestionMark: true
serversTransport:
  insecureSkipVerify: true
ping:
  entryPoint: web
`, email)
}

// pangolinDynamicConf renders the traefik file provider's base (the shape
// the upstream stack ships; Pangolin's generated config rides the http
// provider beside it).
func pangolinDynamicConf() string {
	return `http:
  middlewares:
    redirect-to-https:
      redirectScheme:
        scheme: https
        permanent: true
`
}
