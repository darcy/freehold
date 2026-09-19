package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/identity"
)

// CommonArgs mirrors the Rust CommonArgs flatten (addr/agent-dir/runner-pubkey).
type CommonArgs struct {
	Addr         string
	AgentDir     string
	RunnerPubkey string
}

// defaultAgentDir is the freehold home's ops identity (installer default),
// scoped to the active profile.
func defaultAgentDir() string {
	return filepath.Join(config.StateDir(), "control-plane", "agent-ops")
}

// identityJSON is the runner/agent identity file layout.
type identityJSON struct {
	NostrSecretHex string `json:"nostr_secret_hex"`
	EncSecretHex   string `json:"enc_secret_hex"`
}

// loadRPubkey derives the Nostr pubkey from an identity dir's identity.json.
func loadRPubkey(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		return "", err
	}
	var id identityJSON
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", err
	}
	return flowsPubkey(id.NostrSecretHex)
}

func flowsPubkey(secretHex string) (string, error) {
	secret, err := hexDecode(secretHex)
	if err != nil {
		return "", err
	}
	return pubkeyOf(secret)
}

// resolveRunnerPubkey resolves the runner's Nostr pubkey: flag wins, else
// home-first runner package, else legacy cwd-relative, else config-recorded.
func resolveRunnerPubkey(target, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	homePkg := filepath.Join(config.StateDir(), "runner", target)
	legacyPkg := filepath.Join(".", ".freehold", "runner", target)
	for _, pkg := range []string{homePkg, legacyPkg} {
		if _, err := os.Stat(filepath.Join(pkg, "identity.json")); err == nil {
			pk, err := loadRPubkey(pkg)
			if err != nil {
				return "", fmt.Errorf("runner '%s' found at %s but its identity failed to load: %v", target, pkg, err)
			}
			return pk, nil
		}
	}
	// Config-recorded fallback (the Rust reads freehold installer config).
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
	// minimal TOML scan for [runner] target + pubkey
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

// connect builds an McpClient from CommonArgs + target.
func connect(common *CommonArgs, target string) (*client.McpClient, error) {
	// Ensure the agent identity exists (mint on demand).
	ensureAgentIdentity(common.AgentDir)
	runnerPK, err := resolveRunnerPubkey(target, common.RunnerPubkey)
	if err != nil {
		return nil, err
	}
	auth, err := identity.AgentAuth(common.AgentDir)
	if err != nil {
		return nil, err
	}
	return client.New(client.ConnectURL(common.Addr), auth, runnerPK)
}

// ensureAgentIdentity mints an agent identity dir if missing.
func ensureAgentIdentity(dir string) {
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err == nil {
		return
	}
	_ = mintAgentIdentity(dir)
}

// optOf returns a pointer to s, or nil when empty (optional flag helper).
func optOf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
