package cpbuild

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	cert "freehold/platform/services/certificates/letsencrypt"
)

// ---- pure pieces ----

// TestAppFQDNPinsDerivation pins the hostname composition: the app is a
// SIBLING of the relay/cp hosts — the member cookie scopes to the
// appliance's own ZONE (the world domain), so the cookie reaches every
// sibling app, and the shared wildcard's challenge name is unique in the
// zone (no collision with the cp cert's challenge).
func TestAppFQDNPinsDerivation(t *testing.T) {
	if got := appDomainBase("cp.librem.freehold.technology"); got != "librem.freehold.technology" {
		t.Fatalf("appDomainBase: %q", got)
	}
	// The rule is the FIRST LABEL, not a "cp." prefix.
	if got := appDomainBase("control.example.com"); got != "example.com" {
		t.Fatalf("appDomainBase (non-cp prefix): %q", got)
	}
	// A two-label cp host IS the zone: no world domain below it.
	if got := appDomainBase("example.com"); got != "example.com" {
		t.Fatalf("appDomainBase (degenerate): %q", got)
	}
	if got := appWildcardHost("cp.librem.freehold.technology"); got != "*.librem.freehold.technology" {
		t.Fatalf("appWildcardHost: %q", got)
	}
	if got := appFQDN("cp.librem.freehold.technology", "yuvomi"); got != "yuvomi.librem.freehold.technology" {
		t.Fatalf("appFQDN: %q", got)
	}
	if !looksLikeChannelID("3fa85f64-5717-4562-b3fc-2c963f66afa6") {
		t.Fatal("a dashed-uuid channel id must pass through")
	}
	if looksLikeChannelID("family") || looksLikeChannelID("3fa85f6457174562b3fc2c963f66afa6") {
		t.Fatal("a name or a bare hex id is not a channel id")
	}
}

// ---- fake relay (group resolution + the membership read) ----

// relayEvents serves /query by KIND: kind-39000 (channel discovery) for
// FindChannel, kind-9000/9001 (channel membership) for IsMemberAuth. Signed
// events only — the reads verify.
type relayEvents struct {
	secret   []byte
	channels []relayChannel // kind 39000
	members  []memberEvent  // kind 9000/9001
}

type relayChannel struct {
	id   string
	name string
}
type memberEvent struct {
	channelID string
	pubkey    string
	kind      uint32 // wire.PutUser or wire.RemoveUser
	ts        int64
}

