// Package relay reproduces the freehold-core relay HTTP bridge client
// (core/src/relay_http.rs) — channels (NIP-29), roster, runner meta, memory.
//
// Trust + dedupe cores are ported exactly: per-channel NEWEST trusted event
// wins, sorted for determinism, a malformed winner fails closed, a malformed
// non-winner warns and skips. Every query/publish is NIP-98-authed, status
// checked, and time-bounded (a wedged relay fails closed, never hangs).
package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freehold/orchestrator-go/internal/wire"
)

const (
	profileMessageTag = "fh-profile"
	httpTimeoutSec    = 30
)

// RunnerProfile mirrors core::relay_http::RunnerProfile.
type RunnerProfile struct {
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Address     string  `json:"address"`
	Status      string  `json:"status"`
	NostrPubkey string  `json:"nostr_pubkey"`
	EncPubkey   string  `json:"enc_pubkey"`
	Secret      string  `json:"secret"`
	CreatedAt   uint64  `json:"created_at"`
	RotatedAt   *uint64 `json:"rotated_at,omitempty"`
	Risk        *string `json:"risk,omitempty"`
}

// MemoryDTag is the deterministic 64-hex engram address for (agent pubkey,
// memory key): sha256(agent_pk ‖ "#" ‖ key).
func MemoryDTag(agentPK, key string) string {
	h := sha256.Sum256([]byte(agentPK + "#" + key))
	return hex.EncodeToString(h[:])
}

// RunnerChannelID returns the dashed UUID channel id for a runner:
// sha256(nostr pubkey)[0..16] formatted xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx.
func RunnerChannelID(runnerNostrPubkey string) string {
	h := sha256.Sum256([]byte(runnerNostrPubkey))
	hexs := hex.EncodeToString(h[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexs[0:8], hexs[8:12], hexs[12:16], hexs[16:20], hexs[20:32])
}

// ParseProfileContent parses a stored runner-metadata content string.
func ParseProfileContent(content string) (*RunnerProfile, error) {
	var p RunnerProfile
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		return nil, fmt.Errorf("profile content: %v", err)
	}
	if p.Status != "active" && p.Status != "revoked" {
		return nil, fmt.Errorf("profile bad status %q", p.Status)
	}
	if len(p.NostrPubkey) != 64 || len(p.EncPubkey) != 64 {
		return nil, fmt.Errorf("profile pubkey not 64-hex")
	}
	if p.Name == "" {
		return nil, fmt.Errorf("profile empty name")
	}
	return &p, nil
}

// agent is the bounded HTTP client (time-bounded so a wedged relay fails
// closed, never hangs).
func agent() *http.Client {
	return &http.Client{Timeout: httpTimeoutSec * time.Second}
}

func nip98Authorization(secret []byte, method, url string) (string, error) {
	return wire.Nip98Auth(secret, method, url, time.Now().Unix())
}

