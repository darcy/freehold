package deploy

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCmdPairingAdvertisedOnDomainDeploys(t *testing.T) {
	domain := "relay.example.test"
	cmd := InstallCmd(&RelayDeploySpec{
		RelayName:      "relay",
		DeployDir:      "/srv/data/relay",
		HTTPPort:       3000,
		BuzzRef:        DefaultBufRef,
		OwnerPubkey:    strings.Repeat("a", 64),
		RelayURL:       "https://" + domain,
		OperatorPubkey: strings.Repeat("b", 64),
		Domain:         &domain,
	})
	for _, want := range []string{
		"BUZZ_PAIRING_RELAY_URL=wss://relay.example.test/pair",
		"grep -q \"^BUZZ_PAIRING_RELAY_URL=\" .env",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("InstallCmd missing %q:\n%s", want, cmd)
		}
	}
}

func TestInstallCmdNoPairingWithoutDomain(t *testing.T) {
	cmd := InstallCmd(&RelayDeploySpec{
		RelayName:      "relay",
		DeployDir:      "/srv/data/relay",
		HTTPPort:       3000,
		BuzzRef:        DefaultBufRef,
		OwnerPubkey:    strings.Repeat("a", 64),
		RelayURL:       "http://192.168.30.8:3000",
		OperatorPubkey: strings.Repeat("b", 64),
	})
	if strings.Contains(cmd, "BUZZ_PAIRING_RELAY_URL") {
		t.Errorf("IP-anchored deploy must not advertise pairing:\n%s", cmd)
	}
}

// The pinned bundle's .env.example ships a placeholder CORS allowlist
// (BUZZ_CORS_ORIGINS=https://buzz.example.com). Left in place it rejects the
// desktop webview's preflight — the invite mint/claim fetches are
// webview-issued and CORS-bound, so the invite link silently fails while
// everything else works. InstallCmd must neutralize the var (empty = the
// relay's permissive default).
func TestInstallCmdNeutralizesPlaceholderCors(t *testing.T) {
	cmd := InstallCmd(&RelayDeploySpec{
		RelayName:      "relay",
		DeployDir:      "/srv/data/relay",
		HTTPPort:       3000,
		BuzzRef:        DefaultBufRef,
		OwnerPubkey:    strings.Repeat("a", 64),
		RelayURL:       "http://192.168.30.8:3000",
		OperatorPubkey: strings.Repeat("b", 64),
	})
	for _, want := range []string{
		"grep -q \"^BUZZ_CORS_ORIGINS=\" .env",
		"sed -i \"s/^BUZZ_CORS_ORIGINS=.*/BUZZ_CORS_ORIGINS=/\" .env",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("InstallCmd missing %q:\n%s", want, cmd)
		}
	}
}

// The payload rides a single-quoted `sh -c` (LxcExec) — a single quote in the
// emitted command breaks the transport.
func TestInstallCmdSingleQuoteFree(t *testing.T) {
	domain := "relay.example.test"
	for _, spec := range []*RelayDeploySpec{
		{
			RelayName:      "relay",
			DeployDir:      "/srv/data/relay",
			HTTPPort:       3000,
			BuzzRef:        DefaultBufRef,
			OwnerPubkey:    strings.Repeat("a", 64),
			RelayURL:       "https://" + domain,
			OperatorPubkey: strings.Repeat("b", 64),
			Domain:         &domain,
		},
		{
			RelayName:      "relay",
			DeployDir:      "/srv/data/relay",
			HTTPPort:       3000,
			BuzzRef:        DefaultBufRef,
			OwnerPubkey:    strings.Repeat("a", 64),
			RelayURL:       "http://192.168.30.8:3000",
			OperatorPubkey: strings.Repeat("b", 64),
		},
	} {
		if cmd := InstallCmd(spec); strings.ContainsRune(cmd, '\'') {
			t.Errorf("InstallCmd emitted a single quote (breaks the sh -c wrapper):\n%s", cmd)
		}
	}
}

// InstallCmd stages the rustfs engine (patch + chown + stop + engine-up) but
// must NOT start the relay: DeployRelay runs the media mover between the
// engine-up and ./run.sh start so the one-time mirror verifies first. The
// quay re-point sed is dead on the 1972b7d bundle (no `image: minio/` left)
// and must not come back.
func TestInstallCmdStagesRustfsEngine(t *testing.T) {
	cmd := InstallCmd(&RelayDeploySpec{
		RelayName:      "relay",
		DeployDir:      "/srv/data/relay",
		HTTPPort:       3000,
		BuzzRef:        DefaultBufRef,
		OwnerPubkey:    strings.Repeat("a", 64),
		RelayURL:       "http://192.168.30.8:3000",
		OperatorPubkey: strings.Repeat("b", 64),
	})
	for _, want := range []string{
		"base64 -d > /tmp/fh-rustfs-patch.sh && sh /tmp/fh-rustfs-patch.sh",
		"docker compose --env-file .env stop relay minio minio-init",
		"docker compose --env-file .env up -d minio minio-init",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("InstallCmd missing %q:\n%s", want, cmd)
		}
	}
	for _, banned := range []string{
		"./run.sh start", // the relay starts in its own step, after the mirror
		"quay.io/minio",  // the dead-registry re-point is gone
		"image: minio/",  // docker hub minio re-point is gone
	} {
		if strings.Contains(cmd, banned) {
			t.Errorf("InstallCmd must not contain %q:\n%s", banned, cmd)
		}
	}
}

