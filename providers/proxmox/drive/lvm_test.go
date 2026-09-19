package drive

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"freehold/contract/client"
	"freehold/platform/provisioning/planebase"
)

// fakeRunner is an in-process MCP runner that MODELS the LVM/ZFS surface the
// drive functions drive: an LV set, a per-device fs flag, a mount table, and
// an fstab file. It records every command in order so tests can assert the
// exact command sequence the Rust parity requires.
type fakeRunner struct {
	mu      sync.Mutex
	cmds    []string
	lvs     map[string]bool   // lv names present in the VG
	fs      map[string]bool   // dev path -> has filesystem
	mounted map[string]bool   // host path -> is a mountpoint
	fstab   []string          // fstab lines
	failCmd map[string]int    // cmd substring -> exit code to force
	zfsDS   map[string]bool   // zfs datasets present
	zfsMP   map[string]string // dataset -> mountpoint
	// thinOf: thin volume name -> parent pool name (the pool_lv column;
	// a pool's own row has a blank parent and is not a rider).
	thinOf map[string]string
	// storageCfgThinpool models /etc/pve/storage.cfg's local-lvm thinpool
	// pointer — the BARE pool name (the real block is whitespace-formatted,
	// `\tthinpool data`, no vg prefix and no colon); "" = no pointer line.
	storageCfgThinpool string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		lvs:     map[string]bool{},
		fs:      map[string]bool{},
		mounted: map[string]bool{},
		failCmd: map[string]int{},
		zfsDS:   map[string]bool{},
		zfsMP:   map[string]string{},
		thinOf:  map[string]string{},
	}
}

