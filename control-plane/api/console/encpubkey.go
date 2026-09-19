package console

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"freehold/contract/crypto"
)

// consoleEncPubkey returns this console's X25519 encryption public key (64-hex)
// from its durable identity (`<stateDir>/console/identity.json`). It is PUBLIC —
// a build box seals the CP-owned secrets (DNS creds, litellm) to it — and is
// served on /api/world so a thin box can hand off secrets without a runner to
// exec into the CP.
//
// An ABSENT identity is a legitimate "" (older CP / pre-console). A corrupt one
// (bad JSON, missing/undecodable enc_secret_hex, derivation failure) is a real
// defect and is logged, so it is not silently indistinguishable from "no
// identity" on the public route.
func (s *Server) consoleEncPubkey() string {
	dir := s.stateDir()
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, "console", "identity.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		log.Printf("console identity unreadable at %s: %v", path, err)
		return ""
	}
	var id struct {
		EncSecretHex string `json:"enc_secret_hex"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		log.Printf("console identity malformed at %s: %v", path, err)
		return ""
	}
	if id.EncSecretHex == "" {
		log.Printf("console identity at %s has no enc_secret_hex", path)
		return ""
	}
	secret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		log.Printf("console identity enc_secret_hex at %s is not hex: %v", path, err)
		return ""
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		log.Printf("console identity enc pubkey derivation at %s: %v", path, err)
		return ""
	}
	return hex.EncodeToString(pub)
}
