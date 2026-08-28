package crypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
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
