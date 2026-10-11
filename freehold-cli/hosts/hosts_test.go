package hosts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConfig = `relay_url = 'https://relay.librem.example.test'
relay_ws_url = 'wss://relay.librem.example.test'
cp_url = 'https://cp.librem.example.test'
name = 'librem'
`

// TestHostsVerb pins the tree rename: the edit rewrites the profile's relay/cp
// URLs (the ONLY thing it touches — everything else regenerates from coords on
// the next update), computes the tree targets, and REFUSES a rename that would
// move the world's own domain (every exposed app's name anchors on it) or put
// the relay outside the world's tree.
func TestHostsVerb(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	// Show mode: no flags — prints, changes nothing.
	c := Command()
	c.SetArgs([]string{"--config", cfgPath})
	if err := c.Execute(); err != nil {
		t.Fatalf("show: %v", err)
	}
	if raw, _ := os.ReadFile(cfgPath); !strings.Contains(string(raw), "cp.librem.example.test") {
		t.Fatal("show mode must not touch the config")
	}

	// The tree rename: relay -> chat.<root>, cp -> admin.<root>.
	c = Command()
	c.SetArgs([]string{"--relay", "chat.librem.example.test", "--cp", "admin.librem.example.test", "--config", cfgPath})
	if err := c.Execute(); err != nil {
		t.Fatalf("rename: %v", err)
	}
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"relay_url = 'https://chat.librem.example.test'",
		"relay_ws_url = 'wss://chat.librem.example.test'",
		"cp_url = 'https://admin.librem.example.test'",
	} {
		if !strings.Contains(string(cfg), want) {
			t.Fatalf("the rewrite must carry %s:\n%s", want, cfg)
		}
	}

	// A cp host that moves the world's own domain: refused, config untouched.
	if err := os.WriteFile(cfgPath, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	c = Command()
	c.SetArgs([]string{"--cp", "admin.other.example.test", "--config", cfgPath})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "anchors on it") {
		t.Fatalf("a root-moving cp host must refuse, got %v", err)
	}
	if raw, _ := os.ReadFile(cfgPath); !strings.Contains(string(raw), "cp.librem.example.test") {
		t.Fatal("a refused rename must not touch the config")
	}

	// A relay host outside the world's tree: refused.
	c = Command()
	c.SetArgs([]string{"--relay", "chat.other.example.test", "--config", cfgPath})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "not a child") {
		t.Fatalf("an off-tree relay host must refuse, got %v", err)
	}

	// Renaming the console back onto the root (reverting the re-org) is
	// allowed: the world domain stays put.
	c = Command()
	c.SetArgs([]string{"--cp", "librem.example.test", "--config", cfgPath})
	if err := c.Execute(); err != nil {
		t.Fatalf("console back to the root must be allowed: %v", err)
	}
	if raw, _ := os.ReadFile(cfgPath); !strings.Contains(string(raw), "cp_url = 'https://librem.example.test'") {
		t.Fatalf("the revert must rewrite the cp url:\n%s", raw)
	}
}