// run computes the outcome for one command, mutating model state.
func (f *fakeRunner) run(cmd string) (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	for sub, code := range f.failCmd {
		if strings.Contains(cmd, sub) {
			return code, ""
		}
	}

	switch {
	case strings.HasPrefix(cmd, "lvs --noheadings -o pool_lv,lv_name"):
		// Real lvs form (live-verified): riders name their parent pool
		// BARE in the pool_lv column (`data`, never `pve/data`); pools and
		// plain LVs get a blank first field. The VG comes from the
		// command's trailing argument.
		var lines []string
		for lv := range f.lvs {
			if parent := f.thinOf[lv]; parent != "" {
				lines = append(lines, parent+"  "+lv)
			} else {
				lines = append(lines, "  "+lv)
			}
		}
		return 0, strings.Join(lines, "\n")
	case strings.HasPrefix(cmd, "lvs -a --noheadings -o lv_name"):
		// Mirrors the real `-o lv_name` output: one name per line, no
		// attrs column (lvsNames/lvsAll parse with strings.Fields).
		var names []string
		for lv := range f.lvs {
			names = append(names, lv)
		}
		return 0, strings.Join(names, "\n")

	case strings.HasPrefix(cmd, "lvcreate -L"):
		// lvcreate -L 40G -T <vg>/<pool>: carve the fresh thin pool. `lvs -a`
		// shows the pool as its OWN LV (type t) plus the _tdata/_tmeta pair.
		fields := strings.Fields(cmd)
		if len(fields) >= 4 {
			pool := fields[3] // <vg>/<pool>
			name := pool[strings.Index(pool, "/")+1:]
			f.lvs[name] = true
			f.lvs[name+"_tdata"] = true
			f.lvs[name+"_tmeta"] = true
		}
		return 0, ""

	case strings.HasPrefix(cmd, "lvcreate -V"):
		// lvcreate -V 10G -T <vg>/<pool> -n <lv>: a thin volume riding the pool.
		fields := strings.Fields(cmd)
		if len(fields) >= 7 && fields[5] == "-n" {
			f.lvs[fields[6]] = true
			parent := fields[3] // <vg>/<pool>
			f.thinOf[fields[6]] = parent[strings.Index(parent, "/")+1:]
		}
		return 0, ""

	case strings.HasPrefix(cmd, "blkid -s TYPE -o value"):
		dev := strings.TrimSuffix(strings.TrimPrefix(cmd, "blkid -s TYPE -o value "), " 2>/dev/null")
		dev = strings.Fields(dev)[0]
		if f.fs[dev] {
			return 0, "ext4"
		}
		return 2, ""

	case strings.HasPrefix(cmd, "mkfs.ext4 -q"):
		f.fs[strings.Fields(cmd)[2]] = true
		return 0, ""

	case strings.HasPrefix(cmd, "mkdir -p"):
		return 0, ""

	case strings.HasPrefix(cmd, "mountpoint -q"):
		path := strings.Fields(cmd)[2]
		if f.mounted[path] {
			return 0, ""
		}
		return 1, ""

	case strings.HasPrefix(cmd, "mount /dev"):
		fields := strings.Fields(cmd)
		f.mounted[fields[2]] = true
		return 0, ""

	case strings.HasPrefix(cmd, "grep -qxF") && strings.Contains(cmd, "/etc/fstab"):
		// grep -qxF '<line>' /etc/fstab || echo '<line>' | tee -a /etc/fstab
		line := betweenQuotes(cmd)
		if !containsStr(f.fstab, line) {
			f.fstab = append(f.fstab, line)
		}
		return 0, ""

	case strings.HasPrefix(cmd, "umount"):
		dev := strings.Fields(cmd)[1]
		delete(f.mounted, dev)
		// the destroy sequence strips the fstab line via sed; model it.
		var kept []string
		for _, l := range f.fstab {
			if !strings.HasPrefix(l, dev+" ") {
				kept = append(kept, l)
			}
		}
		f.fstab = kept
		return 0, ""

	case strings.HasPrefix(cmd, "lvremove -f"):
		lv := strings.TrimPrefix(cmd, "lvremove -f ")
		name := lv[strings.Index(lv, "/")+1:]
		if !f.lvs[name] {
			return 5, "Failed to find logical volume"
		}
		delete(f.lvs, name)
		// removing a thin POOL takes its _tdata/_tmeta pair with it.
		delete(f.lvs, name+"_tdata")
		delete(f.lvs, name+"_tmeta")
		return 0, ""

	case strings.HasPrefix(cmd, "chown ") && !strings.HasPrefix(cmd, "chown -"):
		return 0, ""

	case strings.Contains(cmd, "grep -A2 '^lvmthin: local-lvm$'"):
		// The LocalLvmProbeScript: bare pool name from the local-lvm block.
		if f.storageCfgThinpool != "" {
			return 0, f.storageCfgThinpool + "\n"
		}
		return 0, ""

	case strings.Contains(cmd, "awk -v tp=") && strings.Contains(cmd, "/etc/pve/storage.cfg"):
		// The LocalLvmRepointScript: parse `-v tp=<pool>` and set the pointer.
		idx := strings.Index(cmd, "-v tp=")
		rest := strings.Fields(cmd[idx+len("-v tp="):])[0]
		f.storageCfgThinpool = rest
		return 0, ""

	case strings.HasPrefix(cmd, "zfs list -H -o name"):
		ds := strings.TrimSuffix(strings.TrimPrefix(cmd, "zfs list -H -o name "), " >/dev/null 2>&1")
		if f.zfsDS[ds] {
			return 0, ds
		}
		return 1, ""

	case strings.HasPrefix(cmd, "zfs create -p"):
		f.zfsDS[strings.Fields(cmd)[3]] = true
		return 0, ""

	case strings.HasPrefix(cmd, "zfs destroy -r"):
		delete(f.zfsDS, strings.Fields(cmd)[3])
		return 0, ""

	case strings.HasPrefix(cmd, "zfs get -H -o value mountpoint"):
		ds := strings.Fields(cmd)[6]
		if mp, ok := f.zfsMP[ds]; ok && f.zfsDS[ds] {
			return 0, mp + "\n"
		}
		return 0, "none\n"

	case strings.HasPrefix(cmd, "zpool list -H -o name"):
		return 0, ""

	case strings.HasPrefix(cmd, "vgs --noheadings"):
		return 0, "pve"

	default:
		return 0, ""
	}
}

