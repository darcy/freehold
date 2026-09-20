// Package artifact acquires a version's binaries + migration scripts for
// `freehold update`:
//
//   - release assets for a channel (stable/rc): download + sha256-verify the
//     GitHub Release assets, extract migrations.tar.gz;
//   - a sandbox clone+build for an untagged ref/sha (the box has the toolchain);
//   - the local tree for --dev.
//
// All downloads/builds live under the caller's cache dir, never target/.
package artifact

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DefaultRepo is the GitHub owner/name release assets come from; override with
// FREEHOLD_REPO.
const DefaultRepo = "darcy/freehold"

// Repo returns the GitHub slug release assets come from.
func Repo() string {
	if r := os.Getenv("FREEHOLD_REPO"); r != "" {
		return r
	}
	return DefaultRepo
}

// Set is the acquired artifact paths + its version identity.
type Set struct {
	Console       string // freehold-console (release build)
	Runner        string // runner (release build)
	AgentTools    string // freehold-agent-tools
	Freehold      string // the freehold CLI
	MigrationsDir string
	Version       string
	Channel       string
	Commit        string
}

var (
	stableRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	rcRe     = regexp.MustCompile(`^v\d+\.\d+\.\d+-rc\.\d+$`)
)

// SelectTag picks the newest tag for a channel: the highest semver stable tag
// for "stable", the highest vX.Y.Z-rc.N for "rc". ok=false when none match.
func SelectTag(channel string, tags []string) (string, bool) {
	var candidates []string
	for _, t := range tags {
		switch channel {
		case "stable":
			if stableRe.MatchString(t) {
				candidates = append(candidates, t)
			}
		case "rc":
			if rcRe.MatchString(t) {
				candidates = append(candidates, t)
			}
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	sort.Slice(candidates, func(i, j int) bool { return semverLess(candidates[i], candidates[j]) })
	return candidates[len(candidates)-1], true
}

// semverLess compares two vX.Y.Z(-rc.N) tags, returning a<b.
func semverLess(a, b string) bool {
	am, an, ap, ar := parseSemver(a)
	bm, bn, bp, br := parseSemver(b)
	if am != bm {
		return am < bm
	}
	if an != bn {
		return an < bn
	}
	if ap != bp {
		return ap < bp
	}
	return ar < br
}

func parseSemver(t string) (maj, min, pat, rc int) {
	t = strings.TrimPrefix(t, "v")
	rc = 1 << 30 // a release outranks any rc
	if i := strings.Index(t, "-rc."); i >= 0 {
		rc, _ = strconv.Atoi(t[i+4:])
		t = t[:i]
	}
	parts := strings.Split(t, ".")
	if len(parts) == 3 {
		maj, _ = strconv.Atoi(parts[0])
		min, _ = strconv.Atoi(parts[1])
		pat, _ = strconv.Atoi(parts[2])
	}
	return
}

// TagInfo is one GitHub tag.
type TagInfo struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// ListTags fetches the repo's tags (the GitHub API returns newest first).
func ListTags(ctx context.Context) ([]TagInfo, error) {
	url := "https://api.github.com/repos/" + Repo() + "/tags?per_page=100"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list tags: HTTP %s", resp.Status)
	}
	var tags []TagInfo
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// assets is the release asset set.
var assets = []string{"freehold", "freehold-console", "runner", "freehold-agent-tools", "migrations.tar.gz", "checksums.txt"}

// AcquireRelease resolves the channel's newest tag, downloads + verifies its
// assets into dest, and extracts migrations.tar.gz. dest is the profile cache.
func AcquireRelease(ctx context.Context, channel, dest string) (Set, error) {
	tags, err := ListTags(ctx)
	if err != nil {
		return Set{}, err
	}
	names := make([]string, len(tags))
	shaByName := map[string]string{}
	for i, t := range tags {
		names[i] = t.Name
		shaByName[t.Name] = t.Commit.SHA
	}
	tag, ok := SelectTag(channel, names)
	if !ok {
		return Set{}, fmt.Errorf("no %s release tag found in %s", channel, Repo())
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return Set{}, err
	}
	for _, a := range assets {
		if err := download(ctx, tag, a, filepath.Join(dest, a)); err != nil {
			return Set{}, err
		}
	}
	if err := VerifyChecksums(dest); err != nil {
		return Set{}, err
	}
	migDir := filepath.Join(dest, "migrations")
	if err := os.RemoveAll(migDir); err != nil {
		return Set{}, err
	}
	if err := extractTarGz(filepath.Join(dest, "migrations.tar.gz"), migDir); err != nil {
		return Set{}, fmt.Errorf("extract migrations.tar.gz: %w", err)
	}
	return Set{
		Console:       filepath.Join(dest, "freehold-console"),
		Runner:        filepath.Join(dest, "runner"),
		AgentTools:    filepath.Join(dest, "freehold-agent-tools"),
		Freehold:      filepath.Join(dest, "freehold"),
		MigrationsDir: migDir,
		Version:       tag,
		Channel:       channel,
		Commit:        shaByName[tag],
	}, nil
}

// download fetches one release asset over HTTPS (public repo).
func download(ctx context.Context, tag, asset, dst string) error {
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", Repo(), tag, asset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s@%s: HTTP %s", asset, tag, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

// VerifyChecksums checks every entry in <dir>/checksums.txt against the file's
// sha256, failing on any mismatch or missing file.
func VerifyChecksums(dir string) error {
	f, err := os.Open(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return fmt.Errorf("malformed checksums line: %q", line)
		}
		want, name := fields[0], strings.TrimPrefix(fields[1], "*")
		sum, err := sha256File(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if sum != want {
			return fmt.Errorf("checksum mismatch for %s: got %s want %s", name, sum, want)
		}
	}
	return sc.Err()
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTarGz unpacks a .tar.gz into dir.
func extractTarGz(src, dir string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		out := filepath.Join(dir, filepath.Base(hdr.Name))
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, tr); err != nil {
			w.Close()
			return err
		}
		w.Close()
	}
}

// BuildTree returns the Set for a built tree: the local tree (ref/sha empty) or
// a sandbox clone of ref/sha under dest. It runs `just build` in the tree.
func BuildTree(ctx context.Context, ref, sha, localDir, dest string) (Set, error) {
	tree := localDir
	if ref != "" || sha != "" {
		tree = filepath.Join(dest, "src")
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return Set{}, err
		}
		if _, err := os.Stat(filepath.Join(tree, ".git")); err != nil {
			if out, err := run(ctx, "", "git", "clone", "--filter=blob:none", "https://github.com/"+Repo()+".git", tree); err != nil {
				return Set{}, fmt.Errorf("clone: %w: %s", err, out)
			}
		}
		target := sha
		if target == "" {
			target = ref
		}
		if out, err := run(ctx, tree, "git", "fetch", "--quiet", "origin", target); err != nil {
			return Set{}, fmt.Errorf("fetch %s: %w: %s", target, err, out)
		}
		if out, err := run(ctx, tree, "git", "checkout", "--quiet", "FETCH_HEAD"); err != nil {
			return Set{}, fmt.Errorf("checkout %s: %w: %s", target, err, out)
		}
	}
	if out, err := run(ctx, tree, "just", "build"); err != nil {
		return Set{}, fmt.Errorf("just build: %w: %s", err, out)
	}
	rel := func(p string) string { return filepath.Join(tree, p) }
	version, _ := run(ctx, tree, "git", "describe", "--tags", "--always", "--dirty")
	commit, _ := run(ctx, tree, "git", "rev-parse", "--short=12", "HEAD")
	return Set{
		Console:       rel("target/release/freehold-console"),
		Runner:        rel("target/release/runner"),
		AgentTools:    rel("target/release/freehold-agent-tools"),
		Freehold:      rel("target/debug/freehold"),
		MigrationsDir: rel("migrations"),
		Version:       strings.TrimSpace(version),
		Channel:       "dev",
		Commit:        strings.TrimSpace(commit),
	}, nil
}

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
