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
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
	cert "freehold/platform/services/certificates/letsencrypt"
)

// ---- pure pieces ----

// TestAppFQDNPinsDerivation pins the hostname composition: the app is a
// CHILD of the world's own domain — the per-world subtree the member cookie
// scopes to (never a shared-zone-level name), and the shared wildcard's
// challenge name stays inside the world.
func TestAppFQDNPinsDerivation(t *testing.T) {
	cases := []struct{ host, want string }{
		// A cp.-prefixed host strips to its world domain.
		{"cp.librem.freehold.technology", "librem.freehold.technology"},
		// The admin.-prefixed shape (the re-org's operator face) strips the same.
		{"admin.librem.freehold.technology", "librem.freehold.technology"},
		// An unprefixed host that already IS its world domain: unchanged.
		{"sandbox.freehold.technology", "sandbox.freehold.technology"},
		// A prefixed 3-label host does NOT strip (stripping would land on a
		// zone this world doesn't own).
		{"cp.example.com", "cp.example.com"},
		{"control.example.com", "control.example.com"},
		// A two-label host IS the zone: unchanged (exposure refuses it).
		{"example.com", "example.com"},
	}
	for _, c := range cases {
		if got := WorldDomain(c.host); got != c.want {
			t.Fatalf("WorldDomain(%q): %q, want %q", c.host, got, c.want)
		}
	}
	if got := appWildcardHost("cp.librem.freehold.technology", ""); got != "*.librem.freehold.technology" {
		t.Fatalf("appWildcardHost: %q", got)
	}
	if got := appFQDN("sandbox.freehold.technology", "", "litellm"); got != "litellm.sandbox.freehold.technology" {
		t.Fatalf("appFQDN: %q", got)
	}
	if got := appFQDN("cp.librem.freehold.technology", "", "litellm"); got != "litellm.librem.freehold.technology" {
		t.Fatalf("appFQDN (prefixed host): %q", got)
	}
	// The explicit override: an operator who owns the whole zone puts the
	// world AT the apex — every name derives from the zone, not the CP host.
	if got := config.EffectiveWorldDomain("admin.casaquerencia.net", "casaquerencia.net"); got != "casaquerencia.net" {
		t.Fatalf("EffectiveWorldDomain (apex override): %q", got)
	}
	if got := config.EffectiveWorldDomain("admin.casaquerencia.net", ""); got != "admin.casaquerencia.net" {
		t.Fatalf("EffectiveWorldDomain (no override, 3-label host stays): %q", got)
	}
	if got := config.EffectiveWorldDomain("admin.casaquerencia.net", "other.example"); got != "admin.casaquerencia.net" {
		t.Fatalf("EffectiveWorldDomain (override the CP host is not a child of, ignored): %q", got)
	}
	if got := config.EffectiveWorldDomain("admin.casaquerencia.net", "net"); got != "admin.casaquerencia.net" {
		t.Fatalf("EffectiveWorldDomain (a TLD is not a registrable domain, ignored): %q", got)
	}
	if got := appFQDN("admin.casaquerencia.net", "casaquerencia.net", "sparkdash"); got != "sparkdash.casaquerencia.net" {
		t.Fatalf("appFQDN (apex override): %q", got)
	}
	if got := appWildcardHost("casaquerencia.net", "casaquerencia.net"); got != "*.casaquerencia.net" {
		t.Fatalf("appWildcardHost (apex override): %q", got)
	}
	if !appDomainIsZone("example.com") || appDomainIsZone("sandbox.freehold.technology") {
		t.Fatal("appDomainIsZone: only a two-label host is the zone itself")
	}
	// The portal host: the world root when the console moved off it (the
	// re-org's admin.<root> shape); empty while the console IS the root.
	if got := appsPortalHost("admin.librem.example.test", ""); got != "librem.example.test" {
		t.Fatalf("appsPortalHost (renamed console): %q", got)
	}
	if got := appsPortalHost("cp.librem.example.test", ""); got != "librem.example.test" {
		t.Fatalf("appsPortalHost (prefixed console): %q", got)
	}
	if got := appsPortalHost("sandbox.freehold.technology", ""); got != "" {
		t.Fatalf("appsPortalHost (console at the root) must be empty: %q", got)
	}
	// With the apex override the console moved off the root too — the portal
	// IS the zone apex.
	if got := appsPortalHost("admin.casaquerencia.net", "casaquerencia.net"); got != "casaquerencia.net" {
		t.Fatalf("appsPortalHost (apex override): %q", got)
	}
	if got := appsPortalHost("admin.casaquerencia.net", ""); got != "" {
		t.Fatalf("appsPortalHost (3-label host, no override — the console IS the root): %q", got)
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
	spec      *Spec
	apps      *agenttools.AppsStore
	cmds      []string
	uploads   []string // remote paths
	bodies    map[string]string
	relaySrv  *httptest.Server
	cfWrites  *int      // the fake CF's record-write counter
	cfDeletes *int      // the fake CF's record-delete counter (the shape-migration test)
	cfBodies  *[]string // the fake CF's write bodies (the A-record content)
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
	fc := selfSigned(t, "*.librem.example.test")

	spec := &Spec{
		StateDir: toolsetState, AgentRegistry: testRegistryForExpose(t),
		RelayURL: srv.URL, RelayAuthURL: srv.URL, RelayHost: "relay.librem.example.test",
		RelayIP: "10.78.0.11", CpHost: "cp.librem.example.test", CpIP: "10.78.0.12",
		ProxyIP: "192.168.30.5", K3sVmid: 105, K3sIP: "10.78.0.13",
		RunnerTarget: "proxmox-box", Sec: nostrSec, Audience: nostrSecPK,
		OwnerPub: strings.Repeat("a", 64),
	}
	// A fake Cloudflare API (the zone cred's CLOUDFLARE_BASE_URL points here —
	// a test must never touch the real API): /zones resolves one zone, record
	// writes succeed (and COUNT — the lan mode's no-public-DNS skip pins
	// against the counter); deletes count too (the stale-shape migration).
	var cfWrites, cfDeletes int
	var cfBodies []string
	cfBodiesPtr := &cfBodies
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/zones" || (r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records")) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success":     true,
				"result":      []map[string]interface{}{{"id": "zoneid", "name": "librem.example.test", "status": "active"}},
				"result_info": map[string]interface{}{"total_pages": 1},
			})
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			cfWrites++
			if b, rerr := io.ReadAll(r.Body); rerr == nil {
				*cfBodiesPtr = append(*cfBodiesPtr, string(b))
			}
		}
		if r.Method == http.MethodDelete {
			cfDeletes++
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
	te := &testExposer{spec: spec, apps: apps, bodies: map[string]string{}, cfWrites: &cfWrites, cfDeletes: &cfDeletes, cfBodies: cfBodiesPtr}
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

// sealPangolinCreds writes the pangolin cred sealed to the fixture's
// console identity (the same shape the box hand-off produces).
func (te *testExposer) sealPangolinCreds(t *testing.T, base, key string) {
	t.Helper()
	secret, err := te.spec.consoleEncSecretAt(te.spec.consoleStateRoot())
	if err != nil {
		t.Fatal(err)
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.SaveCreds(
		filepath.Join(te.spec.consoleStateRoot(), "world-secrets", "pangolin.json"),
		"pangolin", map[string]string{"PANGOLIN_BASE": base, "PANGOLIN_KEY": key},
		func(recipientPub, aad, plain []byte) ([]byte, error) { return crypto.Seal(recipientPub, aad, plain) },
		pub, "pangolin",
	); err != nil {
		t.Fatal(err)
	}
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
	if !ok || rec.Group != family || rec.FQDN != "yuvomi.librem.example.test" {
		t.Fatalf("record: %+v ok=%v", rec, ok)
	}
	if !strings.Contains(report, "https://yuvomi.librem.example.test") {
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
	if !strings.Contains(cfg, "yuvomi.librem.example.test {") ||
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
	// shared wildcard would cover the whole zone and its challenge would
	// collide with the cp cert's.
	te.spec.CpHost = "librem.example"
	if _, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:3000", Group: family, Requester: strings.Repeat("b", 64),
	}); err == nil || !strings.Contains(err.Error(), "the zone itself") {
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
		Name: "yuvomi", FQDN: appFQDN(te.spec.CpHost, "", "yuvomi"), Target: "10.78.0.13:3000",
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
// WITHOUT any public DNS (zero record writes hit the zone), while a family
// app on the same coords does write its record.
func TestLanVisibilitySkipsPublicDNS(t *testing.T) {
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42

	te := newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: strings.Repeat("b", 64), kind: wire.PutUser, ts: 100}})
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
	if *te.cfWrites != 0 {
		t.Fatalf("a lan app must not write public DNS records, saw %d", *te.cfWrites)
	}
	// The control: a family app on the same coords DOES write the record.
	if _, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "family-app", Target: "10.78.0.13:3000", Group: family, Requester: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatalf("the family app exposes: %v", err)
	}
	if *te.cfWrites < 1 {
		t.Fatal("a family app must point its public DNS record")
	}
}

