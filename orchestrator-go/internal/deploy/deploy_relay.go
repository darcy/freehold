package deploy

import (
	"fmt"
	"os"
	"strings"
	"time"

	"freehold/orchestrator-go/internal/bootstrap"
	"freehold/orchestrator-go/internal/client"
)

// RelayDeploySpec mirrors orchestrator::relay::RelayDeploySpec.
type RelayDeploySpec struct {
	RelayName      string
	DeployDir      string
	HTTPPort       uint16
	BuzzRef        string
	LXc            *uint32
	OwnerPubkey    string
	RelayURL       string
	OperatorPubkey string
	Domain         *string
}

// RelayDeployResult is the deploy outcome.
type RelayDeployResult struct {
	RelayURL string
	Detail   string
}

// gitDataChownCmd returns the git-volume ownership fix run before first start.
func gitDataChownCmd() string {
	return "docker compose --env-file .env -f compose.yml run --rm --user 0 " +
		"--entrypoint chown relay -R buzz:buzz /data/git"
}

func isHexPubkey(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// InstallCmd reproduces the upstream bundle install command.
func InstallCmd(spec *RelayDeploySpec) string {
	dir := spec.DeployDir
	owner := spec.OwnerPubkey
	port := spec.HTTPPort
	host := ""
	if spec.Domain != nil {
		host = *spec.Domain
	} else {
		host = strings.TrimPrefix(strings.TrimPrefix(spec.RelayURL, "http://"), "https://")
		host = strings.TrimSuffix(host, "/")
	}
	rwsScheme := "ws"
	if spec.Domain != nil || strings.HasPrefix(strings.TrimSpace(spec.RelayURL), "https://") {
		rwsScheme = "wss"
	}
	rws := rwsScheme + "://" + host
	rhttp := spec.RelayURL
	if spec.Domain != nil {
		rhttp = "https://" + *spec.Domain
	} else {
		rhttp = strings.TrimSuffix(spec.RelayURL, "/")
	}
	return fmt.Sprintf(
		"set -e; cd %s/deploy/compose && (test -f .env || cp .env.example .env) && "+
			"(grep -q \"^BUZZ_HTTP_PORT=\" .env && "+
			"sed -i \"s/^BUZZ_HTTP_PORT=.*/BUZZ_HTTP_PORT=%d/\" .env || "+
			"echo \"BUZZ_HTTP_PORT=%d\" >> .env) && "+
			"(grep -q \"^RELAY_OWNER_PUBKEY=\" .env && "+
			"sed -i \"s/^RELAY_OWNER_PUBKEY=.*/RELAY_OWNER_PUBKEY=%s/\" .env || "+
			"echo \"RELAY_OWNER_PUBKEY=%s\" >> .env) && "+
			"(grep -q \"^BUZZ_DOMAIN=\" .env && "+
			"sed -i \"s|^BUZZ_DOMAIN=.*|BUZZ_DOMAIN=%s|\" .env || "+
			"echo \"BUZZ_DOMAIN=%s\" >> .env) && "+
			"(grep -q \"^RELAY_URL=\" .env && "+
			"sed -i \"s|^RELAY_URL=.*|RELAY_URL=%s|\" .env || "+
			"echo \"RELAY_URL=%s\" >> .env) && "+
			"(grep -q \"^BUZZ_MEDIA_BASE_URL=\" .env && "+
			"sed -i \"s|^BUZZ_MEDIA_BASE_URL=.*|BUZZ_MEDIA_BASE_URL=%s/media|\" .env || "+
			"echo \"BUZZ_MEDIA_BASE_URL=%s/media\" >> .env) && "+
			"(grep -q \"^BUZZ_MEDIA_SERVER_DOMAIN=\" .env && "+
			"sed -i \"s|^BUZZ_MEDIA_SERVER_DOMAIN=.*|BUZZ_MEDIA_SERVER_DOMAIN=%s|\" .env || "+
			"echo \"BUZZ_MEDIA_SERVER_DOMAIN=%s\" >> .env) && "+
			"for k in BUZZ_RELAY_PRIVATE_KEY BUZZ_GIT_HOOK_HMAC_SECRET POSTGRES_PASSWORD "+
			"REDIS_PASSWORD BUZZ_S3_ACCESS_KEY BUZZ_S3_SECRET_KEY; do "+
			"if grep -q \"^$k=CHANGE_ME\" .env; then "+
			"v=$(od -An -N32 -tx1 /dev/urandom | tr -d \"\\n \"); "+
			"sed -i \"s/^$k=CHANGE_ME.*/$k=$v/\" .env; "+
			"fi; done && "+
			"(grep -qE \"=CHANGE_ME\" .env && echo \"still has CHANGE_ME placeholders in "+
			"%s/deploy/compose/.env\" >&2 && exit 1 || true) && "+
			"%s && ./run.sh start",
		dir, port, port, owner, owner, host, host, rws, rws, rhttp, rhttp, host, host,
		dir, gitDataChownCmd())
}

// DeployRelay reproduces orchestrator::relay::deploy_relay.
func DeployRelay(clientConn *client.McpClient, target string, spec *RelayDeploySpec) (*RelayDeployResult, error) {
	if err := bootstrap.Plain(spec.RelayName); err != nil {
		return nil, err
	}
	if err := SafeDeployDir(spec.DeployDir); err != nil {
		return nil, err
	}
	if err := bootstrap.PlainPath(spec.BuzzRef); err != nil {
		return nil, err
	}
	if !isHexPubkey(spec.OwnerPubkey) {
		return nil, fmt.Errorf("owner pubkey must be a 64-character hex Nostr pubkey (got %q)", spec.OwnerPubkey)
	}
	if !isHexPubkey(spec.OperatorPubkey) {
		return nil, fmt.Errorf("operator pubkey must be a 64-character hex Nostr pubkey (got %q)", spec.OperatorPubkey)
	}
	if spec.Domain != nil {
		if err := bootstrap.Plain(*spec.Domain); err != nil {
			return nil, err
		}
		for _, c := range *spec.Domain {
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.'
			if !ok {
				return nil, fmt.Errorf("domain must be a bare DNS name (got %q)", *spec.Domain)
			}
		}
	}

	if err := checkDocker(clientConn, target, spec.LXc); err != nil {
		return nil, err
	}

	dl := fmt.Sprintf("set -e; mkdir -p %s && curl -fsSL https://github.com/block/buzz/archive/%s.tar.gz -o %s/buzz.tar.gz",
		spec.DeployDir, spec.BuzzRef, spec.DeployDir)
	if _, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, dl), "download bundle", 180); err != nil {
		return nil, err
	}
	extract := fmt.Sprintf("tar -xzf %s/buzz.tar.gz -C %s --strip-components=1", spec.DeployDir, spec.DeployDir)
	if _, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, extract), "extract bundle", 180); err != nil {
		return nil, err
	}
	install := InstallCmd(spec)
	if _, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, install), "run.sh start", 600); err != nil {
		return nil, err
	}

	// TLS on the domain (local CA posture).
	if spec.Domain != nil {
		compose := spec.DeployDir + "/deploy/compose"
		d := *spec.Domain
		tls := fmt.Sprintf(
			"set -e; cd %s && mkdir -p certs && "+
				"if [ ! -f certs/ca.crt ]; then "+
				"openssl req -x509 -newkey rsa:3072 -keyout certs/ca.key -out certs/ca.crt -days 3650 -nodes "+
				"-subj \"/CN=freehold local CA\" && "+
				"openssl req -newkey rsa:3072 -keyout certs/%s.key -out /tmp/%s.csr -nodes "+
				"-subj \"/CN=%s\" && "+
				"printf \"subjectAltName=DNS:%s\\n\" > /tmp/%s.ext && "+
				"openssl x509 -req -in /tmp/%s.csr -CA certs/ca.crt -CAkey certs/ca.key "+
				"-CAcreateserial -out certs/%s.crt -days 825 -extfile /tmp/%s.ext && "+
				"rm -f /tmp/%s.csr /tmp/%s.ext; fi && "+
				"printf \"%%s\\n\" \"%s {{\" "+
				"\"  encode zstd gzip\" "+
				"\"  tls /etc/caddy/certs/%s.crt /etc/caddy/certs/%s.key\" "+
				"\"  reverse_proxy relay:3000\" "+
				"\"}}\" > Caddyfile && "+
				"(grep -q \"certs:/etc/caddy/certs\" compose.caddy.yml || "+
				"sed -i \"s#- ./Caddyfile:/etc/caddy/Caddyfile:ro#- ./Caddyfile:/etc/caddy/Caddyfile:ro\\n      - ./certs:/etc/caddy/certs:ro#\" compose.caddy.yml) && "+
				"docker compose up -d >/dev/null 2>&1 && "+
				"echo TLS-LOCAL-CA-%s",
			compose, d, d, d, d, d, d, d, d, d, d, d, d, d, d)
		if _, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, tls), "tls local CA", 120); err != nil {
			return nil, err
		}
	}

	// Poll /_liveness.
	healthy := false
	var lastErr error
	for i := 0; i < 30; i++ {
		probe := fmt.Sprintf("curl -fsS http://127.0.0.1:%d/_liveness", spec.HTTPPort)
		_, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, probe), "relay liveness", 30)
		if err == nil {
			healthy = true
			break
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	if !healthy {
		return nil, fmt.Errorf("relay did not pass /_liveness on port %d within the poll window (last probe error: %v)",
			spec.HTTPPort, lastErr)
	}

	// Invite the operator (buzz-admin through the runner).
	operator := spec.OperatorPubkey
	if err := bootstrap.Plain(operator); err == nil {
		inviteDir := spec.DeployDir + "/deploy/compose"
		invite := addMemberCmd(inviteDir, operator, nil)
		if _, err := bootstrap.ExecToOK(clientConn, target, lxcCmd(spec.LXc, invite), "invite operator", 120); err != nil {
			// Surfaced warning; deploy stands.
			fmt.Fprintln(os.Stderr, "WARN: installer invite failed — the relay is up and the deploy stands:", err)
		}
	}

	relayURL := ""
	tlsNote := ""
	if spec.Domain != nil {
		relayURL = "https://" + *spec.Domain
		tlsNote = fmt.Sprintf(" TLS on the domain via the LOCAL CA (%s/deploy/compose/certs/ca.crt — import it into your devices to trust https://%s + wss://%s)",
			spec.DeployDir, *spec.Domain, *spec.Domain)
	} else {
		host := strings.TrimPrefix(strings.TrimPrefix(spec.RelayURL, "http://"), "https://")
		host = strings.TrimSuffix(host, "/")
		relayURL = fmt.Sprintf("http://%s:%d", host, spec.HTTPPort)
	}
	detail := fmt.Sprintf("Buzz relay deployed from the pinned bundle (%s, ref %s) and healthy at %s (loopback-proven)%s",
		spec.DeployDir, spec.BuzzRef, relayURL, tlsNote)
	return &RelayDeployResult{RelayURL: relayURL, Detail: detail}, nil
}

// addMemberCmd reproduces relay_member::add_member_cmd (buzz-admin through
// the runner at the compose dir).
func addMemberCmd(composeDir, pubkey string, role *string) string {
	roleSuffix := ""
	if role != nil && *role != "" {
		roleSuffix = " --role " + *role
	}
	return fmt.Sprintf("cd %s && docker compose exec -T relay buzz-admin add-member --pubkey %s%s",
		composeDir, pubkey, roleSuffix)
}
