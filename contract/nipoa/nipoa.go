// Package nipoa mints and verifies the NIP-OA owner-attestation `auth` tag: the
// credential by which an owner key authorizes an agent key to act as that agent.
// It is the client-side half of the agent memory plane: `buzz mem` reads the
// owner out of this tag (BUZZ_AUTH_TAG), so the tag is what makes an agent's own
// kind:30174 engrams addressable at all.
//
// The tag is four elements: ["auth", <owner-pk-hex>, <conditions>, <sig-hex>].
// The signed preimage is the UTF-8 bytes of
//
//	"nostr:agent-auth:" || event.pubkey || ":" || <conditions>
//
// and the signature is BIP-340 Schnorr over SHA256(preimage) with the OWNER's
// secret. Two rules here are load-bearing, not ceremony:
//
//   - <conditions> is signed verbatim. Reordering, deduplicating, or otherwise
//     normalizing it changes the preimage and invalidates the tag, so callers
//     pass one fixed canonical string and never rebuild it ad hoc.
//   - owner == agent is INVALID (self-attestation must be rejected). It is also
//     operationally fatal on the memory path: an engram's NIP-44 conversation key
//     is derived from the (agent, owner) pair, and the degenerate self-pair is
//     what makes the relay refuse the write for a missing `p` tag. Minting
//     refuses it rather than ship a pod whose memory writes fail at the relay.
package nipoa

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"freehold/contract/crypto"
)

// Domain is the fixed NIP-OA preimage prefix. Its exact bytes are part of the
// signed message: one character of drift yields a tag no verifier accepts.
const Domain = "nostr:agent-auth:"

// AgentEngramKind is the kind the agent memory plane publishes (kind:30174).
// The attestation is bounded to it, so a leaked pod env authorises memory
// writes and nothing else: a holder of the tag cannot post some other kind
// under the owner's attestation.
const AgentEngramKind = 30174

// EngramConditions is the single canonical conditions string this package
// mints. The attestation is bounded to the agent-engram kind, so a stolen pod
// env cannot post some other kind under the owner's name. It is signed
// verbatim (see the package doc) and must never be rebuilt ad hoc; a test
// pins it to AgentEngramKind so the two can never disagree.
const EngramConditions = "kind=30174"

// maxStamp is the protocol's stated upper bound for a created_at clause value;
// maxKind bounds a kind= clause.
const (
	maxStamp = 4294967295
	maxKind  = 65535
)

var hex64 = regexp.MustCompile(`^` + `[\da-f]{64}$`)
var hex128 = regexp.MustCompile(`^` + `[\da-f]{128}$`)

// Tag is a parsed auth tag.
type Tag struct {
	Owner      string // <owner-pk-hex>: the attesting owner
	Conditions string // <conditions>: signed verbatim
	Sig        string // <sig-hex>: 128 lowercase hex
}

// JSON renders the tag exactly as the buzz CLI's --auth-tag / BUZZ_AUTH_TAG
// parser expects it: a four-element JSON array of strings. Serialization goes
// through encoding/json so a Tag assembled outside Mint (a parsed tag, a
// future caller) cannot render malformed output the CLI only reports at the
// agent's first write.
func (t Tag) JSON() string {
	b, err := json.Marshal([]string{"auth", t.Owner, t.Conditions, t.Sig})
	if err != nil {
		// Marshal of []string cannot fail; a Tag must never render a broken tag
		// silently, so the impossible stays loud.
		panic("nipoa: tag JSON: " + err.Error())
	}
	return string(b)
}

// Mint signs an attestation for agentPk under ownerSecret, bounded to
// conditions. Every input is validated first: a half-formed tag is worse than
// no tag, because the CLI accepts it and the relay then rejects the write with
// a message naming neither the tag nor its author.
func Mint(agentPk string, ownerSecret []byte, conditions string) (Tag, error) {
	if len(ownerSecret) != 32 {
		return Tag{}, fmt.Errorf("nipoa: owner secret must be 32 bytes, got %d", len(ownerSecret))
	}
	ownerPk, err := crypto.PubkeyFromSecret(ownerSecret)
	if err != nil {
		return Tag{}, fmt.Errorf("nipoa: owner secret unusable: %w", err)
	}
	if !hex64.MatchString(agentPk) {
		return Tag{}, fmt.Errorf("nipoa: agent pubkey must be 64 lowercase hex (got %d chars)", len(agentPk))
	}
	if agentPk == ownerPk {
		return Tag{}, fmt.Errorf("nipoa: refusing self-attestation: owner and agent are the same key")
	}
	if err := validConditions(conditions); err != nil {
		return Tag{}, err
	}
	sig, err := sign(agentPk, ownerSecret, conditions)
	if err != nil {
		return Tag{}, err
	}
	return Tag{Owner: ownerPk, Conditions: conditions, Sig: sig}, nil
}