// TestAppsCertReuseCoversTheHost pins the reuse gate's domain check: a
// mirror cert for the OLD shape (*.cp.librem…) must not seed as the apps'
// wildcard after the covered domain moved to the world domain — a stale
// shape's cert would TLS-mismatch every app behind an "apps reconciled"
// report; the mirror miss falls through to a fresh issue.
func TestAppsCertReuseCoversTheHost(t *testing.T) {
	oldShape := selfSigned(t, "*.cp.librem.example.test")
	if certCovers(oldShape, "*.librem.example.test") {
		t.Fatal("the old shape's cert must not cover the moved wildcard")
	}
	if !certCovers(selfSigned(t, "*.librem.example.test"), "*.librem.example.test") {
		t.Fatal("the matching wildcard must cover")
	}
	if certCovers([]byte("not a pem"), "*.librem.example.test") {
		t.Fatal("garbage must not cover")
	}
}

// TestExposeRidesThePangolinPath pins the pangolin public path: with the
// coords' pangolin block set, the expose verb ensures the site, finds or
// creates the resource on the base domain, and points the target at the
// app's freehold-subnet address — while the local edge (cert + Caddyfile)
// still converges for the LAN surface. The A record follows the DNSIP (the
// VPS) when set, and stays on the proxy IP when it's empty (the dev/test
// posture that exercises the tunnel without moving public DNS).
func TestExposeRidesThePangolinPath(t *testing.T) {
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	homelab := strings.Repeat("b", 64)
	var calls []string
	pang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/org/freehold/sites":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"sites":[{"siteId":3,"name":"librem","type":"newt"}]}}`))
		case "/v1/org/freehold/domains":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"domains":[{"domainId":"domain1","baseDomain":"librem.example","verified":true}]}}`))
		case "/v1/org/freehold/resources":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"resources":[]}}`))
		case "/v1/org/freehold/resource":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"resourceId":7,"name":"yuvomi","subdomain":"yuvomi","fullDomain":"yuvomi.librem.example","domainId":"domain1","ssl":true,"enabled":true}}`))
		case "/v1/resource/7":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":null}`))
		case "/v1/resource/7/targets":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"targets":[]}}`))
		case "/v1/resource/7/target":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"targetId":1}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"success":false,"error":true,"message":"no such route"}`))
		}
	}))
	defer pang.Close()

	te := newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: homelab, kind: wire.PutUser, ts: 100}})
	te.spec.Pangolin = &config.PangolinCoords{Org: "freehold", Site: "librem", Domain: "librem.example", DNSIP: "203.0.113.9"}
	te.sealPangolinCreds(t, pang.URL, "yi5o.thesecret")

	report, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:31400", Group: "family", Requester: homelab,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "live") {
		t.Fatalf("the expose must land live: %s", report)
	}
	want := []string{
		"GET /v1/org/freehold/sites",
		"GET /v1/org/freehold/domains",
		"GET /v1/org/freehold/resources",
		"PUT /v1/org/freehold/resource",
		"POST /v1/resource/7", // the sso gate — created resources are never open
		"GET /v1/resource/7/targets",
		"PUT /v1/resource/7/target",
	}
	for i, w := range want {
		if len(calls) <= i || calls[i] != w {
			t.Fatalf("pangolin call %d: got %v want %s", i, calls, w)
		}
	}
	// The app's A record points at the VPS, not the proxy.
	if !strings.Contains(strings.Join(*te.cfBodies, "\n"), "203.0.113.9") {
		t.Fatalf("the A record must ride the VPS: %v", *te.cfBodies)
	}
	// The local edge still converges (the LAN surface).
	if len(te.cmds) == 0 || !strings.Contains(strings.Join(te.cmds, "\n"), "kubectl") {
		t.Fatal("the local edge apply must still run for the LAN surface")
	}
}

