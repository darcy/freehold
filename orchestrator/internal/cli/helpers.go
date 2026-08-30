package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"freehold/orchestrator/internal/crypto"
	"freehold/orchestrator/internal/wire"
)

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }

func pubkeyOf(secret []byte) (string, error) {
	return crypto.PubkeyFromSecret(secret)
}

func fillRand(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
}

// mintAgentIdentity creates an agent identity dir (nostr + enc secrets) if
// missing, writing identity.json 0600.
func mintAgentIdentity(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	nostrSecret := make([]byte, 32)
	encSecret := make([]byte, 32)
	fillRand(nostrSecret)
	fillRand(encSecret)
	doc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(nostrSecret),
		"enc_secret_hex":   hex.EncodeToString(encSecret),
	}
	return wire.WriteJSON0600(filepath.Join(dir, "identity.json"), doc)
}

// agentIdentity loads an agent identity from dir.
func agentIdentity(dir string) (*identityJSON, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		return nil, err
	}
	var id identityJSON
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// agentSecretHex returns the agent's nostr secret hex (operator identity).
func agentSecretHex(dir string) (string, error) {
	id, err := agentIdentity(dir)
	if err != nil {
		return "", err
	}
	return id.NostrSecretHex, nil
}

func secretFromHex(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("secret must be 32 bytes")
	}
	copy(out[:], b)
	return out, nil
}
