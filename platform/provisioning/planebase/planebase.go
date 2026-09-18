// Package planebase is the durable volume plane's pure decision layer port
// (originally planebase.rs): naming + resolution DECISIONS, decoupled
// from the exec driver so ordering/consent semantics are hermetic-testable.
package planebase

import (
	"fmt"
	"strings"
)

// Guest paths — the ARCHITECTURE convention's single source of truth.
const (
	GuestPathDockerRoot  = "/var/lib/docker"
	GuestPathRelayDeploy = "/srv/data/relay"
	GuestPathCP          = "/srv/data/cp"
	GuestPathK8sVolumes  = "/srv/data/k8s-volumes"
)

// MountSpec is one reference MOUNT of a durable dataset into a guest, born at
// LXC create (the locked "born on the plane, never set post-hoc" rule).
type MountSpec struct {
	Source    string // the dataset/volume to bind
	GuestPath string // the guest mount point
}

// BackupFlag is the backup rule: /srv/data mounts + the relay docker root are
// the backup set (backup=1); everything else is excluded (backup=0).
func BackupFlag(guestPath string) uint8 {
	inData := guestPath == "/srv/data" || strings.HasPrefix(guestPath, "/srv/data/")
	if inData || guestPath == GuestPathDockerRoot {
		return 1
	}
	return 0
}

// Tenant kinds that get their own durable dataset.
type Tenant int

const (
	TenantRelay Tenant = iota
	TenantCp
	TenantK3sVolumes
)

var tenantNames = [...]string{"relay", "cp", "k3s-volumes"}

func (t Tenant) String() string { return tenantNames[t] }

// LxcRole is the tenant's LXC role suffix (bootstrap's <domain>-<role>).
func (t Tenant) LxcRole() string {
	switch t {
	case TenantRelay:
		return "relay"
	case TenantCp:
		return "cp"
	default:
		return "k3s"
	}
}

var tenantAll = [...]Tenant{TenantRelay, TenantCp, TenantK3sVolumes}

// TenantAll lists every tenant.
func TenantAll() []Tenant { return tenantAll[:] }

// RelayChild is one of the relay's TWO child datasets.
type RelayChild int

const (
	RelayChildDockerRoot RelayChild = iota
	RelayChildDeployDir
)

func (c RelayChild) String() string {
	switch c {
	case RelayChildDockerRoot:
		return "docker-root"
	default:
		return "deploy"
	}
}

// Backend is the Proxmox backend resolution ORDER (locked: ZFS → LVM-thin).
type Backend int

const (
	BackendZfs Backend = iota
	BackendLvmThin
)

// ExistingBackend is a healthy backend the resolution DETECTED already present.
type ExistingBackend int

const (
	ExistingZfs ExistingBackend = iota
	ExistingLvmThin
)

// BackendKind is the recorded storage-driver kind (lowercase serde).
type BackendKind string

const (
	KindZfs     BackendKind = "zfs"
	KindLvmThin BackendKind = "lvmth"
)

// ResolveAction is what the resolution stage decides to DO.
type ResolveAction struct {
	Kind     string           // Reuse | Create | Bail
	Detected *ExistingBackend // for Reuse
	Backend  *Backend         // for Create
	Pool     string
	Message  string // for Bail
}

// VpsStorage is the VPS storage outcome.
type VpsStorage struct {
	BlockVolume bool // true = block volume; false = LocalDir
	VolumeID    string
	Mount       string
	LocalDir    string // when !BlockVolume
}

// NormalizeDomain flattens '.' to '-' and validates a dataset-safe name.
func NormalizeDomain(domain string) (string, error) {
	normalized := strings.ReplaceAll(domain, ".", "-")
	if normalized == "" || len(normalized) > 48 {
		return "", fmt.Errorf("domain %q yields an invalid dataset name", domain)
	}
	for _, c := range normalized {
		if !isAsciiAlnum(c) && c != '-' {
			return "", fmt.Errorf("domain %q contains characters not allowed in a dataset name", domain)
		}
	}
	return normalized, nil
}

func isAsciiAlnum(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// DatasetPath is `<pool>/freehold/<domain-with-dashes>/<tenant>`.
func DatasetPath(pool, domain string, tenant Tenant) (string, error) {
	dom, err := NormalizeDomain(domain)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/freehold/%s/%s", pool, dom, tenant.String()), nil
}

// VpsVolumeLabel is the flattened block-volume label `fh-<domain>-<tenant>`.
func VpsVolumeLabel(domain string, tenant Tenant) (string, error) {
	dom, err := NormalizeDomain(domain)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("fh-%s-%s", dom, tenant.String()), nil
}

// RelayChildDataset is one of the relay's two child datasets under its tenant
// parent.
func RelayChildDataset(pool, domain string, child RelayChild) (string, error) {
	parent, err := DatasetPath(pool, domain, TenantRelay)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s", parent, child.String()), nil
}

// LvmLVName is the flat thin-LV name for a tenant: `freehold-<domain>-<tenant>`.
func LvmLVName(domain string, tenant Tenant) (string, error) {
	dom, err := NormalizeDomain(domain)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("freehold-%s-%s", dom, tenant.String()), nil
}

// LvmRelayChildLVName is the flat thin-LV name for one relay child:
// `freehold-<domain>-relay-<child>`.
func LvmRelayChildLVName(domain string, child RelayChild) (string, error) {
	dom, err := NormalizeDomain(domain)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("freehold-%s-relay-%s", dom, child.String()), nil
}

// String names an ExistingBackend (for reports).
func (b ExistingBackend) String() string {
	if b == ExistingZfs {
		return "zfs"
	}
	return "lvm-thin"
}

// String names a Backend (for reports).
func (b Backend) String() string {
	if b == BackendZfs {
		return "zfs"
	}
	return "lvm-thin"
}
