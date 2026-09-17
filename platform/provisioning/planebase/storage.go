package planebase

import (
	"fmt"
	"strings"
)

// This file is the durable plane's storage-SELECTION decision layer: the pure,
// hermetic-testable classification an operator (often a non-expert) sees before
// freehold touches anything. The exec/detection lives in `bootstrap`; the
// rendering + prompting lives in the CLIs. Keeping the safety rules here means
// the "what is safe to choose" contract is testable without a host.

// Safety is the data-impact classification shown to the operator. It is the
// one thing that decides whether an option is selectable at all:
//
//	Safe    — freehold coexists without touching anything else (ZFS datasets,
//	          an empty VG, freehold's own data).
//	Caution — usable, but shares capacity with live guests; needs an explicit
//	          typed confirmation.
//	Blocked — choosing it would require destroying data freehold does not
//	          recognize as its own. It is LISTED (so the operator understands
//	          why) but cannot be selected.
type Safety int

const (
	Safe Safety = iota
	Caution
	Blocked
)

func (s Safety) String() string {
	switch s {
	case Safe:
		return "safe"
	case Caution:
		return "caution"
	default:
		return "blocked"
	}
}

// Provenance records whether entries are POSITIVELY freehold's, identified by
// the naming conventions below (never guessed from a foreign name).
type Provenance struct {
	Freehold bool
	Domains  []string // the flattened domains found (deduped)
	Volumes  int      // count of freehold-namespaced LVs/datasets
}

// add records one discovered freehold entry (counts + dedups the domain).
func (p *Provenance) Add(domain string) {
	p.Freehold = true
	p.Volumes++
	p.addDomain(domain)
}

// addDomain dedups a domain without touching the volume count.
func (p *Provenance) addDomain(domain string) {
	for _, d := range p.Domains {
		if d == domain {
			return
		}
	}
	p.Domains = append(p.Domains, domain)
}

// Merge folds another provenance in (domains deduped, volume counts summed).
func (p *Provenance) Merge(o Provenance) {
	if !o.Freehold {
		return
	}
	p.Freehold = true
	p.Volumes += o.Volumes
	for _, d := range o.Domains {
		p.addDomain(d)
	}
}

// DomainMatches reports whether any provenance domain equals the given
// (un-normalized) relay domain, and whether the provenance carries any domains
// at all (a name-only freehold pool has none). Names are domain-derived, so a
// mismatch means freehold cannot reconnect to the data.
func (p Provenance) DomainMatches(domain string) (matched, hasDomains bool) {
	want, err := NormalizeDomain(domain)
	if err != nil {
		want = domain
	}
	hasDomains = len(p.Domains) > 0
	for _, d := range p.Domains {
		if d == want {
			return true, true
		}
	}
	return false, hasDomains
}

// isMine reports whether a provenance describes THIS world's plane: it must
// carry at least one domain AND that domain must equal the current domain. A
// name-only freehold pool (no domains) or another world's data is not mine.
func isMine(p Provenance, domain string) bool {
	matched, hasDomains := p.DomainMatches(domain)
	return matched && hasDomains
}

// otherVolumes counts a pool's riders that are not this world's own freehold
// entries: another world's freehold LVs count (they share the pool's capacity).
// want is the normalized current domain; internal pool bookkeeping LVs never
// reach Riders (bootstrap filters them).
func otherVolumes(p PoolInfo, want string) int {
	n := 0
	for _, r := range p.Riders {
		if dom, fh := FreeholdLV(r); fh && dom == want {
			continue
		}
		n++
	}
	return n
}

// PoolInfo is one thin pool inside a VG.
type PoolInfo struct {
	Name        string
	SizeGB      uint64
	DataPercent float64
	Riders      []string // LV names riding the pool (guest disks + freehold LVs)
	LocalLvm    bool     // PVE's `local-lvm` storage currently points here
	Freehold    Provenance
	// OtherVolumes is the count of riders that are NOT this world's own
	// freehold entries (another world's freehold LVs count here; internal
	// pool bookkeeping LVs are already excluded upstream). Populated by
	// BuildOptions, which is the only layer that knows the current domain.
	// > 0 means reusing the pool shares capacity with someone else's data.
	OtherVolumes int
}

