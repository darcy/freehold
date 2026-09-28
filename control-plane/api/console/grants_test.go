package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/api/cpbuild"
	"freehold/control-plane/state"
)

// TestGrantsFor pins the merge rule: a successful non-empty live roster wins
// (it is what gates exec); empty/failed reads fall back to the shipped
// package grants; an unreadable package keeps grants nil (the "unreadable"
// anomaly) rather than masquerading as an honest empty list.
func TestGrantsFor(t *testing.T) {
	pkg := []string{"pk-a", "pk-b"}
	cases := []struct {
		name       string
		live       []string
		liveErr    error
		pkg        []string
		pkgRead    bool
		wantGrants []string
		wantSource string
	}{
		{"live wins", []string{"pk-live"}, nil, pkg, true, []string{"pk-live"}, "live"},
		{"empty roster falls back to package", nil, nil, pkg, true, pkg, "package"},
		{"roster error falls back to package", nil, errFake, pkg, true, pkg, "package"},
		{"package mode with no channel", nil, nil, pkg, true, pkg, "package"},
		{"unreadable package stays nil", nil, nil, nil, false, nil, ""},
		{"nothing readable stays nil", nil, errFake, nil, false, nil, ""},
	}
	for _, tc := range cases {
		grants, source := grantsFor(tc.live, tc.liveErr, tc.pkg, tc.pkgRead)
		if source != tc.wantSource {
			t.Errorf("%s: source = %q, want %q", tc.name, source, tc.wantSource)
		}
		if len(grants) != len(tc.wantGrants) {
			t.Errorf("%s: grants = %v, want %v", tc.name, grants, tc.wantGrants)
		}
	}
}

var errFake = &fakeError{}

type fakeError struct{}

func (*fakeError) Error() string { return "roster read failed" }

// TestOverviewGrantsSourceAndColocated drives /api/overview through the
// no-relay path (no roster query — the package result must stand, labeled
// "package") and asserts the co-located flag lands on the named runner.
func TestOverviewGrantsSourceAndColocated(t *testing.T) {
	sec := adminSecret()
	adminPK, _ := crypto.PubkeyFromSecret(sec)
	s, store := testServer(t, NewAuth([]string{adminPK}, ""))
	s.Builder = &cpbuild.Spec{RunnerTarget: "co-box"}
	cs := make([]byte, 32)
	cs[0] = 0x11
	cpk, _ := crypto.PubkeyFromSecret(cs)
	s.ConsolePubkey = cpk

	pkgDir := t.TempDir()
	if err := wire.New(map[string]string{}, nil, []string{"operator-pk", "console-pk"}).WriteToDir(pkgDir); err != nil {
		t.Fatal(err)
	}
	deadDir := t.TempDir() // no secrets.json — the "unreadable package" anomaly
	store.InsertRunner("co-box", state.RunnerRecord{NostrPubkey: strings.Repeat("a", 64), EncPubkey: strings.Repeat("b", 64), Status: state.RunnerActive, PackageDir: pkgDir, CreatedAt: 1})
	store.InsertRunner("door", state.RunnerRecord{NostrPubkey: strings.Repeat("c", 64), EncPubkey: strings.Repeat("d", 64), Status: state.RunnerActive, PackageDir: deadDir, CreatedAt: 1})
	_ = store.Save()

	session := loginSession(t, s, sec)
	r := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body.String())
	}
	var ov struct {
		Runners []struct {
			Name         string      `json:"name"`
			Grants       interface{} `json:"grants"`
			GrantsSource string      `json:"grants_source"`
			Colocated    bool        `json:"colocated"`
		} `json:"runners"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&ov); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]interface{}{}
	for _, r := range ov.Runners {
		raw, _ := json.Marshal(map[string]interface{}{
			"grants": r.Grants, "source": r.GrantsSource, "colocated": r.Colocated,
		})
		var m map[string]interface{}
		_ = json.Unmarshal(raw, &m)
		byName[r.Name] = m
	}
	co, ok := byName["co-box"]
	if !ok {
		t.Fatalf("co-box missing: %v", byName)
	}
	if co["colocated"] != true || co["source"] != "package" {
		t.Errorf("co-box: %+v", co)
	}
	if g, ok := co["grants"].([]interface{}); !ok || len(g) != 2 {
		t.Errorf("co-box grants = %v, want the package list", co["grants"])
	}
	door := byName["door"]
	if door["source"] != "" || door["grants"] != nil {
		t.Errorf("door (unreadable package): %+v, want null grants / empty source", door)
	}
}

// loginSession drives the challenge/login flow and returns the session cookie.
func loginSession(t *testing.T, s *Server, sec []byte) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/challenge", nil))
	var chal struct {
		Nonce string `json:"nonce"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &chal)
	pk, sig, tags := signLoginEvent(t, sec, chal.Nonce, time.Now().Unix())
	body, _ := json.Marshal(map[string]interface{}{
		"nonce": chal.Nonce, "pubkey": pk, "created_at": time.Now().Unix(), "tags": tags, "sig": sig,
	})
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body)))
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}
