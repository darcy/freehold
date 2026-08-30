package crypto

import (
	"strings"
	"testing"
)

// TestParsePubkeyInput pins the npub/hex acceptance contract every pubkey
// prompt relies on (rebuild/install/bootstrap call this on the operator's
// input) — same fixture as core/src/identity.rs's own test, the Rust
// primitive this reproduces.
func TestParsePubkeyInput(t *testing.T) {
	const hex = "1dc07610f40192b0cef0d008cab7f2e86000682ade2eca5bc52ea2d04b6da157"

	got, err := ParsePubkeyInput("npub1rhq8vy85qxftpnhs6qyv4dljapsqq6p2mchv5k79963dqjmd59tsnsg30j")
	if err != nil || got != hex {
		t.Errorf("npub decode = %q, %v — want %s", got, err, hex)
	}

	// 64-hex is accepted and NORMALIZED to lowercase (Rust's
	// parse_pubkey_input does to_ascii_lowercase): uppercase hex is valid
	// input but must not reach the CP's admin whitelist exact-match.
	if got, err := ParsePubkeyInput(hex); err != nil || got != hex {
		t.Errorf("lowercase hex passthrough = %q, %v", got, err)
	}
	if got, err := ParsePubkeyInput(strings.ToUpper(hex)); err != nil || got != hex {
		t.Errorf("uppercase hex should normalize to lowercase: got %q, %v — want %s", got, err, hex)
	}
	if got, err := ParsePubkeyInput("  " + hex + "\n"); err != nil || got != hex {
		t.Errorf("hex with surrounding whitespace should trim + pass: got %q, %v", got, err)
	}

	for _, bad := range []string{"npub1rhqSY85", "not-a-pubkey", "ABC", strings.Repeat("0", 63)} {
		if _, err := ParsePubkeyInput(bad); err == nil {
			t.Errorf("ParsePubkeyInput(%q) should fail", bad)
		}
	}
}
