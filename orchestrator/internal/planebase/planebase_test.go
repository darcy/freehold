package planebase

import (
	"strings"
	"testing"
)

func TestDatasetPathCarriesWorldAndTenant(t *testing.T) {
	cases := []struct {
		pool, domain string
		tenant       Tenant
		want         string
	}{
		{"rpool", "freehold-test.darcydev.net", TenantRelay, "rpool/freehold/freehold-test-darcydev-net/relay"},
		{"rpool", "freehold-test.darcydev.net", TenantCp, "rpool/freehold/freehold-test-darcydev-net/cp"},
		{"rpool", "freehold-test.darcydev.net", TenantK3sVolumes, "rpool/freehold/freehold-test-darcydev-net/k3s-volumes"},
	}
	for _, c := range cases {
		got, err := DatasetPath(c.pool, c.domain, c.tenant)
		if err != nil {
			t.Fatalf("DatasetPath errored: %v", err)
		}
		if got != c.want {
			t.Errorf("DatasetPath(%s,%s,%s) = %q want %q", c.pool, c.domain, c.tenant.String(), got, c.want)
		}
	}
}

func TestRelayChildSitsUnderRelayTenantParent(t *testing.T) {
	if got, _ := RelayChildDataset("rpool", "t.d", RelayChildDockerRoot); got != "rpool/freehold/t-d/relay/docker-root" {
		t.Errorf("docker-root = %q", got)
	}
	if got, _ := RelayChildDataset("rpool", "t.d", RelayChildDeployDir); got != "rpool/freehold/t-d/relay/deploy" {
		t.Errorf("deploy = %q", got)
	}
}

func TestVpsLabelIsFlatWithDashes(t *testing.T) {
	got, err := VpsVolumeLabel("freehold-test.darcydev.net", TenantRelay)
	if err != nil {
		t.Fatal(err)
	}
	if got != "fh-freehold-test-darcydev-net-relay" {
		t.Errorf("label = %q", got)
	}
	for _, r := range got {
		if r == '.' {
			t.Fatal("flattened label has no dots")
		}
	}
}

func TestLvmLVNameIsFlatAndDerivable(t *testing.T) {
	got, _ := LvmLVName("freehold-test.darcydev.net", TenantRelay)
	if got != "freehold-freehold-test-darcydev-net-relay" {
		t.Errorf("lv name = %q", got)
	}
	child, _ := LvmRelayChildLVName("t.d", RelayChildDockerRoot)
	if child != "freehold-t-d-relay-docker-root" {
		t.Errorf("relay child lv = %q", child)
	}
	cp, _ := LvmLVName("a.b", TenantCp)
	if strings.Contains(cp, ".") || strings.Contains(cp, "/") {
		t.Errorf("lvm name must be flat: %q", cp)
	}
}

func TestBadDomainIsRejected(t *testing.T) {
	if _, err := DatasetPath("rpool", "has space", TenantCp); err == nil {
		t.Fatal("expected error for domain with space")
	}
	if _, err := DatasetPath("rpool", "", TenantCp); err == nil {
		t.Fatal("expected error for empty domain")
	}
}

func TestBackupFlag(t *testing.T) {
	if BackupFlag("/var/lib/docker") != 1 {
		t.Error("docker root must be backup=1")
	}
	if BackupFlag("/srv/data/relay") != 1 {
		t.Error("/srv/data mount must be backup=1")
	}
	if BackupFlag("/srv/nobackup") != 0 {
		t.Error("non-data mount must be backup=0")
	}
}
