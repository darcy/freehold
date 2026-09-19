package provisioner

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"freehold/contract/crypto"
	"freehold/contract/relay"
	"freehold/contract/wire"
	"freehold/control-plane/api/cpstate"
	"freehold/control-plane/state"
)

// ---- B2 rotate ----

// RotateSecret re-seals the NEW credential to the runner's existing encryption
// key, re-ships the package (targets + grants preserved), and updates state —
// the old value is gone everywhere (the erase lever). Roll back on save
// failure: restore the prior ciphertext in memory and re-ship the old package.
func RotateSecret(store *state.StateStore, name string, newSecret []byte) (*state.SecretRecord, error) {
	before, ok := store.GetSecret(name)
	if !ok {
		return nil, fmt.Errorf("secret %s not found", name)
	}
	runnerRec, ok := store.GetRunner(before.Runner)
	if !ok {
		return nil, fmt.Errorf("runner %s not found", before.Runner)
	}
	if runnerRec.Status == state.RunnerRevoked {
		return nil, fmt.Errorf("runner %s is revoked", before.Runner)
	}
	encPub, err := hexToArr(runnerRec.EncPubkey)
	if err != nil {
		return nil, err
	}
	sealed, err := crypto.Seal(encPub[:], []byte(name), newSecret)
	if err != nil {
		return nil, err
	}
	ciphertextHex := fmt.Sprintf("%x", sealed)
	// Grants are read STRICTLY (the doc contract: a read error is an error —
	// silently shipping a runner nobody may call hides the reason). Fail the
	// rotate rather than strip every grant from the re-shipped package.
	grants, err := loadGrants(runnerRec.PackageDir)
	if err != nil {
		return nil, fmt.Errorf("rotate %s: read shipped grants: %w", name, err)
	}
	pkg := wire.New(
		map[string]string{name: ciphertextHex},
		map[string]wire.TargetMeta{name: {Kind: before.Kind, Address: before.Address, Secret: name}},
		grants,
	)
	if err := pkg.WriteToDir(runnerRec.PackageDir); err != nil {
		return nil, err
	}
	now := uint64(time.Now().Unix())
	if err := store.UpdateSecretCiphertext(name, ciphertextHex, now); err != nil {
		return nil, err
	}
	if err := store.Save(); err != nil {
		_ = store.SetSecretCiphertext(name, before.CiphertextHex, before.RotatedAt)
		old := wire.New(
			map[string]string{name: before.CiphertextHex},
			map[string]wire.TargetMeta{name: {Kind: before.Kind, Address: before.Address, Secret: name}},
			currentGrantsOrEmpty(runnerRec.PackageDir),
		)
		_ = old.WriteToDir(runnerRec.PackageDir)
		return nil, err
	}
	rec, _ := store.GetSecret(name)
	return &rec, nil
}

// ---- B3 revoke ----

// RevokeRunner flips the runner to revoked (blocking provision/rotate) and
// best-effort removes the shipped secrets.json so the stored credential dies
// with membership. Idempotent on the state; re-runs retry the cleanup.
func RevokeRunner(store *state.StateStore, name string) (*state.RunnerRecord, error) {
	prior, ok := store.GetRunner(name)
	if !ok {
		return nil, fmt.Errorf("runner %s not found", name)
	}
	if prior.Status == state.RunnerRevoked {
		removeShippedSecrets(&prior)
		return &prior, nil
	}
	if err := store.SetRunnerStatus(name, state.RunnerRevoked); err != nil {
		return nil, err
	}
	if err := store.Save(); err != nil {
		_ = store.SetRunnerStatus(name, prior.Status)
		return nil, err
	}
	removeShippedSecrets(&prior)
	rec, _ := store.GetRunner(name)
	return &rec, nil
}

func removeShippedSecrets(rec *state.RunnerRecord) {
	_ = os.Remove(filepath.Join(rec.PackageDir, wire.SECRETS_FILE))
}

// ---- D2 grant / revoke-grant (shipped-package grants) ----

// GrantAgent adds an agent pubkey to a runner's shipped-package grants
// (idempotent), so the runner's live grant check picks it up without a restart.
func GrantAgent(store *state.StateStore, name, agentPubkey string) ([]string, error) {
	if !isPubkey(agentPubkey) {
		return nil, fmt.Errorf("invalid agent pubkey %q: must be 64 hex chars", agentPubkey)
	}
	rec, ok := store.GetRunner(name)
	if !ok {
		return nil, fmt.Errorf("runner %s not found", name)
	}
	if rec.Status == state.RunnerRevoked {
		return nil, fmt.Errorf("runner %s is revoked", name)
	}
	pkg, err := wire.Load(rec.PackageDir)
	if err != nil {
		return nil, err
	}
	if !contains(pkg.Grants, agentPubkey) {
		pkg.Grants = append(pkg.Grants, agentPubkey)
		if err := pkg.WriteToDir(rec.PackageDir); err != nil {
			return nil, err
		}
	}
	return pkg.Grants, nil
}

