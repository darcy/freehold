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

// TestIsCommunityMemberAuthPinsTheTrustAnchor pins the member gate's relay
// read: the listing counts only when signed by the RELAY's key (a forged
// 13534 from any other author admits nobody), an absent listing is not a
// member, and the newest listing per d-tag decides — a republished list that
// drops the member revokes them even though the old one still exists.
func TestIsCommunityMemberAuthPinsTheTrustAnchor(t *testing.T) {
	relaySec := mustSecret(t, 1)
	relayPK := testPubkeyOf(t, relaySec)
	member := mustSecret(t, 2)
	memberPK := testPubkeyOf(t, member)
	outsider := mustSecret(t, 3)

	list := func(author []byte, ts int64, members ...string) map[string]interface{} {
		tags := [][]string{}
		for _, m := range members {
			tags = append(tags, []string{"p", m})
		}
		return sign(t, author, wire.KINDNip43Membership, ts, tags, "")
	}

	// relay-signed list naming the member → member.
	current := []map[string]interface{}{list(relaySec, 100, memberPK)}
	srv := serveEvents(t, current)
	ok, err := IsCommunityMemberAuth(srv.URL, srv.URL, relaySec, relayPK, memberPK)
	if err != nil || !ok {
		t.Fatalf("listed member: ok=%v err=%v", ok, err)
	}

	// An empty list: not a member.
	srv = serveEvents(t, nil)
	if ok, err := IsCommunityMemberAuth(srv.URL, srv.URL, relaySec, relayPK, memberPK); ok || err != nil {
		t.Fatalf("empty list must not admit: ok=%v err=%v", ok, err)
	}

	// A TAMPERED listing (its content mangled after signing) is skipped —
	// the read verifies each event like the roster read does, so an injected
	// or edited listing is not the relay's word.
	tampered := list(relaySec, 100, memberPK)
	tampered["content"] = "tampered"
	srv = serveEvents(t, []map[string]interface{}{tampered})
	if ok, err := IsCommunityMemberAuth(srv.URL, srv.URL, relaySec, relayPK, memberPK); ok || err != nil {
		t.Fatalf("tampered listing must not admit: ok=%v err=%v", ok, err)
	}

	// A FORGED list (the outsider's key) must admit nobody — the filter pins
	// authors=[relayPubkey], so the fake never matters, but pin the read too.
	forged := []map[string]interface{}{list(outsider, 100, memberPK)}
	srv = serveEvents(t, forged)
	if ok, err := IsCommunityMemberAuth(srv.URL, srv.URL, relaySec, relayPK, memberPK); ok || err != nil {
		t.Fatalf("forged author must not admit: ok=%v err=%v", ok, err)
	}

	// Newest listing wins: the member was REMOVED by the ts-200 republish.
	srv = serveEvents(t, []map[string]interface{}{
		list(relaySec, 100, memberPK),
		list(relaySec, 200),
	})
	if ok, err := IsCommunityMemberAuth(srv.URL, srv.URL, relaySec, relayPK, memberPK); ok || err != nil {
		t.Fatalf("a dropped member must not pass on the stale listing: ok=%v err=%v", ok, err)
	}

	// ...and the mirror: the newest listing re-adds them.
	srv = serveEvents(t, []map[string]interface{}{
		list(relaySec, 200),
		list(relaySec, 100, memberPK),
		list(relaySec, 300, memberPK, testPubkeyOf(t, mustSecret(t, 9))),
	})
	if ok, err := IsCommunityMemberAuth(srv.URL, srv.URL, relaySec, relayPK, memberPK); !ok || err != nil {
		t.Fatalf("the newest listing re-admitting must pass: ok=%v err=%v", ok, err)
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
	if _, err := IsCommunityMemberAuth(srv.URL, srv.URL, relaySec, relayPK, testPubkeyOf(t, mustSecret(t, 2))); err == nil {
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