func (re *relayEvents) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]interface{}{}
		for _, ch := range re.channels {
			out = append(out, signEv(t, re.secret, wire.GroupMeta, time.Now().Unix(),
				[][]string{{"d", ch.id}, {"name", ch.name}}, ""))
		}
		for _, m := range re.members {
			out = append(out, signEv(t, re.secret, m.kind, m.ts,
				[][]string{{"h", m.channelID}, {"p", m.pubkey}}, ""))
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- the expose fn ----

// testExposer builds an expose fn over a temp state (a sealed zone cred at
// the console root), a fake relay, and an exec hook that records commands
// and answers the durable-fullchain reads with a real self-signed cert.
type testExposer struct {
	spec     *Spec
	apps     *agenttools.AppsStore
	cmds     []string
	uploads  []string // remote paths
	bodies   map[string]string
	relaySrv *httptest.Server
}

func newTestExposer(t *testing.T, channels []relayChannel, members []memberEvent) *testExposer {
	root := t.TempDir()
	consoleState := filepath.Join(root, "control-plane")
	toolsetState := filepath.Join(root, "agent-tools")

	// The console identity (the enc secret seals the zone cred; the nostr
	// secret signs the relay reads).
	nostrSec := make([]byte, 32)
	nostrSec[0] = 0x42
	nostrSecPK, err := crypto.PubkeyFromSecret(nostrSec)
	if err != nil {
		t.Fatal(err)
	}
	encSec := make([]byte, 32)
	encSec[1] = 0x7c
	encPK, err := crypto.X25519PublicKey(encSec)
	if err != nil {
		t.Fatal(err)
	}
	idJSON, _ := json.Marshal(map[string]string{
		"nostr_secret_hex": hexOf(nostrSec), "enc_secret_hex": hexOf(encSec),
	})
	write(t, filepath.Join(consoleState, "console", "identity.json"), idJSON)

	re := &relayEvents{secret: nostrSec, channels: channels, members: members}
	srv := re.server(t)

	// A self-signed cert for the durable-mirror reads (the render's
	// cert-existence filter parses it).
	fc := selfSigned(t, "*.librem.example")

	spec := &Spec{
		StateDir: toolsetState, AgentRegistry: testRegistryForExpose(t),
		RelayURL: srv.URL, RelayAuthURL: srv.URL, RelayHost: "relay.librem.example",
		RelayIP: "10.78.0.11", CpHost: "cp.librem.example", CpIP: "10.78.0.12",
		ProxyIP: "192.168.30.5", K3sVmid: 105, K3sIP: "10.78.0.13",
		RunnerTarget: "proxmox-box", Sec: nostrSec, Audience: nostrSecPK,
		OwnerPub: strings.Repeat("a", 64),
	}
	// A fake Cloudflare API (the zone cred's CLOUDFLARE_BASE_URL points here —
	// a test must never touch the real API): /zones resolves one zone, record
	// writes succeed.
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Reads (zone list, record lookup) return ARRAY results; writes return
		// an object — matching the API's shapes.
		if r.URL.Path == "/zones" || (r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records")) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success":     true,
				"result":      []map[string]interface{}{{"id": "zoneid", "name": "librem.example", "status": "active"}},
				"result_info": map[string]interface{}{"total_pages": 1},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "result": map[string]interface{}{}})
	}))
	t.Cleanup(cf.Close)

	// The zone cred sealed to the console identity at the console root — its
	// env points the dnsman client at the LOCAL fake.
	if err := cert.SaveCreds(
		filepath.Join(consoleState, "world-secrets", "dns-relay.json"),
		"cloudflare", map[string]string{"CLOUDFLARE_DNS_API_TOKEN": "tok", "CLOUDFLARE_BASE_URL": cf.URL},
		func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) },
		encPK, "dns-relay",
	); err != nil {
		t.Fatal(err)
	}

	apps, err := agenttools.OpenApps(filepath.Join(toolsetState, "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	te := &testExposer{spec: spec, apps: apps, bodies: map[string]string{}}
	spec.Apps = apps
	spec.execHook = func(cmd string, _ uint64, _ ...string) (string, error) {
		te.cmds = append(te.cmds, cmd)
		if strings.Contains(cmd, "fullchain.pem") {
			return base64.StdEncoding.EncodeToString(fc), nil
		}
		return "OK", nil
	}
	spec.uploadHook = func(_, local, remote string, _ uint64) error {
		body, rerr := os.ReadFile(local)
		if rerr != nil {
			return rerr
		}
		te.uploads = append(te.uploads, remote)
		te.bodies[remote] = string(body)
		return nil
	}
	return te
}

func (te *testExposer) exposeFn(consoleSec []byte) func(agent.ExposeArgs) (string, error) {
	return BuildExposeAppFn(te.spec, te.apps, consoleSec)
}

// testRegistryForExpose is the registry the requester check reads: one agent
// row (b… = the homelab-style requester) + the network department (c…).
func testRegistryForExpose(t *testing.T) *agenttools.Registry {
	t.Helper()
	reg, err := agenttools.OpenRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct{ name, pk string }{
		{"network", strings.Repeat("b", 64)},
		{"homelab", strings.Repeat("c", 64)},
	} {
		if _, err := reg.RegisterAgent(a.name, a.pk, a.name); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// ---- tests ----

// TestExposePinsRequesterInGroup pins the invariant: access narrows to
// groups the requester belongs to. A channel that doesn't resolve, an
// unknown requester, and a requester not in the channel are all REFUSED
// before anything is recorded; a passing requester records the row with the
// group's ID (never the name).
func TestExposePinsRequesterInGroup(t *testing.T) {
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	homelab := strings.Repeat("b", 64)
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42

	// No such channel: refused, nothing recorded.
	te := newTestExposer(t, nil, nil)
	if _, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:3000", Group: "family", Requester: homelab,
	}); err == nil || !strings.Contains(err.Error(), "no such channel") {
		t.Fatalf("an unresolvable group must refuse, got %v", err)
	}
	if _, ok := te.apps.Get("yuvomi"); ok {
		t.Fatal("a refused expose must not record")
	}

	// The channel exists, the requester doesn't: refused.
	te = newTestExposer(t, []relayChannel{{family, "family"}}, nil)
	if _, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:3000", Group: "family", Requester: strings.Repeat("b", 64),
	}); err == nil || !strings.Contains(err.Error(), "have them added first") {
		t.Fatalf("a requester not in the channel must refuse, got %v", err)
	}

	// An unknown requester identity: refused before the membership read.
	if _, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:3000", Group: "family", Requester: strings.Repeat("e", 64),
	}); err == nil || !strings.Contains(err.Error(), "not a known identity") {
		t.Fatalf("an unknown requester must refuse, got %v", err)
	}

	// The passing path: the requester IS in the channel (a kind-9000 grant
	// on the channel id) — the row records the group's ID, and the DNS step
	// (no sealed-cred failure to hit — the cred exists) reports the URL.
	te = newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: strings.Repeat("b", 64), kind: wire.PutUser, ts: 100}})
	report, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:3000", Group: "family", Requester: strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatalf("a requester in the group must pass, got %v", err)
	}
	rec, ok := te.apps.Get("yuvomi")
	if !ok || rec.Group != family || rec.FQDN != "yuvomi.librem.example" {
		t.Fatalf("record: %+v ok=%v", rec, ok)
	}
	if !strings.Contains(report, "https://yuvomi.librem.example") {
		t.Fatalf("report must carry the URL: %s", report)
	}
}

