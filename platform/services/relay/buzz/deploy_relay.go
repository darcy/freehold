package deploy

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"freehold/platform/provisioning"
	"freehold/platform/provisioning/bootstrap"
	"freehold/platform/provisioning/deploy"
)

// DefaultBufRef is the pinned block/buzz ref to fetch. On main past
// 1972b7d the compose's minio images moved to pgsty/silo+mc (Docker Hub,
// sha-pinned) — quay's minio repo went private and every pull 401s.
const DefaultBufRef = "1972b7d256a5d0bb90eea6ef78e93d6b55bb1a12"

const (
	// rustfsImage is the pinned S3 engine the deploy patches the bundle's
	// minio service onto. RustFS is a MinIO-compatible Apache-2.0 engine: the
	// relay's S3 surface (rust-s3 SigV4 path-style, ListObjectVersions-driven
	// deletion, PutBucketPolicy init, range reads, bulk deletes) is covered,
	// and the service NAME stays `minio` so BUZZ_S3_ENDPOINT and the compose
	// depends_on hold. Pinned tag+digest, multi-arch (amd64/arm64).
	rustfsImage = "rustfs/rustfs:1.0.1@sha256:1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c"

	// mediaSrcImage is the pinned engine that re-serves a world's OLD minio
	// volume during the one-time media migration. It is the same silo pin the
	// bundle's compose carries (MinIO-fork xl-format lineage — reads volumes
	// written by any prior minio/silo generation); it never needs to track
	// upstream bumps, the old volume is frozen after cutover.
	mediaSrcImage = "pgsty/silo:RELEASE.2026-09-16T00-00-00Z@sha256:635197cb9f36d01bee221d34d1c7d7960f6a95c48b0b6c01d99cd13bdae51a46"

	// mediaMoverImage is the pinned mc client the mover mirrors with.
	mediaMoverImage = "pgsty/mc:RELEASE.2026-09-16T00-00-00Z@sha256:cfc83108c3abb371f8fb84d99c1fdc88f8c237e022409b0081fb7c0a3be634dd"

	// mediaMarker is written into the rustfs volume after a verified mirror;
	// a world whose volume carries it is post-cutover and never re-mirrored
	// (the old volume is frozen history, and re-mirroring over live data
	// would resurrect deleted media).
	mediaMarker = ".media-migrated-from-minio"

	// mediaMigrateTimeout bounds the mirror step (mc mirror + verify).
	mediaMigrateTimeout uint64 = 3600
)

// checkDocker passes the B1 gate: docker + compose must exist on the target
// (or inside its LXC).
func checkDocker(exec provisioning.GuestExecFunc, guest string) error {
	out, err := exec(guest, "command -v docker && docker compose version", 60)
	if err != nil {
		return err
	}
	return bootstrap.ExpectOK(out, "docker gate")
}

// RelayDeploySpec mirrors the relay deploy spec.
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

