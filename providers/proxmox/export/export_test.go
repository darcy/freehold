package export

import (
	"fmt"
	"strings"
	"testing"

	"freehold/contract/client"
)

// fakeDu models the host's du surface and records every command.
type fakeDu struct {
	cmds   []string
	failOn string
	sizes  map[string]uint64
}

func (f *fakeDu) exec(cmd string, _ uint64) (*client.ExecOutcome, error) {
	f.cmds = append(f.cmds, cmd)
	code := 0
	stdout, stderr := "", ""
	if f.failOn != "" && strings.Contains(cmd, f.failOn) {
		code, stderr = 1, "du: error"
	}
	if strings.HasPrefix(cmd, "du -sb ") {
		// The trailing arg is the source (the --exclude patterns ride
		// before it).
		fl := strings.Fields(cmd)
		src := fl[len(fl)-1]
		if n, ok := f.sizes[src]; ok {
			stdout = fmt.Sprintf("%d %s", n, src)
		} else {
			code, stderr = 1, "du: cannot access"
		}
	}
	return &client.ExecOutcome{Stdout: stdout, Stderr: stderr, ExitCode: &code}, nil
}

func TestEstimateMountsRealBytesFailClosed(t *testing.T) {
	f := &fakeDu{sizes: map[string]uint64{"/plane/cp": 40 << 20, "/plane/relay": 2 << 30}}
	got, total, err := EstimateMounts(f.exec, []string{"/plane/cp", "/plane/relay"}, nil)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if total != 40<<20+2<<30 {
		t.Errorf("total = %d", total)
	}
	if got[0].Source != "/plane/cp" || got[0].Bytes != 40<<20 {
		t.Errorf("cp row: %+v", got[0])
	}
	// A failed du must FAIL, never silently zero the mount in the confirmed
	// estimate.
	f2 := &fakeDu{sizes: map[string]uint64{"/plane/cp": 1}, failOn: "/plane/relay"}
	if _, _, err := EstimateMounts(f2.exec, []string{"/plane/cp", "/plane/relay"}, nil); err == nil {
		t.Fatal("a failed du must abort the estimate, not zero the mount")
	}
	if _, _, err := EstimateMounts(f2.exec, nil, nil); err == nil {
		t.Error("no mounts must error")
	}
}

func TestTarCmdShapesTheBundle(t *testing.T) {
	archive, cmd, err := TarCmd("/srv/nobackup/freehold-export", "librem-20261001", []string{"/freehold/a/cp", "/freehold/a/deploy"}, nil)
	if err != nil {
		t.Fatalf("tar: %v", err)
	}
	if archive != "/srv/nobackup/freehold-export/librem-20261001.tar.gz" {
		t.Errorf("archive = %q", archive)
	}
	if !strings.Contains(cmd, "tar -C / -czf "+archive) {
		t.Errorf("tar cmd wrong: %q", cmd)
	}
	// The members lose the leading slash (tar -C / warns otherwise) and
	// keep their real paths (an untar restores them in place).
	if !strings.Contains(cmd, " freehold/a/cp freehold/a/deploy") {
		t.Errorf("members wrong: %q", cmd)
	}
	if _, _, err := TarCmd("/x", "y", nil, nil); err == nil {
		t.Error("no mounts must error")
	}
}

// TestDriverExcludesRideTheCommands: the daemon-root's storage-driver dirs
// (the pulled images' unpacked layers — 812M of a 1.0G root live-verified)
// are excluded from the du estimate (basename patterns) and the tar (the
// explicit absolute paths); the named volumes stay IN.
func TestDriverExcludesRideTheCommands(t *testing.T) {
	var cmds []string
	f := &fakeDu{sizes: map[string]uint64{"/plane/docker-root": 178 << 20}}
	realExec := f.exec
	exec := ExecFunc(func(cmd string, _ uint64) (*client.ExecOutcome, error) {
		cmds = append(cmds, cmd)
		return realExec(cmd, 300)
	})
	if _, _, err := EstimateMounts(exec, []string{"/plane/docker-root"}, DriverDirs); err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if !strings.Contains(cmds[0], "--exclude=fuse-overlayfs --exclude=overlay2") {
		t.Errorf("the du must skip the driver dirs: %q", cmds[0])
	}
	_, tarCmd, err := TarCmd("/srv/nobackup/freehold-export", "x", []string{"/plane/docker-root"},
		[]string{"/plane/docker-root/fuse-overlayfs", "/plane/docker-root/overlay2"})
	if err != nil {
		t.Fatalf("tar: %v", err)
	}
	if !strings.Contains(tarCmd, "--exclude=/plane/docker-root/fuse-overlayfs") || !strings.Contains(tarCmd, "--exclude=/plane/docker-root/overlay2") {
		t.Errorf("the tar must skip the driver dirs: %q", tarCmd)
	}
}
