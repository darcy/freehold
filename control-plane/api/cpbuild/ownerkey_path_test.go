package cpbuild

import (
	"bytes"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"freehold/contract/crypto"
	cert "freehold/platform/services/certificates/letsencrypt"
)

// TestSealedOwnerPathIsTheConsoleRoot: the sealed operator identity's ONE home
// is the CONSOLE's world-secrets (where the box's build PutSecrets lands). The
// agent-tools server's Spec is rooted at its OWN state dir — keying the lookup
// off StateDir silently missed the record there and refused every
// tool-created agent on the console-identity fallback (seen live). Both Spec
// roots must resolve the SAME path.
func TestSealedOwnerPathIsTheConsoleRoot(t *testing.T) {
	console := &Spec{StateDir: "/srv/data/cp/control-plane"}  // the console's own spec
	agentTools := &Spec{StateDir: "/srv/data/cp/agent-tools"} // the tool server's spec

	want := "/srv/data/cp/control-plane/world-secrets/operator.json"
	if got := console.sealedOwnerPath(); got != want {
		t.Errorf("console-rooted spec: sealed path = %q, want %q", got, want)
	}
	if got := agentTools.sealedOwnerPath(); got != want {
		t.Errorf("agent-tools-rooted spec: sealed path = %q, want %q — a StateDir-keyed lookup misses the record and falls back to the console identity", got, want)
	}
}

// TestOwnerKeyResolvesOnTheAgentToolsSpec: the REAL resolution, not just the
// path string — on an agent-tools-rooted Spec (StateDir = the tool server's
// own root), ownerKey must find the sealed record AND unseal it with the
// CONSOLE's enc key (both at the console root). The StateDir-keyed reads miss
// both there: the unseal dies on a console identity that nothing stages under
// the agent-tools root (seen live: the create refused after the path fix, at
// the unseal).
func TestOwnerKeyResolvesOnTheAgentToolsSpec(t *testing.T) {
	root := t.TempDir()
	spec := &Spec{StateDir: root + "/cp/agent-tools"}
	consoleRoot := spec.consoleStateRoot() // <root>/cp/control-plane

	nostrSec := make([]byte, 32)
	encSec := make([]byte, 32)
	for _, b := range [][]byte{nostrSec, encSec} {
		if _, err := crand.Read(b); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(map[string]string{
		"nostr_secret_hex": hex.EncodeToString(nostrSec),
		"enc_secret_hex":   hex.EncodeToString(encSec),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(consoleRoot, "console"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consoleRoot, "console", "identity.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	ownerSec := make([]byte, 32)
	if _, err := crand.Read(ownerSec); err != nil {
		t.Fatal(err)
	}
	ownerPk, err := crypto.PubkeyFromSecret(ownerSec)
	if err != nil {
		t.Fatal(err)
	}
	encPub, err := crypto.X25519PublicKey(encSec)
	if err != nil {
		t.Fatal(err)
	}
	seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
	if err := os.MkdirAll(filepath.Dir(spec.sealedOwnerPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	// the box build's ship shape (the secret name as the aad)
	if err := cert.SaveCreds(spec.sealedOwnerPath(), "operator",
		map[string]string{"nostr": hex.EncodeToString(ownerSec)}, seal, encPub, "operator"); err != nil {
		t.Fatal(err)
	}

	spec.OwnerPub = ownerPk
	got, err := spec.ownerKey()
	if err != nil {
		t.Fatalf("ownerKey must resolve the sealed record at the console root: %v", err)
	}
	if !bytes.Equal(got, ownerSec) {
		t.Error("ownerKey returned the wrong secret")
	}
}
