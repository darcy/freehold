package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"freehold/contract/crypto"
	"freehold/contract/wire"
)

// serveEvents is a one-filter /query fake: it answers with `events`
// regardless of the filter (the caller asserts the pure reads; the fake only
// proves the wire hop).
func serveEvents(t *testing.T, events []map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(events)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// memberListEvent is the relay's NIP-43 membership list (kind 13534),
// members riding `member` tags — the shape the pinned buzz relay serves.
func memberListEvent(t *testing.T, authorSec []byte, ts int64, members ...string) map[string]interface{} {
	t.Helper()
	tags := [][]string{}
	for _, m := range members {
		tags = append(tags, []string{"member", m, "member"})
	}
	return sign(t, authorSec, wire.KINDNip43Membership, ts, tags, "")
}

// TestIsCommunityMemberAuthPinsTheTrustAnchor pins the member gate's relay
// read: the listing counts only when signed by the RELAY's key — the author
// check and signature verification run client-side (mergeRoster's
// discipline), because the console dials the relay over the plaintext LAN
// and a forged or edited listing must admit nobody.
func TestIsCommunityMemberAuthPinsTheTrustAnchor(t *testing.T) {
	relaySec := mustSecret(t, 1)
	relayPK := testPubkeyOf(t, relaySec)
	memberPK := testPubkeyOf(t, mustSecret(t, 2))
	outsiderSec := mustSecret(t, 3)

	// The relay-signed list naming the member → member.
	srv := serveEvents(t, []map[string]interface{}{memberListEvent(t, relaySec, 100, memberPK)})
	if ok, err := IsCommunityMemberAuth(srv.URL, srv.URL, mustSecret(t, 7), relayPK, memberPK); !ok || err != nil {
		t.Fatalf("listed member: ok=%v err=%v", ok, err)
	}

	// An empty list: not a member.
	srv = serveEvents(t, nil)
	if ok, err := IsCommunityMemberAuth(srv.URL, srv.URL, mustSecret(t, 7), relayPK, memberPK); ok || err != nil {
		t.Fatalf("empty list must not admit: ok=%v err=%v", ok, err)
	}

	// A FORGED list — signed by an outsider's key, relay pubkey in the
	// author FIELD — must admit nobody: the author check is client-side.
	forged := memberListEvent(t, outsiderSec, 100, memberPK)
	forged["pubkey"] = relayPK
	srv = serveEvents(t, []map[string]interface{}{forged})
	if ok, err := IsCommunityMemberAuth(srv.URL, srv.URL, mustSecret(t, 7), relayPK, memberPK); ok || err != nil {
		t.Fatalf("forged author must not admit: ok=%v err=%v", ok, err)
	}

	// A TAMPERED listing — relay-signed, then its content mangled — is
	// skipped: the signature no longer matches, so it is not the relay's
	// word.
	tampered := memberListEvent(t, relaySec, 100, memberPK)
	tampered["content"] = "tampered"
	srv = serveEvents(t, []map[string]interface{}{tampered})
	if ok, err := IsCommunityMemberAuth(srv.URL, srv.URL, mustSecret(t, 7), relayPK, memberPK); ok || err != nil {
		t.Fatalf("tampered listing must not admit: ok=%v err=%v", ok, err)
	}

	// Missing anchors are refused outright.
	srv = serveEvents(t, []map[string]interface{}{memberListEvent(t, relaySec, 100, memberPK)})
	if _, err := IsCommunityMemberAuth(srv.URL, srv.URL, mustSecret(t, 7), "", memberPK); err == nil {
		t.Fatal("a missing relay pubkey must error, not fail open")
	}
}

// TestIsCommunityMemberAuthUnreachableRelayFailsClosed pins the outage
// behavior: a dead relay is an ERROR (the console layer decides grace), never
// a silent yes.
func TestIsCommunityMemberAuthUnreachableRelayFailsClosed(t *testing.T) {
	relaySec := mustSecret(t, 1)
	relayPK := testPubkeyOf(t, relaySec)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	srv.Close() // closed: every dial refused
	if _, err := IsCommunityMemberAuth(srv.URL, srv.URL, mustSecret(t, 7), relayPK, testPubkeyOf(t, mustSecret(t, 2))); err == nil {
		t.Fatal("an unreachable relay must error, not fail open")
	}
}

// testPubkeyOf derives the hex pubkey of a test seed.
func testPubkeyOf(t *testing.T, sec []byte) string {
	t.Helper()
	pk, err := crypto.PubkeyFromSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}
