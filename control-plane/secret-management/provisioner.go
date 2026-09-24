// Package provisioner reproduces control-plane/src/provisioner.rs: provision,
// adopt, rotate a secret — seal TO the runner's enc key, ship the package
// (identity.json + ciphertext-only secrets.json), record pubkeys + ciphertext
// only. NO master key, NO retained private keys, NO plaintext.
package provisioner

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/wire"
	"freehold/control-plane/state"
)

// identity.json is the runner's identity file (private keys).
const identityFile = "identity.json"

// IDENTITY_FILE content (matches core::identity::IDENTITY_FILE).
var _ = identityFile

// ProvisionResult is what provision returns.
type ProvisionResult struct {
	Name        string
	NostrPubkey string
	EncPubkey   string
	PackageDir  string
}

// ProvisionRequest is the input to provision_runner.
type ProvisionRequest struct {
	Name      string
	Kind      string
	Address   string
	Secret    []byte
	RunnerDir string
	Grants    []string
	RiskLevel *string
}

// defaultRisk is the kind-based risk default.
func defaultRisk(kind string) *string {
	var v string
	switch kind {
	case "vultr", "b2", "hetzner", "github", "websearch", "litellm", "kubernetes", "cloudflare", "unifi":
		v = "safe"
	case "ssh":
		v = "risky-install"
	case "local":
		v = "safe"
	default:
		return nil
	}
	return &v
}

func isPubkey(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func hexToArr(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	if len(b) != 32 {
		return out, fmt.Errorf("pubkey must be 32 bytes, got %d", len(b))
	}
	copy(out[:], b)
	return out, nil
}

// ProvisionRunner is the B1 happy path: existing service + credential ->
// runner with the secret encrypted to its key.
func ProvisionRunner(store *state.StateStore, req *ProvisionRequest) (*ProvisionResult, error) {
	// Keep the name a bare name — it flows into package dir paths.
	if req.Name == "" || strings.Contains(req.Name, "/") || strings.HasPrefix(req.Name, ".") {
		return nil, fmt.Errorf("invalid runner name %q: must be a bare name (no '/', no leading '.')", req.Name)
	}
	if req.Name == "local" {
		return nil, fmt.Errorf("invalid runner name %q: must be a bare name (no '/', no leading '.')", req.Name)
	}
	for _, g := range req.Grants {
		if !isPubkey(g) {
			return nil, fmt.Errorf("invalid agent pubkey %q: must be 64 hex chars", g)
		}
	}
	if r, ok := store.GetRunner(req.Name); ok {
		if r.Status == state.RunnerRevoked {
			return nil, fmt.Errorf("runner %s is revoked — provision a new service or restore it", req.Name)
		}
		return nil, fmt.Errorf("runner %s already exists", req.Name)
	}
	// Refuse to clobber a dir already holding a package.
	if _, err := os.Stat(filepath.Join(req.RunnerDir, identityFile)); err == nil {
		return nil, fmt.Errorf("package dir %s already holds a runner — refusing to clobber", req.RunnerDir)
	}
	for name, rec := range store.Snapshot().Runners {
		if rec.PackageDir == req.RunnerDir {
			return nil, fmt.Errorf("package dir %s already holds a runner — refusing to clobber (runner %s)", req.RunnerDir, name)
		}
	}

	// Generate identity + seal the credential TO the runner's enc key.
	_, err := identityGenerate(req.RunnerDir)
	if err != nil {
		return nil, err
	}
	// (identity file written by identityGenerate)

	// load back the pubkeys/secrets for the record.
	nostrPubkey, encPubkeyHex, _, err := loadIdentity(req.RunnerDir)
	if err != nil {
		return nil, err
	}
	encPub, err := hexToArr(encPubkeyHex)
	if err != nil {
		return nil, err
	}
	// aad = secret NAME — pins the blob to the entry it's filed under.
	sealed, err := crypto.Seal(encPub[:], []byte(req.Name), req.Secret)
	if err != nil {
		return nil, err
	}
	ciphertextHex := hex.EncodeToString(sealed)

	// Ship the package: secrets.json holds ciphertext only.
	pkg := wire.New(
		map[string]string{req.Name: ciphertextHex},
		map[string]wire.TargetMeta{req.Name: {Kind: req.Kind, Address: req.Address, Secret: req.Name}},
		req.Grants,
	)
	if err := pkg.WriteToDir(req.RunnerDir); err != nil {
		// identity.json already landed — remove so PackageDirInUse doesn't
		// lock the dir on retry.
		os.Remove(filepath.Join(req.RunnerDir, identityFile))
		return nil, err
	}

	now := uint64(time.Now().Unix())
	risk := req.RiskLevel
	if risk == nil {
		risk = defaultRisk(req.Kind)
	}
	store.InsertRunner(req.Name, state.RunnerRecord{
		NostrPubkey: nostrPubkey,
		EncPubkey:   encPubkeyHex,
		Status:      state.RunnerActive,
		PackageDir:  req.RunnerDir,
		CreatedAt:   now,
		RiskLevel:   risk,
	})
	store.InsertSecret(req.Name, state.SecretRecord{
		Runner:        req.Name,
		Kind:          req.Kind,
		Address:       req.Address,
		CiphertextHex: ciphertextHex,
		CreatedAt:     now,
	})
	if err := store.Save(); err != nil {
		os.Remove(filepath.Join(req.RunnerDir, identityFile))
		os.Remove(filepath.Join(req.RunnerDir, wire.SECRETS_FILE))
		store.RemoveRunner(req.Name)
		store.RemoveSecret(req.Name)
		return nil, err
	}
	return &ProvisionResult{
		Name:        req.Name,
		NostrPubkey: nostrPubkey,
		EncPubkey:   encPubkeyHex,
		PackageDir:  req.RunnerDir,
	}, nil
}

// identityGenerate writes a fresh identity.json into dir and returns the
// enc pubkey hex. Reproduces Identity::generate + write_to_dir.
func identityGenerate(dir string) (string, error) {
	if err := wire.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	// Use the wire SecretPackage write discipline for consistency (the
	// identity file too is secret material, written 0600 atomically).
	nostrSecret := make([]byte, 32)
	encSecret := make([]byte, 32)
	fillRand(nostrSecret)
	fillRand(encSecret)
	nn, err := crypto.PubkeyFromSecret(nostrSecret)
	if err != nil {
		return "", err
	}
	// X25519 enc pubkey from the enc secret.
	encPub, err := x25519Pub(encSecret)
	if err != nil {
		return "", err
	}
	doc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(nostrSecret),
		"enc_secret_hex":   hex.EncodeToString(encSecret),
	}
	_ = nn
	return writeIdentityJSON(dir, doc, encPub)
}

