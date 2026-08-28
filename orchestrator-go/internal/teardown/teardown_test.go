package teardown

import "testing"

func TestDefaultIsWholeWorld(t *testing.T) {
	if ScopeFor(nil, false) != ScopeWholeWorld {
		t.Error("ScopeFor(nil,false) should be whole-world")
	}
	if ScopeFor(nil, true) != ScopeWholeWorld {
		t.Error("ScopeFor(nil,true) should be whole-world")
	}
}

func TestTenantScopedStaysComputeOnlyWithoutData(t *testing.T) {
	ten := "relay"
	if ScopeFor(&ten, false) != ScopeTenantCompute {
		t.Error("tenant without data should be compute-only")
	}
}

func TestTenantDataAddsDatasetDestroy(t *testing.T) {
	ten := "cp"
	if ScopeFor(&ten, true) != ScopeTenantData {
		t.Error("tenant with data should be data+compute")
	}
}

func TestTenantDatasetPath(t *testing.T) {
	got := TenantDataset("rpool", "t.d", "relay")
	if got != "rpool/freehold/t-d/relay" {
		t.Errorf("dataset = %q", got)
	}
}

func TestK3sVolumesMapsToK3sLxc(t *testing.T) {
	if TenantLxcRole("k3s-volumes") != "k3s" {
		t.Error("k3s-volumes -> k3s")
	}
	if TenantLxcRole("k3s") != "k3s" {
		t.Error("k3s -> k3s")
	}
	if TenantLxcRole("relay") != "relay" {
		t.Error("relay -> relay")
	}
	if TenantLxcRole("cp") != "cp" {
		t.Error("cp -> cp")
	}
	if TenantLxcRole("bogus") != "" {
		t.Error("bogus -> empty")
	}
}
