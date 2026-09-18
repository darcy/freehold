package crypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
)

// OpenSSH wire encoding helpers — reproduce ssh-key crate output exactly.

// sshWireString prefixes a []byte with a uint32 length (OpenSSH wire string).
func sshWireString(b []byte) []byte {
	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, uint32(len(b)))
	out.Write(b)
	return out.Bytes()
}

// ED25519 public key blob: string "ssh-ed25519" + string pubkey(32).
func ed25519PublicKeyBlob(pub ed25519.PublicKey) []byte {
	out := sshWireString([]byte("ssh-ed25519"))
	out = append(out, sshWireString(pub)...)
	return out
}

// encodeED25519OpenSSH renders an ed25519 keypair as (openssh-key-v1 PEM,
// public authorized_keys line), byte-matching the ssh-key crate.
// (seed, comment) as in core::generate_ssh_keypair.
func encodeED25519OpenSSH(seed []byte, comment string) (privatePEM []byte, pubLine string, err error) {
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	checkInt := make([]byte, 4)
	if _, err := rand.Read(checkInt); err != nil {
		return nil, "", fmt.Errorf("random checkint: %w", err)
	}

	// Public key blob (header entry).
	pubBlob := ed25519PublicKeyBlob(pub)

	// Private key blob.
	var privBlob bytes.Buffer
	privBlob.Write(checkInt) // checkint1
	privBlob.Write(checkInt) // checkint2
	privBlob.Write(sshWireString([]byte("ssh-ed25519")))
	privBlob.Write(sshWireString(pub))  // pubkey
	privBlob.Write(sshWireString(priv)) // 64-byte private key
	privBlob.Write(sshWireString([]byte(comment)))
	// Padding to block size 8: 1, 2, 3, ...
	padLen := 8 - (privBlob.Len() % 8)
	for i := 1; i <= padLen; i++ {
		privBlob.WriteByte(byte(i))
	}

	// Outer container.
	var outer bytes.Buffer
	outer.Write([]byte("openssh-key-v1\x00"))
	outer.Write(sshWireString([]byte("none")))            // ciphername
	outer.Write(sshWireString([]byte("none")))            // kdfname
	outer.Write(sshWireString([]byte{}))                  // kdfoptions
	_ = binary.Write(&outer, binary.BigEndian, uint32(1)) // nkeys
	outer.Write(sshWireString(pubBlob))
	outer.Write(sshWireString(privBlob.Bytes()))

	base := base64.StdEncoding.EncodeToString(outer.Bytes())

	// PEM with LF line endings (LineEnding::LF), 70-char wrap (OpenSSH base64
	// wraps at 70 columns).
	var pem bytes.Buffer
	pem.WriteString("-----BEGIN OPENSSH PRIVATE KEY-----\n")
	for len(base) > 0 {
		n := 70
		if len(base) < n {
			n = len(base)
		}
		pem.WriteString(base[:n])
		pem.WriteByte('\n')
		base = base[n:]
	}
	pem.WriteString("-----END OPENSSH PRIVATE KEY-----\n")

	// Public authorized_keys line: base64(pub blob) + " " + comment.
	pubLine = "ssh-ed25519 " + base64.StdEncoding.EncodeToString(pubBlob)
	if comment != "" {
		pubLine += " " + comment
	}

	return pem.Bytes(), pubLine, nil
}

// GenerateSSHKeypair generates a fresh ed25519 keypair (private PEM +
// authorized_keys public line), byte-matching core::generate_ssh_keypair. The
// private half is the sealed door credential — it never leaves the box.
func GenerateSSHKeypair(comment string) (privatePEM []byte, pubLine string, err error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, "", err
	}
	return encodeED25519OpenSSH(seed, comment)
}

// SSHPublicKeyFromSeed derives the authorized_keys public line from a fixed
// 32-byte ed25519 seed (deterministic — same seed → same line). The login
// door uses the box's own identity seed (its nostr_secret) so the box's door
// key is stable across re-logins; only the public line is ever presented.
func SSHPublicKeyFromSeed(seed []byte, comment string) (string, error) {
	if len(seed) != ed25519.SeedSize {
		return "", fmt.Errorf("seed must be 32 bytes, got %d", len(seed))
	}
	_, pubLine, err := encodeED25519OpenSSH(seed, comment)
	return pubLine, err
}

