// Package delegate reproduces core/src/delegate.rs — the Phase-E delegation
// wire: CPA hands a task to a relay-addressable peer agent via kind-9 channel
// messages in a CPA-created OPEN channel. Requests/results correlate by an
// echoed `id` in the content envelopes.
package delegate

import (
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"freehold/contract/relay"
	"freehold/contract/wire"
)

const (
	ChannelCreateKind = wire.ChannelCreate
	StreamMsgKind     = wire.ChannelMessage
	JobRequest        = "job-request"
	JobResult         = "job-result"
)

// Envelope is a parsed delegation envelope (request or result).
type Envelope struct {
	Type string  `json:"type"`
	ID   string  `json:"id"`
	Task *string `json:"task,omitempty"`
	OK   *bool   `json:"ok,omitempty"`
	Out  *string `json:"out,omitempty"`
}

// RequestContent builds a job-request envelope JSON.
func RequestContent(id, task string) string {
	b, _ := json.Marshal(map[string]interface{}{"type": JobRequest, "id": id, "task": task})
	return string(b)
}

// ResultContent builds a job-result envelope JSON.
func ResultContent(id string, ok bool, out string) string {
	b, _ := json.Marshal(map[string]interface{}{"type": JobResult, "id": id, "ok": ok, "out": out})
	return string(b)
}

// ParseEnvelope parses a delegation envelope.
func ParseEnvelope(content string) *Envelope {
	var e Envelope
	if err := json.Unmarshal([]byte(content), &e); err != nil {
		return nil
	}
	if e.Type == "" || e.ID == "" {
		return nil
	}
	return &e
}

// EnsureChannel creates (idempotently) the auto-ops OPEN channel (kind 9007,
// h/name/visibility=open).
func EnsureChannel(relayURL string, secret []byte, channelID, name string) error {
	return EnsureChannelAuth(relayURL, relayURL, secret, channelID, name)
}

// EnsureChannelAuth is EnsureChannel with a separate NIP-98 auth URL (the
// pre-Caddy LAN-dial case in agent-tools).
func EnsureChannelAuth(dialURL, authURL string, secret []byte, channelID, name string) error {
	return ensureChannel(dialURL, authURL, secret, channelID, name, "open")
}

// EnsurePrivateChannelAuth creates (idempotently) a PRIVATE channel (kind 9007,
// h/name/visibility=private): only members the owner adds can see it. Used for
// the shared freehold channel and private custom channels, which their creator
// owns and into which it adds members.
func EnsurePrivateChannelAuth(dialURL, authURL string, secret []byte, channelID, name string) error {
	return ensureChannel(dialURL, authURL, secret, channelID, name, "private")
}

// EditChannelAuth publishes a kind-9002 metadata edit for an EXISTING channel,
// signed by its owner/admin: each tag is applied (name, visibility, archived,
// …) and the relay re-emits group discovery. The `h` tag is the channel id.
// Idempotent — re-sending the same tags is a no-op. Used to true up channels a
// world already has (make #freehold private; archive a retired channel).
func EditChannelAuth(dialURL, authURL string, secret []byte, channelID string, tags ...[]string) error {
	all := append([][]string{{"h", channelID}}, tags...)
	return publishSignedAuth(dialURL, authURL, secret, wire.EditMetadata, all, "")
}

func ensureChannel(dialURL, authURL string, secret []byte, channelID, name, visibility string) error {
	return publishSignedAuth(dialURL, authURL, secret, ChannelCreateKind, [][]string{
		{"h", channelID},
		{"name", name},
		{"visibility", visibility},
	}, "")
}

// PostMessage posts a kind-9 channel message, p-tagging the counterparty.
func PostMessage(relayURL string, secret []byte, channelID, mentionPubkey, content string) error {
	return publishSigned(relayURL, secret, StreamMsgKind, [][]string{
		{"h", channelID},
		{"p", mentionPubkey},
	}, content)
}

// PostTaggedMessageAuth is PostMessage with extra tags appended (e.g. a
// ["t", marker] idempotence marker) and a separate NIP-98 auth URL.
func PostTaggedMessageAuth(dialURL, authURL string, secret []byte, channelID, mentionPubkey string, extra [][]string, content string) error {
	tags := append([][]string{
		{"h", channelID},
		{"p", mentionPubkey},
	}, extra...)
	return publishSignedAuth(dialURL, authURL, secret, StreamMsgKind, tags, content)
}

