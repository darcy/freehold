package relay

import (
	"encoding/json"

	"encoding/hex"
	"freehold/contract/crypto"
	"testing"
	"time"

	"freehold/contract/wire"
)

func mustSecret(t *testing.T, v byte) []byte {
	s := make([]byte, 32)
	for i := range s {
		s[i] = v
	}
	return s
}

func sign(t *testing.T, secret []byte, kind uint32, ts int64, tags [][]string, content string) map[string]interface{} {
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

func hexStr(b []byte) string { return hex.EncodeToString(b) }

func TestParseProfileContentAcceptsAndRejects(t *testing.T) {
	ok := `{"name":"ssh","kind":"ssh","address":"host:22","status":"active","nostr_pubkey":"` +
		strRepeat("a", 64) + `","enc_pubkey":"` + strRepeat("b", 64) + `","secret":"ssh","created_at":5,"rotated_at":null}`
	p, err := ParseProfileContent(ok)
	if err != nil {
		t.Fatalf("parse active: %v", err)
	}
	if p.Name != "ssh" || p.Status != "active" || p.RotatedAt != nil {
		t.Fatalf("bad parse: %+v", p)
	}
	if _, err := ParseProfileContent(`{"nope":1}`); err == nil {
		t.Fatal("expected error for missing fields")
	}
	if _, err := ParseProfileContent(ok + ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	badStatus := `{"name":"ssh","kind":"ssh","address":"h","status":"banana","nostr_pubkey":"` +
		strRepeat("a", 64) + `","enc_pubkey":"` + strRepeat("b", 64) + `","secret":"ssh","created_at":5}`
	if _, err := ParseProfileContent(badStatus); err == nil {
		t.Fatal("expected error for bad status")
	}
}

func TestRunnerChannelIDDeterministic(t *testing.T) {
	pk := strRepeat("a", 64)
	id1 := RunnerChannelID(pk)
	id2 := RunnerChannelID(pk)
	if id1 != id2 {
		t.Fatal("channel id must be deterministic")
	}
	if len(id1) != 36 {
		t.Fatalf("channel id should be a 36-char UUID, got %q (%d)", id1, len(id1))
	}
	// Distinct pubkey -> distinct channel id.
	if RunnerChannelID(strRepeat("a", 64)) == RunnerChannelID(strRepeat("b", 64)) {
		t.Fatal("distinct pubkeys must yield distinct channel ids")
	}
}

func TestChannelIDFromName(t *testing.T) {
	id := ChannelIDFromName("ops")
	if len(id) != 36 {
		t.Fatalf("channel id should be a 36-char UUID, got %q (%d)", id, len(id))
	}
	// Deterministic, and '#'/case are ignored (so a re-run targets the same id).
	if ChannelIDFromName("ops") != id || ChannelIDFromName("#OPS") != id || ChannelIDFromName(" ops ") != id {
		t.Fatal("channel id must be deterministic and normalize '#'/case/space")
	}
	if ChannelIDFromName("ops") == ChannelIDFromName("other") {
		t.Fatal("distinct names must yield distinct channel ids")
	}
	if ChannelIDFromName("freehold") == relayFreeholdChannelForTest() {
		t.Fatal("a derived id must not collide with the fixed freehold channel")
	}
}

// relayFreeholdChannelForTest mirrors cpbuild's fixed freehold channel id (the
// derived ids must never collide with it).
func relayFreeholdChannelForTest() string { return "00000000-0000-4000-8000-00000000f0ef" }

func TestMemoryDTag(t *testing.T) {
	d := MemoryDTag(strRepeat("a", 64), "key")
	if len(d) != 64 {
		t.Fatalf("d-tag must be 64 hex, got %d", len(d))
	}
	// Deterministic.
	if MemoryDTag(strRepeat("a", 64), "key") != d {
		t.Fatal("d-tag must be deterministic")
	}
}

// --- merge_runner_metas (newest wins per channel; rogue author ignored) ---

func metaEvent(t *testing.T, who []byte, p *RunnerProfile, ts int64) map[string]interface{} {
	content, _ := jsonMarshal(p)
	h := RunnerChannelID(p.NostrPubkey)
	return sign(t, who, wire.ChannelMessage, ts, [][]string{{"h", h}, {"d", h}, {"t", profileMessageTag}}, string(content))
}

func profile(name, status, nostr string) *RunnerProfile {
	return &RunnerProfile{
		Name: name, Kind: "ssh", Address: "h1:22", Status: status,
		NostrPubkey: nostr, EncPubkey: strRepeat("e", 64), Secret: name,
		CreatedAt: 1,
	}
}

func TestMetaNewestWinsPerChannelAndRogueAuthorIgnored(t *testing.T) {
	secret := mustSecret(t, 7)
	author := pubkeyOf(t, secret)
	alpha := profile("alpha", "active", strRepeat("a", 64))
	beta := profile("beta", "active", strRepeat("b", 64))
	rogue := profile("rogue", "revoked", strRepeat("a", 64))

	events := []map[string]interface{}{
		metaEvent(t, secret, alpha, 10),
		metaEvent(t, secret, beta, 10),
		metaEvent(t, mustSecret(t, 1), rogue, 999), // rogue author, newest
	}
	// Replace alpha with revoked (newest wins).
	revoked := profile("alpha", "revoked", strRepeat("a", 64))
	events = append(events, metaEvent(t, secret, revoked, 11))

	out, err := mergeRunnerMetas(events, author)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 runners, got %d", len(out))
	}
	var alphaOut *RunnerProfile
	for _, p := range out {
		if p.Name == "alpha" {
			alphaOut = p
		}
	}
	if alphaOut == nil || alphaOut.Status != "revoked" {
		t.Fatalf("newest alpha must win (replace): %+v", alphaOut)
	}
	if out[0].Name != "alpha" || out[1].Name != "beta" {
		t.Fatalf("not sorted by name: %+v", out)
	}
}

// --- merge_roster (relay-signed only; rogue ignored) ---

func rosterEvent(t *testing.T, who []byte, ts int64, channel string, members []string) map[string]interface{} {
	// The live buzz 39002 shape: ["p", pk, "", role] — the EMPTY element is part
	// of the signed bytes and must survive the round-trip (BLOCKING-1).
	tags := [][]string{{"d", channel}}
	for _, m := range members {
		tags = append(tags, []string{"p", m, "", "member"})
	}
	return sign(t, who, wire.GroupMembers, ts, tags, "")
}

func TestRosterNewestWinsAndRogueSignedRosterIgnored(t *testing.T) {
	relaySecret := mustSecret(t, 42)
	relayPK := pubkeyOf(t, relaySecret)
	runnerPK := strRepeat("r", 64)
	alice := strRepeat("a", 64)
	channel := RunnerChannelID(runnerPK)
	rogue := mustSecret(t, 1)
	roguePK := pubkeyOf(t, rogue)

	events := []map[string]interface{}{
		rosterEvent(t, relaySecret, 10, channel, []string{runnerPK, alice}),
		// Same channel roster REPLACE (revoke alice) — newest wins.
		rosterEvent(t, relaySecret, 11, channel, []string{runnerPK}),
		// A rogue "roster" with a NEWER timestamp — ignored.
		rosterEvent(t, rogue, 999, channel, []string{runnerPK, roguePK}),
	}
	out, err := mergeRoster(events, relayPK, channel)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0] != runnerPK {
		t.Fatalf("expected only runnerPK, got %v", out)
	}
	// Different channel: nothing leaks across.
	other := RunnerChannelID(strRepeat("x", 64))
	out2, _ := mergeRoster(events, relayPK, other)
	if len(out2) != 0 {
		t.Fatalf("different channel leaked: %v", out2)
	}
}

