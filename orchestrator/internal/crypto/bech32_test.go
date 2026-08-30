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

	// 64-hex passes through untouched.
	if got, err := ParsePubkeyInput(hex); err != nil || got != hex {
		t.Errorf("hex passthrough = %q, %v", got, err)
	}

	for _, bad := range []string{"npub1rhqSY85", "not-a-pubkey", "ABC", strings.Repeat("0", 63)} {
		if _, err := ParsePubkeyInput(bad); err == nil {
			t.Errorf("ParsePubkeyInput(%q) should fail", bad)
		}
	}
}
