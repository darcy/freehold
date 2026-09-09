package agenttools

import (
	"encoding/json"
	"strconv"

	"freehold/contract/relay"
	"freehold/contract/wire"
)

// QueryRoster reads the server's live whitelist — the relay's signed 39002
// roster for this server's own private channel, read fresh per call, fail
// closed on relay error. Members = the `p` tags of the newest valid 39002.
//
// The trust anchor is the ROSTER'S OWN SIGNATURE, not a pre-pinned relay
// pubkey: the relay is the sole publisher of 39002 for a channel (it refuses
// client-authored rosters and re-signs/authorizes its own), so a roster that is
// present at the relay and verifies locally IS the relay's roster. When the
// caller knows the relay's roster-signing key it passes it as relayPubkey and we
// additionally require the author to match; empty means self-consistent only.
// Any query/parse error yields an empty roster (deny all).
func QueryRoster(relayURL, relayPubkey, ownPubkey string, secret []byte) ([]string, error) {
	return QueryRosterAuth(relayURL, relayURL, relayPubkey, ownPubkey, secret)
}

// QueryRosterAuth is QueryRoster with a separate NIP-98 auth URL (the
// pre-Caddy LAN-dial case in agent-tools).
func QueryRosterAuth(dialURL, authURL, relayPubkey, ownPubkey string, secret []byte) ([]string, error) {
	cid := relay.RunnerChannelID(ownPubkey)
	filters := []interface{}{map[string]interface{}{
		"kinds": []interface{}{wire.GroupMembers},
		"#d":    []interface{}{cid},
		"limit": 100,
	}}
	events, err := relay.QueryEventsAuth(dialURL, authURL, secret, filters)
	if err != nil {
		return nil, err
	}
	newest := int64(-1)
	var members []string
	pinNewest := int64(-1)
	var pinMembers []string
	for _, ev := range events {
		author, _ := ev["pubkey"].(string)
		createdAt := intOr(ev["created_at"])
		tags := tagsOf(ev)
		if tagValue(tags, "d") != cid {
			continue
		}
		content, _ := ev["content"].(string)
		sig, _ := ev["sig"].(string)
		if _, err := wire.VerifyEvent(author, createdAt, wire.GroupMembers, tags, content, sig); err != nil {
			continue
		}
		var m []string
		for _, t := range tags {
			if len(t) > 1 && t[0] == "p" {
				m = append(m, t[1])
			}
		}
		if createdAt >= newest {
			newest = createdAt
			members = m
		}
		// A pin match, when supplied, is authoritative over the loose newest.
		if relayPubkey != "" && author == relayPubkey && createdAt >= pinNewest {
			pinNewest = createdAt
			pinMembers = m
		}
	}
	if pinNewest >= 0 {
		members = pinMembers
	}
	return dedupeSorted(members), nil
}

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, m := range in {
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
	return out
}

func tagsOf(ev map[string]interface{}) [][]string {
	var tags [][]string
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
		tags = append(tags, raw...)
	}
	return tags
}

func tagValue(tags [][]string, key string) string {
	for _, t := range tags {
		if len(t) > 1 && t[0] == key {
			return t[1]
		}
	}
	return ""
}

func intOr(v interface{}) int64 {
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
