package drive

import (
	"strings"
	"testing"

	"freehold/contract/client"
	"freehold/platform/provisioning/planebase"
)

// fakeExec records commands and answers them from a script.
type fakeExec struct {
	seen []string
}

func okOut(s string) *client.ExecOutcome {
	code := 0
	return &client.ExecOutcome{Stdout: s, ExitCode: &code}
}

func (f *fakeExec) exec(cmd string, _ uint64) (*client.ExecOutcome, error) {
	f.seen = append(f.seen, cmd)
	if strings.Contains(cmd, "echo present") {
		return okOut("absent\n"), nil
	}
	return okOut(""), nil
}

func TestResolveDirMountsRelayTwoChildren(t *testing.T) {
	fx := &fakeExec{}
	mounts, err := ResolveDirMounts(fx.exec, "relay.example.com", planebase.TenantRelay)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("relay keeps two children, got %d", len(mounts))
	}
	if mounts[0].Source != "/srv/data/planes/relay-example-com/docker-root" || mounts[0].GuestPath != planebase.GuestPathDockerRoot {
		t.Fatalf("docker-root mount: %+v", mounts[0])
	}
	if mounts[1].Source != "/srv/data/planes/relay-example-com/deploy" || mounts[1].GuestPath != planebase.GuestPathRelayDeploy {
		t.Fatalf("deploy mount: %+v", mounts[1])
	}
	// The dir is created AND chowned to the guest's shifted uid (the locked
	// guest-writable rule).
	joined := strings.Join(fx.seen, "\n")
	if !strings.Contains(joined, "mkdir -p /srv/data/planes/relay-example-com/docker-root") {
		t.Fatalf("mkdir missing: %s", joined)
	}
	if !strings.Contains(joined, "chown 100000:100000") {
		t.Fatalf("chown missing: %s", joined)
	}
}

func TestResolveDirMountsDomainKeyed(t *testing.T) {
	fx := &fakeExec{}
	_, err := ResolveDirMounts(fx.exec, "cp.example.com", planebase.TenantCp)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	joined := strings.Join(fx.seen, "\n")
	if !strings.Contains(joined, "/srv/data/planes/cp-example-com/cp") {
		t.Fatalf("cp mount missing: %s", joined)
	}
	if strings.Contains(joined, "cp-example-com/relay") {
		t.Fatalf("worlds must not share a subtree: %s", joined)
	}
}

func TestDestroyDirTenantAbsentIsNoop(t *testing.T) {
	fx := &fakeExec{} // answers "absent" to the presence probe
	destroyed, err := DestroyDirTenant(fx.exec, "relay.example.com", planebase.TenantRelay)
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if destroyed {
		t.Fatal("absent subtree must be a no-op")
	}
	if strings.Contains(strings.Join(fx.seen, "\n"), "rm -rf") {
		t.Fatal("absent subtree must never rm")
	}
}