// TestExposePangolinEmptyDNSKeepsTheProxyIP pins the dev/test posture: a
// pangolin block WITHOUT dns_ip exercises the tunnel while the public A
// record stays on the proxy IP — nothing in public DNS moves.
func TestExposePangolinEmptyDNSKeepsTheProxyIP(t *testing.T) {
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	homelab := strings.Repeat("b", 64)
	pang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/org/freehold/sites":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"sites":[{"siteId":3,"name":"librem","type":"newt"}]}}`))
		case "/v1/org/freehold/domains":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"domains":[{"domainId":"domain1","baseDomain":"librem.example","verified":true}]}}`))
		case "/v1/org/freehold/resources":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"resources":[{"resourceId":9,"name":"yuvomi","subdomain":"yuvomi","fullDomain":"yuvomi.librem.example","domainId":"domain1"}]}}`))
		case "/v1/resource/9":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":null}`)) // the sso re-assert
		case "/v1/resource/9/targets":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"targets":[]}}`))
		case "/v1/resource/9/target":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"targetId":2}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"success":false,"error":true,"message":"no such route"}`))
		}
	}))
	defer pang.Close()

	te := newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: homelab, kind: wire.PutUser, ts: 100}})
	te.spec.Pangolin = &config.PangolinCoords{Org: "freehold", Site: "librem", Domain: "librem.example"}
	te.sealPangolinCreds(t, pang.URL, "yi5o.thesecret")

	if _, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:31400", Group: "family", Requester: homelab,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*te.cfBodies, "\n"), "192.168.30.5") {
		t.Fatalf("the A record must stay on the proxy IP: %v", *te.cfBodies)
	}
}

