package nipoa

import (
	"encoding/hex"
	"strings"
	"testing"

	"freehold/contract/crypto"
)

func sk(n byte) []byte {
	s := make([]byte, 32)
	s[31] = n
	return s
}

// agentPkOf is the BIP-340 x-only pubkey of secret n, which the spec's vectors
// state for n = 1 (owner) and n = 2 (agent).
const (
	vectorOwnerPk = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	vectorAgentPk = "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	vectorConds   = "kind=1&created_at<1713957000"
	vectorSig     = "8b7df2575caf0a108374f8471722b233c53f9ff827a8b0f91861966c3b9dd5cb2e189eae9f49d72187674c2f5bd244145e10ff86c9f257ffe65a1ee5f108b369"
)

// TestVectorVerifies pins the preimage construction to the specification: the
// published vector's own signature MUST verify under this package. A drifted
// prefix, separator, or hash would reject a spec-conformant tag — and every tag
// the appliance mints would then be dead on arrival.
func TestVectorVerifies(t *testing.T) {
	if err := Verify(vectorAgentPk, Tag{Owner: vectorOwnerPk, Conditions: vectorConds, Sig: vectorSig}); err != nil {
		t.Fatalf("published NIP-OA vector rejected: %v", err)
	}
}

func TestMintRoundTrips(t *testing.T) {
	tag, err := MintEngram(vectorAgentPk, sk(1))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tag.Owner != vectorOwnerPk {
		t.Errorf("owner = %s, want the pubkey of the minted secret", tag.Owner)
	}
	if tag.Conditions != EngramConditions {
		t.Errorf("conditions = %q, want %q", tag.Conditions, EngramConditions)
	}
	if err := Verify(vectorAgentPk, tag); err != nil {
		t.Fatalf("minted tag does not verify: %v", err)
	}
	// The agent key is the subject of the tag: it never appears in the tag's
	// own fields, and an attestation minted for one agent must not verify for
	// another — the cross-check runs against a THIRD key unrelated to the mint,
	// so the assertion exercises the signature binding rather than tripping the
	// (earlier) self-attestation guard.
	if strings.Contains(tag.JSON(), vectorAgentPk) {
		t.Errorf("agent pubkey leaked into the tag: %s", tag.JSON())
	}
	third := sk(3)
	thirdPk, err := crypto.PubkeyFromSecret(third)
	if err != nil {
		t.Fatalf("third key: %v", err)
	}
	if err := Verify(thirdPk, tag); err == nil {
		t.Errorf("tag minted for one agent verified against an unrelated agent key")
	}
	if err := Verify(vectorOwnerPk, tag); err == nil {
		t.Errorf("tag verified against its own owner (self-attestation accepted)")
	}
}

// TestJSONShape: the CLI parses BUZZ_AUTH_TAG as a four-element JSON array of
// strings; anything else is a user-facing "malformed" error at first write.
func TestJSONShape(t *testing.T) {
	tag := Tag{Owner: vectorOwnerPk, Conditions: EngramConditions, Sig: strings.Repeat("a", 128)}
	got := tag.JSON()
	want := `["auth","` + vectorOwnerPk + `","kind=30174","` + strings.Repeat("a", 128) + `"]`
	if got != want {
		t.Errorf("JSON() =\n %s\nwant\n %s", got, want)
	}
}

// TestSelfAttestationRefused: owner == agent is invalid by spec, and on the
// memory path it is also the shape that makes the relay reject the write.
func TestSelfAttestationRefused(t *testing.T) {
	ownerSecret := sk(7)
	ownerPk, err := pubkeyOf(ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MintEngram(ownerPk, ownerSecret); err == nil {
		t.Fatal("self-attestation minted")
	}
	if err := Verify(ownerPk, Tag{Owner: ownerPk, Conditions: EngramConditions, Sig: strings.Repeat("a", 128)}); err == nil {
		t.Error("self-attestation verified")
	}
}

// TestConditionsGrammar pins the malformed forms the spec says MUST be
// rejected, plus the well-formed ones that must not be.
func TestConditionsGrammar(t *testing.T) {
	bad := []string{
		"kind=1&",               // trailing delimiter
		"&kind=1",               // leading delimiter
		"kind=1&&created_at<2",  // doubled delimiter
		"kind=01",               // leading zero
		"kind=65536",            // out of range
		"kind=1 ",               // whitespace
		"KIND=1",                // wrong case
		"created_at=1",          // wrong operator
		"kind=-1",               // negative
		"kind=+1",               // explicit plus (not canonical base-10)
		"created_at<4294967296", // past the stated bound
		"",                      // empty is VALID: it is the no-op case
	}
	for _, c := range bad[:len(bad)-1] {
		if _, err := Mint(vectorAgentPk, sk(1), c); err == nil {
			t.Errorf("malformed conditions %q accepted", c)
		}
	}
	if _, err := Mint(vectorAgentPk, sk(1), ""); err != nil {
		t.Errorf("empty conditions rejected: %v", err)
	}
	for _, c := range []string{"kind=30174", "kind=0", "kind=1&created_at<1713957000", "created_at>1&created_at<4294967295"} {
		if _, err := Mint(vectorAgentPk, sk(1), c); err != nil {
			t.Errorf("well-formed conditions %q rejected: %v", c, err)
		}
	}
}

// TestTamperedSigRejected covers the two mutations that matter operationally:
// a flipped signature bit and a reordered conditions string (which changes the
// preimage, so a signer that "helpfully" normalized it would break its own tags).
func TestTamperedSigRejected(t *testing.T) {
	tag, err := Mint(vectorAgentPk, sk(1), "kind=1&created_at<1713957000")
	if err != nil {
		t.Fatal(err)
	}
	flipped := Tag{Owner: tag.Owner, Conditions: tag.Conditions, Sig: flipLast(tag.Sig)}
	if err := Verify(vectorAgentPk, flipped); err == nil {
		t.Error("tampered signature verified")
	}
	reordered := Tag{Owner: tag.Owner, Conditions: "created_at<1713957000&kind=1", Sig: tag.Sig}
	if err := Verify(vectorAgentPk, reordered); err == nil {
		t.Error("reordered conditions verified (preimage must be byte-exact)")
	}
}

func TestMintRejectsBadInputs(t *testing.T) {
	if _, err := MintEngram(vectorAgentPk, sk(1)[:31]); err == nil {
		t.Error("short owner secret accepted")
	}
	for _, bad := range []string{"", "Z" + vectorAgentPk[1:], strings.ToUpper(vectorAgentPk)} {
		if _, err := MintEngram(bad, sk(1)); err == nil {
			t.Errorf("agent pubkey %q accepted", bad)
		}
	}
}

func flipLast(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	b[63] ^= 1
	return hex.EncodeToString(b)
}

func pubkeyOf(s []byte) (string, error) { return crypto.PubkeyFromSecret(s) }
