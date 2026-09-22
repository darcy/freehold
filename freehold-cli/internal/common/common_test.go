package common

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	"freehold/contract/crypto"
)

// connect_url parity (the Rust connect_url_tests).
func TestConnectURLSchemelessGetsMcpPath(t *testing.T) {
	if got := ConnectURL("127.0.0.1:8787"); got != "http://127.0.0.1:8787/mcp" {
		t.Errorf("= %q", got)
	}
}

func TestConnectURLSchemedAlsoGetsMcpPath(t *testing.T) {
	if got := ConnectURL("http://127.0.0.1:8790"); got != "http://127.0.0.1:8790/mcp" {
		t.Errorf("= %q", got)
	}
}

func TestConnectURLTrailingSlashTolerated(t *testing.T) {
	if got := ConnectURL("http://127.0.0.1:8790/"); got != "http://127.0.0.1:8790/mcp" {
		t.Errorf("= %q", got)
	}
}

func TestConnectURLExplicitMcpNotDuplicated(t *testing.T) {
	if got := ConnectURL("http://127.0.0.1:8790/mcp"); got != "http://127.0.0.1:8790/mcp" {
		t.Errorf("= %q", got)
	}
}

// nsec handling (the CLI's nsec decode).
func TestNsecBech32Roundtrip(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	nsecStr := bech32EncodeNsec(raw)
	out, err := crypto.NsecToSecret(nsecStr)
	if err != nil {
		t.Fatalf("nsec decode failed: %v", err)
	}
	for i := range out {
		if out[i] != raw[i] {
			t.Fatalf("decoded byte %d = %02x, want %02x (round-trip lost data)", i, out[i], raw[i])
		}
	}
}

func TestNsecBareHexAccepted(t *testing.T) {
	secret, err := crypto.NsecToSecret("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	if err != nil {
		t.Fatalf("bare hex rejected: %v", err)
	}
	if secret[0] != 0x01 || secret[31] != 0x20 {
		t.Fatalf("bad secret bytes")
	}
}

func TestNsecBadInputsRejected(t *testing.T) {
	if _, err := crypto.NsecToSecret("notansec"); err == nil {
		t.Fatal("expected error for non-nsec input")
	}
}

// bech32EncodeNsec is a minimal BIP-173 bech32 encoder for a 32-byte payload
// under hrp "nsec" (test-only).
func bech32EncodeNsec(payload []byte) string {
	const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	var data []byte
	var buffer uint32
	var rest uint
	for _, b := range payload {
		buffer = (buffer << 8) | uint32(b)
		rest += 8
		for rest >= 5 {
			rest -= 5
			data = append(data, byte((buffer>>rest)&31))
		}
	}
	if rest > 0 {
		data = append(data, byte((buffer<<(5-rest))&31))
	}
	hrp := []byte("nsec")
	var vals []uint32
	for _, c := range hrp {
		vals = append(vals, uint32(c)>>5)
	}
	vals = append(vals, 0)
	for _, c := range hrp {
		vals = append(vals, uint32(c)&31)
	}
	for _, d := range data {
		vals = append(vals, uint32(d))
	}
	for i := 0; i < 6; i++ {
		vals = append(vals, 0)
	}
	polymod := func(v []uint32) uint32 {
		var chk uint32 = 1
		gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
		for _, vv := range v {
			b := chk >> 25
			chk = ((chk & 0x1ffffff) << 5) ^ vv
			for i := 0; i < 5; i++ {
				if (b>>i)&1 == 1 {
					chk ^= gen[i]
				}
			}
		}
		return chk
	}
	chk := polymod(vals) ^ 1
	var out strings.Builder
	out.WriteString("nsec1")
	for _, d := range data {
		out.WriteByte(charset[d])
	}
	for i := 0; i < 6; i++ {
		out.WriteByte(charset[byte((chk>>uint(5*(5-i)))&31)])
	}
	return out.String()
}

// TestExecConfigFlagScopesToOwningProfile guards the teardown bug: an explicit
// --config must scope the state dir to the OWNING profile, not the base home.
func TestExecConfigFlagScopesToOwningProfile(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { config.SetCurrent(nil) })

	name := "librem3"
	profileCfg := config.NewProfilePath(name)
	if err := (&config.Config{CPURL: "https://cp.example"}).Save(profileCfg); err != nil {
		t.Fatal(err)
	}
	profilePkg := filepath.Join(config.NewProfileState(name), "runner", "proxmox-box")
	if err := MintAgentIdentity(profilePkg); err != nil {
		t.Fatal(err)
	}
	basePkg := filepath.Join(config.DefaultStateHome(), "runner", "proxmox-box")
	if err := MintAgentIdentity(basePkg); err != nil {
		t.Fatal(err)
	}
	profilePK, _ := LoadRPubkey(profilePkg)
	basePK, _ := LoadRPubkey(basePkg)
	if profilePK == basePK {
		t.Fatal("test setup: identities must differ")
	}

	cmd := &cobra.Command{Use: "exec"}
	cmd.Flags().String("config", config.ConfigPath(), "config path")
	if err := cmd.Flags().Set("config", profileCfg); err != nil {
		t.Fatal(err)
	}
	if err := ResolveExecProfile(cmd); err != nil {
		t.Fatalf("ResolveExecProfile: %v", err)
	}
	if got := DefaultAgentDir(); !strings.Contains(got, filepath.Join("profiles", name, "control-plane", "agent-ops")) {
		t.Fatalf("agent-dir not scoped to owning profile: %s", got)
	}
	pk, err := ResolveRunnerPubkey("proxmox-box", "")
	if err != nil {
		t.Fatal(err)
	}
	if pk == basePK {
		t.Fatalf("runner pubkey resolved to the stale base package (%s)", basePK)
	}
	if pk != profilePK {
		t.Fatalf("runner pubkey = %s, want the owning profile's %s", pk, profilePK)
	}
}

// TestExecSingleProfileScopesAgentDir guards the review finding: exec
// negotiation must scope BOTH the runner-pubkey lookup AND the signing identity
// dir to the negotiated profile.
func TestExecSingleProfileScopesAgentDir(t *testing.T) {
	t.Setenv("FREEHOLD_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Cleanup(func() { config.SetCurrent(nil) })

	name := "prod"
	if err := (&config.Config{CPURL: "https://cp.example"}).Save(config.NewProfilePath(name)); err != nil {
		t.Fatal(err)
	}
	if config.Resolve(name) == nil {
		t.Fatal("test profile not registered")
	}

	cmd := &cobra.Command{Use: "exec"}
	cmd.Flags().String("config", config.ConfigPath(), "config path")

	if err := ResolveExecProfile(cmd); err != nil {
		t.Fatalf("ResolveExecProfile: %v", err)
	}
	if got := config.ConfigPath(); got != config.NewProfilePath(name) {
		t.Fatalf("config path not scoped to profile: %s", got)
	}
	got := DefaultAgentDir()
	if !strings.Contains(got, filepath.Join("profiles", name, "control-plane", "agent-ops")) {
		t.Fatalf("agent-dir not scoped to profile: %s", got)
	}
}
