package common

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"freehold/contract/client"
	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/crypto"
	"freehold/contract/identity"
	oplogin "freehold/freehold-cli/login"
	"freehold/platform/provisioning/box"
)

// WorldMCP builds the agent-tools MCP client for the world verbs. It signs as
// the OPERATOR identity (the persisted nsec — a console-admin in the toolset
// roster), NOT the box's agent-ops identity. It refreshes the recorded
// agent-tools coords from the console FIRST, so a --data rebuild's fresh
// agent-tools pubkey does not stale every signed call.
func WorldMCP(cfg *config.Config) (*client.McpClient, error) {
	RefreshAgentToolsCoords(cfg)
	if cfg == nil || cfg.AgentToolsURL == "" || cfg.AgentToolsPubkey == "" {
		return nil, fmt.Errorf("no freehold-agent-tools coords recorded (run `freehold login` against the CP)")
	}
	auth, err := identity.AgentAuth(oplogin.Dir())
	if err != nil {
		return nil, fmt.Errorf("this box has no operator identity at %s (run `freehold login` to materialize it): %v", oplogin.Dir(), err)
	}
	return client.New(client.ConnectURL(cfg.AgentToolsURL), auth, cfg.AgentToolsPubkey)
}

// CallAgentToolsText issues one agent-tools tool call and returns the text
// content. world_build runs its stages synchronously for minutes, so it uses
// the long deadline (calling exactly ONE path).
func CallAgentToolsText(mc *client.McpClient, tool string, args map[string]interface{}) (string, error) {
	long := map[string]bool{"world_build": true}
	var raw json.RawMessage
	var err error
	if long[tool] {
		raw, err = mc.CallLong(tool, args)
	} else {
		raw, err = mc.Call(tool, args)
	}
	if err != nil {
		return "", err
	}
	var env struct {
		Result *struct {
			Content []map[string]any `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("agent-tools %s: bad envelope: %w", tool, err)
	}
	if env.Result == nil || len(env.Result.Content) == 0 {
		return "", fmt.Errorf("agent-tools %s: empty result", tool)
	}
	t, _ := env.Result.Content[0]["text"].(string)
	return t, nil
}

// RefreshAgentToolsCoords re-reads the CP's live agent-tools URL + pubkey from
// the console's public /api/world and adopts them into cfg. Best effort.
func RefreshAgentToolsCoords(cfg *config.Config) {
	if cfg == nil || cfg.CPURL == "" {
		return
	}
	sec, err := oplogin.SecretHex()
	if err != nil {
		return
	}
	key, err := oplogin.NsecToSecret(sec)
	if err != nil {
		return
	}
	c, err := oplogin.Login(cfg.CPURL, key)
	if err != nil {
		return
	}
	w, err := c.World()
	if err != nil {
		return
	}
	if AdoptAgentToolsCoords(cfg, w) {
		_ = cfg.Save(ConfigPath())
	}
}

// AdoptAgentToolsCoords copies the CP-reported agent-tools URL + pubkey into
// cfg and reports whether anything changed.
func AdoptAgentToolsCoords(cfg *config.Config, w *console.WorldSummary) bool {
	if cfg == nil || w == nil || w.AgentToolsPubkey == "" {
		return false
	}
	changed := false
	if w.AgentToolsPubkey != cfg.AgentToolsPubkey {
		cfg.AgentToolsPubkey = w.AgentToolsPubkey
		changed = true
	}
	if w.AgentToolsURL != "" && w.AgentToolsURL != cfg.AgentToolsURL {
		cfg.AgentToolsURL = w.AgentToolsURL
		changed = true
	}
	return changed
}

// NoLocalRunner reports whether this box is THIN: such a box drives the world —
// including exec — through the CP.
func NoLocalRunner() bool {
	cfg, err := config.Load(ConfigPath())
	if err != nil || cfg == nil || cfg.Runner.Addr == "" {
		return true
	}
	return !AddrReachable(cfg.Runner.Addr)
}

// AddrReachable reports whether a TCP address accepts a connection within a
// short timeout.
func AddrReachable(addr string) bool {
	c, derr := net.DialTimeout("tcp", addr, 700*time.Millisecond)
	if derr != nil {
		return false
	}
	_ = c.Close()
	return true
}

// WorldExecThroughCP runs cmd on the CP's co-located runner via the world_exec
// tool, so a thin login box has the build box's exec capability. Signed as the
// operator.
func WorldExecThroughCP(target, cmd string, secrets []string, timeoutS uint64) error {
	cfg, err := config.Load(ConfigPath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	mc, err := WorldMCP(cfg)
	if err != nil {
		return err
	}
	args := map[string]interface{}{"target": target, "cmd": cmd}
	if timeoutS > 0 {
		args["timeout_s"] = timeoutS
	}
	if len(secrets) > 0 {
		args["secrets"] = secrets
	}
	text, err := CallAgentToolsText(mc, "world_exec", args)
	if err != nil {
		return fmt.Errorf("world_exec: %w", err)
	}
	if text != "" {
		fmt.Print(text)
	}
	return nil
}

// DoorPubkey derives the box's deterministic SSH door public line from its
// agent-ops identity NOSTR secret seed — the SAME key that signs its world API
// calls. The private half never leaves the box.
func DoorPubkey() (string, error) {
	id, err := identity.Load(box.OpsDir())
	if err != nil {
		return "", fmt.Errorf("this box has no ops identity at %s (run `freehold login` to materialize it): %v", box.OpsDir(), err)
	}
	seed, err := hex.DecodeString(id.NostrSecretHex)
	if err != nil || len(seed) != 32 {
		return "", fmt.Errorf("agent-ops nostr_secret is not a 32-byte seed")
	}
	host, _ := os.Hostname()
	return crypto.SSHPublicKeyFromSeed(seed, "freehold-door-"+host)
}

// DoorAction authorizes or revokes this box's public door key on the host door
// through the CP's co-located runner.
func DoorAction(action string) error {
	if action != "authorize" && action != "revoke" {
		return fmt.Errorf("unknown door action %q (authorize|revoke)", action)
	}
	pubkey, err := DoorPubkey()
	if err != nil {
		return err
	}
	cfg, err := config.Load(ConfigPath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	mc, err := WorldMCP(cfg)
	if err != nil {
		return err
	}
	tool := "world_revoke_door"
	verb := "revoked"
	if action == "authorize" {
		tool = "world_authorize_door"
		verb = "authorized"
	}
	if _, err := CallAgentToolsText(mc, tool, map[string]interface{}{"pubkey": pubkey}); err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	fmt.Printf("door key %s on the host door (%s)\n", verb, pubkey)
	return nil
}
