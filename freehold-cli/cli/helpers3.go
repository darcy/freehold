package cli

import (
	"crypto/rand"
	"encoding/hex"

	"freehold/contract/crypto"
	"freehold/platform/provisioning/bootstrap"
	relaydeploy "freehold/platform/services/relay/buzz"
)

func isHex64(s string) bool {
	return bootstrap.IsHex64(s)
}

func randomHex24() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// pubkeyOfRequester returns the peer's own Nostr pubkey (from its secret).
func pubkeyOfRequester(sec [32]byte) string {
	pk, _ := crypto.PubkeyFromSecret(sec[:])
	return pk
}

var _ = relaydeploy.DefaultBufRef