// PostNoteAuth publishes a bare kind:1 note (NIP-01) — the Buzz Pulse surface
// (its global feed reads kinds:[1]; channel messages are kind 9 and never show
// there). Extra tags ride for provenance/idempotence markers.
func PostNoteAuth(dialURL, authURL string, secret []byte, extra [][]string, content string) error {
	return publishSignedAuth(dialURL, authURL, secret, 1, extra, content)
}

// PollResult is one verified stream message (created_at, content, author).
type PollResult struct {
	CreatedAt int64
	Content   string
	Author    string
}

// PollStream polls the channel for messages newer than since, NO p-filter,
// locally signature-verified, sorted. Reproduces delegate::poll_stream (the
// requester uses the unfiltered path — a #p-filtered query hangs for some
// identities, verified live).
func PollStream(relayURL string, secret []byte, channelID string, since int64) ([]PollResult, error) {
	return pollAuth(relayURL, relayURL, secret, channelID, "", since)
}

// PollStreamAuth is PollStream with a separate NIP-98 auth URL: dial the
// relay directly (the LAN origin pre-Caddy) while SIGNING the canonical
// public URL — the relay rejects an auth whose `u` is the LAN origin.
func PollStreamAuth(dialURL, authURL string, secret []byte, channelID string, since int64) ([]PollResult, error) {
	return pollAuth(dialURL, authURL, secret, channelID, "", since)
}

// PollStreamP polls for messages by mention_pubkey (p-tag) newer than since
// (the executor uses the p-filtered path, which works for its identity).
func PollStreamP(relayURL string, secret []byte, channelID, mentionPubkey string, since int64) ([]PollResult, error) {
	return pollAuth(relayURL, relayURL, secret, channelID, mentionPubkey, since)
}

// TaggedEvent is one signature-verified relay event with its tags intact —
// the mention watcher's unit: id, kind, author, timestamp, tags (root-e/a/h/p
// parsing) and content.
type TaggedEvent struct {
	ID        string
	Kind      uint32
	Author    string
	CreatedAt int64
	Tags      [][]string
	Content   string
}

// Tag returns the first value of tag name ("" when absent).
func (e TaggedEvent) Tag(name string) string {
	for _, t := range e.Tags {
		if len(t) > 1 && t[0] == name {
			return t[1]
		}
	}
	return ""
}

// Root returns the task/issue id the event hangs on: the e tag carrying the
// NIP-01 "root" marker wins; the first e tag is the fallback.
func (e TaggedEvent) Root() string {
	for _, t := range e.Tags {
		if len(t) >= 4 && t[0] == "e" && t[3] == "root" {
			return t[1]
		}
	}
	return e.Tag("e")
}

// HasChannel reports whether the event carries an h tag — the channel-scoped
// shape a per-channel subscription already sees (the watcher must skip these;
// the channel path owns them).
func (e TaggedEvent) HasChannel() bool {
	for _, t := range e.Tags {
		if len(t) > 0 && t[0] == "h" {
			return true
		}
	}
	return false
}

// TaggedFilter is a relay query by kind plus optional tag/author matches —
// the channel-less path a per-channel (#h) subscription can never see. A
// zero/empty field means "no filter" for that dimension (Kinds must be
// non-empty).
type TaggedFilter struct {
	Kinds   []uint32
	P       []string
	E       []string
	Ids     []string
	Authors []string
	Since   int64
	Limit   int
}

