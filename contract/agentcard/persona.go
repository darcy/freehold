// Package agentcard builds and publishes kind-30175 agent persona cards — the
// public marketplace definitions Buzz clients browse and import. The body is
// an explicit allowlist of portable, public fields: no environment variables,
// secrets, machine paths, workspaces, or memories can cross.
package agentcard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"freehold/contract/agentsurface"
	"freehold/contract/relay"
	"freehold/contract/wire"
)

const (
	// PersonaKind is NIP-AP's kind-30175 persona definition.
	PersonaKind = 30175
	// MaxContentBytes is NIP-AP's content-body ceiling (65,535 bytes). The
	// import path's 5 MB cap applies to snapshot artifacts, not this event.
	MaxContentBytes = 65535
	// MaxEventBytes keeps the signed event inside the relay's 64 KB frame with
	// room for JSON/tag overhead.
	MaxEventBytes = 60 * 1024
)

// Persona is the public, portable definition projected onto a kind-30175 card.
// Field order is the NIP-AP wire order; optional fields omit when empty. The
// type physically cannot carry env vars, secrets, workspaces, or memories.
type Persona struct {
	DisplayName        string   `json:"display_name"`
	SystemPrompt       string   `json:"system_prompt,omitempty"`
	ACPCommand         string   `json:"acp_command,omitempty"`
	AvatarURL          string   `json:"avatar_url,omitempty"`
	Runtime            string   `json:"runtime,omitempty"`
	Model              string   `json:"model,omitempty"`
	Provider           string   `json:"provider,omitempty"`
	NamePool           []string `json:"name_pool,omitempty"`
	RespondTo          string   `json:"respond_to,omitempty"`
	RespondToAllowlist []string `json:"respond_to_allowlist,omitempty"`
	Parallelism        int      `json:"parallelism,omitempty"`
	Description        string   `json:"description,omitempty"`
	SessionPolicy      string   `json:"session_policy,omitempty"`
}

var (
	slugRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	portableACP = regexp.MustCompile(`^buzz-[A-Za-z0-9_-]+-acp$`)
	secretLike = []*regexp.Regexp{
		regexp.MustCompile(`nsec1[0-9a-z]{20,}`),
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	}
)

// ValidSlug enforces NIP-AP's d-tag grammar.
func ValidSlug(slug string) bool { return slugRe.MatchString(slug) }

// NormalizePersona trims the public fields and clamps the display-only
// description. It returns exactly what Validate/Publish will use.
func NormalizePersona(p Persona) Persona {
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	p.SystemPrompt = strings.TrimSpace(p.SystemPrompt)
	p.ACPCommand = strings.TrimSpace(p.ACPCommand)
	p.AvatarURL = strings.TrimSpace(p.AvatarURL)
	p.Runtime = strings.TrimSpace(p.Runtime)
	p.Model = strings.TrimSpace(p.Model)
	p.Provider = strings.TrimSpace(p.Provider)
	p.RespondTo = strings.TrimSpace(p.RespondTo)
	p.SessionPolicy = strings.TrimSpace(p.SessionPolicy)
	p.Description = agentsurface.ClampAbout(p.Description)
	for i := range p.NamePool {
		p.NamePool[i] = strings.TrimSpace(p.NamePool[i])
	}
	return p
}

