package cpbuild

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/version"
	"freehold/control-plane/api/agent"
)

// pulseSpecFor builds a Spec whose relay dial hits the fake and whose CPA
// identity exists in a temp dir (the pulse signs as the CPA).
func pulseSpecFor(t *testing.T, f *fakeFirstRunRelay) *Spec {
	t.Helper()
	dir := t.TempDir()
	if _, err := agent.EnsureIdentity(filepath.Join(dir, "agents", "freehold")); err != nil {
		t.Fatal(err)
	}
	return &Spec{
		RelayURL:         f.srv.URL,
		RelayAuthURL:     f.srv.URL,
		RelayHost:        f.srv.Listener.Addr().String(), // host:port => dial hits the fake
		CpaName:          "freehold",
		AgentIdentityDir: dir,
	}
}

// stampVersion overrides the build-time version identity for a test and
// restores it on cleanup.
func stampVersion(t *testing.T, v, commit string) {
	t.Helper()
	oldV, oldC := version.Version, version.Commit
	version.Version, version.Commit = v, commit
	t.Cleanup(func() { version.Version, version.Commit = oldV, oldC })
}

func publishedNote(t *testing.T, raw string) (kind int, content string, tags [][]string) {
	t.Helper()
	var ev struct {
		Kind    int        `json:"kind"`
		Content string     `json:"content"`
		Tags    [][]string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	return ev.Kind, ev.Content, ev.Tags
}

func hasTag(tags [][]string, k, v string) bool {
	for _, tag := range tags {
		if len(tag) >= 2 && tag[0] == k && tag[1] == v {
			return true
		}
	}
	return false
}

// TestStageReleasePulseStable: a stable build posts its OWN release's notes
// and link as a kind:1 note tagged fh-pulse-<tag> — never GitHub's "latest".
func TestStageReleasePulseStable(t *testing.T) {
	stampVersion(t, "v0.8.1", "abc123")
	f := newFakeFirstRunRelay(t, nil)
	s := pulseSpecFor(t, f)
	s.latestRelease = func(tag string) (*ghRelease, error) {
		if tag != "v0.8.1" {
			t.Errorf("fetch must target the RUNNING version, got %q", tag)
		}
		return &ghRelease{TagName: "v0.8.1", Name: "The Data Release", Body: "what's new", URL: "https://github.com/darcy/freehold/releases/tag/v0.8.1"}, nil
	}

	report := s.stageReleasePulse(nil)
	pub := f.published()
	if len(pub) != 1 {
		t.Fatalf("a fresh world must post exactly one pulse, got %d", len(pub))
	}
	kind, content, tags := publishedNote(t, pub[0])
	if kind != 1 {
		t.Fatalf("kind = %d, want 1 (NIP-01 note — the Pulse surface)", kind)
	}
	if !hasTag(tags, "t", "fh-pulse-v0.8.1") {
		t.Fatalf("the note must carry the dedupe marker, got %v", tags)
	}
	for _, want := range []string{"freehold v0.8.1", "The Data Release", "what's new", "releases/tag/v0.8.1"} {
		if !strings.Contains(content, want) {
			t.Fatalf("content must carry %q, got %q", want, content)
		}
	}
	if !strings.Contains(strings.Join(report, "\n"), "release pulse posted (v0.8.1)") {
		t.Fatalf("report must name the post, got %v", report)
	}

	// Same release again: the marker in history suppresses the re-post.
	report = s.stageReleasePulse(report)
	if got := f.published(); len(got) != 1 {
		t.Fatalf("a posted release must not re-post, got %d events", len(got))
	}
	if !strings.Contains(strings.Join(report, "\n"), "release pulse posted (v0.8.1)") {
		t.Fatalf("the dedupe is still a successful stage, got %v", report)
	}

	// A NEW release on the relay's history: posts again.
	f2 := newFakeFirstRunRelay(t, []map[string]interface{}{
		{"id": "old", "kind": float64(1), "tags": []interface{}{[]interface{}{"t", "fh-pulse-v0.8.0"}}},
	})
	s2 := pulseSpecFor(t, f2)
	s2.latestRelease = s.latestRelease
	if report := s2.stageReleasePulse(nil); len(f2.published()) != 1 {
		t.Fatalf("a new release must post, report %v", report)
	}
}

// TestStageReleasePulseDev: a dev build posts the short sha note with the
// commit link; an unstamped build falls back to "Version updated to dev".
func TestStageReleasePulseDev(t *testing.T) {
	stampVersion(t, "main-g53hsu21", "53hsu21def01")
	f := newFakeFirstRunRelay(t, nil)
	s := pulseSpecFor(t, f)
	s.latestRelease = func(tag string) (*ghRelease, error) {
		t.Errorf("a dev build must not fetch a release, got %q", tag)
		return nil, nil
	}

	report := s.stageReleasePulse(nil)
	pub := f.published()
	if len(pub) != 1 {
		t.Fatalf("a dev build must post exactly one pulse, got %d", len(pub))
	}
	kind, content, tags := publishedNote(t, pub[0])
	if kind != 1 {
		t.Fatalf("kind = %d, want 1", kind)
	}
	if !hasTag(tags, "t", "fh-pulse-53hsu21def01") {
		t.Fatalf("the note must carry the dedupe marker, got %v", tags)
	}
	if !strings.Contains(content, "Version updated to 53hsu21def01") {
		t.Fatalf("content must name the sha, got %q", content)
	}
	if !strings.Contains(content, "/commit/53hsu21def01") {
		t.Fatalf("content must link the commit, got %q", content)
	}
	if !strings.Contains(strings.Join(report, "\n"), "release pulse posted (53hsu21def01)") {
		t.Fatalf("report must name the post, got %v", report)
	}

	// Same sha again: quiet.
	if r := s.stageReleasePulse(nil); len(f.published()) != 1 {
		t.Fatalf("a posted sha must not re-post, report %v", r)
	}

	// An unstamped build: honest short note, no link.
	stampVersion(t, "dev", "unknown")
	f3 := newFakeFirstRunRelay(t, nil)
	s3 := pulseSpecFor(t, f3)
	report = s3.stageReleasePulse(nil)
	_, content, _ = publishedNote(t, f3.published()[0])
	if content != "Version updated to dev" {
		t.Fatalf("an unstamped build must post the fallback note, got %q", content)
	}
	if strings.Contains(content, "/commit/") || strings.Contains(strings.Join(report, "\n"), "unknown-unknown") {
		t.Fatalf("the fallback must not link a commit, got %q / %v", content, report)
	}
}

// TestStageReleasePulseBestEffort: a failed release fetch (GitHub down, an
// unknown tag) warns and publishes nothing — the build stays green.
func TestStageReleasePulseBestEffort(t *testing.T) {
	stampVersion(t, "v0.8.1", "abc123")
	f := newFakeFirstRunRelay(t, nil)
	s := pulseSpecFor(t, f)
	s.latestRelease = func(tag string) (*ghRelease, error) {
		return nil, &fakeFetchError{"github release " + tag + ": HTTP 404"}
	}

	report := strings.Join(s.stageReleasePulse(nil), "\n")
	if got := f.published(); len(got) != 0 {
		t.Fatalf("a fetch failure must not publish, got %d events", len(got))
	}
	if !strings.Contains(report, "WARN: release pulse:") {
		t.Fatalf("a fetch failure must WARN, got %v", report)
	}
}

// TestStageReleasePulseSkipsSilently: no relay coords — no pulse, no warn
// (the stage has nothing to post to).
func TestStageReleasePulseSkipsSilently(t *testing.T) {
	stampVersion(t, "v0.8.1", "abc123")
	s := &Spec{CpaName: "freehold"}
	if report := s.stageReleasePulse(nil); len(report) != 0 {
		t.Fatalf("no relay coords must skip silently, got %v", report)
	}
}

type fakeFetchError struct{ msg string }

func (e *fakeFetchError) Error() string { return e.msg }
