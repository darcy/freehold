package planebase

import (
	"strings"
	"testing"
)

func TestFreeholdLVRecognizesOwnNamesOnly(t *testing.T) {
	cases := []struct {
		name, domain string
		ok           bool
	}{
		{"freehold-relay-librem2-freehold-technology-cp", "relay-librem2-freehold-technology", true},
		{"freehold-relay-librem2-freehold-technology-k3s-volumes", "relay-librem2-freehold-technology", true},
		{"freehold-relay-librem2-freehold-technology-relay", "relay-librem2-freehold-technology", true},
		{"freehold-relay-librem2-freehold-technology-relay-docker-root", "relay-librem2-freehold-technology", true},
		{"freehold-relay-librem2-freehold-technology-relay-deploy", "relay-librem2-freehold-technology", true},
		{"vm-100-disk-0", "", false},
		{"data", "", false},
		{"freehold-", "", false},
	}
	for _, c := range cases {
		dom, ok := FreeholdLV(c.name)
		if ok != c.ok || dom != c.domain {
			t.Errorf("FreeholdLV(%q) = %q,%v want %q,%v", c.name, dom, ok, c.domain, c.ok)
		}
	}
}

func TestFreeholdDatasetRecognizesOwnPaths(t *testing.T) {
	if dom, ok := FreeholdDataset("rpool/freehold/t-d/relay/docker-root"); !ok || dom != "t-d" {
		t.Errorf("dataset = %q,%v", dom, ok)
	}
	if dom, ok := FreeholdDataset("rpool/freehold/t-d/cp"); !ok || dom != "t-d" {
		t.Errorf("dataset = %q,%v", dom, ok)
	}
	if _, ok := FreeholdDataset("rpool/vm-100-disk-0"); ok {
		t.Error("foreign dataset must not report provenance")
	}
	if _, ok := FreeholdDataset("rpool/freehold/t-d"); ok {
		t.Error("a bare freehold dir is not enough to claim data")
	}
}

func TestFreeholdPoolName(t *testing.T) {
	for _, n := range []string{"freehold", "freehold-thin", "freehold-librem3-thin"} {
		if !FreeholdPoolName(n) {
			t.Errorf("pool %q must be freehold's", n)
		}
	}
	for _, n := range []string{"data", "rpool", "pve", "freehold-librem3"} {
		if FreeholdPoolName(n) {
			t.Errorf("pool %q is not a freehold pool name", n)
		}
	}
}

func TestBuildOptionsClassifiesSafely(t *testing.T) {
	inv := Inventory{
		Zpools: []ZpoolInfo{{Name: "rpool", SizeGB: 1800, FreeGB: 1700, Health: "ONLINE"}},
		VGs: []VGInfo{
			{Name: "pve", SizeGB: 816, FreeGB: 16, Pools: []PoolInfo{
				{Name: "data", LocalLvm: true, Riders: []string{"vm-100-disk-0", "vm-116-disk-0"}},
			}},
			{Name: "empty-vg", SizeGB: 500, FreeGB: 500},
			{Name: "fh-vg", Freehold: Provenance{Freehold: true, Domains: []string{"t-d"}, Volumes: 4}},
		},
		Devices: []DeviceInfo{
			{Path: "/dev/sda", SizeGB: 1800, Content: "already has data (zfs_member)", Clean: false},
			{Path: "/dev/sdb", SizeGB: 1800, Clean: true},
			{Path: "/dev/sdc", SizeGB: 1800, Importable: "tank", Freehold: Provenance{Freehold: true, Domains: []string{"t-d"}}},
		},
	}
	opts := BuildOptions(inv, "t.d")
	byBackend := map[string]Option{}
	for _, o := range opts {
		byBackend[o.Backend] = o
		if o.Device != "" {
			byBackend[o.Device] = o
		}
	}
	if o := byBackend["rpool"]; o.Safety != Safe || o.Kind != KindReuseZpool {
		t.Errorf("rpool = %+v", o)
	}
	if o := byBackend["pve"]; o.Safety != Caution || o.Kind != KindReuseVG {
		t.Errorf("busy pve must be Caution, got %+v", o)
	}
	if o := byBackend["empty-vg"]; o.Safety != Safe {
		t.Errorf("empty vg must be Safe, got %+v", o)
	}
	if o := byBackend["fh-vg"]; !o.Freehold.Freehold || o.Safety != Safe || !strings.Contains(o.Title, "Reconnect") {
		t.Errorf("this world's freehold vg must carry provenance + Safe + reconnect, got %+v", o)
	}
	if o := byBackend["/dev/sda"]; o.Kind != KindBlocked || o.Safety != Blocked {
		t.Errorf("signed disk must be Blocked, got %+v", o)
	}
	if o := byBackend["/dev/sdb"]; o.Kind != KindCreateDevice || o.Safety != Caution {
		t.Errorf("clean disk must be a Caution create, got %+v", o)
	}
	if o := byBackend["/dev/sdc"]; o.Kind != KindBlocked || o.Safety != Blocked {
		t.Errorf("importable freehold pool must be listed Blocked (no import this phase), got %+v", o)
	}
}

