package cpbuild

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"freehold/agents"
	"freehold/contract/delegate"
	"freehold/contract/identity"
	"freehold/contract/relay"
	"freehold/contract/version"
)

// pulseMarkerPrefix is the #t tag carried by the CPA's release-pulse notes —
// fh-pulse-<key>, where key is the release tag (stable) or commit sha (dev).
// The dedupe guard reads recent notes and scans tags CLIENT-SIDE: a #t-filtered
// query is unreliable for existence checks (the relay applies its SQL limit
// before post-filtering tag constraints — the same trap the welcome guard
// documents). ponytail: 50 notes is the scan window — only pulses are kind:1
// from the CPA, and a slower cadence degrades to one duplicate post.
const pulseMarkerPrefix = "fh-pulse-"

// ghRelease is the slice of a GitHub release the pulse post renders.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	URL     string `json:"html_url"`
}

// stageReleasePulse posts the world's version pulse to Pulse as the CPA
// (@freehold): a stable build posts its OWN release's notes + link (what the
// world now runs — never GitHub's "latest", which would announce a release
// this world has not even updated to), a dev build a short "Version updated
// to <sha>" note. Deduped by marker tag, so repeated same-version builds stay
// quiet. Best-effort: the stage warns and never fails the build — the next
// build retries.
func (s *Spec) stageReleasePulse(report []string) []string {
	if s.RelayURL == "" || s.CpaName == "" {
		return report
	}
	id, err := identity.Load(filepath.Join(s.agentIdentityDir(), "agents", sanitizeDir(s.CpaName)))
	if err != nil {
		return append(report, "WARN: release pulse: the CPA identity is unreadable: "+err.Error())
	}
	nSec, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil {
		return append(report, "WARN: release pulse: "+err.Error())
	}
	pub, err := id.NostrPubkeyHex()
	if err != nil {
		return append(report, "WARN: release pulse: "+err.Error())
	}
	authURL := s.RelayAuthURL
	if authURL == "" {
		authURL = s.RelayURL
	}

	var key, content string
	if version.Channel(version.Current()) == "stable" {
		tag := version.Current()
		rel, err := s.fetchRelease(tag)
		if err != nil {
			return append(report, "WARN: release pulse: "+err.Error())
		}
		key = tag
		content = "freehold " + tag
		if rel.Name != "" && rel.Name != tag {
			content += " — " + rel.Name
		}
		if body := pulseNotes(rel.Body); body != "" {
			content += "\n\n" + body
		}
		content += "\n\n" + rel.URL
	} else {
		key = version.Commit
		if key == "" || key == "unknown" {
			key = "unknown"
			content = "Version updated to dev"
		} else {
			content = "Version updated to " + key + "\n" + s.repoBase() + "/commit/" + key
		}
	}

	if err := s.postPulseOnce(nSec, pub, authURL, key, content); err != nil {
		return append(report, "WARN: release pulse: "+err.Error())
	}
	return append(report, "release pulse posted ("+key+")")
}

// pulseNotes strips the release body down to what reads well in a Pulse note:
// the high-level bullets. The release's trailing `## Test status` table (and
// anything after it) is release-testing bookkeeping, not reading material —
// the release page renders it; the pulse doesn't.
func pulseNotes(body string) string {
	cut := strings.SplitN("\n"+body, "\n## Test status", 2)[0]
	cut = strings.TrimPrefix(strings.TrimRight(cut, "\n \t"), "\n") // drop the split sentinel
	if len(cut) > 8000 {
		cut = cut[:8000] + "…"
	}
	return cut
}

// postPulseOnce publishes the pulse note unless a recent note already carries
// this key's marker. The guard reads the CPA's kind:1 notes and scans tags
// client-side (see pulseMarkerPrefix); the relay DB is durable across CP
// rebuilds, so the guard needs no CP-side state.
func (s *Spec) postPulseOnce(nSec []byte, pub, authURL, key, content string) error {
	evs, err := relay.QueryEventsAuth(s.relayDial(), authURL, nSec, []interface{}{map[string]interface{}{
		"kinds":   []interface{}{1},
		"authors": []interface{}{pub},
		"limit":   50,
	}})
	if err != nil {
		return fmt.Errorf("relay read: %w", err)
	}
	marker := pulseMarkerPrefix + key
	for _, ev := range evs {
		for _, tag := range eventTags(ev) {
			if len(tag) >= 2 && tag[0] == "t" && tag[1] == marker {
				return nil
			}
		}
	}
	return delegate.PostNoteAuth(s.relayDial(), authURL, nSec, [][]string{{"t", marker}}, content)
}

// eventTags pulls the tags array out of a relay event map, tolerating
// malformation (the relay returns raw JSON shapes).
func eventTags(ev map[string]interface{}) [][]string {
	raw, _ := ev["tags"].([]interface{})
	out := make([][]string, 0, len(raw))
	for _, r := range raw {
		row, _ := r.([]interface{})
		t := make([]string, 0, len(row))
		for _, v := range row {
			if str, ok := v.(string); ok {
				t = append(t, str)
			}
		}
		out = append(out, t)
	}
	return out
}

// fetchRelease fetches the GitHub release for the RUNNING version tag.
// Injectable for tests (Spec.latestRelease).
func (s *Spec) fetchRelease(tag string) (*ghRelease, error) {
	if s.latestRelease != nil {
		return s.latestRelease(tag)
	}
	url := "https://api.github.com/repos/" + s.ghRepo() + "/releases/tags/" + tag
	cli := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github release %s: %w", tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github release %s: HTTP %d", tag, resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("github release %s: %w", tag, err)
	}
	return &rel, nil
}

// ghRepo returns the "owner/repo" the world's source lives at — the release
// notes and the dev commit link point there. RepoURL is the override (a fork
// running its own releases); the agents package's upstream default otherwise.
func (s *Spec) ghRepo() string {
	for _, u := range []string{s.RepoURL, agents.UpstreamRepoURL} {
		u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
		if rest, ok := strings.CutPrefix(u, "https://github.com/"); ok && rest != "" {
			return rest
		}
	}
	return "darcy/freehold"
}

// repoBase is the repo's web base for commit links.
func (s *Spec) repoBase() string {
	return "https://github.com/" + s.ghRepo()
}
