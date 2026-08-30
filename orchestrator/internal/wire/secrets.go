// Package wire reproduces the freehold-core secret + Nostr wire formats
// (core/src/secrets.rs, core/src/nip98.rs, core/src/audit.rs,
// core/src/memory.rs) byte-exactly in Go.
package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SECRETS_FILE is the on-disk name of a shipped secret package.
const SECRETS_FILE = "secrets.json"

// TargetMeta is per-target connector metadata shipped alongside ciphertext.
// Mirrors core/src/secrets.rs::TargetMeta.
type TargetMeta struct {
	Kind    string `json:"kind"`
	Address string `json:"address"`
	Secret  string `json:"secret"`
}

// SecretPackage is the control plane's shipped package: secret name -> sealed
// ciphertext (hex). Mirrors core/src/secrets.rs::SecretPackage.
//
// Field order and the always-present targets/grants (serde default) matter;
// encoding/json sorts map keys, so a plain struct + maps yields byte-identical
// output to serde's BTreeMap. Empty grants must serialize as [] (not null),
// so the JSON marshaling path below initializes them explicitly.
type SecretPackage struct {
	Secrets map[string]string     `json:"secrets"`
	Targets map[string]TargetMeta `json:"targets"`
	Grants  []string              `json:"grants"`
}

// jsonPackage orders fields secrets -> targets -> grants (serde struct order).
type jsonPackage struct {
	Secrets map[string]string     `json:"secrets"`
	Targets map[string]TargetMeta `json:"targets"`
	Grants  []string              `json:"grants"`
}

// New builds a SecretPackage with non-nil maps/slice so JSON emission matches
// serde (targets: {}, grants: [] even when empty).
func New(secrets map[string]string, targets map[string]TargetMeta, grants []string) *SecretPackage {
	if secrets == nil {
		secrets = map[string]string{}
	}
	if targets == nil {
		targets = map[string]TargetMeta{}
	}
	if grants == nil {
		grants = []string{}
	}
	return &SecretPackage{Secrets: secrets, Targets: targets, Grants: grants}
}

// MarshalJSON emits serde-compatible JSON (sorted keys, empty containers).
func (p *SecretPackage) MarshalJSON() ([]byte, error) {
	jp := jsonPackage{
		Secrets: p.Secrets,
		Targets: p.Targets,
		Grants:  p.Grants,
	}
	if jp.Secrets == nil {
		jp.Secrets = map[string]string{}
	}
	if jp.Targets == nil {
		jp.Targets = map[string]TargetMeta{}
	}
	if jp.Grants == nil {
		jp.Grants = []string{}
	}
	return json.Marshal(jp)
}

// WriteToDir writes the package atomically (0600) into dir (created 0700).
// Mirrors core::secrets::SecretPackage::write_to_dir.
func (p *SecretPackage) WriteToDir(dir string) error {
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal secret package: %w", err)
	}
	return write0600Atomic(filepath.Join(dir, SECRETS_FILE), data)
}

// Load reads a previously shipped package from dir. Mirrors
// core::secrets::SecretPackage::load.
func Load(dir string) (*SecretPackage, error) {
	raw, err := os.ReadFile(filepath.Join(dir, SECRETS_FILE))
	if err != nil {
		return nil, err
	}
	p := &SecretPackage{}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Bytes returns the serde-compatible JSON bytes for byte-identity checks.
func (p *SecretPackage) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	buf.Write(data)
	return buf.Bytes(), nil
}
