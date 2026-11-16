package agentsurface

import (
	"strings"
	"testing"
)

func good() Profile {
	return Profile{
		Name:        "freehold",
		DisplayName: "freehold",
		About:       "the control plane agent — freehold's main reasoning touchpoint",
		Picture:     "https://cp.example/avatars/abc.png",
		NIP05:       "freehold@cp.example",
		IsAgent:     true,
	}
}

func TestNormalizeProfileAcceptsGood(t *testing.T) {
	got, err := NormalizeProfile(good())
	if err != nil {
		t.Fatalf("good profile rejected: %v", err)
	}
	if got.Name != "freehold" || got.Picture != "https://cp.example/avatars/abc.png" {
		t.Fatalf("normalization changed values: %+v", got)
	}
}

func TestNormalizeProfileRejectsUnsafeFields(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Profile)
		want string
	}{
		{"empty name", func(p *Profile) { p.Name, p.DisplayName = "", "" }, "non-empty"},
		{"name drift", func(p *Profile) { p.DisplayName = "other" }, "in sync"},
		{"zero-width name", func(p *Profile) { p.Name = "free\u200Bhold"; p.DisplayName = p.Name }, "control or formatting"},
		{"bidi about", func(p *Profile) { p.About = "about \u202E" }, "control or formatting"},
		{"too long name", func(p *Profile) { p.Name = strings.Repeat("a", maxNameBytes+1); p.DisplayName = p.Name }, "exceeds"},
		{"too long about", func(p *Profile) { p.About = strings.Repeat("a", maxAboutBytes+1) }, "exceeds"},
		{"bare https picture", func(p *Profile) { p.Picture = "https://" }, "absolute HTTPS"},
		{"http picture", func(p *Profile) { p.Picture = "http://cp.example/a.png" }, "absolute HTTPS"},
		{"userinfo", func(p *Profile) { p.Picture = "https://u:p@cp.example/a.png" }, "credentials"},
		{"query", func(p *Profile) { p.Picture = "https://cp.example/a.png?x=1" }, "query"},
		{"parentheses", func(p *Profile) { p.Picture = "https://cp.example/a(1).png" }, "parentheses"},
		{"nip05 whitespace", func(p *Profile) { p.NIP05 = "free hold@cp.example" }, "nip05"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := good()
			tc.edit(&p)
			if _, err := NormalizeProfile(p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestProfileMatchesContent(t *testing.T) {
	p := good()
	body, err := ProfileBody(p)
	if err != nil {
		t.Fatal(err)
	}
	if !ProfileMatchesContent(string(body), p) {
		t.Fatal("published body must match its own projection")
	}
	// Extra fields are preserved, not a reason to republish.
	if !ProfileMatchesContent(`{"name":"freehold","display_name":"freehold","about":"the control plane agent — freehold's main reasoning touchpoint","picture":"https://cp.example/avatars/abc.png","nip05":"freehold@cp.example","is_agent":true,"lud16":"x@y"}`, p) {
		t.Fatal("extra client fields must not force a republish")
	}
	p.About = "changed"
	if ProfileMatchesContent(string(body), p) {
		t.Fatal("a changed owned field must be a real difference")
	}
	if ProfileMatchesContent("not json", p) {
		t.Fatal("malformed content must not match")
	}
}

func TestClampAbout(t *testing.T) {
	if got := ClampAbout("  short  "); got != "short" {
		t.Fatalf("short about = %q, want trimmed", got)
	}
	long := strings.Repeat("é", maxAboutBytes) // two bytes per rune
	got := ClampAbout(long)
	if len(got) > maxAboutBytes {
		t.Fatalf("clamped about is %d bytes, over the %d limit", len(got), maxAboutBytes)
	}
	if !strings.HasPrefix(got, "é") || strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("clamp cut a rune: %q", got)
	}
	if _, err := NormalizeProfile(Profile{Name: "a", DisplayName: "a", About: got}); err != nil {
		t.Fatalf("clamped about must be a valid profile body: %v", err)
	}
}