func fillRand(b []byte) {
	_, _ = cryptoRandRead(b)
}

// writeIdentityJSON writes identity.json (0600 atomic) and returns enc pubkey.
func writeIdentityJSON(dir string, doc map[string]string, encPub string) (string, error) {
	return encPub, wire.WriteJSON0600(filepath.Join(dir, identityFile), doc)
}

// x25519Pub derives the X25519 public key hex from a 32-byte secret.
func x25519Pub(secret []byte) (string, error) {
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(pub), nil
}

// loadIdentity reads the pubkeys/secrets from an identity.json.
func loadIdentity(dir string) (nostrPubkey, encPubkeyHex string, encSecret []byte, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return
	}
	var doc struct {
		NostrSecretHex string `json:"nostr_secret_hex"`
		EncSecretHex   string `json:"enc_secret_hex"`
	}
	if err = jsonUnmarshal(raw, &doc); err != nil {
		return
	}
	ns, err := hex.DecodeString(doc.NostrSecretHex)
	if err != nil {
		return
	}
	nostrPubkey, err = crypto.PubkeyFromSecret(ns)
	if err != nil {
		return
	}
	encSecret, err = hex.DecodeString(doc.EncSecretHex)
	if err != nil {
		return
	}
	encPubBytes, err := crypto.X25519PublicKey(encSecret)
	if err != nil {
		return
	}
	encPubkeyHex = hex.EncodeToString(encPubBytes)
	return
}

// SecretCreatorRequest is the second Kind of request (step 13):
// name + nostr_pubkey/enc_pubkey + package_dir. Kept for completeness with
// the Rust provisioner surface; provision_runner covers the main path.
type SecretCreatorRequest struct {
	Name        string
	NostrPubkey string
	EncPubkey   string
	PackageDir  string
}

// cryptoRandRead is the CSPRNG backing fillRand.
func cryptoRandRead(b []byte) (int, error) { return rand.Read(b) }

// jsonUnmarshal wraps encoding/json.Unmarshal.
func jsonUnmarshal(data []byte, v interface{}) error { return json.Unmarshal(data, v) }