// TestExposePangolinSkipsLAN pins finding two: a LAN-visibility record gets
// NO pangolin resource — its whole point is that the name resolves only
// inside the world; a public resource would contradict it.
func TestExposePangolinSkipsLAN(t *testing.T) {
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	homelab := strings.Repeat("b", 64)

	var calls []string
	pang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"success":false,"error":true,"message":"must never be called"}`))
	}))
	defer pang.Close()

	te := newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: homelab, kind: wire.PutUser, ts: 100}})
	te.spec.Pangolin = &config.PangolinCoords{Org: "freehold", Site: "librem", Domain: "librem.example", DNSIP: "203.0.113.9"}
	te.sealPangolinCreds(t, pang.URL, "k")

	report, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:31400", Group: "family", Requester: homelab,
		Visibility: agenttools.VisibilityLAN,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "live") {
		t.Fatalf("the LAN expose lands: %s", report)
	}
	if len(calls) != 0 {
		t.Fatalf("a LAN record must not touch pangolin: %v", calls)
	}
}

// TestExposePangolinSealsTheNewtSecret pins finding three: a site CREATED
// by the ensure has its one-time Newt secret sealed to the console identity
// immediately — discarding it would burn the identity.
func TestExposePangolinSealsTheNewtSecret(t *testing.T) {
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	homelab := strings.Repeat("b", 64)

	pang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/org/freehold/sites":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"sites":[]}}`))
		case "/v1/org/freehold/site":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"siteId":5,"name":"librem","type":"newt","newtId":"noo9","newtSecret":"one-time-sek","address":"100.90.0.0/16"}}`))
		case "/v1/org/freehold/domains":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"domains":[{"domainId":"domain1","baseDomain":"librem.example","verified":true}]}}`))
		case "/v1/org/freehold/resources":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"resources":[]}}`))
		case "/v1/org/freehold/site/5/resource":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"resourceId":8,"name":"yuvomi","subdomain":"yuvomi","fullDomain":"yuvomi.librem.example","domainId":"domain1"}}`))
		case "/v1/resource/8/targets":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"targets":[]}}`))
		case "/v1/resource/8/target":
			_, _ = w.Write([]byte(`{"success":true,"error":false,"data":{"targetId":3}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"success":false,"error":true,"message":"no such route"}`))
		}
	}))
	defer pang.Close()

	te := newTestExposer(t, []relayChannel{{family, "family"}},
		[]memberEvent{{channelID: family, pubkey: homelab, kind: wire.PutUser, ts: 100}})
	te.spec.Pangolin = &config.PangolinCoords{Org: "freehold", Site: "librem", Domain: "librem.example"}
	te.sealPangolinCreds(t, pang.URL, "k")

	if _, err := te.exposeFn(consoleSec)(agent.ExposeArgs{
		Name: "yuvomi", Target: "10.78.0.13:31400", Group: "family", Requester: homelab,
	}); err != nil {
		t.Fatal(err)
	}
	secret, err := te.spec.consoleEncSecretAt(te.spec.consoleStateRoot())
	if err != nil {
		t.Fatal(err)
	}
	open := func(sec, aad, blob []byte) ([]byte, error) { return crypto.Open(sec, aad, blob) }
	_, env, err := cert.LoadCreds(filepath.Join(te.spec.consoleStateRoot(), "world-secrets", "pangolin-newt.json"), open, secret)
	if err != nil {
		t.Fatalf("the one-time Newt secret must be sealed: %v", err)
	}
	if env["NEWT_ID"] != "noo9" || env["NEWT_SECRET"] != "one-time-sek" {
		t.Fatalf("the identity must land verbatim: %v", env)
	}
}

// TestWorldAppsMigratesStaleShapeRecords pins the DNS migration: a row whose
// stored FQDN was written by an older naming shape (the released per-app
// shape, <name>.<cpHost> — a name the shared wildcard never covered)
// repoints the public record to the DERIVED hostname and deletes the stale
// one, so an old-shape name never lingers pointing at the edge.
func TestWorldAppsMigratesStaleShapeRecords(t *testing.T) {
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	te := newTestExposer(t, []relayChannel{{family, "family"}}, nil)
	if err := te.apps.Expose(agenttools.AppRecord{
		Name: "yuvomi", FQDN: "yuvomi.cp.librem.example.test", Target: "10.78.0.13:3000",
		Visibility: agenttools.VisibilityFamily, Auth: agenttools.AuthGate,
		Group: family, Owner: "op", Requester: strings.Repeat("b", 64), CreatedAt: 7,
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := te.spec.worldApps()
	if err != nil {
		t.Fatalf("worldApps: %v", err)
	}
	if !strings.Contains(rep, "apps reconciled (1)") {
		t.Fatalf("worldApps report: %q", rep)
	}
	if *te.cfDeletes < 1 {
		t.Fatal("the stale-shape record must be deleted")
	}
	if *te.cfWrites < 1 {
		t.Fatal("the derived record must be pointed")
	}
	// The render serves the DERIVED name (the wildcard covers it).
	cfg := te.bodies["/tmp/fh-caddyfile"]
	if !strings.Contains(cfg, "yuvomi.librem.example.test {") {
		t.Fatalf("the render must carry the derived hostname: %s", cfg)
	}
}

// TestAppsCertWaitsOutPendingOrders pins the shared-challenge guard: while a
// relay/cp cert order is un-resolved (its state file exists), the apps
// wildcard's issue refuses — when the cp host IS the world domain the two
// orders challenge at the SAME name (_acme-challenge.<world-domain>), and a
// fresh apps order would purge the pending one's challenge TXT out from
// under it. The next build/expose re-ensures.
func TestAppsCertWaitsOutPendingOrders(t *testing.T) {
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	te := newTestExposer(t, []relayChannel{{family, "family"}}, nil)
	if err := te.apps.Expose(agenttools.AppRecord{
		Name: "yuvomi", FQDN: appFQDN(te.spec.CpHost, "", "yuvomi"), Target: "10.78.0.13:3000",
		Visibility: agenttools.VisibilityFamily, Auth: agenttools.AuthGate,
		Group: family, Owner: "op", Requester: strings.Repeat("b", 64), CreatedAt: 7,
	}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(te.spec.consoleStateRoot(), "world-secrets", "cert-pending-cp.json"), []byte("{}"))
	rep, err := te.spec.worldApps()
	if err != nil {
		t.Fatalf("worldApps: %v", err)
	}
	if !strings.Contains(rep, "still pending") {
		t.Fatalf("a pending relay/cp order must hold the apps cert off (in the report): %q", rep)
	}
}

// TestAppsCertWaitsOutThePortalOrder pins the guard's portal leg: the portal
// cert challenges at the world root's OWN name (_acme-challenge.<root>) — the
// same name the apps wildcard challenges at — so a pending portal order holds
// the apps cert off too. (The fixture's console sits at cp.librem…, so the
// portal host is librem.example.test and the slot exists.)
func TestAppsCertWaitsOutThePortalOrder(t *testing.T) {
	family := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	te := newTestExposer(t, []relayChannel{{family, "family"}}, nil)
	if err := te.apps.Expose(agenttools.AppRecord{
		Name: "yuvomi", FQDN: appFQDN(te.spec.CpHost, "", "yuvomi"), Target: "10.78.0.13:3000",
		Visibility: agenttools.VisibilityFamily, Auth: agenttools.AuthGate,
		Group: family, Owner: "op", Requester: strings.Repeat("b", 64), CreatedAt: 7,
	}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(te.spec.consoleStateRoot(), "world-secrets", "cert-pending-portal.json"), []byte("{}"))
	rep, err := te.spec.worldApps()
	if err != nil {
		t.Fatalf("worldApps: %v", err)
	}
	if !strings.Contains(rep, "still pending") {
		t.Fatalf("a pending portal order must hold the apps cert off: %q", rep)
	}
}

// TestRenderDropsAppsBehindAStaleShapeCert pins the render's half of
// cert-before-config: a mirror cert that is VALID but covers the apps
// wildcard only under an older shape (the world domain moved, the fresh
// issue has not landed) must carry NO app vhosts — the DNS step already
// repointed every app to the derived name, and a rendered vhost behind a
// mismatched cert TLS-mismatches them all.
func TestRenderDropsAppsBehindAStaleShapeCert(t *testing.T) {
	te := newTestExposer(t, nil, nil)
	if err := te.apps.Expose(agenttools.AppRecord{
		Name: "yuvomi", FQDN: appFQDN(te.spec.CpHost, "", "yuvomi"), Target: "10.78.0.13:3000",
		Visibility: agenttools.VisibilityFamily, Auth: agenttools.AuthGate,
		Group: "3fa85f64-5717-4562-b3fc-2c963f66afa6", Owner: "op", Requester: strings.Repeat("b", 64), CreatedAt: 7,
	}); err != nil {
		t.Fatal(err)
	}
	te.spec.execHook = func(cmd string, _ uint64, _ ...string) (string, error) {
		if strings.Contains(cmd, "fullchain.pem") {
			// The OLD shape's wildcard: valid, wrong name.
			return base64.StdEncoding.EncodeToString(selfSigned(t, "*.cp.librem.example.test")), nil
		}
		return "OK", nil
	}
	if body := te.spec.renderCaddyfile(); strings.Contains(body, "yuvomi.librem.example.test {") {
		t.Fatalf("a stale-shape cert must not carry app vhosts: %s", body)
	}
	// The control: the matching shape carries them.
	te.spec.execHook = func(cmd string, _ uint64, _ ...string) (string, error) {
		if strings.Contains(cmd, "fullchain.pem") {
			return base64.StdEncoding.EncodeToString(selfSigned(t, "*.librem.example.test")), nil
		}
		return "OK", nil
	}
	if body := te.spec.renderCaddyfile(); !strings.Contains(body, "yuvomi.librem.example.test {") {
		t.Fatalf("the matching wildcard must carry the app vhosts: %s", body)
	}
}

// TestCertStageDiscardsAStalePendingOrder pins the pending-file lifecycle: a
// relay/cp order whose state file survived a crash is MOOT once the durable
// mirror holds a valid cert for that slot — the reuse path seeds the PVC and
// must discard the state file, or certPending reads it forever and holds the
// apps wildcard off (a deterministic refusal at every build tail).
func TestCertStageDiscardsAStalePendingOrder(t *testing.T) {
	te := newTestExposer(t, nil, nil)
	pendingPath := filepath.Join(te.spec.consoleStateRoot(), "world-secrets", "cert-pending-cp.json")
	write(t, pendingPath, []byte("{}"))
	// The durable mirror answers VALID certs + keys, PER SLOT (the reuse
	// gate checks coverage — each slot's cert must cover its own host,
	// including the portal slot: the world root); the seed and every other
	// pct exec just succeed.
	te.spec.execHook = func(cmd string, _ uint64, _ ...string) (string, error) {
		if strings.Contains(cmd, "base64 -w0 <") {
			switch {
			case strings.Contains(cmd, "caddy-edge/relay"):
				return base64.StdEncoding.EncodeToString(selfSigned(t, "relay.librem.example.test")), nil
			case strings.Contains(cmd, "caddy-edge/portal"):
				return base64.StdEncoding.EncodeToString(selfSigned(t, "librem.example.test")), nil
			default:
				return base64.StdEncoding.EncodeToString(selfSigned(t, "cp.librem.example.test")), nil
			}
		}
		return "OK", nil
	}
	if err := te.spec.worldCert(); err != nil {
		t.Fatalf("worldCert (full reuse): %v", err)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatal("the reuse path must discard a stale pending order's state file")
	}
	if te.spec.certPending() {
		t.Fatal("certPending must read false once the stale order is discarded")
	}
}
