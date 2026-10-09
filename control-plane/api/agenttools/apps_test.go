package agenttools

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"freehold/contract/crypto"
	"freehold/control-plane/api/agent"
)

// TestAppsStoreRoundTripPinsNoClobber pins the exposure registry: a record
// round-trips (name + group id + visibility/auth), a second Expose on the
// same name or the same fqdn is REFUSED (a silently-replaced record would
// strand config and grants — the same no-clobber discipline provision
// applies to runner names), and unexpose removes it.
func TestAppsStoreRoundTripPinsNoClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apps.json")
	apps, err := OpenApps(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := AppRecord{
		Name: "yuvomi", FQDN: "yuvomi.librem.freehold.technology", Target: "10.78.0.13:3000",
		Visibility: VisibilityFamily, Auth: AuthGate,
		Group: "3fa85f64-5717-4562-b3fc-2c963f66afa6", Owner: "op", Requester: "req",
		CreatedAt: 100,
	}
	if err := apps.Expose(rec); err != nil {
		t.Fatal(err)
	}
	if err := apps.Expose(rec); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a second expose of the same name must be refused, got %v", err)
	}
	sameFQDN := rec
	sameFQDN.Name = "yuvomi2"
	if err := apps.Expose(sameFQDN); err == nil || !strings.Contains(err.Error(), "already serves") {
		t.Fatalf("a second expose of the same fqdn must be refused, got %v", err)
	}

	// Re-open: the record survives (persistence is the rebuild-safety half).
	fresh, err := OpenApps(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := fresh.Get("yuvomi")
	if !ok || got.Group != rec.Group || got.Visibility != VisibilityFamily || got.Auth != AuthGate {
		t.Fatalf("round trip: %+v ok=%v", got, ok)
	}
	if n := len(fresh.List()); n != 1 {
		t.Fatalf("list: %d records", n)
	}

	// Unexpose removes it; a second unexpose is an error.
	if _, err := fresh.Unexpose("yuvomi"); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Unexpose("yuvomi"); err == nil {
		t.Fatal("unexposing an absent app must error")
	}
}

// TestAppsStoreValidation pins the field contracts: a name must be a DNS
// label (it composes the fqdn), a target must be host:port, and
// visibility/auth accept only the known values — defaults fill when absent.
func TestAppsStoreValidation(t *testing.T) {
	apps, err := OpenApps("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		rec  AppRecord
		want string // the error substring; "" = accepted
	}{
		{"bad name", AppRecord{Name: "Not_A_Label", Target: "h:80", Group: "g"}, "DNS label"},
		{"bad target", AppRecord{Name: "ok", Target: "just-a-host", Group: "g"}, "host:port"},
		{"bad visibility", AppRecord{Name: "ok", Target: "h:80", Group: "g", Visibility: "world"}, "visibility"},
		{"bad auth", AppRecord{Name: "ok", Target: "h:80", Group: "g", Auth: "yolo"}, "auth"},
		{"no group", AppRecord{Name: "ok", Target: "h:80"}, "group is required"},
		{"defaults fill", AppRecord{Name: "ok", Target: "h:80", Group: "g"}, ""},
	}
	for _, tc := range cases {
		err := apps.Expose(tc.rec)
		if tc.want == "" && err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: want %q in error, got %v", tc.name, tc.want, err)
		}
	}
	got, _ := apps.Get("ok")
	if got.Visibility != VisibilityFamily || got.Auth != AuthGate {
		t.Fatalf("defaults must fill family+gate, got %s/%s", got.Visibility, got.Auth)
	}
}