// shipScript output must decode back to the script and carry no single quote
// (the sh -c transport is single-quote-free).
func TestShipScriptRoundTrip(t *testing.T) {
	script := composePatchScript("/srv/data/relay")
	cmd := shipScript("/tmp/fh-rustfs-patch.sh", script)
	if strings.ContainsRune(cmd, '\'') {
		t.Fatalf("shipScript emitted a single quote (breaks the sh -c wrapper):\n%s", cmd)
	}
	idx := strings.Index(cmd, " ") + 1
	end := strings.Index(cmd, " | base64 -d")
	got, err := base64.StdEncoding.DecodeString(cmd[idx:end])
	if err != nil {
		t.Fatalf("shipScript payload is not base64: %v", err)
	}
	if string(got) != script {
		t.Fatalf("shipScript round-trip mismatch")
	}
}

// The compose patch is the one non-trivial piece of text surgery in the
// deploy — run it (real sh + awk) against a faithful copy of the bundle's
// compose.yml and assert the exact shape: the minio service becomes rustfs
// under the SAME service name, minio-init and the relay are untouched, the
// new volume is declared, and a re-run is a no-op.
func TestComposePatchScriptOnBundleFixture(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "deploy", "compose")
	if err := os.MkdirAll(compose, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "compose-bundle.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compose, "compose.yml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	patch := composePatchScript(dir)
	// The script's own greps fail the run on a bad patch; the second run
	// proves idempotence.
	scriptPath := filepath.Join(t.TempDir(), "patch.sh")
	if err := os.WriteFile(scriptPath, []byte(patch), 0o755); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if out, err := exec.Command("sh", scriptPath).CombinedOutput(); err != nil {
			t.Fatalf("patch run %d failed: %v\n%s", run, err, out)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "deploy", "compose", "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{
		"    image: " + rustfsImage,
		"      RUSTFS_VOLUMES: /data",
		"      RUSTFS_ACCESS_KEY: ${BUZZ_S3_ACCESS_KEY:?set BUZZ_S3_ACCESS_KEY}",
		"      - buzz-rustfs-data:/data",
		"http://127.0.0.1:9000/health",
		"  buzz-rustfs-data:",
		"  minio-init:",                             // the init job survives
		"mc anonymous set none",                     // its body is untouched
		"condition: service_completed_successfully", // the relay's depends_on holds
		"buzz-minio-data:",                          // the old volume stays declared
		"ghcr.io/block/buzz:main",                   // the relay service is untouched
	} {
		if !strings.Contains(s, want) {
			t.Errorf("patched compose missing %q:\n%s", want, s)
		}
	}
	for _, banned := range []string{
		"pgsty/silo",              // the old engine is fully replaced
		"MINIO_ROOT_USER",         // silo's env is gone
		"  buzz-minio-data:/data", // no service mounts the old volume anymore
		"compose.yml.next",        // the temp file never leaks into the result
	} {
		if strings.Contains(s, banned) {
			t.Errorf("patched compose must not contain %q:\n%s", banned, s)
		}
	}
}

// The mover must be self-guarding: no old volume (fresh world) skips, the
// cutover marker skips, and the mirror is object-level over S3 with a diff
// gate — the relay only ever starts serving after the verify passes.
func TestMediaMoverScriptGuards(t *testing.T) {
	s := mediaMoverScript("/srv/data/relay")
	for _, want := range []string{
		"docker volume inspect buzz-minio-data",                 // fresh-world skip
		"test -f /data/" + mediaMarker,                          // post-cutover skip
		"mc mirror --overwrite \"src/$BUCKET\" \"dst/$BUCKET\"", // object-level copy
		"mc diff \"src/$BUCKET\" \"dst/$BUCKET\"",               // the verify gate
		"docker network ls --format '{{.Name}}'",                // derived, not assumed
		"docker rm -f buzz-media-src",                           // no stray engine left
		"server /data",                                          // old engine re-serves the volume
	} {
		if !strings.Contains(s, want) {
			t.Errorf("mover script missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "docker compose") { // engines only; compose owns the services
		t.Errorf("mover script must not drive compose:\n%s", s)
	}
	mover := shipScript("/tmp/fh-media-mover.sh", s)
	if strings.ContainsRune(mover, '\'') {
		t.Errorf("mover command emitted a single quote (breaks the sh -c wrapper):\n%s", mover)
	}
}
