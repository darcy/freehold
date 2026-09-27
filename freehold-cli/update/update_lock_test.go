package update

import "testing"

// TestLockIsPerProfile: the update lock must serialize the SAME world (same
// config path) but NOT different profiles on one box — a shared lock file let
// one profile's stale lock block the others.
func TestLockIsPerProfile(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	unlockA, err := lock("/etc/freehold/a.toml")
	if err != nil {
		t.Fatalf("lock a: %v", err)
	}
	if _, err := lock("/etc/freehold/a.toml"); err == nil {
		t.Fatal("a second lock of the SAME profile must fail while held")
	}
	unlockB, err := lock("/etc/freehold/b.toml")
	if err != nil {
		t.Fatalf("a different profile must lock independently: %v", err)
	}
	unlockB()
	unlockA()
	reA, err := lock("/etc/freehold/a.toml")
	if err != nil {
		t.Fatalf("lock a after release: %v", err)
	}
	reA()
}