// Validate checks the card against NIP-AP and the client catalog's sanitizers.
func Validate(p Persona) error {
	p = NormalizePersona(p)
	if p.DisplayName == "" {
		return fmt.Errorf("card display_name is required")
	}
	if len(p.DisplayName) > 100 || !agentsurface.VisibleText(p.DisplayName) {
		return fmt.Errorf("card display_name is not valid visible text")
	}
	if !agentsurface.VisibleText(p.SystemPrompt) {
		return fmt.Errorf("card system_prompt carries a control or formatting character")
	}
	if p.Description != "" && !agentsurface.VisibleText(p.Description) {
		return fmt.Errorf("card description carries a control or formatting character")
	}
	if p.ACPCommand != "" && p.ACPCommand != "buzz-acp" && !portableACP.MatchString(p.ACPCommand) {
		return fmt.Errorf("card acp_command must be buzz-acp or a portable buzz-*-acp alias")
	}
	if err := agentsurface.ValidatePicture(p.AvatarURL); err != nil {
		return fmt.Errorf("card avatar: %w", err)
	}
	if p.Runtime != "" && (len(p.Runtime) > 64 || !agentsurface.VisibleText(p.Runtime)) {
		return fmt.Errorf("card runtime is not valid visible text")
	}
	if p.Model != "" && (len(p.Model) > 128 || !agentsurface.VisibleText(p.Model)) {
		return fmt.Errorf("card model is not valid visible text")
	}
	if p.Provider != "" && (len(p.Provider) > 128 || !agentsurface.VisibleText(p.Provider)) {
		return fmt.Errorf("card provider is not valid visible text")
	}
	if len(p.NamePool) > 32 {
		return fmt.Errorf("card name_pool exceeds 32 entries")
	}
	for _, name := range p.NamePool {
		if name == "" || len(name) > 64 || !agentsurface.VisibleText(name) {
			return fmt.Errorf("card name_pool entry is not valid visible text")
		}
	}
	switch p.RespondTo {
	case "", "owner-only", "anyone":
	case "allowlist":
		// A public card must not carry a private access allowlist.
		return fmt.Errorf("card respond_to may not be allowlist")
	default:
		return fmt.Errorf("card respond_to must be owner-only, anyone, or empty")
	}
	if len(p.RespondToAllowlist) != 0 {
		return fmt.Errorf("card respond_to_allowlist must be empty")
	}
	if p.Parallelism != 0 && (p.Parallelism < 1 || p.Parallelism > 32) {
		return fmt.Errorf("card parallelism must be 1..32")
	}
	switch p.SessionPolicy {
	case "", "channel", "thread":
	default:
		return fmt.Errorf("card session_policy must be channel or thread")
	}
	for _, s := range []string{p.DisplayName, p.SystemPrompt, p.Description, p.Runtime, p.Model, p.Provider} {
		if credentialLike(s) {
			return fmt.Errorf("card carries a credential-like string; refusing to publish")
		}
	}
	for _, s := range p.NamePool {
		if credentialLike(s) {
			return fmt.Errorf("card carries a credential-like string; refusing to publish")
		}
	}
	body, err := Body(p)
	if err != nil {
		return err
	}
	if len(body) > MaxContentBytes {
		return fmt.Errorf("card content exceeds %d bytes", MaxContentBytes)
	}
	return nil
}

// Body serializes a normalized, validated persona body.
func Body(p Persona) ([]byte, error) {
	p = NormalizePersona(p)
	return json.Marshal(p)
}

// BuildEventJSON builds a signed kind-30175 event with exactly one d tag and
// the shared=true catalog tag. It validates before signing.
func BuildEventJSON(p Persona, slug string, ownerSecret []byte, ts int64) (string, error) {
	if !ValidSlug(slug) {
		return "", fmt.Errorf("card slug %q is not a NIP-AP slug", slug)
	}
	if err := Validate(p); err != nil {
		return "", err
	}
	body, err := Body(p)
	if err != nil {
		return "", err
	}
	tags := [][]string{{"d", slug}, {"shared", "true"}}
	pubkey, id, sig, err := wire.SignEvent(ownerSecret, PersonaKind, ts, tags, string(body))
	if err != nil {
		return "", fmt.Errorf("card signing: %w", err)
	}
	ev := map[string]interface{}{
		"id": id, "pubkey": pubkey, "created_at": ts, "kind": PersonaKind,
		"tags": tags, "content": string(body), "sig": sig,
	}
	evBytes, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}
	if len(evBytes) > MaxEventBytes {
		return "", fmt.Errorf("card event exceeds %d bytes", MaxEventBytes)
	}
	return string(evBytes), nil
}

