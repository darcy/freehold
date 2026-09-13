// Profile handling: multi-tenant connection profiles. A profile is one
// logged-in tenant — its own config file (profiles/<name>/config.toml) and its
// own scoped state dir (<FREEHOLD_HOME|~/.freehold>/profiles/<name>). The
// filesystem is the registry: each subdir of ProfilesDir holding a config.toml
// is a profile.
//
// There is no implicit "default" profile and no legacy single-config layout:
// every tenant is a named profile under ProfilesDir. A box that pre-dates
// profiles is not auto-adopted — the operator re-runs `freehold login` to add a
// named profile for its world.
package config

import (
	"os"
	"path/filepath"
	"sort"
)

// Profile is one selected tenant's config + scoped state.
type Profile struct {
	Name       string // the dir name
	ConfigPath string
	StateDir   string
}

// current is the negotiated active profile. nil = none negotiated yet; the path
// helpers fall back to the freehold state/config roots so deep callers always
// get a usable path, but every world-touching command negotiates a profile
// before running (see negotiateProfile).
var current *Profile

// SetCurrent pins the active profile for the process.
func SetCurrent(p *Profile) { current = p }

// Current returns the negotiated profile (nil = none yet).
func Current() *Profile { return current }

// StateDir returns the effective state dir for the active profile, or the
// freehold state base when none is negotiated. The profile-aware replacement
// for freeholdHome()/freeholdStateDir()/identityHome().
func StateDir() string {
	if current != nil {
		return current.StateDir
	}
	return DefaultStateHome()
}

// ConfigPath returns the effective config path for the active profile, or the
// default config path when none is negotiated. The profile-aware replacement
// for DefaultPath()/defaultConfigPath()/defaultTuiConfigPath().
func ConfigPath() string {
	if current != nil {
		return current.ConfigPath
	}
	return DefaultPath()
}

// ProfilesDir is where named profiles live (~/.config/freehold/profiles). Always
// returns an absolute path — mirroring DefaultPath/DefaultStateHome, HOME falls
// back to /root when unset so the registry never scatters CWD-relative.
func ProfilesDir() string {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	if xdg != "" {
		return filepath.Join(xdg, "freehold", "profiles")
	}
	return filepath.Join(home, ".config", "freehold", "profiles")
}

// DefaultStateHome is the freehold state root that all profiles sit under
// (~/.freehold, FREEHOLD_HOME override): the parent of every profile's state.
func DefaultStateHome() string {
	if h := os.Getenv("FREEHOLD_HOME"); h != "" {
		return h
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return filepath.Join(home, ".freehold")
}

// List returns every registered profile, sorted by name.
func List() []*Profile {
	dirs, err := os.ReadDir(ProfilesDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(ProfilesDir(), d.Name(), "config.toml")); err == nil {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names)
	out := make([]*Profile, 0, len(names))
	for _, n := range names {
		out = append(out, &Profile{
			Name:       n,
			ConfigPath: filepath.Join(ProfilesDir(), n, "config.toml"),
			StateDir:   filepath.Join(DefaultStateHome(), "profiles", n),
		})
	}
	return out
}

// Resolve returns the named profile, or nil.
func Resolve(name string) *Profile {
	for _, p := range List() {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// NewProfilePath returns the config path a login would write for a new named
// profile (the dir is NOT created; callers create it on write).
func NewProfilePath(name string) string {
	return filepath.Join(ProfilesDir(), name, "config.toml")
}

// NewProfileState returns the state dir a login would use for a new named
// profile.
func NewProfileState(name string) string {
	return filepath.Join(DefaultStateHome(), "profiles", name)
}

// ValidProfileName reports whether name is safe as a profile dir name (letters,
// digits, dash, underscore; not blank).
func ValidProfileName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	// No leading/trailing dash or underscore — a clean filesystem dir slug.
	if name[0] == '-' || name[0] == '_' || name[len(name)-1] == '-' || name[len(name)-1] == '_' {
		return false
	}
	return true
}
