package proxmox

import (
	"testing"

	"freehold/platform/provisioning/planebase"
)

func TestParseHumanGB(t *testing.T) {
	cases := map[string]uint64{"1.8T": 1843, "953.9G": 953, "40G": 40, "512M": 0, "": 0, "garbage": 0}
	for in, want := range cases {
		if got := parseHumanGB(in); got != want {
			t.Errorf("parseHumanGB(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestImportablePoolsAssociatesDevice(t *testing.T) {
	out := "   pool: tank\n     id: 1\n  state: ONLINE\n config:\n\n        tank        ONLINE\n          sda1      ONLINE\n"
	got := importablePools(out)
	if got["sda1"] != "tank" {
		t.Errorf("sda1 => %q, want tank", got["sda1"])
	}
}

func TestImportablePoolsIgnoresNonDevices(t *testing.T) {
	// Pool named like a disk: its root vdev line must not register as a device.
	// The `id:` line is outside config; a by-id leaf is not a kernel name.
	out := "   pool: sdb\n     id: 12345678901234567890\n  state: ONLINE\n config:\n\n        sdb         ONLINE\n          sdc1      ONLINE\n        ata-WDC-XYZ  ONLINE\n"
	got := importablePools(out)
	if _, ok := got["sdb"]; ok {
		t.Errorf("root vdev/pool name sdb must not be a device: %v", got)
	}
	if got["sdc1"] != "sdb" {
		t.Errorf("sdc1 => %q, want sdb (got %v)", got["sdc1"], got)
	}
	if _, ok := got["ata-WDC-XYZ"]; ok {
		t.Errorf("by-id name must not register: %v", got)
	}
}

func TestClassifyDiskFailClosed(t *testing.T) {
	env := &deviceState{pvs: map[string]bool{"/dev/sdc1": true}, swaps: map[string]bool{"/dev/sdd1": true}, imports: map[string]string{"sdb1": "freehold-thin", "sdz": "freehold-whole"}}
	cases := []struct {
		name       string
		disk       lsblkNode
		clean      bool
		importable string
		freehold   bool
	}{
		{"clean", lsblkNode{Name: "sda", Size: "1.8T", Type: "disk"}, true, "", false},
		{"zfs member importable freehold", lsblkNode{Name: "sdb", Type: "disk", Children: []lsblkNode{{Name: "sdb1", FSType: "zfs_member"}}}, false, "freehold-thin", true},
		{"whole-disk zfs member importable", lsblkNode{Name: "sdz", Type: "disk", FSType: "zfs_member"}, false, "freehold-whole", false},
		{"mounted os disk", lsblkNode{Name: "nvme0", Type: "disk", Children: []lsblkNode{{Name: "nvme0p3", FSType: "LVM2_member"}}}, false, "", false},
		{"pv child", lsblkNode{Name: "sdc", Type: "disk", Children: []lsblkNode{{Name: "sdc1", FSType: "LVM2_member"}}}, false, "", false},
		{"swap child", lsblkNode{Name: "sdd", Type: "disk", Children: []lsblkNode{{Name: "sdd1", FSType: "swap"}}}, false, "", false},
		{"filesystem on partition", lsblkNode{Name: "sde", Type: "disk", Children: []lsblkNode{{Name: "sde1", FSType: "ext4"}}}, false, "", false},
		{"mounted child", lsblkNode{Name: "sdf", Type: "disk", Children: []lsblkNode{{Name: "sdf1", FSType: "ext4", Mountpoint: "/data"}}}, false, "", false},
	}
	for _, c := range cases {
		d := deviceInfoFor(c.disk)
		classifyDisk(d, c.disk, env)
		if d.Clean != c.clean {
			t.Errorf("%s: Clean = %v, want %v (content %q)", c.name, d.Clean, c.clean, d.Content)
		}
		if d.Importable != c.importable {
			t.Errorf("%s: Importable = %q, want %q", c.name, d.Importable, c.importable)
		}
		if d.Freehold.Freehold != c.freehold {
			t.Errorf("%s: freehold = %v, want %v", c.name, d.Freehold.Freehold, c.freehold)
		}
	}
}

func TestInternalLV(t *testing.T) {
	for _, n := range []string{"data_tdata", "data_tmeta", "cache_cdata", "cache_cmeta", "lvol0_pmspare"} {
		if !internalLV(n) {
			t.Errorf("%q must be internal", n)
		}
	}
	for _, n := range []string{"data", "freehold-t-d-cp", "vm-100-disk-0"} {
		if internalLV(n) {
			t.Errorf("%q is user data, not internal", n)
		}
	}
}

func deviceInfoFor(disk lsblkNode) *planebase.DeviceInfo {
	return &planebase.DeviceInfo{Path: "/dev/" + disk.Name, SizeGB: parseHumanGB(disk.Size)}
}
