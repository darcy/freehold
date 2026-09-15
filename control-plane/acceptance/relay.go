// Package acceptance is the Go re-home of the Chunk-1/2 acceptance gate: the
// CP provisioner lifecycle, the console HTTP surface, and the relay-channel
// fold — exercised against real wire contracts (NIP-98 auth, NIP-01 filters,
// relay-signed rosters) on loopback. It is the hermetic coverage the Rust
// console crate used to provide.
//
// relay.go is the fake buzz HTTP bridge: the subset of surfaces the CP's
// runner-lifecycle port uses, with REAL NIP-98 verification so the tests
// exercise the actual wire contract instead of a hand-wave.
package acceptance

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"freehold/contract/wire"
)

// relaySecret is the fixture relay's identity — rosters are signed with it
// (modeling BUZZ_RELAY_PRIVATE_KEY); runners verify against RelayPubkey().
func relaySecret() []byte {
	s := make([]byte, 32)
	for i := range s {
		s[i] = 42
	}
	return s
}

// RelayPubkey is the relay's nostr pubkey — the roster trust anchor.
func RelayPubkey() string {
	pk, _, _, _ := wire.SignEvent(relaySecret(), 1, 1, [][]string{}, "")
	return pk
}

// relayEvent is one stored Nostr event (normalized for filter/tag walks).
type relayEvent struct {
	ID        string     `json:"id"`
	Pubkey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      uint32     `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

func (e relayEvent) tag(name string) string {
	for _, t := range e.Tags {
		if len(t) > 1 && t[0] == name {
			return t[1]
		}
	}
	return ""
}

func (e relayEvent) tagAll(name string) []string {
	var out []string
	for _, t := range e.Tags {
		if len(t) > 1 && t[0] == name {
			out = append(out, t[1])
		}
	}
	return out
}

type relayChannel struct {
	owner   string
	members map[string]bool
}

// RelayState is the fake relay's event store + channel registry.
type RelayState struct {
	mu          sync.Mutex
	baseURL     string
	events      []relayEvent
	authed      []string
	channels    map[string]*relayChannel
	blockEvents bool
}

// NewRelayState builds an empty fake relay.
func NewRelayState() *RelayState {
	return &RelayState{channels: map[string]*relayChannel{}}
}

// SetBaseURL records the loopback base the fixture is listening on. NIP-98
// auth is verified against baseURL+path (the Go relay client signs the same
// URL it dials).
func (s *RelayState) SetBaseURL(u string) {
	s.mu.Lock()
	s.baseURL = strings.TrimSuffix(u, "/")
	s.mu.Unlock()
}

func (s *RelayState) base() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseURL
}

// Events returns a copy of the stored events.
func (s *RelayState) Events() []relayEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]relayEvent(nil), s.events...)
}

// AuthedCallers returns the pubkeys that have successfully authenticated.
func (s *RelayState) AuthedCallers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authed...)
}

// AppendEvent stores an event directly (bypassing HTTP) for adversary tests.
func (s *RelayState) AppendEvent(ev relayEvent) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

// RelayEvent is the exported shape of a stored event for tests.
type RelayEvent = relayEvent

// SetBlockEvents makes POST /events return 500 (degradation tests).
func (s *RelayState) SetBlockEvents(b bool) {
	s.mu.Lock()
	s.blockEvents = b
	s.mu.Unlock()
}

// ChannelMembers returns the current roster of a channel (the relay's authority).
func (s *RelayState) ChannelMembers(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.membersLocked(id)
}

func (s *RelayState) membersLocked(id string) []string {
	ch := s.channels[id]
	if ch == nil {
		return nil
	}
	out := make([]string, 0, len(ch.members))
	for m := range ch.members {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Handler routes the two real bridge surfaces: POST /query and POST /events.
func (s *RelayState) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/query", s.handleQuery)
	mux.HandleFunc("/events", s.handlePublish)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// verifyNIP98 checks the Authorization header (kind 27235, sig, method, u)
// and returns the caller's pubkey.
func (s *RelayState) verifyNIP98(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Nostr ") {
		return "", fmt.Errorf("no authorization header")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Nostr "))
	if err != nil {
		return "", fmt.Errorf("bad auth base64: %w", err)
	}
	var ev relayEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return "", fmt.Errorf("bad auth event: %w", err)
	}
	if ev.Kind != wire.KINDHTTPAuth {
		return "", fmt.Errorf("auth event not kind %d", wire.KINDHTTPAuth)
	}
	if ev.tag("method") != r.Method {
		return "", fmt.Errorf("auth method mismatch")
	}
	if want := s.base() + r.URL.Path; ev.tag("u") != want {
		return "", fmt.Errorf("auth url mismatch: got %q want %q", ev.tag("u"), want)
	}
	if _, err := wire.VerifyEvent(ev.Pubkey, ev.CreatedAt, ev.Kind, ev.Tags, ev.Content, ev.Sig); err != nil {
		return "", fmt.Errorf("bad auth signature: %w", err)
	}
	return ev.Pubkey, nil
}

// handleQuery implements POST /query with a NIP-01 filter array and the
// member-gated roster read.
func (s *RelayState) handleQuery(w http.ResponseWriter, r *http.Request) {
	caller, err := s.verifyNIP98(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.authed = append(s.authed, caller)
	s.mu.Unlock()

	var body []map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) == 0 {
		writeJSON(w, http.StatusOK, []relayEvent{})
		return
	}
	filter := body[0]
	kinds := kindSet(filter["kinds"])
	dTags := strList(filter["#d"])
	pTags := strList(filter["#p"])
	hTags := strList(filter["#h"])
	tTags := strList(filter["#t"])
	authors := strList(filter["authors"])
	since := int64(num(filter["since"]))

	// Member gate: roster reads (kind 39002) for a channel require the caller
	// to be a member or the owner (the LIVE enforcement: non-members 403).
	if kinds[wire.GroupMembers] {
		ids := dTags
		if len(ids) == 0 {
			ids = hTags
		}
		if len(ids) > 0 {
			s.mu.Lock()
			ok := true
			for _, id := range ids {
				ch := s.channels[id]
				if ch == nil || (!ch.members[caller] && ch.owner != caller) {
					ok = false
					break
				}
			}
			s.mu.Unlock()
			if !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error":   "relay_membership_required",
					"message": "You must be a relay member to access this relay",
				})
				return
			}
		}
	}

	s.mu.Lock()
	out := []relayEvent{}
	for _, e := range s.events {
		if len(kinds) > 0 && !kinds[e.Kind] {
			continue
		}
		if since > 0 && e.CreatedAt < since {
			continue
		}
		if len(authors) > 0 && !containsStr(authors, e.Pubkey) {
			continue
		}
		if len(dTags) > 0 && !anyIn(dTags, e.tagAll("d")) {
			continue
		}
		if len(pTags) > 0 && !anyIn(pTags, e.tagAll("p")) {
			continue
		}
		if len(hTags) > 0 && !anyIn(hTags, e.tagAll("h")) {
			continue
		}
		if len(tTags) > 0 && !anyIn(tTags, e.tagAll("t")) {
			continue
		}
		out = append(out, e)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// handlePublish implements POST /events with the runner-lifecycle channel
// semantics (9007 create, 9000 put-user, 9001 remove-user) and refused
// client-authored rosters (39002).
func (s *RelayState) handlePublish(w http.ResponseWriter, r *http.Request) {
	caller, err := s.verifyNIP98(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.authed = append(s.authed, caller)
	blocked := s.blockEvents
	s.mu.Unlock()
	if blocked {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "events blocked"})
		return
	}
	var ev relayEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if status, errMsg := s.applyChannelSemantics(caller, ev); status != 0 {
		writeJSON(w, status, map[string]string{"error": errMsg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// applyChannelSemantics executes 9007/9000/9001 and refuses client rosters.
// Returns a non-zero HTTP status when the relay must refuse.
func (s *RelayState) applyChannelSemantics(caller string, ev relayEvent) (int, string) {
	h := ev.tag("h")
	if h == "" {
		s.mu.Lock()
		s.events = append(s.events, ev)
		s.mu.Unlock()
		return 0, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Kind {
	case wire.ChannelCreate:
		if s.channels[h] == nil {
			s.channels[h] = &relayChannel{owner: caller, members: map[string]bool{caller: true}}
			s.publishRosterLocked(h)
		}
	case wire.PutUser, wire.RemoveUser:
		ch := s.channels[h]
		if ch == nil {
			return http.StatusNotFound, "relay: no such channel"
		}
		if ch.owner != caller {
			return http.StatusForbidden, "restricted: not the channel owner"
		}
		target := ev.tag("p")
		if target != "" {
			if ev.Kind == wire.PutUser {
				ch.members[target] = true
			} else {
				delete(ch.members, target)
			}
			s.publishRosterLocked(h)
		}
	case wire.GroupMembers:
		// Rosters are relay-minted; a rogue member must not grant itself exec.
		return http.StatusForbidden, "restricted: roster is relay-signed"
	}
	s.events = append(s.events, ev)
	return 0, ""
}

// publishRosterLocked relay-signs a kind-39002 roster snapshot (d + one p per
// member) and stores it. Caller holds s.mu.
func (s *RelayState) publishRosterLocked(channelID string) {
	members := s.membersLocked(channelID)
	tags := [][]string{{"d", channelID}}
	for _, m := range members {
		tags = append(tags, []string{"p", m})
	}
	ts := time.Now().Unix()
	pk, id, sig, err := wire.SignEvent(relaySecret(), wire.GroupMembers, ts, tags, "")
	if err != nil {
		return
	}
	s.events = append(s.events, relayEvent{
		ID: id, Pubkey: pk, CreatedAt: ts, Kind: wire.GroupMembers, Tags: tags, Sig: sig,
	})
}

// ---- filter helpers ----

func kindSet(v interface{}) map[uint32]bool {
	out := map[uint32]bool{}
	for _, k := range intList(v) {
		out[uint32(k)] = true
	}
	return out
}

func intList(v interface{}) []int64 {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	var out []int64
	for _, x := range arr {
		out = append(out, num(x))
	}
	return out
}

func strList(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	var out []string
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func num(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func anyIn(want, got []string) bool {
	for _, g := range got {
		if containsStr(want, g) {
			return true
		}
	}
	return false
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
