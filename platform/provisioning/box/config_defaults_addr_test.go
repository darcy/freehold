package box

import (
	"os"
	"testing"
)

// The build's exec fallback dials the runner MCP at Flags.Addr. The --addr
// flag is optional, so the profile config's [runner] addr must fill it — an
// empty Addr renders `freehold exec --addr  …`, which dials http:///mcp.
func TestApplyConfigDefaultsFillsRunnerAddr(t *testing.T) {
	cfgPath := writeTempConfig(t, "127.0.0.1:9999")
	f := Flags{}
	if err := ApplyConfigDefaults(&f, cfgPath); err != nil {
		t.Fatal(err)
	}
	if f.Addr != "127.0.0.1:9999" {
		t.Fatalf("Addr = %q, want the config's 127.0.0.1:9999", f.Addr)
	}
}

// An explicit --addr wins over the config's.
func TestApplyConfigDefaultsKeepsExplicitAddr(t *testing.T) {
	cfgPath := writeTempConfig(t, "127.0.0.1:9999")
	f := Flags{Addr: "127.0.0.1:7000"}
	if err := ApplyConfigDefaults(&f, cfgPath); err != nil {
		t.Fatal(err)
	}
	if f.Addr != "127.0.0.1:7000" {
		t.Fatalf("explicit --addr overridden: %q", f.Addr)
	}
}

// A config with no runner addr leaves Addr empty (the build's own fallback
// applies) — the fill must not invent a value.
func TestApplyConfigDefaultsLeavesBlankAddr(t *testing.T) {
	cfgPath := writeTempConfig(t, "")
	f := Flags{}
	if err := ApplyConfigDefaults(&f, cfgPath); err != nil {
		t.Fatal(err)
	}
	if f.Addr != "" {
		t.Fatalf("Addr = %q, want empty", f.Addr)
	}
}

func writeTempConfig(t *testing.T, runnerAddr string) string {
	t.Helper()
	p := t.TempDir() + "/config.toml"
	content := "relay_url = 'https://relay.x.test'\n" +
		"relay_ws_url = 'wss://relay.x.test'\n" +
		"cp_url = 'https://cp.x.test'\n" +
		"name = 't'\n" +
		"host = 'root@h'\n" +
		"operator_pubkey = 'aa'\n" +
		"[runner]\n" +
		"addr = '" + runnerAddr + "'\n" +
		"target = 'proxmox-box'\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
