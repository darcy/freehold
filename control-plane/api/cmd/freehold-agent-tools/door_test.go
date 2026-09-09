package main

import (
	"encoding/hex"
	"strings"
	"testing"

	"freehold/contract/crypto"
)

// TestDoorKeyRe proves the DOOR_SPEC §2.5 shell-injection gate: a valid
// authorized_keys line passes; anything with a shell metacharacter or a
// whitespace run is refused before it could reach the shell.
func TestDoorKeyRe(t *testing.T) {
	good := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK+HLuYJgIwF6hxhCPX5hd3p5WmD5vVL3qc5rcZ1uolS box"
	if !doorKeyRe.MatchString(good) {
		t.Fatalf("valid authorized_keys line must pass: %q", good)
	}
	if !doorKeyRe.MatchString("ssh-rsa AAAAB3NzaC1yc2EAAAA user@host") {
		t.Fatal("ssh-rsa line must pass")
	}
	for _, bad := range []string{
		"ssh-ed25519 AAAA$(rm -rf /) box", // command substitution
		"ssh-ed25519 AAAA`id` box",         // backticks
		"ssh-ed25519 AAAA'; rm -rf /; box", // quote breakout
		"ssh-ed25519 AAAA  box",            // whitespace run (double space)
		"ssh-ed25519 AAAA box\n",           // trailing newline
		"ssh-ed25519 AAA\nAAAA box",        // embedded newline
		"random text",                      // not a key line
	} {
		if doorKeyRe.MatchString(bad) {
			t.Fatalf("shell-hostile pubkey must be rejected: %q", bad)
		}
	}
}

// TestSSHPublicKeyFromSeedDeterministic proves the box's door key is stable:
// the same identity seed yields the same public line (so re-login authorizes
// the SAME key, and a revoke actually removes it).
func TestSSHPublicKeyFromSeedDeterministic(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	a, err := crypto.SSHPublicKeyFromSeed(seed, "freehold-door-box")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := crypto.SSHPublicKeyFromSeed(seed, "freehold-door-box")
	if a != b {
		t.Fatalf("same seed must yield the same door key: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "ssh-ed25519 ") {
		t.Fatalf("door key must be an ed25519 line: %q", a)
	}
	other := append([]byte(nil), seed...)
	other[0] ^= 0xff
	c, _ := crypto.SSHPublicKeyFromSeed(other, "freehold-door-box")
	if c == a {
		t.Fatal("different seed must yield a different door key")
	}
	// The seed round-trips through hex (the CLI decodes nostr_secret hex).
	if _, err := hex.DecodeString(hex.EncodeToString(seed)); err != nil {
		t.Fatal(err)
	}
}