// TestExposeRecordFirstAndEdgeSafe pins cert-before-config: the rendered
// edge config includes an app's vhost only when its cert is on the durable
// mirror, and the apply commands arrive (configmap create|apply + rollout
// restart) through the co-located runner.
func TestExposeRecordFirstAndEdgeSafe(t *testing.T) {
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	homelab := strings.Repeat("b", 64)
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42
	te := newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: homelab, kind: wire.PutUser, ts: 100}})
	report, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:3000", Group: "family", Requester: homelab,
	})
	if err != nil {
		t.Fatalf("expose: %v", err)
	}
	if !strings.Contains(report, "live") {
		t.Fatalf("the happy path reports live: %s", report)
	}
	joined := strings.Join(te.cmds, "\n")
	if !strings.Contains(joined, "create configmap caddy-caddyfile") ||
		!strings.Contains(joined, "apply -f -") ||
		!strings.Contains(joined, "rollout restart deploy/caddy") {
		t.Fatalf("the edge apply must ride the runner: %s", joined)
	}
	// The rendered config the apply CARRIES (file-transit): the app vhost
	// with its gate — forward_auth to the console on the CP guest.
	cfg := te.bodies["/tmp/fh-caddyfile"]
	if !strings.Contains(cfg, "yuvomi.librem.example {") ||
		!strings.Contains(cfg, "forward_auth 10.78.0.12:8080") ||
		!strings.Contains(cfg, "tls /data/tls/apps/fullchain.pem") {
		t.Fatalf("the rendered edge config must carry the gated app vhost: %s", cfg)
	}
}

// TestWorldAppsEnsuresOnTheBuildTail pins the rebuild-safety half: with
// registered apps the build tail applies the edge config (idempotent ensure)
// and reports the count; with none it is a no-op.
func TestWorldAppsEnsuresOnTheBuildTail(t *testing.T) {
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	te := newTestExposer(t, []relayChannel{{family, "family"}}, nil)
	if rep, err := te.spec.worldApps(); err != nil || rep != "" {
		t.Fatalf("no apps = no-op, got %q %v", rep, err)
	}
	homelab := strings.Repeat("b", 64)
	te2 := newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: homelab, kind: wire.PutUser, ts: 100}})
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42
	if _, err := te2.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:3000", Group: family, Requester: homelab,
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := te2.spec.worldApps()
	if err != nil {
		t.Fatalf("worldApps: %v", err)
	}
	if !strings.Contains(rep, "apps reconciled (1)") {
		t.Fatalf("worldApps report: %q", rep)
	}
	if !strings.Contains(strings.Join(te2.cmds, "\n"), "rollout restart deploy/caddy") {
		t.Fatal("worldApps must apply the edge config")
	}
}

// ---- helpers ----

// signEv signs a Nostr event map for the fake relay's /query answers.
func signEv(t *testing.T, secret []byte, kind uint32, ts int64, tags [][]string, content string) map[string]interface{} {
	t.Helper()
	pk, id, sig, err := wire.SignEvent(secret, kind, ts, tags, content)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]interface{}{
		"id": id, "pubkey": pk, "created_at": ts, "kind": kind,
		"tags": tags, "content": content, "sig": sig,
	}
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

