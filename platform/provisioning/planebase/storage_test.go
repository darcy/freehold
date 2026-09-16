package planebase

import "testing"

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
	opts := BuildOptions(inv)
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
	if o := byBackend["fh-vg"]; !o.Freehold.Freehold || o.Safety != Safe {
		t.Errorf("freehold vg must carry provenance + Safe, got %+v", o)
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
	opts := BuildOptions(inv)
	i := Recommend(opts)
	if i < 0 || !opts[i].Freehold.Freehold {
		t.Fatalf("recommend must prefer the freehold plane, got %d %+v", i, opts)
	}

	// No safe option at all => no recommendation (-1): the CLI must not guess.
	onlyBusy := BuildOptions(Inventory{VGs: []VGInfo{{Name: "pve", Pools: []PoolInfo{{Name: "data", Riders: []string{"vm-1"}}}}}})
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
