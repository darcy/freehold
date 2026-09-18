package proxmox

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"freehold/contract/client"
	"freehold/platform/provisioning/planebase"
	"freehold/providers/proxmox/drive"
)

// This file is the READ-ONLY storage inventory: it enumerates every zpool, LVM
// volume group (with its thin pools, their riders, and PVE's local-lvm pointer)
// and every whole-disk candidate, classifying each with the DATA each one
// carries. Nothing here mutates a host; the selection/consent discipline lives
// in planebase + the CLIs. Detection is fail-closed: anything the probe cannot
// positively prove is empty is reported as NOT clean.

// StorageInventory probes the host's storage in one read-only pass.
func StorageInventory(c *client.McpClient, target string) (planebase.Inventory, error) {
	inv := planebase.Inventory{}

	zpools, err := zpoolInfos(c, target)
	if err != nil {
		return inv, err
	}
	inv.Zpools = zpools

	vgs, err := vgInfos(c, target)
	if err != nil {
		return inv, err
	}
	inv.VGs = vgs

	devices, err := deviceInfos(c, target)
	if err != nil {
		return inv, err
	}
	inv.Devices = devices
	return inv, nil
}

// ---- zpools -----------------------------------------------------------------

func zpoolInfos(c *client.McpClient, target string) ([]planebase.ZpoolInfo, error) {
	out, err := Exec(c, target, "zpool list -H -o name,size,free,health 2>/dev/null || true", 60)
	if err != nil {
		return nil, err
	}
	var pools []planebase.ZpoolInfo
	for _, line := range strings.Split(out.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		z := planebase.ZpoolInfo{
			Name:   fields[0],
			SizeGB: parseHumanGB(fields[1]),
			FreeGB: parseHumanGB(fields[2]),
			Health: fields[3],
		}
		// Dataset audit: how much lives here and whether any of it is
		// freehold's (the reconnect signal). Best-effort: an absent zfs
		// command leaves the pool with zero datasets, not an error.
		if ds, err := Exec(c, target, "zfs list -H -r -o name "+z.Name+" 2>/dev/null || true", 60); err == nil {
			for _, d := range strings.Split(ds.Stdout, "\n") {
				d = strings.TrimSpace(d)
				if d == "" || d == z.Name {
					continue
				}
				z.Datasets++
				if dom, ok := planebase.FreeholdDataset(d); ok {
					z.Freehold.Add(dom)
				}
			}
		}
		pools = append(pools, z)
	}
	return pools, nil
}

// ---- LVM volume groups ------------------------------------------------------

func vgInfos(c *client.McpClient, target string) ([]planebase.VGInfo, error) {
	out, err := Exec(c, target, "vgs --noheadings --units g --nosuffix -o vg_name,vg_size,vg_free 2>/dev/null || true", 60)
	if err != nil {
		return nil, err
	}
	localPool, err := localLvmPool(c, target)
	if err != nil {
		return nil, err
	}
	var vgs []planebase.VGInfo
	for _, line := range strings.Split(out.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		vg := planebase.VGInfo{
			Name:   fields[0],
			SizeGB: parseHumanGB(fields[1]),
			FreeGB: parseHumanGB(fields[2]),
		}
		pools, err := PoolInfos(c, target, vg.Name, localPool)
		if err != nil {
			return nil, err
		}
		vg.Pools = pools
		vgs = append(vgs, vg)
	}
	return vgs, nil
}

// PoolInfos audits one VG's thin pools: size, usage, every LV riding each pool
// (the guest disks + freehold volumes sharing its capacity), freehold
// provenance, and whether PVE's local-lvm points at it.
func PoolInfos(c *client.McpClient, target, vg, localPool string) ([]planebase.PoolInfo, error) {
	names, err := drive.ThinPools(c, target, vg)
	if err != nil {
		return nil, err
	}
	out, err := Exec(c, target,
		fmt.Sprintf("lvs --noheadings --units g --nosuffix -o lv_name,lv_size,pool_lv,data_percent %s 2>/dev/null || true", vg), 60)
	if err != nil {
		return nil, err
	}
	byName := map[string]*planebase.PoolInfo{}
	var ordered []*planebase.PoolInfo
	for _, n := range names {
		p := &planebase.PoolInfo{Name: n, LocalLvm: n == localPool}
		if planebase.FreeholdPoolName(n) {
			p.Freehold.Freehold = true
		}
		byName[n] = p
		ordered = append(ordered, p)
	}
	for _, line := range strings.Split(out.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		lv := strings.Trim(fields[0], "[]")
		if p, ok := byName[lv]; ok {
			// Pool row: lv_name lv_size data_percent (the blank pool_lv
			// column collapses under Fields), so the percent is the LAST
			// field either way.
			if len(fields) >= 2 {
				p.SizeGB = uint64(parseFloat(fields[1]))
			}
			if len(fields) >= 3 {
				p.DataPercent = parseFloat(fields[len(fields)-1])
			}
			continue
		}
		// A thin volume: lv_name lv_size pool_lv data_percent. Skip a pool's
		// OWN internal volumes (`_tdata`/`_tmeta`/`_cdata`/`_cmeta`/
		// `_pmspare`): they are not data anyone could lose, and counting them
		// would make even an empty pool look shared.
		if len(fields) >= 3 && !internalLV(lv) {
			if p, ok := byName[fields[2]]; ok {
				p.Riders = append(p.Riders, lv)
				if dom, fh := planebase.FreeholdLV(lv); fh {
					p.Freehold.Add(dom)
				}
			}
		}
	}
	var pools []planebase.PoolInfo
	for _, p := range ordered {
		pools = append(pools, *p)
	}
	return pools, nil
}