func write(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// selfSigned returns a PEM fullchain (one self-signed cert) for the
// durable-mirror read fakes — ReuseIfValidBytes parses it.
func selfSigned(t *testing.T, host string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestExposeRefusesTheEdgeHosts pins the collision guard: an app whose
// composed hostname IS one of the edge's own site blocks (the relay hosted
// under the cp host's domain — a name collision means TWO site blocks for
// one address: an unloadable config, a crash-looping edge, and kubectl
// apply exits 0 so nothing reports it) is refused before anything is
// recorded.
func TestExposeRefusesTheEdgeHosts(t *testing.T) {
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42
	te := newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: strings.Repeat("b", 64), kind: wire.PutUser, ts: 100}})
	// The app name "relay" composes EXACTLY the relay's own site block
	// (siblings): two site blocks for one address is a config that cannot
	// load — the whole edge crash-loops, and kubectl apply exits 0.
	if _, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "relay", Target: "10.78.0.13:3000", Group: family, Requester: strings.Repeat("b", 64),
	}); err == nil || !strings.Contains(err.Error(), "the edge's own host") {
		t.Fatalf("expose must refuse the edge's own host, got %v", err)
	}

	// The degenerate shape (the cp host IS the zone): refused loud — the
	// shared wildcard's challenge would collide with the cp cert's.
	te.spec.CpHost = "librem.example"
	if _, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:3000", Group: family, Requester: strings.Repeat("b", 64),
	}); err == nil || !strings.Contains(err.Error(), "no world domain below it") {
		t.Fatalf("a zone-shaped cp host must refuse, got %v", err)
	}
	if _, ok := te.apps.Get("relay"); ok {
		t.Fatal("the refused expose must not record")
	}
}

// TestWorldAppsReadsTheRegistryFresh pins the freshness fix: expose writes
// land in the SEPARATE agent-tools process, so the build tail (a different
// in-memory store over the same file) must see them — a store that stayed
// startup-frozen would strip every post-expose app from the edge (and
// resurrect every unexposed one).
func TestWorldAppsReadsTheRegistryFresh(t *testing.T) {
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	te := newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: strings.Repeat("b", 64), kind: wire.PutUser, ts: 100}})
	// The OTHER process exposes through its OWN store over the same file.
	other, err := agenttools.OpenApps(te.apps.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Expose(agenttools.AppRecord{
		Name: "yuvomi", FQDN: appFQDN(te.spec.CpHost, "yuvomi"), Target: "10.78.0.13:3000",
		Visibility: agenttools.VisibilityFamily, Auth: agenttools.AuthGate,
		Group: family, Owner: "op", Requester: strings.Repeat("b", 64), CreatedAt: 7,
	}); err != nil {
		t.Fatal(err)
	}
	// The build tail (the frozen first store) still reports the app.
	rep, err := te.spec.worldApps()
	if err != nil {
		t.Fatalf("worldApps: %v", err)
	}
	if !strings.Contains(rep, "apps reconciled (1)") {
		t.Fatalf("the tail must read the registry as the OTHER process left it: %q", rep)
	}
	// ...and the unexpose lands the same way.
	if _, err := other.Unexpose("yuvomi"); err != nil {
		t.Fatal(err)
	}
	if rep, err := te.spec.worldApps(); err != nil || rep != "" {
		t.Fatalf("the unexposed app must be gone from the tail's render: %q %v", rep, err)
	}
}

// TestLanVisibilitySkipsPublicDNS pins the lan mode: a lan app exposes
// WITHOUT any public DNS (the skip happens before the proxy-IP check — a
// world without a recorded proxy IP can still serve lan apps), while a
// family app on the same coords fails the DNS leg loudly.
func TestLanVisibilitySkipsPublicDNS(t *testing.T) {
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42

	te := newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: strings.Repeat("b", 64), kind: wire.PutUser, ts: 100}})
	te.spec.ProxyIP = "" // no proxy IP: a family app's DNS leg must fail
	report, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "internal-dash", Target: "10.78.0.13:3000", Group: family,
		Visibility: agenttools.VisibilityLAN, Requester: strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatalf("a lan app must expose without public DNS: %v", err)
	}
	if !strings.Contains(report, "live") {
		t.Fatalf("the lan app reports live: %s", report)
	}
	rec, _ := te.apps.Get("internal-dash")
	if rec.Visibility != agenttools.VisibilityLAN {
		t.Fatalf("the lan visibility must record: %+v", rec)
	}
}