// PublishPersona publishes a card signed by the owner. A caller that wants a
// monotonic created_at past the current head uses PublishPersonaAt.
func PublishPersona(dialURL, authURL string, ownerSecret []byte, p Persona, slug string) error {
	return PublishPersonaAt(dialURL, authURL, ownerSecret, p, slug, time.Now().Unix())
}

// PublishPersonaAt publishes a card at an explicit created_at.
func PublishPersonaAt(dialURL, authURL string, ownerSecret []byte, p Persona, slug string, ts int64) error {
	ev, err := BuildEventJSON(p, slug, ownerSecret, ts)
	if err != nil {
		return err
	}
	return relay.PublishEventJSONAuth(dialURL, authURL, ownerSecret, ev)
}

// QueryPersonaAuth reads the newest kind-30175 head for (owner, slug), signed
// by the owner. It verifies the event before returning its content/tags.
func QueryPersonaAuth(dialURL, authURL string, ownerSecret []byte, ownerPk, slug string) (content string, tags [][]string, createdAt int64, err error) {
	evs, err := relay.QueryEventsAuth(dialURL, authURL, ownerSecret, []interface{}{map[string]interface{}{
		"kinds":   []interface{}{PersonaKind},
		"authors": []interface{}{ownerPk},
		"#d":      []interface{}{slug},
		"limit":   1,
	}})
	if err != nil {
		return "", nil, 0, err
	}
	var newest map[string]interface{}
	for _, ev := range evs {
		author, _ := ev["pubkey"].(string)
		createdAt := intOf(ev["created_at"])
		tags := tagsOf(ev)
		if author != ownerPk || tagValue(tags, "d") != slug {
			continue
		}
		content, _ := ev["content"].(string)
		sig, _ := ev["sig"].(string)
		if _, err := wire.VerifyEvent(author, createdAt, PersonaKind, tags, content, sig); err != nil {
			continue
		}
		if newest == nil || createdAt > intOf(newest["created_at"]) {
			newest = ev
		}
	}
	if newest == nil {
		return "", nil, 0, nil
	}
	content, _ = newest["content"].(string)
	tags = tagsOf(newest)
	return content, tags, intOf(newest["created_at"]), nil
}

// PersonaMatches reports whether the current head already carries the desired
// body and shared tag. Extra body fields are preserved: a matching projection
// means there is nothing to republish.
func PersonaMatches(content string, tags [][]string, p Persona, slug string) bool {
	want := NormalizePersona(p)
	var got Persona
	if json.Unmarshal([]byte(content), &got) != nil {
		return false
	}
	got = NormalizePersona(got)
	if got.DisplayName != want.DisplayName ||
		got.SystemPrompt != want.SystemPrompt ||
		got.ACPCommand != want.ACPCommand ||
		got.AvatarURL != want.AvatarURL ||
		got.Runtime != want.Runtime ||
		got.Model != want.Model ||
		got.Provider != want.Provider ||
		!slices.Equal(got.NamePool, want.NamePool) ||
		got.RespondTo != want.RespondTo ||
		!slices.Equal(got.RespondToAllowlist, want.RespondToAllowlist) ||
		got.Parallelism != want.Parallelism ||
		got.Description != want.Description ||
		got.SessionPolicy != want.SessionPolicy {
		return false
	}
	var ds, shared int
	for _, t := range tags {
		if len(t) > 0 && t[0] == "d" {
			ds++
			if len(t) != 2 || t[1] != slug {
				return false
			}
		}
		if len(t) > 0 && t[0] == "shared" {
			shared++
			if len(t) != 2 || t[1] != "true" {
				return false
			}
		}
	}
	return ds == 1 && shared == 1
}

func credentialLike(s string) bool {
	for _, re := range secretLike {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func intOf(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func tagsOf(ev map[string]interface{}) [][]string {
	var out [][]string
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
			out = append(out, row)
		}
	case [][]string:
		out = append(out, raw...)
	}
	return out
}

func tagValue(tags [][]string, key string) string {
	for _, t := range tags {
		if len(t) > 1 && t[0] == key {
			return t[1]
		}
	}
	return ""
}