// SSHPrivateKeyPEMFromSeed derives the openssh-key-v1 PRIVATE key from a fixed
// 32-byte ed25519 seed. This is the transient-access half of DOOR_SPEC: the
// install/teardown box derives its own deterministic door key from its
// agent-ops identity seed so it can reach the host directly, before any runner
// is serving. The caller writes it to a 0600 temp file and deletes it.
func SSHPrivateKeyPEMFromSeed(seed []byte, comment string) ([]byte, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("seed must be 32 bytes, got %d", len(seed))
	}
	pem, _, err := encodeED25519OpenSSH(seed, comment)
	return pem, err
}

// ExtractED25519PublicKeyLine derives the authorized_keys public line from
// an openssh-key-v1 private-key PEM (ed25519 only — the only kind the
// provisioner generates). This is the door-key RECOVERY path: a reused
// runner package holds the private key as a sealed credential; when the
// door gate was skipped and ssh auth fails, rebuild re-derives the PUBLIC
// half to show the operator. The private half is parsed past, never
// returned. Mirrors ssh-key's openssh-key-v1 container layout.
func ExtractED25519PublicKeyLine(pemBytes []byte) (string, error) {
	body, err := pemUnwrap(pemBytes, "OPENSSH PRIVATE KEY")
	if err != nil {
		return "", err
	}
	const magic = "openssh-key-v1\x00"
	if !bytes.HasPrefix(body, []byte(magic)) {
		return "", fmt.Errorf("not an openssh-key-v1 container")
	}
	rest := body[len(magic):]
	var cipher, kdf, kdfOpts []byte
	if cipher, rest, err = readSshWireString(rest); err != nil {
		return "", fmt.Errorf("ciphername: %w", err)
	}
	if kdf, rest, err = readSshWireString(rest); err != nil {
		return "", fmt.Errorf("kdfname: %w", err)
	}
	if kdfOpts, rest, err = readSshWireString(rest); err != nil {
		return "", fmt.Errorf("kdfoptions: %w", err)
	}
	if string(cipher) != "none" || string(kdf) != "none" || len(kdfOpts) != 0 {
		return "", fmt.Errorf("encrypted private keys are not supported (cipher=%q kdf=%q)", cipher, kdf)
	}
	if len(rest) < 4 {
		return "", fmt.Errorf("truncated container: no key count")
	}
	nkeys := binary.BigEndian.Uint32(rest[:4])
	rest = rest[4:]
	if nkeys != 1 {
		return "", fmt.Errorf("expected 1 key, got %d", nkeys)
	}
	var pubBlob []byte
	if pubBlob, rest, err = readSshWireString(rest); err != nil {
		return "", fmt.Errorf("public key blob: %w", err)
	}
	// Public blob: string "ssh-ed25519" + string pubkey(32).
	kind, inner, err := readSshWireString(pubBlob)
	if err != nil || string(kind) != "ssh-ed25519" {
		return "", fmt.Errorf("expected an ssh-ed25519 key, got %q", kind)
	}
	pub, _, err := readSshWireString(inner)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("bad ed25519 public key in blob")
	}
	// The private blob carries the comment: checkint(4)+checkint(4) +
	// string kind + string pub + string priv(64) + string comment (+pad).
	comment := ""
	if len(rest) >= 4 {
		var privBlob []byte
		if privBlob, _, err = readSshWireString(rest); err == nil && len(privBlob) >= 8 {
			p := privBlob[8:]
			for _, skip := range []string{"kind", "pub", "priv"} {
				var v []byte
				if v, p, err = readSshWireString(p); err != nil {
					break
				}
				_ = v
				_ = skip
			}
			if err == nil {
				var c []byte
				if c, _, err = readSshWireString(p); err == nil {
					comment = string(c)
				}
			}
		}
	}
	line := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(pubBlob)
	if comment != "" {
		line += " " + comment
	}
	return line, nil
}

// readSshWireString consumes one uint32-length-prefixed OpenSSH wire string.
func readSshWireString(b []byte) (val, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, fmt.Errorf("truncated wire string")
	}
	n := binary.BigEndian.Uint32(b[:4])
	if uint32(len(b)-4) < n {
		return nil, nil, fmt.Errorf("wire string overflows buffer")
	}
	return b[4 : 4+n], b[4+n:], nil
}

// pemUnwrap decodes the base64 body between the armor lines (any wrap
// width, LF or CRLF).
func pemUnwrap(pemBytes []byte, label string) ([]byte, error) {
	var body []byte
	in := false
	for _, l := range strings.Split(string(pemBytes), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "-----BEGIN "+label+"-----":
			in = true
		case l == "-----END "+label+"-----":
			in = false
		case in:
			body = append(body, l...)
		}
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("no %q PEM body found", label)
	}
	out, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		return nil, fmt.Errorf("bad PEM base64: %w", err)
	}
	return out, nil
}
