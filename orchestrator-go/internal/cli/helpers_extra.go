package cli

import (
	"encoding/hex"

	"freehold/orchestrator-go/internal/crypto"
	"freehold/orchestrator-go/internal/relay"
)

// nsecToSecret accepts nsec1<bech32> or bare 64-hex -> raw 32-byte secret.
func nsecToSecret(s string) ([32]byte, error) {
	return crypto.NsecToSecret(s)
}

// agentSecretFromDir returns the agent's nostr secret hex from an identity dir.
func agentSecretFromDir(dir string) (string, error) {
	id, err := agentIdentity(dir)
	if err != nil {
		return "", err
	}
	return id.NostrSecretHex, nil
}

// writeMemoryCmd writes an engram via the relay bridge.
func writeMemoryCmd(relayURL string, secret [32]byte, key, value string) error {
	return relay.WriteMemory(relayURL, secret[:], key, value)
}

// readMemoryCmd reads an engram via the relay bridge.
func readMemoryCmd(relayURL string, secret [32]byte, key string) (string, bool, error) {
	return relay.ReadMemory(relayURL, secret[:], key)
}

// encSecretFromDir returns the agent's encryption secret hex.
func encSecretFromDir(dir string) (string, error) {
	id, err := agentIdentity(dir)
	if err != nil {
		return "", err
	}
	return id.EncSecretHex, nil
}

func hexBytes(s string) ([]byte, error) { return hex.DecodeString(s) }