// QueryTaggedAuth queries the relay for events matching f, locally
// signature-verified — unverifiable events are dropped, never surfaced.
func QueryTaggedAuth(dialURL, authURL string, secret []byte, f TaggedFilter) ([]TaggedEvent, error) {
	if len(f.Kinds) == 0 {
		return nil, nil
	}
	filter := map[string]interface{}{
		"kinds": strsToIface(uint32sToStrings(f.Kinds)),
		"since": f.Since,
		"limit": f.Limit,
	}
	if f.Limit == 0 {
		filter["limit"] = 500
	}
	if len(f.P) > 0 {
		filter["#p"] = strsToIface(f.P)
	}
	if len(f.E) > 0 {
		filter["#e"] = strsToIface(f.E)
	}
	if len(f.Ids) > 0 {
		filter["ids"] = strsToIface(f.Ids)
	}
	if len(f.Authors) > 0 {
		filter["authors"] = strsToIface(f.Authors)
	}
	events, err := relay.QueryEventsAuth(dialURL, authURL, secret, []interface{}{filter})
	if err != nil {
		return nil, err
	}
	allowed := make(map[uint32]bool, len(f.Kinds))
	for _, k := range f.Kinds {
		allowed[k] = true
	}
	return collectTagged(events, allowed), nil
}

// collectTagged signature-verifies each event and keeps those whose kind is
// allowed (the relay's order is preserved; callers sort if they care).
func collectTagged(events []map[string]interface{}, allowed map[uint32]bool) []TaggedEvent {
	var out []TaggedEvent
	for _, ev := range events {
		kind := uint32(int64num(ev["kind"]))
		if !allowed[kind] {
			continue
		}
		author, _ := ev["pubkey"].(string)
		createdAt := int64num(ev["created_at"])
		tags := parseTags(ev)
		content, _ := ev["content"].(string)
		sig, _ := ev["sig"].(string)
		if _, err := wire.VerifyEvent(author, createdAt, kind, tags, content, sig); err != nil {
			continue
		}
		id, _ := ev["id"].(string)
		out = append(out, TaggedEvent{ID: id, Kind: kind, Author: author, CreatedAt: createdAt, Tags: tags, Content: content})
	}
	return out
}

func uint32sToStrings(ks []uint32) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = strconv.FormatUint(uint64(k), 10)
	}
	return out
}

func strsToIface(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func pollAuth(dialURL, authURL string, secret []byte, channelID, p string, since int64) ([]PollResult, error) {
	filter := map[string]interface{}{
		"kinds": []interface{}{StreamMsgKind},
		"#h":    []interface{}{channelID},
		"since": since,
		"limit": 100,
	}
	if p != "" {
		filter["#p"] = []interface{}{p}
	}
	events, err := relay.QueryEventsAuth(dialURL, authURL, secret, []interface{}{filter})
	if err != nil {
		return nil, err
	}
	out := collectVerified(events)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

func publishSigned(relayURL string, secret []byte, kind uint32, tags [][]string, content string) error {
	return publishSignedAuth(relayURL, relayURL, secret, kind, tags, content)
}

func publishSignedAuth(dialURL, authURL string, secret []byte, kind uint32, tags [][]string, content string) error {
	ts := time.Now().Unix()
	pubkey, id, sig, err := wire.SignEvent(secret, kind, ts, tags, content)
	if err != nil {
		return err
	}
	ev := map[string]interface{}{
		"id": id, "pubkey": pubkey, "created_at": ts, "kind": kind,
		"tags": tags, "content": content, "sig": sig,
	}
	evBytes, _ := json.Marshal(ev)
	return relay.PublishEventJSONAuth(dialURL, authURL, secret, string(evBytes))
}

// collectVerified locally signature-verifies every event.
func collectVerified(events []map[string]interface{}) []PollResult {
	var out []PollResult
	for _, ev := range events {
		author, _ := ev["pubkey"].(string)
		createdAt := int64num(ev["created_at"])
		tags := parseTags(ev)
		content, _ := ev["content"].(string)
		sig, _ := ev["sig"].(string)
		if _, err := wire.VerifyEvent(author, createdAt, StreamMsgKind, tags, content, sig); err != nil {
			continue
		}
		out = append(out, PollResult{CreatedAt: createdAt, Content: content, Author: author})
	}
	return out
}

func parseTags(ev map[string]interface{}) [][]string {
	var tags [][]string
	if raw, ok := ev["tags"].([]interface{}); ok {
		for _, t := range raw {
			var row []string
			if arr, ok := t.([]interface{}); ok {
				for _, a := range arr {
					if s, ok := a.(string); ok {
						row = append(row, s)
					}
				}
			}
			tags = append(tags, row)
		}
	}
	return tags
}

func int64num(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}