func betweenQuotes(s string) string {
	start := strings.Index(s, "'")
	if start < 0 {
		return ""
	}
	end := strings.Index(s[start+1:], "'")
	if end < 0 {
		return ""
	}
	return s[start+1 : start+1+end]
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// serve returns an McpClient wired to an httptest server backed by this fake.
func (f *fakeRunner) serve(t *testing.T) *client.McpClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cmd, _ := req.Params.Arguments["cmd"].(string)
		code, stdout := f.run(cmd)
		out := map[string]interface{}{
			"stdout":    stdout,
			"stderr":    "",
			"exit_code": code,
			"timed_out": false,
		}
		text, _ := json.Marshal(out)
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"isError": false,
				"content": []map[string]interface{}{
					{"type": "text", "text": string(text)},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	// A nonzero signing secret (SignBIP340 rejects the zero scalar); the fake
	// server does not verify the signature — only the JSON-RPC shape matters.
	var secret [32]byte
	secret[31] = 1
	c, err := client.New(srv.URL, &client.AgentAuth{Secret: secret, Pubkey: strings.Repeat("a", 64)}, strings.Repeat("b", 64))
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

func TestEnsureLvmLvFreshVGCarvesPoolThenLV(t *testing.T) {
	f := newFakeRunner()
	c := f.exec()
	err := EnsureLvmLv(c, "pve", "freehold-t-d-cp", "/freehold/t-d/cp", 10, 40, "")
	if err != nil {
		t.Fatalf("EnsureLvmLv: %v", err)
	}
	seq := f.cmds
	// The pool carve must come BEFORE the LV create.
	poolIdx := indexOfContaining(seq, "lvcreate -L 40G -T pve/freehold-thin")
	lvIdx := indexOfContaining(seq, "lvcreate -V 10G -T pve/freehold-thin -n freehold-t-d-cp")
	if poolIdx < 0 || lvIdx < 0 || poolIdx > lvIdx {
		t.Fatalf("pool carve must precede LV create; seq=%v", seq)
	}
	// mkfs (blkid-gated) -> mount -> fstab, all present.
	for _, want := range []string{"mkfs.ext4 -q /dev/pve/freehold-t-d-cp", "mount /dev/pve/freehold-t-d-cp /freehold/t-d/cp", "grep -qxF"} {
		if indexOfContaining(seq, want) < 0 {
			t.Errorf("missing %q in %v", want, seq)
		}
	}
	if !containsStr(f.fstab, "/dev/pve/freehold-t-d-cp /freehold/t-d/cp ext4 defaults 0 2") {
		t.Errorf("fstab missing the mount line: %v", f.fstab)
	}
}

func TestEnsureLvmLvReusesExistingThinPool(t *testing.T) {
	f := newFakeRunner()
	f.lvs["data_tdata"] = true
	f.lvs["data_tmeta"] = true
	c := f.exec()
	err := EnsureLvmLv(c, "pve", "freehold-t-d-cp", "/freehold/t-d/cp", 10, 40, "")
	if err != nil {
		t.Fatalf("EnsureLvmLv: %v", err)
	}
	if i := indexOfContaining(f.cmds, "lvcreate -L"); i >= 0 {
		t.Errorf("must NOT carve a fresh pool when one exists: %v", f.cmds)
	}
	if i := indexOfContaining(f.cmds, "lvcreate -V 10G -T pve/data -n freehold-t-d-cp"); i < 0 {
		t.Errorf("LV must be carved in the EXISTING pool `data`: %v", f.cmds)
	}
}

func TestEnsureLvmLvHonorsOperatorSizes(t *testing.T) {
	f := newFakeRunner()
	c := f.exec()
	if err := EnsureLvmLv(c, "pve", "freehold-t-d-cp", "/freehold/t-d/cp", 12, 30, ""); err != nil {
		t.Fatalf("EnsureLvmLv: %v", err)
	}
	if i := indexOfContaining(f.cmds, "lvcreate -L 30G -T pve/freehold-thin"); i < 0 {
		t.Errorf("pool size must honor poolSizeGB=30: %v", f.cmds)
	}
	if i := indexOfContaining(f.cmds, "lvcreate -V 12G"); i < 0 {
		t.Errorf("LV size must honor lvSizeGB=12: %v", f.cmds)
	}
}

func TestEnsureLvmLvIdempotentSecondRun(t *testing.T) {
	f := newFakeRunner()
	c := f.exec()
	if err := EnsureLvmLv(c, "pve", "freehold-t-d-cp", "/freehold/t-d/cp", 10, 40, ""); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	before := len(f.cmds)
	if err := EnsureLvmLv(c, "pve", "freehold-t-d-cp", "/freehold/t-d/cp", 10, 40, ""); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	second := f.cmds[before:]
	for _, banned := range []string{"lvcreate", "mkfs", "mount /dev"} {
		if i := indexOfContaining(second, banned); i >= 0 {
			t.Errorf("second ensure must not re-%s: %v", banned, second)
		}
	}
	// fstab must NOT be double-appended.
	count := 0
	for _, l := range f.fstab {
		if l == "/dev/pve/freehold-t-d-cp /freehold/t-d/cp ext4 defaults 0 2" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("fstab line appended %d times, want exactly 1: %v", count, f.fstab)
	}
}

func TestResolveLvmMountsRelayTwoChildren(t *testing.T) {
	f := newFakeRunner()
	c := f.exec()
	mounts, err := ResolveLvmMounts(c, "pve", "t.d", planebase.TenantRelay, 10, 40, "")
	if err != nil {
		t.Fatalf("ResolveLvmMounts: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("relay = 2 child mounts, got %d: %v", len(mounts), mounts)
	}
	if mounts[0].Source != "/freehold/t-d/docker-root" || mounts[0].GuestPath != "/var/lib/docker" {
		t.Errorf("docker-root mount wrong: %+v", mounts[0])
	}
	if mounts[1].Source != "/freehold/t-d/deploy" || mounts[1].GuestPath != "/srv/data/relay" {
		t.Errorf("deploy mount wrong: %+v", mounts[1])
	}
	// BOTH LV mount ROOTS chowned to the guest's shifted uid — top dir ONLY
	// (never -R: a surviving plane's container-owned subtrees must not be
	// re-rooted; see ChownGuestUid).
	for _, want := range []string{
		"chown 100000:100000 /freehold/t-d/docker-root",
		"chown 100000:100000 /freehold/t-d/deploy",
	} {
		if indexOfContaining(f.cmds, want) < 0 {
			t.Errorf("missing chown %q: %v", want, f.cmds)
		}
	}
	for _, banned := range []string{"chown -R "} {
		if i := indexOfContaining(f.cmds, banned); i >= 0 {
			t.Errorf("recursive chown on a plane mount root: %v", f.cmds[i])
		}
	}
}

func TestResolveLvmMountsCpSingleMount(t *testing.T) {
	f := newFakeRunner()
	c := f.exec()
	mounts, err := ResolveLvmMounts(c, "pve", "t.d", planebase.TenantCp, 10, 40, "")
	if err != nil {
		t.Fatalf("ResolveLvmMounts: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("cp = 1 mount, got %d", len(mounts))
	}
	if mounts[0].Source != "/freehold/t-d/cp" || mounts[0].GuestPath != "/srv/data/cp" {
		t.Errorf("cp mount wrong: %+v", mounts[0])
	}
}

// TestResolveLvmMountsSurvivingPlaneNeverRecursiveChowns is the live-incident
// regression: a rebuild after compute-only teardown re-resolves a SURVIVING
// plane. The old unconditional `chown -R 100000:100000 <mount>` re-rooted
// every container-owned subtree (redis 999, postgres 70, buzz 1000 -> guest
// root) and every non-root service EACCESed. Surviving LV + fs + mounted
// must yield a NON-recursive top-dir chown and nothing else.
func TestResolveLvmMountsSurvivingPlaneNeverRecursiveChowns(t *testing.T) {
	f := newFakeRunner()
	// BOTH relay children survive the compute-only teardown: LV + fs + mounted.
	for _, pair := range []struct {
		child planebase.RelayChild
		host  string
	}{
		{planebase.RelayChildDockerRoot, "/freehold/t-d/docker-root"},
		{planebase.RelayChildDeployDir, "/freehold/t-d/deploy"},
	} {
		lv, _ := planebase.LvmRelayChildLVName("t.d", pair.child)
		f.lvs[lv] = true
		f.thinOf[lv] = "freehold-thin"
		f.fs["/dev/pve/"+lv] = true
		f.mounted[pair.host] = true
	}
	c := f.exec()
	if _, err := ResolveLvmMounts(c, "pve", "t.d", planebase.TenantRelay, 10, 40, ""); err != nil {
		t.Fatalf("ResolveLvmMounts on surviving plane: %v", err)
	}
	if indexOfContaining(f.cmds, "chown 100000:100000 /freehold/t-d/docker-root") < 0 {
		t.Errorf("surviving mount root must still be chowned (top dir): %v", f.cmds)
	}
	for i, cmd := range f.cmds {
		if strings.HasPrefix(cmd, "chown -R") {
			t.Errorf("recursive chown on surviving plane: cmds[%d]=%q", i, cmd)
		}
	}
	// Pure re-resolution: no lvcreate / mkfs / mount churn on a live LV.
	for _, banned := range []string{"lvcreate", "mkfs.ext4", "mount /dev"} {
		if i := indexOfContaining(f.cmds, banned); i >= 0 {
			t.Errorf("surviving plane must not re-create/re-mount: cmds[%d]=%q", i, f.cmds[i])
		}
	}
}

func TestDestroyLvmTenantUmountSedPrecedesLvremove(t *testing.T) {
	f := newFakeRunner()
	f.lvs["freehold-t-d-cp"] = true
	f.mounted["/freehold/t-d/cp"] = true
	f.fs["/dev/pve/freehold-t-d-cp"] = true
	f.fstab = append(f.fstab, "/dev/pve/freehold-t-d-cp /freehold/t-d/cp ext4 defaults 0 2")
	c := f.exec()

	destroyed, err := DestroyLvmTenant(c, "pve", "t.d", planebase.TenantCp)
	if err != nil {
		t.Fatalf("DestroyLvmTenant: %v", err)
	}
	if !destroyed {
		t.Fatal("destroyed should be true for a present LV")
	}
	umountIdx := indexOfContaining(f.cmds, "umount /dev/pve/freehold-t-d-cp")
	lvremoveIdx := indexOfContaining(f.cmds, "lvremove -f pve/freehold-t-d-cp")
	if umountIdx < 0 || lvremoveIdx < 0 || umountIdx > lvremoveIdx {
		t.Fatalf("umount+fstab-strip must PRECEDE lvremove; seq=%v", f.cmds)
	}
	if len(f.fstab) != 0 {
		t.Errorf("fstab not stripped: %v", f.fstab)
	}
	if f.lvs["freehold-t-d-cp"] {
		t.Error("LV still present after destroy")
	}
}

func TestDestroyLvmTenantAbsentIsNoop(t *testing.T) {
	f := newFakeRunner()
	c := f.exec()
	destroyed, err := DestroyLvmTenant(c, "pve", "t.d", planebase.TenantCp)
	if err != nil {
		t.Fatalf("DestroyLvmTenant: %v", err)
	}
	if destroyed {
		t.Error("absent LV must report destroyed=false")
	}
	if indexOfContaining(f.cmds, "lvremove") >= 0 {
		t.Errorf("no lvremove for an absent LV: %v", f.cmds)
	}
	if indexOfContaining(f.cmds, "umount") >= 0 {
		t.Errorf("no umount for an absent LV: %v", f.cmds)
	}
}

func TestDestroyLvmTenantRelayRemovesBothChildren(t *testing.T) {
	f := newFakeRunner()
	f.lvs["freehold-t-d-relay-docker-root"] = true
	f.lvs["freehold-t-d-relay-deploy"] = true
	c := f.exec()
	destroyed, err := DestroyLvmTenant(c, "pve", "t.d", planebase.TenantRelay)
	if err != nil {
		t.Fatalf("DestroyLvmTenant: %v", err)
	}
	if !destroyed {
		t.Error("relay destroy must report destroyed=true")
	}
	for _, lv := range []string{"freehold-t-d-relay-docker-root", "freehold-t-d-relay-deploy"} {
		if f.lvs[lv] {
			t.Errorf("relay child %s still present", lv)
		}
	}
}

func TestDestroyLvmTenantLvremoveFailureLeavesDataIntact(t *testing.T) {
	f := newFakeRunner()
	f.lvs["freehold-t-d-cp"] = true
	f.failCmd["lvremove"] = 5 // simulate a busy LV refusing removal
	c := f.exec()
	destroyed, err := DestroyLvmTenant(c, "pve", "t.d", planebase.TenantCp)
	if err == nil {
		t.Fatal("a real lvremove failure must surface as an error")
	}
	if !strings.Contains(err.Error(), "data is INTACT") {
		t.Errorf("error must state the data is INTACT: %v", err)
	}
	if destroyed {
		t.Error("destroyed must be false when lvremove failed")
	}
	if !f.lvs["freehold-t-d-cp"] {
		t.Error("LV must survive a failed lvremove")
	}
}

func TestDestroyTenantBackendDispatchesOnKind(t *testing.T) {
	// LVM branch drives lvs/lvremove.
	f := newFakeRunner()
	f.lvs["freehold-t-d-cp"] = true
	c := f.exec()
	destroyed, err := DestroyTenantBackend(c, planebase.KindLvmThin, "pve", "t.d", planebase.TenantCp)
	if err != nil || !destroyed {
		t.Fatalf("lvmth destroy: destroyed=%v err=%v", destroyed, err)
	}
	if indexOfContaining(f.cmds, "lvremove") < 0 {
		t.Error("lvmth branch must drive lvremove")
	}

	// ZFS branch drives zfs destroy.
	fz := newFakeRunner()
	fz.zfsDS["rpool/freehold/t-d/cp"] = true
	fz.zfsMP["rpool/freehold/t-d/cp"] = "/rpool/freehold/t-d/cp"
	cz := fz.exec()
	destroyed, err = DestroyTenantBackend(cz, planebase.KindZfs, "rpool", "t.d", planebase.TenantCp)
	if err != nil || !destroyed {
		t.Fatalf("zfs destroy: destroyed=%v err=%v", destroyed, err)
	}
	if indexOfContaining(fz.cmds, "zfs destroy -r rpool/freehold/t-d/cp") < 0 {
		t.Errorf("zfs branch must drive zfs destroy: %v", fz.cmds)
	}
}

func TestResolveTenantMountsZfsEnsuresAndChowns(t *testing.T) {
	f := newFakeRunner()
	f.zfsMP["rpool/freehold/t-d/cp"] = "/rpool/freehold/t-d/cp"
	c := f.exec()
	mounts, err := ResolveTenantMounts(c, "rpool", "t.d", planebase.TenantCp)
	if err != nil {
		t.Fatalf("ResolveTenantMounts: %v", err)
	}
	if len(mounts) != 1 || mounts[0].Source != "/rpool/freehold/t-d/cp" || mounts[0].GuestPath != "/srv/data/cp" {
		t.Fatalf("cp zfs mount wrong: %+v", mounts)
	}
	if !f.zfsDS["rpool/freehold/t-d/cp"] {
		t.Error("dataset not ensured")
	}
	if indexOfContaining(f.cmds, "chown 100000:100000 /rpool/freehold/t-d/cp") < 0 {
		t.Errorf("zfs mountpoint root must be chowned (non-recursive): %v", f.cmds)
	}
}

// indexOfContaining returns the index of the first cmd containing sub.
func indexOfContaining(cmds []string, sub string) int {
	for i, c := range cmds {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// exec adapts the fakeRunner to the host-exec seam (the production transport
// contract) without going through the MCP server.
func (f *fakeRunner) exec() ExecFunc {
	return func(cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
		code, out := f.run(cmd)
		return &client.ExecOutcome{Stdout: out, ExitCode: &code}, nil
	}
}
