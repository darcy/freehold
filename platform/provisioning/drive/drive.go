// Package drive reproduces the durable-plane storage driver
// (originally drive.rs): the read-only storage-info snapshot for the
// TUI's DATA view, driven through the runner's ONE exec primitive.
package drive

import (
	"fmt"
	"strconv"
	"strings"

	"freehold/platform/provisioning/bootstrap"
	"freehold/contract/client"
	"freehold/platform/provisioning/planebase"
)

// MountUsage is one durable-plane mount's host size/used + guest liveness.
type MountUsage struct {
	Role         string
	Source       string
	Guest        string
	Size         *uint64
	Used         *uint64
	GuestMounted *bool
}

// StorageInfo is the live, read-only plane snapshot.
type StorageInfo struct {
	Capacity string
	Mounts   []MountUsage
}

// HumanBytes renders a short capacity string ("1.2T" / "814.7M" / "40K").
func HumanBytes(b uint64) string {
	const KIB = 1024.0
	v := float64(b)
	switch {
	case v >= KIB*KIB*KIB*KIB:
		return fmt.Sprintf("%.1fT", v/(KIB*KIB*KIB*KIB))
	case v >= KIB*KIB*KIB:
		return fmt.Sprintf("%.1fG", v/(KIB*KIB*KIB))
	case v >= KIB*KIB:
		return fmt.Sprintf("%.0fM", v/(KIB*KIB))
	case v >= KIB:
		return fmt.Sprintf("%.0fK", v/KIB)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// HostCapacity: ZFS = zpool size/alloc/free; LVM = VG size/free + thin-pool
// data%. Advisory — degrades to "—" on probe failure.
func HostCapacity(c *client.McpClient, target string, kind planebase.BackendKind, pool string) string {
	switch kind {
	case planebase.KindZfs:
		out, err := bootstrap.Exec(c, target, "zpool list -H -p -o size,alloc,free "+pool+" 2>/dev/null || true", 60)
		if err != nil || out.ExitCode == nil || *out.ExitCode != 0 {
			return "zpool capacity unreadable"
		}
		var nums []uint64
		for _, f := range strings.Fields(out.Stdout) {
			if n, err := strconv.ParseUint(f, 10, 64); err == nil {
				nums = append(nums, n)
			}
		}
		if len(nums) >= 3 {
			return fmt.Sprintf("zpool %s · %s alloc of %s · %s free", pool, HumanBytes(nums[1]), HumanBytes(nums[0]), HumanBytes(nums[2]))
		}
		return "zpool capacity unreadable"
	case planebase.KindLvmThin:
		out, err := bootstrap.Exec(c, target, "vgs --noheadings --units b -o vg_size,vg_free "+pool+" 2>/dev/null || true", 60)
		var size, free *uint64
		if err == nil && out.ExitCode != nil && *out.ExitCode == 0 {
			var nums []uint64
			for _, f := range strings.Fields(out.Stdout) {
				f = strings.TrimRight(f, "Bb")
				if n, err := strconv.ParseUint(f, 10, 64); err == nil {
					nums = append(nums, n)
				}
			}
			if len(nums) >= 2 {
				size, free = &nums[0], &nums[1]
			}
		}
		var s string
		if size != nil && free != nil {
			s = fmt.Sprintf("vg %s · %s of %s · %s free", pool, HumanBytes(*size-*free), HumanBytes(*size), HumanBytes(*free))
		} else {
			s = fmt.Sprintf("vg %s · size unreadable", pool)
		}
		if p := thinPoolDataPct(c, target, pool); p != nil {
			s += fmt.Sprintf(" · thin pool data %.1f%%", *p)
		}
		return s
	default:
		return "—"
	}
}

func thinPoolDataPct(c *client.McpClient, target, vg string) *float64 {
	out, err := bootstrap.Exec(c, target, "lvs -a --noheadings -o lv_name,data_percent "+vg+" 2>/dev/null || true", 60)
	if err != nil {
		return nil
	}
	return parseLvsDataPct(out.Stdout)
}

func parseLvsDataPct(stdout string) *float64 {
	type row struct {
		name string
		pct  *float64
	}
	var rows []row
	for _, line := range strings.Split(stdout, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		name := strings.Trim(parts[0], "[]")
		var p *float64
		if f, err := strconv.ParseFloat(parts[1], 64); err == nil {
			p2 := f
			p = &p2
		}
		rows = append(rows, row{name, p})
	}
	var pool string
	for _, r := range rows {
		if strings.HasSuffix(r.name, "_tmeta") {
			pool = strings.TrimSuffix(r.name, "_tmeta")
		}
	}
	if pool == "" {
		return nil
	}
	hasData := false
	for _, r := range rows {
		if r.name == pool+"_tdata" {
			hasData = true
		}
	}
	if !hasData {
		return nil
	}
	for _, r := range rows {
		if r.name == pool {
			return r.pct
		}
	}
	return nil
}

// ParseHLine parses one "H <source> <size> <used>" probe line ("-" = absent).
func ParseHLine(line string) (src string, size, used *uint64) {
	rest, ok := strings.CutPrefix(line, "H ")
	if !ok {
		return
	}
	parts := strings.Fields(rest)
	if len(parts) < 3 {
		return
	}
	if parts[1] != "-" {
		if n, err := strconv.ParseUint(parts[1], 10, 64); err == nil {
			size = &n
		}
	}
	if parts[2] != "-" {
		if n, err := strconv.ParseUint(parts[2], 10, 64); err == nil {
			used = &n
		}
	}
	return parts[0], size, used
}

// ParseGLine parses one "G <vmid> <guest> ok|down" probe line.
func ParseGLine(line string) (vmid uint32, mp string, ok bool, valid bool) {
	rest, validp := strings.CutPrefix(line, "G ")
	if !validp {
		return
	}
	parts := strings.Fields(rest)
	if len(parts) < 3 {
		return
	}
	n, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return
	}
	return uint32(n), parts[1], parts[2] == "ok", true
}

// MountArg is one mount reference for StorageInfo.
type MountArg struct {
	Role   string
	Source string
	Guest  string
	VMID   *uint32
}

// StorageInfo probes the live plane: host size/used per mount source plus
// guest bind-mount liveness, and the host capacity summary. mounts reference
// (role, source, guest, vmid); vmid nil skips the guest probe.
func ProbeStorage(c *client.McpClient, target string, kind planebase.BackendKind, pool string, mounts []MountArg) (*StorageInfo, error) {
	var srcs []string
	for _, m := range mounts {
		srcs = append(srcs, m.Source)
	}
	hOut, err := ExecHost(c, target, strings.Join(srcs, " "))
	if err != nil {
		return nil, err
	}
	host := map[string][2]*uint64{}
	for _, line := range strings.Split(hOut, "\n") {
		if src, size, used := ParseHLine(line); src != "" {
			host[src] = [2]*uint64{size, used}
		}
	}
	guests := map[[2]interface{}]bool{}
	var pairs []string
	for _, m := range mounts {
		if m.VMID != nil {
			pairs = append(pairs, fmt.Sprintf("%d:%s", *m.VMID, m.Guest))
		}
	}
	if len(pairs) > 0 {
		gOut, err := ExecGuests(c, target, strings.Join(pairs, " "))
		if err == nil {
			for _, line := range strings.Split(gOut, "\n") {
				if vmid, mp, ok, valid := ParseGLine(line); valid {
					guests[[2]interface{}{vmid, mp}] = ok
				}
			}
		}
	}
	capacity := HostCapacity(c, target, kind, pool)
	var rows []MountUsage
	for _, m := range mounts {
		sz, us := host[m.Source][0], host[m.Source][1]
		var gm *bool
		if m.VMID != nil {
			if v, found := guests[[2]interface{}{*m.VMID, m.Guest}]; found {
				gm = &v
			}
		}
		rows = append(rows, MountUsage{Role: m.Role, Source: m.Source, Guest: m.Guest, Size: sz, Used: us, GuestMounted: gm})
	}
	return &StorageInfo{Capacity: capacity, Mounts: rows}, nil
}

// ExecHost runs the host source probe (zfs used/avail or df).
func ExecHost(c *client.McpClient, target, srcList string) (string, error) {
	cmd := `for p in ` + srcList + `; do if zfs list -H "$p" >/dev/null 2>&1; then echo "H $p $(zfs list -H -p -o used,avail "$p" | awk '{print $1+$2 " " $1}')"; elif mountpoint -q "$p" 2>/dev/null; then echo "H $p $(df -B1 "$p" | tail -1 | awk '{print $2 " " $3}')"; else echo "H $p - -"; fi; done`
	out, err := bootstrap.Exec(c, target, cmd, 120)
	if err != nil {
		return "", err
	}
	return out.Stdout, nil
}

// ExecGuests runs the guest bind-mount liveness probe.
func ExecGuests(c *client.McpClient, target, spec string) (string, error) {
	cmd := `for s in ` + spec + `; do vmid=${s%%:*}; mp=${s#*:}; if pct status "$vmid" 2>/dev/null | grep -q running; then if pct exec "$vmid" -- mountpoint -q "$mp" 2>/dev/null; then echo "G $vmid $mp ok"; else echo "G $vmid $mp down"; fi; else echo "G $vmid $mp down"; fi; done`
	out, err := bootstrap.Exec(c, target, cmd, 120)
	if err != nil {
		return "", err
	}
	return out.Stdout, nil
}