func TestRecommendPrefersFreeholdThenSafe(t *testing.T) {
	inv := Inventory{
		Zpools: []ZpoolInfo{{Name: "rpool", FreeGB: 100}},
		VGs:    []VGInfo{{Name: "fh-vg", Freehold: Provenance{Freehold: true, Domains: []string{"t-d"}, Volumes: 4}}},
	}
	opts := BuildOptions(inv, "t.d")
	i := Recommend(opts)
	if i < 0 || !opts[i].Freehold.Freehold {
		t.Fatalf("recommend must prefer the freehold plane, got %d %+v", i, opts)
	}

	// No safe option at all => no recommendation (-1): the CLI must not guess.
	onlyBusy := BuildOptions(Inventory{VGs: []VGInfo{{Name: "pve", Pools: []PoolInfo{{Name: "data", Riders: []string{"vm-1"}}}}}}, "")
	if Recommend(onlyBusy) != -1 {
		t.Errorf("all-caution inventory must recommend nothing, got %d", Recommend(onlyBusy))
	}
}

func TestInventoryEmpty(t *testing.T) {
	if !(Inventory{}).Empty() {
		t.Error("zero inventory must be Empty (the brand-new box case)")
	}
	if (Inventory{Devices: []DeviceInfo{{Path: "/dev/sda"}}}).Empty() {
		t.Error("a device must not read as Empty")
	}
}

func TestFailingZpoolIsBlockedAndNotRecommended(t *testing.T) {
	inv := Inventory{Zpools: []ZpoolInfo{
		{Name: "bad", FreeGB: 100, Health: "DEGRADED"},
		{Name: "good", FreeGB: 100, Health: "ONLINE"},
	}}
	opts := BuildOptions(inv, "")
	bad, _ := FindOption(opts, "bad")
	if bad.Kind != KindBlocked || bad.Safety != Blocked || !strings.Contains(bad.Reason, "DEGRADED") {
		t.Errorf("failing pool must be Blocked with a reason, got %+v", bad)
	}
	good, _ := FindOption(opts, "good")
	if good.Kind != KindReuseZpool {
		t.Errorf("healthy pool usable, got %+v", good)
	}
	if i := Recommend(opts); i < 0 || opts[i].Backend != "good" {
		t.Errorf("recommend must skip the failing pool, got %d", i)
	}
}

func TestProvenanceDomainMatches(t *testing.T) {
	p := Provenance{Freehold: true, Domains: []string{"relay-example-com"}, Volumes: 4}
	if m, h := p.DomainMatches("relay.example.com"); !m || !h {
		t.Errorf("dotted domain must match the flattened provenance: m=%v h=%v", m, h)
	}
	if m, h := p.DomainMatches("other.example.com"); m || !h {
		t.Errorf("different domain must not match: m=%v h=%v", m, h)
	}
	// A name-only freehold pool (no domains) reports no domains, not a match.
	empty := Provenance{Freehold: true}
	if m, h := empty.DomainMatches("anything"); m || h {
		t.Errorf("domainless provenance: m=%v h=%v", m, h)
	}
}

