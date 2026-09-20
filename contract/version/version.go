// Package version is the shared build-time version identity. The build stamps
// it (justfile + CI) with:
//
//	-ldflags "-X freehold/contract/version.Version=<git describe> \
//	          -X freehold/contract/version.Commit=<sha>"
//
// One source of truth for `freehold --version`, `freehold-console version`,
// the runner's MCP handshake, and the CP's reported pin. An unstamped plain
// `go build` reports "dev"/"unknown" rather than a lie.
package version

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Version is the git-describe string: vX.Y.Z | vX.Y.Z-rc.N | main-gabc123 |
// vX.Y.Z-4-gabc123-dirty. Commit is the short (or full) HEAD sha.
var (
	Version = "dev"
	Commit  = "unknown"
)

// Channel classifies a version string as stable | rc | dev:
//   - vX.Y.Z              → stable
//   - vX.Y.Z-rc.N         → rc
//   - everything else     → dev (untagged describe strings, dirty trees, "dev")
func Channel(v string) string {
	core := strings.TrimSuffix(v, "-dirty")
	if !strings.HasPrefix(core, "v") {
		return "dev"
	}
	if strings.Contains(core, "-rc.") {
		return "rc"
	}
	if strings.Contains(core, "-") {
		return "dev"
	}
	return "stable"
}

// Current returns the embedded Version, for convenience at call sites that do
// not take a version argument.
func Current() string { return Version }

// FileName is the world's version stamp under the CP state dir. Install and
// update write it over the deploy transport; the console reads it and reports
// it. build/teardown never write it.
const FileName = "version.json"

// Pin is the stamped world version identity.
type Pin struct {
	Version string `json:"version"`
	Channel string `json:"channel"`
	Commit  string `json:"commit,omitempty"`
}

// Read loads a pin from path. A missing file is not an error: it returns the
// zero Pin, so a CP deployed before stamping still serves.
func Read(path string) (Pin, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Pin{}, nil
		}
		return Pin{}, err
	}
	var p Pin
	if err := json.Unmarshal(raw, &p); err != nil {
		return Pin{}, err
	}
	return p, nil
}

// Write persists a pin at path (file 0600, parent 0700, atomic rename).
func Write(path string, p Pin) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
