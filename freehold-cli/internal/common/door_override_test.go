package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/config"
)

// TestDoorExecWithKeyOverride pins the transport override: an explicit
// --ssh-key is used VERBATIM (no box DOOR_SPEC derivation — a CP guest has no
// ops identity) and a failed probe fails with the door message, not the
// DOOR_SPEC one.
func TestDoorExecWithKeyOverride(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	if err := os.WriteFile(key, []byte("not-a-real-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Host: "root@127.0.0.1:1"}
	_, _, cleanup, err := DoorExecWithKey(cfg, key)
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	if err == nil {
		t.Fatal("expected the probe to fail against a closed port")
	}
	if strings.Contains(err.Error(), "DOOR_SPEC") {
		t.Fatalf("the override must not fall back to the box's DOOR_SPEC derivation: %v", err)
	}
	if !strings.Contains(err.Error(), "authorized on") {
		t.Fatalf("expected the door-probe failure message: %v", err)
	}
}