// composePatchScript returns the (guest-run) script that patches the bundle's
// compose.yml onto the RustFS engine. The bundle is re-extracted at every
// build, so the patch is marker-gated and re-applies after every extract.
// It replaces ONLY the `minio` service block — the service NAME survives so
// the relay's BUZZ_S3_ENDPOINT (http://minio:9000), the relay's depends_on,
// and the minio-init mc job (mb + anonymous-none, both supported by RustFS)
// hold unchanged — and adds the new data volume. Shipped base64 (the sh -c
// exec wrapper is single-quote-free; the script itself is not).
func composePatchScript(dir string) string {
	return "set -e\ncd " + dir + "/deploy/compose\n" +
		"grep -q \"rustfs/rustfs\" compose.yml && exit 0\n" +
		`awk '
  BEGIN { inminio = 0; replaced = 0; voladded = 0 }
  /^  minio:$/ && replaced == 0 {
    replaced = 1
    inminio = 1
    print "  minio:"
    print "    image: ` + rustfsImage + `"
    print "    environment:"
    print "      RUSTFS_VOLUMES: /data"
    print "      RUSTFS_ADDRESS: 0.0.0.0:9000"
    print "      RUSTFS_ACCESS_KEY: ${BUZZ_S3_ACCESS_KEY:?set BUZZ_S3_ACCESS_KEY}"
    print "      RUSTFS_SECRET_KEY: ${BUZZ_S3_SECRET_KEY:?set BUZZ_S3_SECRET_KEY}"
    print "    volumes:"
    print "      - buzz-rustfs-data:/data"
    print "    healthcheck:"
    print "      test: [\"CMD\", \"curl\", \"-f\", \"http://127.0.0.1:9000/health\"]"
    print "      interval: 5s"
    print "      timeout: 5s"
    print "      retries: 12"
    print "      start_period: 10s"
    print "    restart: unless-stopped"
    print "    networks:"
    print "      - buzz-net"
    next
  }
  inminio == 1 && /^  minio-init:$/ { inminio = 0 }
  inminio == 1 { next }
  /^networks:$/ && voladded == 0 {
    print "  buzz-rustfs-data:"
    print "    labels:"
    print "      com.buzz.volume: rustfs"
    voladded = 1
  }
  { print }
' compose.yml > compose.yml.next
mv compose.yml.next compose.yml
grep -q "rustfs/rustfs" compose.yml
grep -q "buzz-rustfs-data" compose.yml
`
}

// mediaMoverScript returns the (guest-run) one-time migration of a world's
// old minio media volume onto the rustfs volume. Engines cannot read each
// other's on-disk format, so the copy is object-level over S3: the old
// volume is re-served by the pinned silo image (MinIO-fork xl-format lineage — it
// reads what any prior minio/silo generation wrote) and mc mirrors the
// bucket across, verified with mc diff before the marker (and the relay)
// come back. Compose project-prefixes the volume names, so both are DERIVED
// the same way the network is — a bare name would be an orphan volume and
// the migration would silently skip (an upgraded world would serve an empty
// engine green). Idempotent at every point: the chown heals the engine's
// root-owned data dir, mirror resumes a partial run, diff gates the cutover,
// and the marker makes the whole step a no-op afterward (post-cutover the
// old volume is frozen history — the relay is the only writer and it now
// writes to the rustfs volume). Skips worlds with no old volume (fresh
// installs) at all.
func mediaMoverScript(dir string) string {
	return "set -e\ncd " + dir + "/deploy/compose\n" +
		`set -a
. ./.env
set +a
BUZZ_S3_BUCKET="${BUZZ_S3_BUCKET:-buzz-media}"
NET=$(docker network ls --format '{{.Name}}' | grep -E 'buzz-net$' | head -1)
[ -n "$NET" ]
docker network inspect "$NET" > /dev/null
RUSTVOL=$(docker volume ls --format '{{.Name}}' | grep -E 'buzz-rustfs-data$' | head -1)
[ -n "$RUSTVOL" ]
docker run --rm -v "$RUSTVOL":/data alpine chown -R 10001:10001 /data
OLDVOL=$(docker volume ls --format '{{.Name}}' | grep -E 'buzz-minio-data$' | head -1)
[ -n "$OLDVOL" ] || exit 0
if docker run --rm -v "$RUSTVOL":/data alpine test -f /data/` + mediaMarker + `; then
  exit 0
fi
docker rm -f buzz-media-src > /dev/null 2>&1 || true
docker run -d --name buzz-media-src --network "$NET" \
  -v "$OLDVOL":/data \
  -e MINIO_ROOT_USER="$BUZZ_S3_ACCESS_KEY" \
  -e MINIO_ROOT_PASSWORD="$BUZZ_S3_SECRET_KEY" \
  ` + mediaSrcImage + ` server /data
cleanup() { docker rm -f buzz-media-src > /dev/null 2>&1 || true; }
trap cleanup EXIT
docker run --rm --network "$NET" \
  -e SRC_AK="$BUZZ_S3_ACCESS_KEY" -e SRC_SK="$BUZZ_S3_SECRET_KEY" \
  --entrypoint /bin/sh ` + mediaMoverImage + ` -euc '
for i in $(seq 1 90); do
  mc alias set src http://buzz-media-src:9000 "$SRC_AK" "$SRC_SK" && exit 0
  sleep 2
done
echo "the old media engine never answered" >&2
exit 1
'
docker run --rm --network "$NET" \
  -e SRC_AK="$BUZZ_S3_ACCESS_KEY" -e SRC_SK="$BUZZ_S3_SECRET_KEY" \
  -e DST_AK="$BUZZ_S3_ACCESS_KEY" -e DST_SK="$BUZZ_S3_SECRET_KEY" \
  -e BUCKET="$BUZZ_S3_BUCKET" \
  --entrypoint /bin/sh ` + mediaMoverImage + ` -euc '
mc alias set src http://buzz-media-src:9000 "$SRC_AK" "$SRC_SK"
mc alias set dst http://minio:9000 "$DST_AK" "$DST_SK"
mc mb --ignore-existing "dst/$BUCKET"
mc mirror --overwrite "src/$BUCKET" "dst/$BUCKET"
mc diff "src/$BUCKET" "dst/$BUCKET"
'
docker run --rm -v "$RUSTVOL":/data alpine touch /data/` + mediaMarker + `
`
}