func pubkeyOf(t *testing.T, secret []byte) string {
	t.Helper()
	pk, err := publicKeyHex(secret)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

func strRepeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func jsonMarshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

var _ = time.Now
var _ = hex.DecodeString

// publicKeyHex derives the x-only pubkey hex from a secret.
func publicKeyHex(secret []byte) (string, error) {
	return crypto.PubkeyFromSecret(secret)
}

// TestIsMemberFromEvents pins the membership read the reconcile consults
// before every channel write: the NEWEST event naming the member decides
// (9000 add => member, 9001 remove => not), events naming other members are
// ignored, and an empty set is not a member. This is what keeps a converged
// world from re-asserting memberships on every bring-up — each re-assert is
// a fresh "you were added" event on the relay.
func TestIsMemberFromEvents(t *testing.T) {
	me, other := "a1a1a1a1a1a1", "b2b2b2b2b2b2"
	ev := func(kind int, ts int, member string) map[string]interface{} {
		return map[string]interface{}{
			"kind":       float64(kind),
			"created_at": float64(ts),
			"tags":       []interface{}{[]interface{}{"h", "chan"}, []interface{}{"p", member}},
		}
	}
	add, remove := wire.PutUser, wire.RemoveUser

	if isMemberFromEvents(nil, me) {
		t.Fatal("no events must read as not a member")
	}
	if isMemberFromEvents([]map[string]interface{}{ev(add, 1, other)}, me) {
		t.Fatal("events naming another member must be ignored")
	}
	if !isMemberFromEvents([]map[string]interface{}{ev(add, 1, me)}, me) {
		t.Fatal("a lone add must read as a member")
	}
	if isMemberFromEvents([]map[string]interface{}{ev(add, 1, me), ev(remove, 2, me)}, me) {
		t.Fatal("a remove after an add must read as NOT a member")
	}
	if !isMemberFromEvents([]map[string]interface{}{ev(remove, 1, me), ev(add, 2, me)}, me) {
		t.Fatal("a re-add after a remove must read as a member")
	}
	// Newest wins regardless of slice order.
	if isMemberFromEvents([]map[string]interface{}{ev(remove, 5, me), ev(add, 2, me)}, me) {
		t.Fatal("the newest event must decide, not the last in the slice")
	}
}