// VGInfo is one LVM volume group.
type VGInfo struct {
	Name     string
	SizeGB   uint64
	FreeGB   uint64
	Pools    []PoolInfo
	Freehold Provenance
}

// ZpoolInfo is one ZFS pool.
type ZpoolInfo struct {
	Name     string
	SizeGB   uint64
	FreeGB   uint64
	Health   string
	Datasets int
	Freehold Provenance
}

// DeviceInfo is one whole-disk candidate for creating a new backend. Content is
// a PLAIN-LANGUAGE description of what is on the disk ("" = nothing detected);
// Clean is the fail-closed verdict that no signature/PV/zpool/mount/OS/swap
// rides it. Importable names an exported zpool recoverable from the device.
type DeviceInfo struct {
	Path       string
	SizeGB     uint64
	Model      string
	Content    string
	Clean      bool
	Importable string
	Freehold   Provenance
}

// Inventory is everything the read-only probe found on a host.
type Inventory struct {
	Zpools  []ZpoolInfo
	VGs     []VGInfo
	Devices []DeviceInfo
}

// Empty reports whether nothing usable and nothing blocked was found at all —
// the "brand-new box" case where freehold must create a backend.
func (inv Inventory) Empty() bool {
	return len(inv.Zpools) == 0 && len(inv.VGs) == 0 && len(inv.Devices) == 0
}

// Kind names a candidate action.
type Kind string

const (
	KindReuseZpool   Kind = "reuse-zpool"   // use an existing ZFS pool
	KindReuseVG      Kind = "reuse-vg"      // use a pool inside an existing LVM VG
	KindCreateDevice Kind = "create-device" // create a new backend on a clean disk
	KindBlocked      Kind = "blocked"       // listed, not selectable
)

// Option is a classified, operator-presentable candidate. Title/Impact are the
// plain-language sentences the CLIs render; a non-expert never sees the raw
// VG/pool/LV vocabulary unless they ask for details.
type Option struct {
	Kind     Kind
	Safety   Safety
	Backend  string // VG or zpool name ("" for a device create)
	Pools    []PoolInfo
	Device   string
	Freehold Provenance
	Title    string // short label, e.g. "Use the spare disk group “rpool”"
	Impact   string // one plain sentence on what freehold will / will not touch
	Reason   string // blocked: why it cannot be chosen
}

