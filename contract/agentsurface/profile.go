// Package agentsurface holds the public-shape rules for an agent's Buzz
// surfaces: its kind-0 profile (and, later, marketplace cards). These are the
// fields a client renders, so the CP validates them before publishing — a
// malformed publish silently degrades to initials or an empty name, which is
// worse than failing the build loudly.
package agentsurface

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxNameBytes    = 100
	maxAboutBytes   = 280
	maxNIP05Bytes   = 255
	maxPictureBytes = 2048
)

// Profile is the kind-0 body freehold publishes for a managed agent. Field
// names match Buzz's profile readers: display_name is the rendered name, name
// is kept in sync, about is the registry purpose, picture is a hosted HTTPS
// avatar, nip05 is a self-declared handle, and is_agent marks the row as an
// agent. Buzz derives ownership from the profile's auth tag, not from
// is_agent; the field is still emitted for clients that read it.
type Profile struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	About       string `json:"about,omitempty"`
	Picture     string `json:"picture,omitempty"`
	NIP05       string `json:"nip05,omitempty"`
	IsAgent     bool   `json:"is_agent"`
}

// NormalizeProfile trims the body and validates every field the client will
// render. It returns the normalized profile so callers publish exactly what
// was validated (never a second, differently-trimmed copy).
func NormalizeProfile(p Profile) (Profile, error) {
	p.Name = strings.TrimSpace(p.Name)
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	p.About = strings.TrimSpace(p.About)
	p.Picture = strings.TrimSpace(p.Picture)
	p.NIP05 = strings.TrimSpace(p.NIP05)
	if p.Name == "" || p.DisplayName == "" {
		return Profile{}, fmt.Errorf("profile name/display_name must be non-empty")
	}
	if p.Name != p.DisplayName {
		return Profile{}, fmt.Errorf("profile name and display_name must stay in sync (%q != %q)", p.Name, p.DisplayName)
	}
	if !visibleText(p.Name) {
		return Profile{}, fmt.Errorf("profile name carries a control or formatting character")
	}
	if !visibleText(p.About) {
		return Profile{}, fmt.Errorf("profile about carries a control or formatting character")
	}
	if len(p.Name) > maxNameBytes {
		return Profile{}, fmt.Errorf("profile name exceeds %d bytes", maxNameBytes)
	}
	if len(p.About) > maxAboutBytes {
		return Profile{}, fmt.Errorf("profile about exceeds %d bytes", maxAboutBytes)
	}
	if p.NIP05 != "" {
		if len(p.NIP05) > maxNIP05Bytes || strings.ContainsAny(p.NIP05, " \t\r\n") || !visibleText(p.NIP05) {
			return Profile{}, fmt.Errorf("profile nip05 is not a visible, whitespace-free handle")
		}
	}
	if p.Picture != "" {
		if err := ValidatePicture(p.Picture); err != nil {
			return Profile{}, err
		}
	}
	return p, nil
}

// ClampAbout bounds a display-only about string to the profile limit. It is
// used for values freehold does not author (the registry purpose): a long
// purpose must degrade the display, never fail the whole build.
func ClampAbout(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxAboutBytes {
		return s
	}
	b := []byte(s[:maxAboutBytes])
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return strings.TrimSpace(string(b))
}

// ValidatePicture enforces the hosted-avatar rule: an absolute HTTPS URL with
// no credentials, query, or fragment, no whitespace/parentheses (Buzz's
// catalog sanitizer rejects them), and within the client's length bound.
func ValidatePicture(raw string) error {
	if raw == "" {
		return fmt.Errorf("profile picture is required")
	}
	if len(raw) > maxPictureBytes {
		return fmt.Errorf("profile picture exceeds %d bytes", maxPictureBytes)
	}
	if strings.ContainsAny(raw, " \t\r\n()") {
		return fmt.Errorf("profile picture carries whitespace or parentheses")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("profile picture is not a URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.Path == "" {
		return fmt.Errorf("profile picture must be an absolute HTTPS URL with a path")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("profile picture must not carry credentials, query, or fragment")
	}
	return nil
}

// ProfileBody serializes a normalized profile. The caller publishes these
// exact bytes.
func ProfileBody(p Profile) ([]byte, error) {
	p, err := NormalizeProfile(p)
	if err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// ProfileMatchesContent reports whether the current kind-0 content already
// carries the fields freehold would publish. Extra fields are preserved: a
// matching projection means there is nothing to republish, so a client-side
// edit to a field freehold does not own is never clobbered by reconcile.
func ProfileMatchesContent(content string, want Profile) bool {
	want, err := NormalizeProfile(want)
	if err != nil {
		return false
	}
	var got struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		About       string `json:"about"`
		Picture     string `json:"picture"`
		NIP05       string `json:"nip05"`
		IsAgent     bool   `json:"is_agent"`
	}
	if json.Unmarshal([]byte(content), &got) != nil {
		return false
	}
	return got.Name == want.Name &&
		got.DisplayName == want.DisplayName &&
		got.About == want.About &&
		got.Picture == want.Picture &&
		got.NIP05 == want.NIP05 &&
		got.IsAgent == want.IsAgent
}

// visibleText rejects control and Unicode format characters (the invisible
// characters Buzz's sanitizers refuse: bidi overrides, zero-width spaces,
// BOMs). Newlines/tabs are allowed in prose fields; names are trimmed and
// checked separately.
func visibleText(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' || r == '\r' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
