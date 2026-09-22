// Package common is the shared helper layer for the freehold CLI verbs: runner
// and agent identity loading, profile negotiation, the world MCP client, the
// destructive-confirmation gate, runner-key refs, and the CP-preserving
// teardown. Each verb package imports it; it imports no verb package.
package common

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/identity"
	"freehold/contract/wire"
	"freehold/platform/provisioning/bootstrap"
)

// CommonArgs mirrors the Rust CommonArgs flatten (addr/agent-dir/runner-pubkey).
type CommonArgs struct {
	Addr         string
	AgentDir     string
	RunnerPubkey string
}

// DefaultAgentDir is the freehold home's ops identity, scoped to the active
// profile.
func DefaultAgentDir() string {
	return filepath.Join(config.StateDir(), "control-plane", "agent-ops")
}

// identityJSON is the runner/agent identity file layout.
type identityJSON struct {
	NostrSecretHex string `json:"nostr_secret_hex"`
	EncSecretHex   string `json:"enc_secret_hex"`
}

// AddCommonFlags registers the shared runner flags so they exist at parse time.
func AddCommonFlags(cmd *cobra.Command, _ *CommonArgs) {
	cmd.Flags().String("addr", "127.0.0.1:8787", "Running runner MCP address (host:port or full URL)")
	cmd.Flags().String("agent-dir", DefaultAgentDir(), "Agent identity dir (minted on demand if missing). Defaults to the freehold home's ops identity (legacy cwd-relative fallback)")
	cmd.Flags().String("runner-pubkey", "", "The RUNNER's Nostr pubkey — RESOLVED from ./.freehold/runner/<target> when omitted (you can't know it before provisioning; freehold reads it)")
}

// ReadCommonFlags reads the shared runner flags into a fresh CommonArgs.
func ReadCommonFlags(cmd *cobra.Command) *CommonArgs {
	c := &CommonArgs{}
	c.Addr, _ = cmd.Flags().GetString("addr")
	c.AgentDir, _ = cmd.Flags().GetString("agent-dir")
	c.RunnerPubkey, _ = cmd.Flags().GetString("runner-pubkey")
	return c
}

// LoadRPubkey derives the Nostr pubkey from an identity dir's identity.json.
func LoadRPubkey(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		return "", err
	}
	var id identityJSON
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", err
	}
	return pubkeyOfHex(id.NostrSecretHex)
}

func pubkeyOfHex(secretHex string) (string, error) {
	secret, err := hex.DecodeString(secretHex)
	if err != nil {
		return "", err
	}
	return crypto.PubkeyFromSecret(secret)
}

// ResolveRunnerPubkey resolves the runner's Nostr pubkey: flag wins, else
// home-first runner package, else legacy cwd-relative, else config-recorded.
func ResolveRunnerPubkey(target, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	homePkg := filepath.Join(config.StateDir(), "runner", target)
	legacyPkg := filepath.Join(".", ".freehold", "runner", target)
	for _, pkg := range []string{homePkg, legacyPkg} {
		if _, err := os.Stat(filepath.Join(pkg, "identity.json")); err == nil {
			pk, err := LoadRPubkey(pkg)
			if err != nil {
				return "", fmt.Errorf("runner '%s' found at %s but its identity failed to load: %v", target, pkg, err)
			}
			return pk, nil
		}
	}
	if pk, ok := configRecordedPubkey(target); ok {
		return pk, nil
	}
	return "", fmt.Errorf("runner '%s' not found and no config-recorded pubkey — provision it first", target)
}

// configRecordedPubkey reads the active profile's config runner pubkey.
func configRecordedPubkey(target string) (string, bool) {
	raw, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		return "", false
	}
	text := string(raw)
	if !strings.Contains(text, "[runner]") && !strings.Contains(text, "[runner.ssh]") {
		return "", false
	}
	var runnerTarget, pubkey string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "target") && strings.Contains(line, "=") {
			val := strings.Trim(strings.SplitN(line, "=", 2)[1], " \"'\t")
			if runnerTarget == "" {
				runnerTarget = val
			}
		}
		if strings.HasPrefix(line, "pubkey") && strings.Contains(line, "=") {
			val := strings.Trim(strings.SplitN(line, "=", 2)[1], " \"'\t")
			pubkey = val
		}
	}
	if runnerTarget == target && pubkey != "" && len(pubkey) == 64 {
		return pubkey, true
	}
	return "", false
}

// Connect builds an McpClient from CommonArgs + target.
func Connect(common *CommonArgs, target string) (*client.McpClient, error) {
	EnsureAgentIdentity(common.AgentDir)
	runnerPK, err := ResolveRunnerPubkey(target, common.RunnerPubkey)
	if err != nil {
		return nil, err
	}
	auth, err := identity.AgentAuth(common.AgentDir)
	if err != nil {
		return nil, err
	}
	return client.New(client.ConnectURL(common.Addr), auth, runnerPK)
}

// EnsureAgentIdentity mints an agent identity dir if missing.
func EnsureAgentIdentity(dir string) {
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err == nil {
		return
	}
	_ = MintAgentIdentity(dir)
}

// MintAgentIdentity creates an agent identity dir (nostr + enc secrets) if
// missing, writing identity.json 0600.
func MintAgentIdentity(dir string) error {
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

func fillRand(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
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

// EncSecretFromDir returns the agent's encryption secret hex.
func EncSecretFromDir(dir string) (string, error) {
	id, err := agentIdentity(dir)
	if err != nil {
		return "", err
	}
	return id.EncSecretHex, nil
}

// HexBytes decodes a hex string.
func HexBytes(s string) ([]byte, error) { return hex.DecodeString(s) }

// IsHex64 reports whether s is a 64-character hex string.
func IsHex64(s string) bool { return bootstrap.IsHex64(s) }

// ConfirmDestructive prompts for an explicit "yes" and returns an explicit
// abort error otherwise — never nil on a non-yes answer (or EOF), so a
// mistyped/blank answer can never fall through to a destructive action.
func ConfirmDestructive(what string) error {
	var answer string
	fmt.Printf("proceed? [type yes] ")
	if _, err := fmt.Scanln(&answer); err != nil || answer != "yes" {
		return fmt.Errorf("%s aborted (not confirmed)", what)
	}
	return nil
}

// OptOf returns a pointer to s, or nil when empty (optional flag helper).
func OptOf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