// internalLV reports whether an LV is an LVM thin pool's own bookkeeping
// volume (data/meta cache/pool-metadata spare) rather than user data.
func internalLV(name string) bool {
	for _, suffix := range []string{"_tdata", "_tmeta", "_cdata", "_cmeta", "_pmspare"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// localLvmPool returns the thin pool PVE's stock local-lvm storage points at
// ("" when there is no block/pointer). Read-only.
func localLvmPool(c *client.McpClient, target string) (string, error) {
	out, err := Exec(c, target, drive.LocalLvmProbeScript+" 2>/dev/null || true", 30)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Stdout), nil
}

// ---- whole-disk candidates --------------------------------------------------

type lsblkNode struct {
	Name       string      `json:"name"`
	Size       string      `json:"size"`
	Type       string      `json:"type"`
	FSType     string      `json:"fstype"`
	Mountpoint string      `json:"mountpoint"`
	Model      string      `json:"model"`
	Children   []lsblkNode `json:"children"`
}

type lsblkOut struct {
	Blockdevices []lsblkNode `json:"blockdevices"`
}

// deviceInfos enumerates whole disks and classifies whether freehold could use
// one to create a backend. A disk is Clean only when NOTHING on it (or any
// partition) is a filesystem, a PV, a zpool member, a mount, or swap. Anything
// else is listed as Blocked with a plain reason — freehold never erases a
// device that carries data.
func deviceInfos(c *client.McpClient, target string) ([]planebase.DeviceInfo, error) {
	disks, env, err := deviceEnv(c, target)
	if err != nil {
		return nil, err
	}
	var devices []planebase.DeviceInfo
	for _, disk := range disks {
		if disk.Type != "disk" {
			continue
		}
		d := planebase.DeviceInfo{
			Path:   "/dev/" + disk.Name,
			SizeGB: parseHumanGB(disk.Size),
			Model:  strings.TrimSpace(disk.Model),
		}
		classifyDisk(&d, disk, env)
		devices = append(devices, d)
	}
	return devices, nil
}

// ProbeDevice classifies ONE device path (whole disk) with the same fail-closed
// rules the inventory uses. Called before a create so `zpool create`/`pvcreate`
// can never run blind against a device that carries data.
func ProbeDevice(c *client.McpClient, target, dev string) (planebase.DeviceInfo, error) {
	disks, env, err := deviceEnv(c, target)
	if err != nil {
		return planebase.DeviceInfo{}, err
	}
	name := strings.TrimPrefix(dev, "/dev/")
	for _, disk := range disks {
		if disk.Name != name {
			continue
		}
		d := planebase.DeviceInfo{Path: dev, SizeGB: parseHumanGB(disk.Size), Model: strings.TrimSpace(disk.Model)}
		classifyDisk(&d, disk, env)
		return d, nil
	}
	return planebase.DeviceInfo{Path: dev}, fmt.Errorf("device %s is not a whole disk freehold can see — pass a whole block device (e.g. /dev/sdb)", dev)
}

// deviceEnv gathers the lsblk tree plus the PV/swap/importable-pool sets used
// to classify every disk in one pass.
func deviceEnv(c *client.McpClient, target string) ([]lsblkNode, *deviceState, error) {
	out, err := Exec(c, target, "lsblk -J -o NAME,SIZE,TYPE,FSTYPE,MOUNTPOINT,MODEL 2>/dev/null || true", 60)
	if err != nil {
		return nil, nil, err
	}
	var tree lsblkOut
	if err := json.Unmarshal([]byte(out.Stdout), &tree); err != nil {
		return nil, nil, fmt.Errorf("parse lsblk: %w", err)
	}
	env := &deviceState{pvs: map[string]bool{}, swaps: map[string]bool{}}
	pvsOut, err := Exec(c, target, "pvs --noheadings -o pv_name 2>/dev/null || true", 60)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range strings.Fields(pvsOut.Stdout) {
		env.pvs[strings.TrimSpace(p)] = true
	}
	swapOut, err := Exec(c, target, "swapon --show --noheadings -o NAME 2>/dev/null || true", 60)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range strings.Fields(swapOut.Stdout) {
		env.swaps[strings.TrimSpace(s)] = true
	}
	importOut, err := Exec(c, target, "zpool import 2>/dev/null || true", 60)
	if err != nil {
		return nil, nil, err
	}
	env.imports = importablePools(importOut.Stdout)
	return tree.Blockdevices, env, nil
}

// deviceState is the host-wide context a disk classification needs.
type deviceState struct {
	pvs     map[string]bool
	swaps   map[string]bool
	imports map[string]string
}

// classifyDisk walks a disk and its children, accumulating the plain-language
// reason it cannot be used and the importable-pool (if any) it carries.
func classifyDisk(d *planebase.DeviceInfo, disk lsblkNode, env *deviceState) {
	var reasons []string
	var walk func(n lsblkNode)
	walk = func(n lsblkNode) {
		name := n.Name
		dev := "/dev/" + name
		switch {
		case n.Mountpoint != "":
			reasons = append(reasons, "it is in use (mounted at "+n.Mountpoint+")")
		case env.swaps[dev]:
			reasons = append(reasons, "it is used as swap space")
		case env.pvs[dev]:
			reasons = append(reasons, "it already belongs to an LVM volume group")
		case n.FSType == "zfs_member":
			if pool, ok := env.imports[name]; ok {
				if planebase.FreeholdPoolName(pool) {
					d.Importable = pool
					d.Freehold.Freehold = true
				} else {
					d.Importable = pool
				}
			} else {
				reasons = append(reasons, "it already has a storage pool or filesystem on it")
			}
		case n.FSType != "":
			reasons = append(reasons, "it already has data on it ("+n.FSType+")")
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	// Walk the disk itself (a whole-disk PV / zpool member / filesystem) and
	// every child partition. A partitioned disk's own node carries no fs, so
	// this only adds a reason when the disk genuinely holds something — and a
	// whole-disk zpool member gets the same importable/freehold signal as a
	// partition would.
	walk(disk)
	if len(disk.Children) > 0 && len(reasons) == 0 {
		reasons = append(reasons, "it already has partitions on it")
	}
	d.Content = strings.Join(reasons, "; ")
	d.Clean = len(reasons) == 0 && d.Importable == ""
}

// importablePools associates an available-to-import zpool with each DEVICE that
// carries it, from `zpool import` output. Only the `config:` section is
// scanned, the root vdev line is skipped (a pool can be named like a disk), and
// only real device-name shapes match — so the pool-name/id/action lines never
// masquerade as a device.
func importablePools(text string) map[string]string {
	out := map[string]string{}
	pool := ""
	inConfig := false
	firstConfigLine := true
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(t, "pool:"); ok {
			pool = strings.TrimSpace(name)
			inConfig = false
			continue
		}
		if t == "config:" {
			inConfig = true
			firstConfigLine = true
			continue
		}
		if pool == "" || !inConfig {
			continue
		}
		if t == "" {
			// The tree's blank separator line does not end the section; only
			// the next `pool:` line does.
			continue
		}
		if firstConfigLine {
			// The root vdev line — often the pool name itself.
			firstConfigLine = false
			continue
		}
		for _, f := range strings.Fields(t) {
			if base, ok := deviceLeafBase(f); ok {
				if _, seen := out[base]; !seen {
					out[base] = pool
				}
			}
		}
	}
	return out
}

// deviceLeafBase returns a kernel device-leaf name for a token, or ok=false.
func deviceLeafBase(f string) (string, bool) {
	name := strings.TrimPrefix(f, "/dev/")
	if deviceNameRe.MatchString(name) {
		return name, true
	}
	return "", false
}

var deviceNameRe = regexp.MustCompile(`^(sd[a-z]+[0-9]*|nvme[0-9]+n[0-9]+(p[0-9]+)?|vd[a-z]+[0-9]*|hd[a-z]+[0-9]*|md[0-9]+|dm-[0-9]+|loop[0-9]+)$`)

// ---- small parsers ----------------------------------------------------------

// parseHumanGB converts `1.8T`, `953.9G`, `40G`, `512M` to whole GB.
func parseHumanGB(s string) uint64 {
	if s == "" {
		return 0
	}
	mult := 1.0
	switch s[len(s)-1] {
	case 'T', 't':
		mult = 1024
	case 'G', 'g':
		mult = 1
	case 'M', 'm':
		mult = 1.0 / 1024
	case 'K', 'k':
		mult = 1.0 / (1024 * 1024)
	default:
	}
	num := strings.TrimRight(s, "TtGgMmKk")
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	if gb := v * mult; gb > 0 {
		return uint64(gb)
	}
	return 0
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}
