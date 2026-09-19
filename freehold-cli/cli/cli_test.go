package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"freehold/contract/crypto"
	"freehold/freehold-cli/cli/flows"
)

// connect_url parity (the Rust connect_url_tests).
func TestConnectURLSchemelessGetsMcpPath(t *testing.T) {
	if got := flows.ConnectURL("127.0.0.1:8787"); got != "http://127.0.0.1:8787/mcp" {
		t.Errorf("= %q", got)
	}
}

func TestConnectURLSchemedAlsoGetsMcpPath(t *testing.T) {
	if got := flows.ConnectURL("http://127.0.0.1:8790"); got != "http://127.0.0.1:8790/mcp" {
		t.Errorf("= %q", got)
	}
}

func TestConnectURLTrailingSlashTolerated(t *testing.T) {
	if got := flows.ConnectURL("http://127.0.0.1:8790/"); got != "http://127.0.0.1:8790/mcp" {
		t.Errorf("= %q", got)
	}
}

func TestConnectURLExplicitMcpNotDuplicated(t *testing.T) {
	if got := flows.ConnectURL("http://127.0.0.1:8790/mcp"); got != "http://127.0.0.1:8790/mcp" {
		t.Errorf("= %q", got)
	}
}

// nsec_tests parity (the CLI's nsec handling).
func TestNsecBech32Roundtrip(t *testing.T) {
	// Encode a real 32-byte secret as bech32 nsec and round-trip it, asserting
	// the decoded bytes EXACTLY match the input (IMPORTANT-6 — the previous
	// assertion was unreachable/vacuous).
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
// under hrp "nsec" (test-only; mirrors the Rust bech32 crate's byte encoding).
func bech32EncodeNsec(payload []byte) string {
	const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
	// Convert 8-bit payload to 5-bit groups.
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
	// hrp expand + data + checksum.
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

// Regression (operator report 2026-08-30 — "! teardown failed:  — the pool
// is NOT removed"): teardown's DestroyPool drives this binary with the
// shared runner flags (--addr / --agent-dir). Every `storage` subcommand —
// including destroy-pool — must register them, or the whole --data
// teardown dies on the pool step with "unknown flag: --addr".
func TestStorageSubcommandsAcceptRunnerFlags(t *testing.T) {
	for _, sc := range []*cobra.Command{
		storageResolveCmd, storageEnsureCmd, storageInfoCmd,
		storageDestroyCmd, storageDestroyPoolCmd,
	} {
		for _, flag := range []string{"addr", "agent-dir", "runner-pubkey", "target"} {
			if sc.Flags().Lookup(flag) == nil {
				t.Errorf("storage %s must register --%s", sc.Name(), flag)
			}
		}
	}
}
