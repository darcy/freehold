// The apps registry: the agent-exposed services' durable records (apps.json
// under the state dir — beside registry.json/facts.json, survives a
// teardown). One record per exposed service; the SOURCE OF TRUTH the edge
// config renders from and the build tail re-ensures from — a rebuild brings
// the apps back from these rows, the same way capability doors re-stage.
//
// Group: every app rides a relay CHANNEL (NIP-29) as its ACL — group holds
// the channel ID, never the name (names change; the display layer resolves
// id → current name). #general is the everyone-channel and the default; a
// group other than the community-wide one is validated at expose time (the
// requester must already be IN the channel — access narrows to groups the
// requester belongs to, never widens).
package agenttools

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Visibilities.
const (
	VisibilityFamily = "family" // public DNS + the member gate (the default)
	VisibilityPublic = "public" // public DNS, NO gate — operator-only to set
	VisibilityLAN    = "lan"    // no public DNS record
)

// Auth modes for an exposed app.
const (
	AuthGate = "gate" // the console's member gate (forward_auth) — the default
	AuthApp  = "app"  // the app brings its own auth; the gate does not front it
	AuthNone = "none" // nothing in front of it — operator-only to set
)

var appNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// AppRecord is one exposed service.
type AppRecord struct {
	Name       string `json:"name"`
	FQDN       string `json:"fqdn"`
	Target     string `json:"target"`
	Visibility string `json:"visibility"`
	Auth       string `json:"auth"`
	Group      string `json:"group"` // the relay channel ID
	Owner      string `json:"owner"`
	Requester  string `json:"requester"`
	CreatedAt  int64  `json:"created_at"`
}

// AppsStore is the apps registry (thread-safe; a mutex like the other stores).
type AppsStore struct {
	mu    sync.Mutex
	path  string
	apps  map[string]AppRecord // name -> record
	order []string             // creation order, for stable listing
}

// OpenApps loads (creating if needed) the apps registry at path. A loaded
// store's listing order re-derives from CreatedAt (the map has no order; the
// file is the map).
func OpenApps(path string) (*AppsStore, error) {
	a := &AppsStore{path: path, apps: map[string]AppRecord{}}
	if a.path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(raw, &a.apps); err != nil {
				return nil, fmt.Errorf("malformed apps %s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	for n := range a.apps {
		a.order = append(a.order, n)
	}
	sort.Slice(a.order, func(i, j int) bool {
		ri, rj := a.apps[a.order[i]], a.apps[a.order[j]]
		if ri.CreatedAt != rj.CreatedAt {
			return ri.CreatedAt < rj.CreatedAt
		}
		return ri.Name < rj.Name
	})
	return a, nil
}

func (a *AppsStore) save() error {
	if a.path == "" {
		return nil // memory-only (tests)
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a.apps, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// ValidAppName reports whether name is a DNS label (the app's fqdn is
// <name>.<world-domain>, so the name must be one that composes).
func ValidAppName(name string) bool { return appNameRe.MatchString(name) }

// ValidTarget reports whether target is a host:port the edge can proxy to.
func ValidTarget(target string) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" || port == "" {
		return false
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return false
	}
	return true
}

// Expose records a new app. Refuses a clobbered name or fqdn (the same
// discipline provision refuses a clobbered runner name — a record that
// silently replaced another would strand grants and config), an invalid
// name/target, or unknown visibility/auth values.
func (a *AppsStore) Expose(rec AppRecord) error {
	if !ValidAppName(rec.Name) {
		return fmt.Errorf("expose %q: name must be a DNS label (a-z0-9 and dashes)", rec.Name)
	}
	if !ValidTarget(rec.Target) {
		return fmt.Errorf("expose %q: target must be host:port (the service's internal address)", rec.Name)
	}
	if rec.Visibility == "" {
		rec.Visibility = VisibilityFamily
	}
	switch rec.Visibility {
	case VisibilityFamily, VisibilityPublic, VisibilityLAN:
	default:
		return fmt.Errorf("expose %q: visibility must be family|public|lan (got %q)", rec.Name, rec.Visibility)
	}
	if rec.Auth == "" {
		rec.Auth = AuthGate
	}
	switch rec.Auth {
	case AuthGate, AuthApp, AuthNone:
	default:
		return fmt.Errorf("expose %q: auth must be gate|app|none (got %q)", rec.Name, rec.Auth)
	}
	if rec.Group == "" {
		return fmt.Errorf("expose %q: group is required (the relay channel the access rides — default: the #general channel's id)", rec.Name)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.apps[rec.Name]; ok {
		return fmt.Errorf("expose: an app named %q already exists (unexpose it first)", rec.Name)
	}
	for name, other := range a.apps {
		if other.FQDN == rec.FQDN {
			return fmt.Errorf("expose: %q already serves %s (unexpose it first)", name, rec.FQDN)
		}
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().Unix()
	}
	a.apps[rec.Name] = rec
	a.order = append(a.order, rec.Name)
	return a.save()
}

// Unexpose removes an app's record. Not-found is an error (the caller
// reports it); the edge-side teardown is the caller's job.
func (a *AppsStore) Unexpose(name string) (AppRecord, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.apps[name]
	if !ok {
		return AppRecord{}, fmt.Errorf("unexpose: no app named %q", name)
	}
	delete(a.apps, name)
	for i, n := range a.order {
		if n == name {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	if err := a.save(); err != nil {
		a.apps[name] = rec // the record is the intent — put it back on a failed save
		return rec, err
	}
	return rec, nil
}

// List returns the records in creation order.
func (a *AppsStore) List() []AppRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AppRecord, 0, len(a.order))
	for _, n := range a.order {
		if rec, ok := a.apps[n]; ok {
			out = append(out, rec)
		}
	}
	return out
}

// Get returns one app's record.
func (a *AppsStore) Get(name string) (AppRecord, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.apps[name]
	return rec, ok
}
