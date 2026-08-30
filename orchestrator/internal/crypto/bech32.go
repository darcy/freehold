package crypto

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// BIP-173 bech32 charset.
const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32PolymodStep(acc uint32, v uint32) uint32 {
	b := acc >> 25
	acc = ((acc & 0x1ffffff) << 5) ^ v
	var gen = [5]uint32{
		0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3,
	}
	for i := 0; i < 5; i++ {
		if (b>>i)&1 == 1 {
			acc ^= gen[i]
		}
	}
	return acc
}

// bech32Decode verifies the BIP-173 checksum (over the EXPANDED hrp) and
// returns (hrp, payload-bytes). Mirrors core/src/identity.rs::bech32_decode.
func bech32Decode(s string) (string, []byte, error) {
	s = strings.ToLower(s)
	pos := strings.LastIndexByte(s, '1')
	if pos < 0 {
		return "", nil, fmt.Errorf("bech32 string must contain a '1' separator")
	}
	hrp, data := s[:pos], s[pos+1:]
	if hrp == "" {
		return "", nil, fmt.Errorf("bech32 string has an empty hrp")
	}
	if len(data) < 6 {
		return "", nil, fmt.Errorf("bech32 string too short")
	}
	// Checksum over the EXPANDED hrp: (hi bits, 0, lo bits), then data chars.
	vals := make([]uint32, 0, len(hrp)*2+1+len(data))
	for _, b := range []byte(hrp) {
		vals = append(vals, uint32(b)>>5)
	}
	vals = append(vals, 0)
	for _, b := range []byte(hrp) {
		vals = append(vals, uint32(b)&31)
	}
	for _, c := range []byte(data) {
		p := strings.IndexByte(bech32Charset, c)
		if p < 0 {
			return "", nil, fmt.Errorf("invalid bech32 char %q", c)
		}
		vals = append(vals, uint32(p))
	}
	acc := uint32(1)
	for _, v := range vals {
		acc = bech32PolymodStep(acc, v)
	}
	if acc != 1 {
		return "", nil, fmt.Errorf("bad bech32 checksum (typo?)")
	}
	raw := data[:len(data)-6]
	var bits uint32
	var accv uint32
	var out []byte
	for _, c := range []byte(raw) {
		p := strings.IndexByte(bech32Charset, c)
		if p < 0 {
			return "", nil, fmt.Errorf("invalid bech32 char %q", c)
		}
		accv = (accv << 5) | uint32(p)
		bits += 5
		if bits >= 8 {
			bits -= 8
			out = append(out, byte((accv>>bits)&0xff))
		}
	}
	return hrp, out, nil
}

// NsecToSecret decodes an operator's nsec to its 32-byte secret. Accepts
// `nsec1<bech32>` OR bare 64-hex — reproducing orchestrator/src/cli.rs::
// nsec_to_secret. The bech32 payload IS the secret (no 5->8 repack).
func NsecToSecret(s string) ([32]byte, error) {
	var out [32]byte
	if strings.HasPrefix(s, "nsec1") {
		hrp, bytes, err := bech32Decode(s)
		if err != nil {
			return out, fmt.Errorf("bad nsec1 encoding: %w", err)
		}
		if hrp != "nsec" {
			return out, fmt.Errorf("not an nsec hrp (got %s)", hrp)
		}
		if len(bytes) != 32 {
			return out, fmt.Errorf(
				"nsec1 payload is %d bytes, not 32 — a Nostr nsec is a 32-BYTE secret (~63 chars, nsec1 + bech32). Re-copy the FULL nsec from your wallet (check: `echo -n <key> | wc -c` = 63)",
				len(bytes))
		}
		copy(out[:], bytes)
		wipe(bytes)
		return out, nil
	}
	if isHex64(s) {
		dec, err := hex.DecodeString(s)
		if err != nil {
			return out, fmt.Errorf("invalid hex secret: %w", err)
		}
		copy(out[:], dec)
		return out, nil
	}
	return out, fmt.Errorf("expected nsec1<bech32> or a 64-character hex secret")
}

// the 64-hex form — reproducing core/src/identity.rs::parse_pubkey_input,
// including the TRIM and the hex LOWERCASING (uppercase hex, though valid,
// would exact-mismatch the CP console's admin whitelist, which the relay
// keys by the lowercase form).
func ParsePubkeyInput(input string) (string, error) {
	t := strings.TrimSpace(input)
	if strings.HasPrefix(t, "npub1") {
		hrp, out, err := bech32Decode(t)
		if err != nil {
			return "", fmt.Errorf("bad npub1 encoding: %w", err)
		}
		if hrp != "npub" {
			return "", fmt.Errorf("expected npub1... prefix, got %s1...", hrp)
		}
		if len(out) != 32 {
			return "", fmt.Errorf("npub payload is %d bytes, expected 32", len(out))
		}
		return hex.EncodeToString(out), nil
	}
	if isHex64(t) {
		hex.DecodeString(t) // validated by isHex64
		return strings.ToLower(t), nil
	}
	return "", fmt.Errorf("expected npub1<bech32> or a 64-character hex pubkey")
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// wipe zeroes a byte slice (secret hygiene; matches the Rust zeroize intent).
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