// BuildOptions flattens an inventory into classified candidates in
// recommendation-friendly order: THIS world's own plane first (the reconnect
// case), then safe reuse, then cautions, then clean-device creates, then the
// blocked entries (always shown, never selectable).
//
// domain is the current world's relay domain. A backend is "this world's
// plane" only when its provenance carries a domain EQUAL to it — freehold data
// belonging to another world (or a name-only pool with no domains) is ordinary
// reuse, never a reconnect. Storage is shared: another world's freehold LVs
// riding a thin pool count as capacity shared with someone else.
func BuildOptions(inv Inventory, domain string) []Option {
	var freeholdPlane, safe, caution, create, blocked []Option
	want, _ := NormalizeDomain(domain)

	for _, z := range inv.Zpools {
		opt := Option{Kind: KindReuseZpool, Safety: Safe, Backend: z.Name, Freehold: z.Freehold}
		// A pool the kernel reports as anything but ONLINE is a failing pool:
		// never write the plane onto it. Unknown ("") is treated as usable so
		// an older/partial probe does not spuriously block a healthy pool.
		if z.Health != "" && z.Health != "ONLINE" {
			opt.Kind = KindBlocked
			opt.Safety = Blocked
			opt.Title = fmt.Sprintf("disk group “%s”", z.Name)
			opt.Reason = fmt.Sprintf("its health is %s — freehold will not store data on a failing pool", z.Health)
			blocked = append(blocked, opt)
			continue
		}
		if isMine(z.Freehold, domain) {
			opt.Title = fmt.Sprintf("Reconnect to your previous freehold data in “%s”", z.Name)
			opt.Impact = "Freehold adds its own folders here and reconnects to the data it already owns. Nothing else on this pool is changed."
			freeholdPlane = append(freeholdPlane, opt)
			continue
		}
		// Foreign-domain or name-only freehold datasets live in their own
		// `<pool>/freehold/<domain>` namespace, so this is ordinary safe reuse.
		opt.Title = fmt.Sprintf("Use the spare disk group “%s”", z.Name)
		opt.Impact = fmt.Sprintf("Freehold adds its own folders to this pool (%s free). It changes nothing else.", humanGB(z.FreeGB))
		safe = append(safe, opt)
	}

	for _, vg := range inv.VGs {
		// A VG's freehold provenance is the union of its pools' (bootstrap
		// need not pre-merge; keeping the merge here makes the pure layer
		// robust and the tests simple).
		prov := vg.Freehold
		for _, p := range vg.Pools {
			prov.Merge(p.Freehold)
		}
		// Copy the pools so we can annotate OtherVolumes without mutating the
		// caller's inventory.
		pools := make([]PoolInfo, len(vg.Pools))
		copy(pools, vg.Pools)
		for i := range pools {
			pools[i].OtherVolumes = otherVolumes(pools[i], want)
		}
		opt := Option{Kind: KindReuseVG, Backend: vg.Name, Pools: pools, Freehold: prov}
		riders, local := vgRiders(vg)
		switch {
		case isMine(prov, domain):
			opt.Safety = Safe
			opt.Title = fmt.Sprintf("Reconnect to your previous freehold data in “%s”", vg.Name)
			opt.Impact = "Freehold reconnects to the data it already owns here. Nothing else is changed."
			freeholdPlane = append(freeholdPlane, opt)
		case riders > 0:
			opt.Safety = Caution
			opt.Title = fmt.Sprintf("Use “%s” storage (%s free)", vg.Name, humanGB(vg.FreeGB))
			opt.Impact = fmt.Sprintf("This already holds %d of your volumes, so freehold would share the space. They could run out of room.", riders)
			if local {
				opt.Impact += " It is also where Proxmox keeps its own VM storage."
			}
			caution = append(caution, opt)
		default:
			opt.Safety = Safe
			opt.Title = fmt.Sprintf("Use “%s” storage (%s free)", vg.Name, humanGB(vg.FreeGB))
			opt.Impact = "Freehold creates its own space here. Nothing currently uses it."
			safe = append(safe, opt)
		}
	}

	for _, d := range inv.Devices {
		opt := Option{Kind: KindCreateDevice, Device: d.Path, Freehold: d.Freehold}
		label := d.Path
		if d.Model != "" {
			label = fmt.Sprintf("%s (%s)", d.Path, d.Model)
		}
		switch {
		case d.Importable != "":
			opt.Kind = KindBlocked
			opt.Safety = Blocked
			opt.Title = label
			opt.Reason = fmt.Sprintf("holds a previous storage pool “%s” that is not in use right now", d.Importable)
			if d.Freehold.Freehold {
				opt.Reason = fmt.Sprintf("holds your previous freehold data (pool “%s”), not in use right now", d.Importable)
			}
			blocked = append(blocked, opt)
		case !d.Clean:
			opt.Kind = KindBlocked
			opt.Safety = Blocked
			opt.Title = label
			opt.Reason = d.Content
			if opt.Reason == "" {
				opt.Reason = "has data on it"
			}
			blocked = append(blocked, opt)
		default:
			opt.Safety = Caution
			opt.Title = fmt.Sprintf("Prepare %s", label)
			opt.Impact = fmt.Sprintf("This disk (%s) appears empty. Freehold will erase it and use it for your data.", humanGB(d.SizeGB))
			create = append(create, opt)
		}
	}

	out := make([]Option, 0, len(freeholdPlane)+len(safe)+len(caution)+len(create)+len(blocked))
	out = append(out, freeholdPlane...)
	out = append(out, safe...)
	out = append(out, caution...)
	out = append(out, create...)
	out = append(out, blocked...)
	return out
}