// RevokeGrant removes an agent pubkey from a runner's shipped-package grants
// (idempotent; the last grant leaves the package fail-closed).
func RevokeGrant(store *state.StateStore, name, agentPubkey string) ([]string, error) {
	if !isPubkey(agentPubkey) {
		return nil, fmt.Errorf("invalid agent pubkey %q: must be 64 hex chars", agentPubkey)
	}
	rec, ok := store.GetRunner(name)
	if !ok {
		return nil, fmt.Errorf("runner %s not found", name)
	}
	if rec.Status == state.RunnerRevoked {
		return nil, fmt.Errorf("runner %s is revoked", name)
	}
	pkg, err := wire.Load(rec.PackageDir)
	if err != nil {
		return nil, err
	}
	if contains(pkg.Grants, agentPubkey) {
		out := pkg.Grants[:0]
		for _, g := range pkg.Grants {
			if g != agentPubkey {
				out = append(out, g)
			}
		}
		pkg.Grants = out
		if err := pkg.WriteToDir(rec.PackageDir); err != nil {
			return nil, err
		}
	}
	return pkg.Grants, nil
}

// ---- adopt ----

// AdoptRunner records an EXISTING runner (its package already holds keypair +
// sealed credential) into the console registry — no credential shipped, nothing
// re-sealed; the record's ciphertext is the package's first-target secret.
func AdoptRunner(store *state.StateStore, name, kind, address, packageDir string, mcpAddr *string, riskLevel *string) (*state.RunnerRecord, error) {
	if name == "" || containsStr(name, "/") || hasPrefix(name, ".") {
		return nil, fmt.Errorf("invalid runner name %q: must be a bare name", name)
	}
	if _, ok := store.GetRunner(name); ok {
		return nil, fmt.Errorf("runner %s already exists", name)
	}
	nostrPubkey, encPubkeyHex, _, err := loadIdentity(packageDir)
	if err != nil {
		return nil, err
	}
	ciphertextHex := ""
	if pkg, err := wire.Load(packageDir); err == nil {
		for _, t := range pkg.Targets {
			if ct, ok := pkg.Secrets[t.Secret]; ok {
				ciphertextHex = ct
				break
			}
		}
	}
	now := uint64(time.Now().Unix())
	runner := state.RunnerRecord{
		NostrPubkey: nostrPubkey,
		EncPubkey:   encPubkeyHex,
		Status:      state.RunnerActive,
		PackageDir:  packageDir,
		CreatedAt:   now,
		McpAddr:     mcpAddr,
		RiskLevel:   riskLevel,
	}
	store.InsertRunner(name, runner)
	store.InsertSecret(name, state.SecretRecord{
		Runner:        name,
		Kind:          kind,
		Address:       address,
		CiphertextHex: ciphertextHex,
		CreatedAt:     now,
	})
	if err := store.Save(); err != nil {
		return nil, err
	}
	return &runner, nil
}

// ---- add_secret (extra named secrets on an existing runner) ----

// AddSecret adds an EXTRA named secret to an existing runner's package (sealed
// to the runner's enc key under aad = its name), re-shipping with all entries
// preserved and recording a SecretRecord. Roll back on save failure.
func AddSecret(store *state.StateStore, runner, secretName string, value []byte) (*state.SecretRecord, error) {
	if secretName == "" || containsStr(secretName, "/") || hasPrefix(secretName, ".") {
		return nil, fmt.Errorf("invalid secret name %q", secretName)
	}
	runnerRec, ok := store.GetRunner(runner)
	if !ok {
		return nil, fmt.Errorf("runner %s not found", runner)
	}
	if runnerRec.Status == state.RunnerRevoked {
		return nil, fmt.Errorf("runner %s is revoked", runner)
	}
	encPub, err := hexToArr(runnerRec.EncPubkey)
	if err != nil {
		return nil, err
	}
	sealed, err := crypto.Seal(encPub[:], []byte(secretName), value)
	if err != nil {
		return nil, err
	}
	ciphertextHex := fmt.Sprintf("%x", sealed)
	pkg, err := wire.Load(runnerRec.PackageDir)
	if err != nil {
		return nil, err
	}
	if pkg.Secrets == nil {
		pkg.Secrets = map[string]string{}
	}
	pkg.Secrets[secretName] = ciphertextHex
	if err := pkg.WriteToDir(runnerRec.PackageDir); err != nil {
		return nil, err
	}
	now := uint64(time.Now().Unix())
	rec := state.SecretRecord{Runner: runner, Kind: "extra", CiphertextHex: ciphertextHex, CreatedAt: now}
	store.InsertSecret(secretName, rec)
	if err := store.Save(); err != nil {
		store.RemoveSecret(secretName)
		if pkg2, err2 := wire.Load(runnerRec.PackageDir); err2 == nil {
			delete(pkg2.Secrets, secretName)
			_ = pkg2.WriteToDir(runnerRec.PackageDir)
		}
		return nil, err
	}
	return &rec, nil
}

