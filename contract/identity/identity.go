// Package identity is the runner/agent identity.json format loader. It is the
// thin format leaf shared by the server and the local operator CLI, so both
// sides read the same identity file the same way.
package identity

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"freehold/contract/client"
	"freehold/contract/crypto"
)

const identityFile = "identity.json"

// Identity is the runner's JSON identity (nostr + enc secrets).
type Identity struct {
	NostrSecretHex string `json:"nostr_secret_hex"`
	EncSecretHex   string `json:"enc_secret_hex"`
}

// Load reads identity.json in dir.
func Load(dir string) (*Identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// NostrPubkeyHex derives the Nostr x-only pubkey from the identity secret.
func (id *Identity) NostrPubkeyHex() (string, error) {
	secret, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil {
		return "", err
	}
	return pubkeyFromSecret(secret)
}

// AgentAuth loads the agent identity (identity.json in dir) and its signing
// auth.
func AgentAuth(dir string) (*client.AgentAuth, error) {
	id, err := Load(dir)
	if err != nil {
		return nil, fmt.Errorf("bad agent identity dir %s: %v", dir, err)
	}
	secret, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil {
		return nil, fmt.Errorf("bad agent identity dir %s: %v", dir, err)
	}
	pubkey, err := pubkeyFromSecret(secret)
	if err != nil {
		return nil, err
	}
	auth := &client.AgentAuth{}
	copy(auth.Secret[:], secret)
	auth.Pubkey = pubkey
	return auth, nil
}

// pubkeyFromSecret derives the Nostr x-only pubkey hex from a secret.
func pubkeyFromSecret(secret []byte) (string, error) {
	return crypto.PubkeyFromSecret(secret)
}