// TestExposureDispatchScope pins the exposure capability's containment: only
// the network department's identity or an operator may call the exposure
// verbs (a custom agent asking directly is a containment failure — it routes
// through Network in conversation), and the widening acts (visibility public
// / auth none) are operator-only even for the network agent.
func TestExposureDispatchScope(t *testing.T) {
	aud := "aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55aa55"
	netSec := make([]byte, 32)
	netSec[0] = 2
	netPK, _ := crypto.PubkeyFromSecret(netSec)
	labSec := make([]byte, 32)
	labSec[0] = 3
	labPK, _ := crypto.PubkeyFromSecret(labSec)
	opSec := make([]byte, 32)
	opSec[0] = 4
	opPK, _ := crypto.PubkeyFromSecret(opSec)

	srv := &Server{
		Audience: aud,
		Grants:   func() ([]string, error) { return []string{netPK, labPK}, nil },
		// The operator is the break-glass PEER (roster-independent): their
		// signature verifies against themselves.
		OperatorPeer: opPK,
		Tools: &agent.Tools{
			Console:  &Registry{},
			Expose:   func(args agent.ExposeArgs) (string, error) { return "exposed " + args.Name, nil },
			Unexpose: func(name string) (string, error) { return "unexposed " + name, nil },
		},
		IsAgent: func(caller string) bool { return caller == netPK || caller == labPK },
		AgentName: func(caller string) (string, bool) {
			switch caller {
			case netPK:
				return "network", true
			case labPK:
				return "homelab", true
			}
			return "", false
		},
	}
	call := func(secret []byte, pk, tool, args string) string {
		raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
		ts := time.Now().Unix()
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(raw))
		req.Header.Set(PubkeyHeader, pk)
		req.Header.Set(TSHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(SigHeader, signForTest(secret, aud, ts, raw))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	// The network agent exposes (defaults pass through; the record's
	// defaults fill downstream).
	out := call(netSec, netPK, "expose_app", `{"name":"yuvomi","target":"10.0.0.9:3000","group":"family"}`)
	if !strings.Contains(out, "exposed yuvomi") {
		t.Fatalf("network exposes: %s", out)
	}
	// A custom agent is refused — the ask routes through Network.
	out = call(labSec, labPK, "expose_app", `{"name":"x","target":"h:80","group":"general"}`)
	if !strings.Contains(out, "exposure is Network") {
		t.Fatalf("a custom agent must be refused: %s", out)
	}
	out = call(labSec, labPK, "unexpose_app", `{"name":"x"}`)
	if !strings.Contains(out, "exposure is Network") {
		t.Fatalf("a custom agent's unexpose must be refused: %s", out)
	}
	// The network agent cannot widen (public / auth none).
	out = call(netSec, netPK, "expose_app", `{"name":"x","target":"h:80","group":"g","visibility":"public"}`)
	if !strings.Contains(out, "operator-level act") {
		t.Fatalf("network must not set public: %s", out)
	}
	out = call(netSec, netPK, "expose_app", `{"name":"x","target":"h:80","group":"g","auth":"none"}`)
	if !strings.Contains(out, "operator-level act") {
		t.Fatalf("network must not set auth none: %s", out)
	}
	// The operator may widen — and needs no name (not a registry agent).
	out = call(opSec, opPK, "expose_app", `{"name":"x","target":"h:80","group":"g","visibility":"public","auth":"none"}`)
	if !strings.Contains(out, "exposed x") {
		t.Fatalf("the operator widens: %s", out)
	}
}

// TestValidTargetCharset pins the injection guard: the target composes the
// SHARED Caddyfile verbatim, so the host part is hostname/IP-charset only —
// whitespace, quotes, braces, and backslashes are a broken or injected site
// block (the whole edge crash-loops on it), never a value.
func TestValidTargetCharset(t *testing.T) {
	for _, ok := range []string{"10.0.0.9:3000", "yuvomi.lan:8080", "[::1]:8080", "host-name.example:443"} {
		if !ValidTarget(ok) {
			t.Fatalf("%q must pass", ok)
		}
	}
	for _, bad := range []string{"10.0.0.9 :3000", "\"h\":80", "h{:80", "h}:80", "h\\:80", "h\nx:80", "h x:80"} {
		if ValidTarget(bad) {
			t.Fatalf("%q must be refused", bad)
		}
	}
}

// TestFailedSaveKeepsDiskTruth pins the save-failure recovery: tmp+rename is
// atomic, so a failed save leaves the ORIGINAL file intact — and the memory
// must re-derive from it (no phantom Expose row refusing retries, no
// memory-only Unexpose hiding a live row from List — the render's source).
func TestFailedSaveKeepsDiskTruth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apps.json")
	apps, err := OpenApps(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := AppRecord{Name: "yuvomi", FQDN: "yuvomi.example", Target: "10.0.0.9:3000",
		Visibility: VisibilityFamily, Auth: AuthGate, Group: "g", CreatedAt: 1}
	if err := apps.Expose(rec); err != nil {
		t.Fatal(err)
	}
	// Break persistence: the store's own dir stays, writes into it fail.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := apps.Unexpose("yuvomi"); err == nil {
		t.Fatal("the unexpose must fail (the save cannot land)")
	}
	// The memory matches the DISK: the row is still listed (it never left)
	// and a second expose is still the honest "already exists".
	if _, ok := apps.Get("yuvomi"); !ok {
		t.Fatal("the record must survive a failed unexpose save")
	}
	if n := len(apps.List()); n != 1 {
		t.Fatalf("List must still see the record after a failed save: %d", n)
	}
	if err := apps.Expose(rec); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("the retry must be refused by the surviving record, got %v", err)
	}
}
