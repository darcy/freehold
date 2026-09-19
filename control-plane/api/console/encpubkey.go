package console

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"freehold/contract/crypto"
)

// consoleEncPubkey returns this console's X25519 encryption public key (64-hex)
// from its durable identity (`<stateDir>/console/identity.json`), or "" when the
// identity is not present/readable. It is PUBLIC — a build box seals the
// CP-owned secrets (DNS creds, litellm) to it — and is served on /api/world so a
// thin box can hand off secrets without a runner to exec into the CP.
func (s *Server) consoleEncPubkey() string {
	dir := s.stateDir()
	if dir == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, "console", "identity.json"))
	if err != nil {
		return ""
	}
	var id struct {
		EncSecretHex string `json:"enc_secret_hex"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return ""
	}
	secret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return ""
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(pub)
}
