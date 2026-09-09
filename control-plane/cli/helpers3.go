package cli

import (
	"crypto/rand"
	"encoding/hex"

	"freehold/platform/provisioning/bootstrap"
	"freehold/contract/crypto"
	relaydeploy "freehold/platform/services/relay/buzz"
	"freehold/control-plane/secret-management"
	"freehold/contract/state"
	"freehold/contract/wire"
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

// writeSecretPackage writes a SecretPackage to a dir (provision helper).
func writeSecretPackage(dir string, secrets map[string]string, targets map[string]wire.TargetMeta, grants []string) error {
	return wire.New(secrets, targets, grants).WriteToDir(dir)
}

// openState opens the CP state store at dir.
func openState(dir string) (*state.StateStore, error) {
	return state.Open(dir)
}

func provRunner(store *state.StateStore, req *provisioner.ProvisionRequest) (*provisioner.ProvisionResult, error) {
	return provisioner.ProvisionRunner(store, req)
}

var _ = relaydeploy.DefaultBufRef