// shipScript wraps a script for the single-quote-free exec surface: base64
// rides the sh -c wrapper, the decoded file runs clean on the guest.
func shipScript(path, script string) string {
	return "echo " + base64.StdEncoding.EncodeToString([]byte(script)) +
		" | base64 -d > " + path + " && sh " + path
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

// InstallCmd reproduces the upstream bundle install command. Domain-anchored
// deploys also advertise the NIP-AB device-pairing sidecar
// (BUZZ_PAIRING_RELAY_URL — the relay publishes it in NIP-11 so Buzz clients
// pair through the edge's /pair route; the sidecar itself is a k3s pod
// deployed by the terraform services phase, not part of this compose stack).
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
	pairEnv := ""
	if spec.Domain != nil {
		pairURL := "wss://" + *spec.Domain + "/pair"
		pairEnv = fmt.Sprintf(
			"(grep -q \"^BUZZ_PAIRING_RELAY_URL=\" .env && "+
				"sed -i \"s|^BUZZ_PAIRING_RELAY_URL=.*|BUZZ_PAIRING_RELAY_URL=%s|\" .env || "+
				"echo \"BUZZ_PAIRING_RELAY_URL=%s\" >> .env) && ",
			pairURL, pairURL)
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
			// the pinned bundle's .env.example ships a placeholder CORS allowlist
			// (BUZZ_CORS_ORIGINS=https://buzz.example.com). Left in place it
			// rejects the desktop webview's preflight (origin tauri://localhost /
			// http://tauri.localhost) — the invite mint/claim/policy fetches are
			// webview-issued and CORS-bound, while WS and the Rust bridge never
			// apply CORS, so ONLY invite links break. Empty = the relay's own
			// default (build_cors_layer returns permissive when unset); auth is
			// unaffected — every mutating route still requires a signed NIP-98
			// header.
			"(grep -q \"^BUZZ_CORS_ORIGINS=\" .env && "+
			"sed -i \"s/^BUZZ_CORS_ORIGINS=.*/BUZZ_CORS_ORIGINS=/\" .env || "+
			"echo \"BUZZ_CORS_ORIGINS=\" >> .env) && "+
			pairEnv+
			"for k in BUZZ_RELAY_PRIVATE_KEY BUZZ_GIT_HOOK_HMAC_SECRET POSTGRES_PASSWORD "+
			"REDIS_PASSWORD BUZZ_S3_ACCESS_KEY BUZZ_S3_SECRET_KEY; do "+
			"if grep -q \"^$k=CHANGE_ME\" .env; then "+
			"v=$(od -An -N32 -tx1 /dev/urandom | tr -d \"\\n \"); "+
			"sed -i \"s/^$k=CHANGE_ME.*/$k=$v/\" .env; "+
			"fi; done && "+
			"(grep -qE \"=CHANGE_ME\" .env && echo \"still has CHANGE_ME placeholders in "+
			"%s/deploy/compose/.env\" >&2 && exit 1 || true) && "+
			// The bundle's minio service is patched onto the RustFS engine
			// (marker-gated, survives every re-extract). The relay itself is
			// NOT started here: the engine comes up alone (up minio
			// minio-init) so the media mover can verify the one-time mirror
			// before the relay ever serves from it — DeployRelay runs the
			// mover, then ./run.sh start as its own step.
			shipScript("/tmp/fh-rustfs-patch.sh", composePatchScript(dir))+" && "+
			"%s && "+
			"docker compose --env-file .env stop relay minio minio-init && "+
			"docker compose --env-file .env up -d minio minio-init",
		dir, port, port, owner, owner, host, host, rws, rws, rhttp, rhttp, host, host,
		dir, gitDataChownCmd())
}