// ---- Chunk 2.6.1: relay channel sync (the console-owner credential) ----

// consoleSecret loads the console's own Nostr secret from its state dir (the
// channel-owner credential that signs roster writes).
func consoleSecret(stateDir string) ([]byte, error) {
	return cpstate.ConsoleSecret(stateDir)
}

// SyncRunnerChannel creates the runner's private channel (owner = console),
// members the runner, and publishes the profile meta. Idempotent.
func SyncRunnerChannel(store *state.StateStore, relayURL, name, stateDir string) error {
	profile, err := runnerProfile(store, name)
	if err != nil {
		return err
	}
	secret, err := consoleSecret(stateDir)
	if err != nil {
		return err
	}
	rpk := profile.NostrPubkey
	if err := relay.CreateRunnerChannel(relayURL, secret, rpk, name); err != nil {
		return fmt.Errorf("relay create channel: %w", err)
	}
	if err := relay.PutUser(relayURL, secret, rpk, rpk); err != nil {
		return fmt.Errorf("relay member runner: %w", err)
	}
	if err := relay.PublishRunnerMeta(relayURL, secret, profile); err != nil {
		return fmt.Errorf("relay publish meta: %w", err)
	}
	return nil
}

// PutUserMembership adds an agent pubkey to the runner's channel (kind 9000).
func PutUserMembership(store *state.StateStore, relayURL, name, agentPubkey, stateDir string) error {
	rec, ok := store.GetRunner(name)
	if !ok {
		return fmt.Errorf("runner %s not found", name)
	}
	secret, err := consoleSecret(stateDir)
	if err != nil {
		return err
	}
	if err := relay.PutUser(relayURL, secret, rec.NostrPubkey, agentPubkey); err != nil {
		return fmt.Errorf("relay put-user: %w", err)
	}
	return nil
}

// RemoveUserMembership removes an agent pubkey from the runner's channel (kind 9001).
func RemoveUserMembership(store *state.StateStore, relayURL, name, agentPubkey, stateDir string) error {
	rec, ok := store.GetRunner(name)
	if !ok {
		return fmt.Errorf("runner %s not found", name)
	}
	secret, err := consoleSecret(stateDir)
	if err != nil {
		return err
	}
	if err := relay.RemoveUser(relayURL, secret, rec.NostrPubkey, agentPubkey); err != nil {
		return fmt.Errorf("relay remove-user: %w", err)
	}
	return nil
}

// RevokeRunnerChannel cuts the runner off ON the relay (removes it from its
// own roster + best-effort removes shipped-package grants) and re-publishes
// the revoked meta.
func RevokeRunnerChannel(store *state.StateStore, relayURL, name, stateDir string) error {
	rec, ok := store.GetRunner(name)
	if !ok {
		return fmt.Errorf("runner %s not found", name)
	}
	secret, err := consoleSecret(stateDir)
	if err != nil {
		return err
	}
	rpk := rec.NostrPubkey
	if err := relay.RemoveUser(relayURL, secret, rpk, rpk); err != nil {
		return fmt.Errorf("relay remove runner: %w", err)
	}
	if pkg, err := wire.Load(rec.PackageDir); err == nil {
		for _, g := range pkg.Grants {
			_ = relay.RemoveUser(relayURL, secret, rpk, g)
		}
	}
	profile, err := runnerProfile(store, name)
	if err != nil {
		return err
	}
	if err := relay.PublishRunnerMeta(relayURL, secret, profile); err != nil {
		return fmt.Errorf("relay publish revoked meta: %w", err)
	}
	return nil
}

// runnerProfile builds the runner's current kind-39000 meta content (secret
// NAME only — never ciphertext).
func runnerProfile(store *state.StateStore, name string) (*relay.RunnerProfile, error) {
	rec, ok := store.GetRunner(name)
	if !ok {
		return nil, fmt.Errorf("runner %s not found", name)
	}
	sec, ok := store.GetSecret(name)
	if !ok {
		return nil, fmt.Errorf("secret %s not found", name)
	}
	status := "active"
	if rec.Status == state.RunnerRevoked {
		status = "revoked"
	}
	return &relay.RunnerProfile{
		Name:        name,
		Kind:        sec.Kind,
		Address:     sec.Address,
		Status:      status,
		NostrPubkey: rec.NostrPubkey,
		EncPubkey:   rec.EncPubkey,
		Secret:      name,
		CreatedAt:   rec.CreatedAt,
		RotatedAt:   sec.RotatedAt,
		Risk:        rec.RiskLevel,
	}, nil
}

// currentGrantsOrEmpty loads the shipped-package grants BEST-EFFORT for a
// rollback path (a restore failure must not block the rollback).
func currentGrantsOrEmpty(dir string) []string {
	g, err := loadGrants(dir)
	if err != nil {
		return []string{}
	}
	return g
}

func loadGrants(dir string) ([]string, error) {
	pkg, err := wire.Load(dir)
	if err != nil {
		return nil, err
	}
	return pkg.Grants, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