func TestBuildOptionsDomainScoped(t *testing.T) {
	// A shared box: one VG carries THIS world's freehold LVs AND another
	// world's, plus a live guest disk.
	inv := Inventory{VGs: []VGInfo{{
		Name: "shared", SizeGB: 500, FreeGB: 200,
		Freehold: Provenance{Freehold: true, Domains: []string{"mine-com", "other-com"}, Volumes: 6},
		Pools: []PoolInfo{{Name: "freehold-thin", Riders: []string{
			"freehold-mine-com-relay", "freehold-mine-com-cp",
			"freehold-other-com-relay", "vm-100-disk-0",
		}}},
	}}}

	byBackend := func(domain string) Option {
		t.Helper()
		o, ok := FindOption(BuildOptions(inv, domain), "shared")
		if !ok {
			t.Fatalf("no option for shared (domain %q)", domain)
		}
		return o
	}

	// The matching domain reconnects, and only foreign entries count as sharing.
	if o := byBackend("mine.com"); o.Safety != Safe || !strings.Contains(o.Title, "Reconnect") {
		t.Errorf("matching domain must reconnect, got %+v", o)
	} else if o.Pools[0].OtherVolumes != 2 {
		t.Errorf("OtherVolumes = %d, want 2 (other world's LV + guest)", o.Pools[0].OtherVolumes)
	}
	// A different world's domain reconnects FROM ITS OWN point of view.
	if o := byBackend("other.com"); !strings.Contains(o.Title, "Reconnect") {
		t.Errorf("other world's own domain must reconnect to it, got %+v", o)
	} else if o.Pools[0].OtherVolumes != 3 {
		t.Errorf("OtherVolumes = %d, want 3", o.Pools[0].OtherVolumes)
	}
	// Installing a NEW domain on a shared box is Caution + ordinary reuse, never
	// a reconnect — and erasing would only ever touch this world's (absent) data.
	if o := byBackend("third.com"); o.Safety != Caution || strings.Contains(o.Title, "Reconnect") {
		t.Errorf("new domain on a shared box must be Caution reuse, got %+v", o)
	} else if o.Pools[0].OtherVolumes != 4 {
		t.Errorf("OtherVolumes = %d, want 4 (nothing is ours)", o.Pools[0].OtherVolumes)
	}
}

func TestBuildOptionsNameOnlyFreeholdPoolIsOrdinaryReuse(t *testing.T) {
	// A pool recognized as freehold's by NAME alone (no domains yet) has nothing
	// to reconnect to or erase: it is a safe reuse, not a reconnect.
	inv := Inventory{
		VGs: []VGInfo{{Name: "pve", SizeGB: 500, FreeGB: 500, Pools: []PoolInfo{
			{Name: "freehold-thin", Freehold: Provenance{Freehold: true}},
		}}},
		Zpools: []ZpoolInfo{{Name: "rpool", FreeGB: 100, Health: "ONLINE",
			Freehold: Provenance{Freehold: true, Domains: []string{"other-com"}}}},
	}
	opts := BuildOptions(inv, "mine.com")
	if o, _ := FindOption(opts, "pve"); o.Safety != Safe || strings.Contains(o.Title, "Reconnect") {
		t.Errorf("name-only freehold pool must be ordinary Safe reuse, got %+v", o)
	}
	if o, _ := FindOption(opts, "rpool"); o.Safety != Safe || strings.Contains(o.Title, "Reconnect") {
		t.Errorf("another world's zpool datasets are Safe reuse, got %+v", o)
	}
}

func TestValidStorageName(t *testing.T) {
	for _, ok := range []string{"freehold-thin", "fh.prod_1", "data"} {
		if !ValidStorageName(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", "has space", "x;rm -rf /", "a'b", "a/b", strings.Repeat("x", 65)} {
		if ValidStorageName(bad) {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