// DeployRelay reproduces the relay deploy driver.
func DeployRelay(exec provisioning.GuestExecFunc, spec *RelayDeploySpec) (*RelayDeployResult, error) {
	guest := ""
	if spec.LXc != nil {
		guest = strconv.FormatUint(uint64(*spec.LXc), 10)
	}
	runToOK := func(step, cmd string, timeoutS uint64) error {
		out, err := exec(guest, cmd, timeoutS)
		if err != nil {
			return err
		}
		return bootstrap.ExpectOK(out, step)
	}
	if err := bootstrap.Plain(spec.RelayName); err != nil {
		return nil, err
	}
	if err := deploy.SafeDeployDir(spec.DeployDir); err != nil {
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

	if err := checkDocker(exec, guest); err != nil {
		return nil, err
	}

	dl := fmt.Sprintf("set -e; mkdir -p %s && ok=0; for i in 1 2 3; do if curl -fsSL --max-time 180 --retry 2 https://github.com/block/buzz/archive/%s.tar.gz -o %s/buzz.tar.gz; then ok=1; break; fi; sleep 3; done; [ \"$ok\" = 1 ]",
		spec.DeployDir, spec.BuzzRef, spec.DeployDir)
	if err := runToOK("download bundle", dl, 600); err != nil {
		return nil, err
	}
	extract := fmt.Sprintf("tar -xzf %s/buzz.tar.gz -C %s --strip-components=1", spec.DeployDir, spec.DeployDir)
	if err := runToOK("extract bundle", extract, 180); err != nil {
		return nil, err
	}
	install := InstallCmd(spec)
	if err := runToOK("stage relay engine", install, 600); err != nil {
		return nil, err
	}
	// The one-time media migration runs while the relay is still down and the
	// rustfs engine is already up (InstallCmd staged it): a world with a live
	// minio volume gets its bucket mirrored + verified here; fresh worlds
	// no-op. Only after the mirror verifies does the relay start serving.
	if err := runToOK("migrate media", shipScript("/tmp/fh-media-mover.sh", mediaMoverScript(spec.DeployDir)), mediaMigrateTimeout); err != nil {
		return nil, err
	}
	start := "cd " + spec.DeployDir + "/deploy/compose && ./run.sh start"
	if err := runToOK("run.sh start", start, 600); err != nil {
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
		if err := runToOK("tls local CA", tls, 120); err != nil {
			return nil, err
		}
	}

	// Poll /_liveness.
	healthy := false
	var lastErr error
	for i := 0; i < 30; i++ {
		probe := fmt.Sprintf("curl -fsS http://127.0.0.1:%d/_liveness", spec.HTTPPort)
		err := runToOK("relay liveness", probe, 30)
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
		if err := runToOK("invite operator", invite, 120); err != nil {
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
