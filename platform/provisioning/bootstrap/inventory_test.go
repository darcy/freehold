package bootstrap

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

func TestClassifyDiskFailClosed(t *testing.T) {
	env := &deviceState{pvs: map[string]bool{"/dev/sdc1": true}, swaps: map[string]bool{"/dev/sdd1": true}, imports: map[string]string{"sdb1": "freehold-thin"}}
	cases := []struct {
		name       string
		disk       lsblkNode
		clean      bool
		importable string
		freehold   bool
	}{
		{"clean", lsblkNode{Name: "sda", Size: "1.8T", Type: "disk"}, true, "", false},
		{"zfs member importable freehold", lsblkNode{Name: "sdb", Type: "disk", Children: []lsblkNode{{Name: "sdb1", FSType: "zfs_member"}}}, false, "freehold-thin", true},
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

// deviceInfoFor mirrors deviceInfos' construction for classifyDisk tests.
func deviceInfoFor(disk lsblkNode) *planebase.DeviceInfo {
	return &planebase.DeviceInfo{Path: "/dev/" + disk.Name, SizeGB: parseHumanGB(disk.Size)}
}
