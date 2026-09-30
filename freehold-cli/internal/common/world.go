package common

import (
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"time"

	"freehold/contract/config"
	"freehold/contract/console"
	"freehold/contract/crypto"
	"freehold/contract/identity"
	oplogin "freehold/freehold-cli/login"
	"freehold/platform/provisioning/box"
)

// WorldExecThroughCP runs cmd on the CP's co-located runner via the console's
// operator-scoped /api/world-exec (the drive-through-CP exec surface a thin
// login box has) — session-authed, no relay roster.
func WorldExecThroughCP(target, cmd string, secrets []string, timeoutS uint64) error {
	cfg, err := config.Load(ConfigPath())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	c, err := ConsoleLogin(cfg)
	if err != nil {
		return err
	}
	text, err := c.WorldExec(target, cmd, secrets, timeoutS)
	if err != nil {
		return fmt.Errorf("world_exec: %w", err)
	}
	if text != "" {
		fmt.Print(text)
	}
	return nil
}

// ConsoleLogin logs the operator in to the CP, preferring the https CPURL — a
// NIP-98 signed event must not travel over plaintext by default — and falling
// back to the recorded LAN IP ONLY when the public edge is unreachable.
func ConsoleLogin(cfg *config.Config) (*console.Client, error) {
	sec, err := oplogin.SecretHex()
	if err != nil {
		return nil, err
	}
	key, err := oplogin.NsecToSecret(sec)
	if err != nil {
		return nil, err
	}
	var lastErr error
	if cfg.CPURL != "" {
		if c, err := oplogin.Login(cfg.CPURL, key); err == nil {
			return c, nil
		} else {
			lastErr = err
		}
	}
	if ip := config.LxcIP(cfg.Lxc.Cp); ip != "" {
		if c, err := oplogin.Login("http://"+ip+":8080", key); err == nil {
			return c, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no CP URL configured")
	}
	return nil, lastErr
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
// through the console's operator-scoped /api/world-door (session-authed, no
// relay roster).
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
	c, err := ConsoleLogin(cfg)
	if err != nil {
		return err
	}
	if action == "authorize" {
		err = c.WorldDoorAuthorize(pubkey)
	} else {
		err = c.WorldDoorRevoke(pubkey)
	}
	if err != nil {
		return fmt.Errorf("world door %s: %w", action, err)
	}
	fmt.Printf("door key %s on the host door (%s)\n", action+"d", pubkey)
	return nil
}