// QueryEvents POSTs /query with a filters ARRAY (NIP-98 auth), status
// checked, body parsed as an event array.
func QueryEvents(relayURL string, authSecret []byte, filters interface{}) ([]map[string]interface{}, error) {
	url := strings.TrimSuffix(relayURL, "/") + "/query"
	auth, err := nip98Authorization(authSecret, "POST", url)
	if err != nil {
		return nil, fmt.Errorf("query auth: %w", err)
	}
	body, _ := json.Marshal(filters)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("query request failed: %w", err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := agent().Do(req)
	if err != nil {
		return nil, fmt.Errorf("query request failed: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("query read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("query returned HTTP %d: %s", resp.StatusCode, rb)
	}
	var evs []map[string]interface{}
	if err := json.Unmarshal(rb, &evs); err != nil {
		return nil, fmt.Errorf("query parse: %v: %s", err, rb)
	}
	return evs, nil
}

// PublishEventJSON POSTs an already-signed Nostr event JSON to /events
// (NIP-98 auth). Status checked.
func PublishEventJSON(relayURL string, authSecret []byte, eventJSON string) error {
	url := strings.TrimSuffix(relayURL, "/") + "/events"
	auth, err := nip98Authorization(authSecret, "POST", url)
	if err != nil {
		return fmt.Errorf("event publish auth: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(eventJSON))
	if err != nil {
		return fmt.Errorf("event publish request failed: %w", err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := agent().Do(req)
	if err != nil {
		return fmt.Errorf("event publish request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("event publish returned HTTP %d: %s", resp.StatusCode, rb)
	}
	return nil
}

// publishEvent builds a signed NIP-01 event and publishes it.
func publishEvent(relayURL string, secret []byte, kind uint32, tags [][]string, content string) error {
	ts := time.Now().Unix()
	pubkey, id, sig, err := wire.SignEvent(secret, kind, ts, tags, content)
	if err != nil {
		return fmt.Errorf("sign event: %w", err)
	}
	ev := map[string]interface{}{
		"id": id, "pubkey": pubkey, "created_at": ts, "kind": kind,
		"tags": tags, "content": content, "sig": sig,
	}
	evBytes, _ := json.Marshal(ev)
	return PublishEventJSON(relayURL, secret, string(evBytes))
}

// CreateRunnerChannel creates (or re-asserts) a runner's private channel
// (kind 9007): tags h + name + visibility=private. Idempotent.
func CreateRunnerChannel(relayURL string, consoleSecret []byte, runnerNostrPubkey, name string) error {
	h := RunnerChannelID(runnerNostrPubkey)
	tags := [][]string{
		{"h", h},
		{"name", "#runner-" + name},
		{"visibility", "private"},
	}
	return publishEvent(relayURL, consoleSecret, wire.ChannelCreate, tags, "")
}

// PutUser adds a member to a runner channel (kind 9000 put-user), idempotent.
func PutUser(relayURL string, consoleSecret []byte, runnerNostrPubkey, memberPubkey string) error {
	return membershipCommand(relayURL, consoleSecret, wire.PutUser, runnerNostrPubkey, memberPubkey)
}

// RemoveUser removes a member from a runner channel (kind 9001 remove-user).
func RemoveUser(relayURL string, consoleSecret []byte, runnerNostrPubkey, memberPubkey string) error {
	return membershipCommand(relayURL, consoleSecret, wire.RemoveUser, runnerNostrPubkey, memberPubkey)
}

func membershipCommand(relayURL string, consoleSecret []byte, kind uint32, runnerNostrPubkey, memberPubkey string) error {
	h := RunnerChannelID(runnerNostrPubkey)
	tags := [][]string{{"h", h}, {"p", memberPubkey}}
	return publishEvent(relayURL, consoleSecret, kind, tags, "")
}

// PublishRunnerMeta publishes (or replaces) a runner profile as a kind-9
// channel message marked t=fh-profile.
func PublishRunnerMeta(relayURL string, consoleSecret []byte, profile *RunnerProfile) error {
	h := RunnerChannelID(profile.NostrPubkey)
	content, _ := json.Marshal(profile)
	tags := [][]string{
		{"h", h},
		{"d", h},
		{"t", profileMessageTag},
	}
	return publishEvent(relayURL, consoleSecret, wire.ChannelMessage, tags, string(content))
}

// QueryRunnerMetas fetches the CURRENT profile of every runner (kind-9
// fh-profile messages), newest trusted per channel, sorted by name.
func QueryRunnerMetas(relayURL string, expectedAuthor string, authSecret []byte) ([]*RunnerProfile, error) {
	filters := []interface{}{map[string]interface{}{
		"kinds": []interface{}{wire.ChannelMessage},
		"#t":    []interface{}{profileMessageTag},
		"limit": 1000,
	}}
	events, err := QueryEvents(relayURL, authSecret, filters)
	if err != nil {
		return nil, err
	}
	return mergeRunnerMetas(events, expectedAuthor)
}

// parseTags extracts tag arrays and the `h` tag value from an event.
func parseTags(ev map[string]interface{}) ([][]string, string) {
	var tags [][]string
	h := ""
	switch raw := ev["tags"].(type) {
	case []interface{}:
		for _, t := range raw {
			var row []string
			if arr, ok := t.([]interface{}); ok {
				for _, a := range arr {
					if s, ok := a.(string); ok {
						row = append(row, s)
					}
				}
			} else if ss, ok := t.([]string); ok {
				row = append(row, ss...)
			}
			tags = append(tags, row)
		}
	case [][]string:
		for _, t := range raw {
			tags = append(tags, append([]string(nil), t...))
		}
	}
	for _, t := range tags {
		if len(t) > 1 && t[0] == "h" {
			h = t[1]
		}
	}
	return tags, h
}

type profileEntry struct {
	ts int64
	p  *RunnerProfile
}

func mergeRunnerMetas(events []map[string]interface{}, expectedAuthor string) ([]*RunnerProfile, error) {
	best := map[string]profileEntry{}
	for _, ev := range events {
		author, _ := ev["pubkey"].(string)
		if author != expectedAuthor {
			continue
		}
		createdAt := intOr(ev["created_at"])
		tags, h := parseTags(ev)
		if h == "" {
			continue // missing h tag — skip
		}
		content, _ := ev["content"].(string)
		sig, _ := ev["sig"].(string)
		if _, err := wire.VerifyEvent(author, createdAt, wire.ChannelMessage, tags, content, sig); err != nil {
			continue // local signature verify failed — skip
		}
		// Newest (or same-second, later-inserted) wins per channel.
		if cur, ok := best[h]; !ok || createdAt >= cur.ts {
			profile, err := ParseProfileContent(content)
			if err != nil {
				return nil, err // malformed WINNER fails the fold closed
			}
			best[h] = profileEntry{createdAt, profile}
		}
	}
	out := make([]*RunnerProfile, 0, len(best))
	for _, e := range best {
		out = append(out, e.p)
	}
	// Sort by name for a stable fold.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Name < out[i].Name {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// QueryChannelRoster reads the runner's current roster (its exec whitelist):
// kinds [39002] filtered by #d = the runner channel. Members = the `p` tags,
// sorted. Trust anchor = relay_pubkey (verified locally); newest wins.
func QueryChannelRoster(relayURL, relayPubkey, runnerNostrPubkey string, authSecret []byte) ([]string, error) {
	chanID := RunnerChannelID(runnerNostrPubkey)
	filters := []interface{}{map[string]interface{}{
		"kinds": []interface{}{wire.GroupMembers},
		"#d":    []interface{}{chanID},
		"limit": 100,
	}}
	events, err := QueryEvents(relayURL, authSecret, filters)
	if err != nil {
		return nil, err
	}
	return mergeRoster(events, relayPubkey, chanID)
}

func mergeRoster(events []map[string]interface{}, relayPubkey, channelID string) ([]string, error) {
	newestTS := int64(-1)
	var newest []string
	for _, ev := range events {
		author, _ := ev["pubkey"].(string)
		if author != relayPubkey {
			continue
		}
		createdAt := intOr(ev["created_at"])
		tags, _ := parseTags(ev)
		// The roster's channel attribution is its d tag.
		if tagValue(tags, "d") != channelID {
			continue
		}
		content, _ := ev["content"].(string)
		sig, _ := ev["sig"].(string)
		if _, err := wire.VerifyEvent(author, createdAt, wire.GroupMembers, tags, content, sig); err != nil {
			continue
		}
		var members []string
		for _, t := range tags {
			if len(t) > 1 && t[0] == "p" {
				members = append(members, t[1])
			}
		}
		if newestTS < 0 || createdAt >= newestTS {
			newestTS = createdAt
			newest = members
		}
	}
	// Sort + dedup.
	seen := map[string]bool{}
	var out []string
	for _, m := range newest {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func tagValue(tags [][]string, key string) string {
	for _, t := range tags {
		if len(t) > 1 && t[0] == key {
			return t[1]
		}
	}
	return ""
}

func readFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// intOr coerces created_at from a json float64, a Go int64/int, or json.Number.
func intOr(v interface{}) int64 {
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

// WriteMemory writes (replaces) one encrypted memory engram (kind 30174,
// d-tag MemoryDTag, p-tag = the agent's pubkey).
func WriteMemory(relayURL string, agentNostrSecret []byte, key, value string) error {
	pk, _, _, err := wire.SignEvent(agentNostrSecret, 1, time.Now().Unix(), [][]string{}, "")
	if err != nil {
		return err
	}
	content, err := wire.SealMemory(agentNostrSecret, value)
	if err != nil {
		return err
	}
	dTag := MemoryDTag(pk, key)
	ts := time.Now().Unix()
	_, id, sig, err := wire.SignEvent(agentNostrSecret, wire.MemoryKind, ts, [][]string{{"d", dTag}, {"p", pk}}, content)
	if err != nil {
		return err
	}
	ev := map[string]interface{}{
		"id": id, "pubkey": pk, "created_at": ts, "kind": wire.MemoryKind,
		"tags": [][]string{{"d", dTag}, {"p", pk}}, "content": content, "sig": sig,
	}
	evBytes, _ := json.Marshal(ev)
	return PublishEventJSON(relayURL, agentNostrSecret, string(evBytes))
}

// ReadMemory reads the agent's own memory value for key (kind-30174, newest
// wins, author-verified, decrypts with the agent's enc key).
func ReadMemory(relayURL string, agentNostrSecret []byte, key string) (string, bool, error) {
	pk, _, _, err := wire.SignEvent(agentNostrSecret, 1, time.Now().Unix(), [][]string{}, "")
	if err != nil {
		return "", false, err
	}
	dTag := MemoryDTag(pk, key)
	filters := []interface{}{map[string]interface{}{
		"kinds":   []interface{}{wire.MemoryKind},
		"#d":      []interface{}{dTag},
		"authors": []interface{}{pk},
		"limit":   20,
	}}
	url := strings.TrimSuffix(relayURL, "/") + "/query"
	auth, err := nip98Authorization(agentNostrSecret, "POST", url)
	if err != nil {
		return "", false, err
	}
	body, _ := json.Marshal(filters)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return "", false, fmt.Errorf("memory query request failed: %w", err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := agent().Do(req)
	if err != nil {
		return "", false, fmt.Errorf("memory query request failed: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false, fmt.Errorf("memory query read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false, fmt.Errorf("memory query returned HTTP %d: %s", resp.StatusCode, rb)
	}
	var events []map[string]interface{}
	if err := json.Unmarshal(rb, &events); err != nil {
		return "", false, fmt.Errorf("memory query parse: %v: %s", err, rb)
	}
	bestTS := int64(-1)
	bestSealed := ""
	for _, ev := range events {
		author, _ := ev["pubkey"].(string)
		if author != pk {
			continue
		}
		createdAt := intOr(ev["created_at"])
		tags, _ := parseTags(ev)
		content, _ := ev["content"].(string)
		sig, _ := ev["sig"].(string)
		if _, err := wire.VerifyEvent(author, createdAt, wire.MemoryKind, tags, content, sig); err != nil {
			continue
		}
		if bestTS < 0 || createdAt >= bestTS {
			bestTS = createdAt
			bestSealed = content
		}
	}
	if bestSealed == "" {
		return "", false, nil
	}
	val, err := wire.OpenMemory(agentNostrSecret, bestSealed)
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// PublishProfile publishes kind-0 metadata for an identity (name/about).
func PublishProfile(relayURL string, secret []byte, name, about string) error {
	content, _ := json.Marshal(map[string]string{"name": name, "about": about, "display_name": name})
	ts := time.Now().Unix()
	pubkey, id, sig, err := wire.SignEvent(secret, 0, ts, [][]string{}, string(content))
	if err != nil {
		return err
	}
	ev := map[string]interface{}{
		"id": id, "pubkey": pubkey, "created_at": ts, "kind": 0,
		"tags": [][]string{}, "content": string(content), "sig": sig,
	}
	evBytes, _ := json.Marshal(ev)
	return PublishEventJSON(relayURL, secret, string(evBytes))
}

// JoinChannel requests to join a channel (kind 9021).
func JoinChannel(relayURL string, secret []byte, channelID string) error {
	tags := [][]string{{"h", channelID}}
	return publishEvent(relayURL, secret, 9021, tags, "")
}