// MintEngram mints the one attestation the agent memory plane needs.
func MintEngram(agentPk string, ownerSecret []byte) (Tag, error) {
	return Mint(agentPk, ownerSecret, EngramConditions)
}

// Verify checks a tag against agentPk: shape, the self-attestation rule, each
// clause, then the owner signature over the canonical preimage.
func Verify(agentPk string, t Tag) error {
	if !hex64.MatchString(agentPk) {
		return fmt.Errorf("nipoa: agent pubkey must be 64 lowercase hex")
	}
	if !hex64.MatchString(t.Owner) {
		return fmt.Errorf("nipoa: owner pubkey must be 64 lowercase hex")
	}
	if t.Owner == agentPk {
		return fmt.Errorf("nipoa: self-attestation is invalid")
	}
	if err := validConditions(t.Conditions); err != nil {
		return err
	}
	if !hex128.MatchString(t.Sig) {
		return fmt.Errorf("nipoa: signature must be 128 lowercase hex")
	}
	sig, err := hex.DecodeString(t.Sig)
	if err != nil {
		return fmt.Errorf("nipoa: signature not hex: %w", err)
	}
	sum := sha256.Sum256([]byte(preimage(agentPk, t.Conditions)))
	return crypto.VerifyBIP340(t.Owner, sum[:], sig)
}

func sign(agentPk string, ownerSecret []byte, conditions string) (string, error) {
	sum := sha256.Sum256([]byte(preimage(agentPk, conditions)))
	sig, err := crypto.SignBIP340(ownerSecret, sum[:])
	if err != nil {
		return "", fmt.Errorf("nipoa: signing attestation: %w", err)
	}
	return hex.EncodeToString(sig), nil
}

func preimage(agentPk, conditions string) string {
	return Domain + agentPk + ":" + conditions
}

// validConditions enforces the clause grammar: the empty string, or `&`-joined
// clauses with no empty member, no whitespace, canonical base-10 numbers, and
// values inside their stated ranges. The string is never rewritten: its exact
// bytes are the signed preimage.
func validConditions(s string) error {
	if s == "" {
		return nil
	}
	if strings.ContainsAny(s, " \t\n\r") {
		return fmt.Errorf("nipoa: conditions %q carries whitespace", s)
	}
	for _, c := range strings.Split(s, "&") {
		switch {
		case c == "":
			return fmt.Errorf("nipoa: conditions %q has an empty clause", s)
		case strings.HasPrefix(c, "kind="):
			if _, err := canonical(c[len("kind="):], maxKind); err != nil {
				return fmt.Errorf("nipoa: conditions clause %q is not kind=0..65535", c)
			}
		case strings.HasPrefix(c, "created_at<"):
			if _, err := canonical(c[len("created_at<"):], maxStamp); err != nil {
				return fmt.Errorf("nipoa: conditions clause %q is not a valid created_at< clause", c)
			}
		case strings.HasPrefix(c, "created_at>"):
			if _, err := canonical(c[len("created_at>"):], maxStamp); err != nil {
				return fmt.Errorf("nipoa: conditions clause %q is not a valid created_at> clause", c)
			}
		default:
			return fmt.Errorf("nipoa: unsupported conditions clause %q", c)
		}
	}
	return nil
}

// canonical parses a canonical base-10 non-negative integer (no sign, no
// leading zeroes beyond a bare "0") bounded by max.
func canonical(s string, max int64) (int64, error) {
	if s == "" || s[0] == '+' || s[0] == '-' || (s[0] == '0' && len(s) != 1) {
		return 0, fmt.Errorf("not canonical base-10")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > max {
		return 0, fmt.Errorf("out of range")
	}
	return n, nil
}