// Recommend returns the index of the option the operator should take by
// default, or -1 when nothing is safely usable (the CLIs then must NOT invent a
// default — they present the choice or stop). A freehold plane is preferred
// (reconnect, don't orphan), else any Safe option.
func Recommend(opts []Option) int {
	for i, o := range opts {
		if o.Kind != KindBlocked && o.Safety == Safe && o.Freehold.Freehold {
			return i
		}
	}
	for i, o := range opts {
		if o.Kind != KindBlocked && o.Safety == Safe {
			return i
		}
	}
	return -1
}

// FindOption returns the option matching a backend or device name (the
// `--plane-pool` selector). The bool is false when nothing matches.
func FindOption(opts []Option, name string) (Option, bool) {
	for _, o := range opts {
		if name != "" && (o.Backend == name || o.Device == name) {
			return o, true
		}
	}
	return Option{}, false
}

// PoolNames lists a VG option's thin-pool names (for the STORAGE-THINPOOL line
// and the reuse/carve prompt).
func PoolNames(o Option) []string {
	names := make([]string, 0, len(o.Pools))
	for _, p := range o.Pools {
		names = append(names, p.Name)
	}
	return names
}

// vgRiders counts the non-pool LVs riding any pool in a VG (the guest disks +
// freehold LVs sharing its thin capacity), and whether PVE's local-lvm points
// at one of its pools.
func vgRiders(vg VGInfo) (riders int, local bool) {
	for _, p := range vg.Pools {
		riders += len(p.Riders)
		if p.LocalLvm {
			local = true
		}
	}
	return riders, local
}

// humanGB renders a size in GB as TB when large; plain-language for the menu.
func humanGB(gb uint64) string {
	if gb >= 1024 {
		return fmt.Sprintf("%.1f TB", float64(gb)/1024)
	}
	return fmt.Sprintf("%d GB", gb)
}

// FreeholdLV parses a freehold-namespaced thin LV name
// (`freehold-<flattened-domain>-<tenant>[-<child>]`) and returns the flattened
// domain. It only matches freehold's own exact conventions — a foreign name
// never reports provenance.
func FreeholdLV(name string) (domain string, ok bool) {
	if !strings.HasPrefix(name, "freehold-") {
		return "", false
	}
	// Longest suffixes first: `-relay` must not swallow `-relay-docker-root`.
	for _, suffix := range []string{"-relay-docker-root", "-relay-deploy", "-k3s-volumes", "-cp", "-relay"} {
		if strings.HasSuffix(name, suffix) {
			dom := strings.TrimSuffix(name, suffix)
			dom = strings.TrimPrefix(dom, "freehold-")
			if dom != "" {
				return dom, true
			}
		}
	}
	return "", false
}

// FreeholdDataset parses a freehold dataset path
// (`<pool>/freehold/<domain>/<tenant>[/<child>]`) and returns the flattened
// domain.
func FreeholdDataset(path string) (domain string, ok bool) {
	idx := strings.Index(path, "/freehold/")
	if idx < 0 {
		return "", false
	}
	parts := strings.Split(path[idx+len("/freehold/"):], "/")
	if len(parts) < 2 || parts[0] == "" {
		return "", false
	}
	switch parts[1] {
	case "relay", "cp", "k3s-volumes":
		return parts[0], true
	}
	return "", false
}

// FreeholdPoolName reports whether a thin-pool / zpool name follows freehold's
// own pool conventions (`freehold`, `freehold-thin`, `freehold-<x>-thin`). The
// recorded config's `plane.thin_pool` is a stronger signal where available.
func FreeholdPoolName(name string) bool {
	if name == "freehold" || name == "freehold-thin" {
		return true
	}
	return strings.HasPrefix(name, "freehold-") && strings.HasSuffix(name, "-thin")
}

// ValidStorageName reports whether a name is safe to use as a thin-pool / VG
// name AND to interpolate into a shell command (the PVE storage.cfg re-point
// awk). It is the same conservative alphabet the backend drivers use:
// [A-Za-z0-9._-], non-empty, bounded.
func ValidStorageName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, c := range name {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}
