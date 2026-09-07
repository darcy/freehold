// Package oplogin is the operator's local login ledger: where their own nsec
// is persisted (freeholdHome()/control-plane/operator) and how the local CLI /
// TUI establish the CP console session with it. The operator's pubkey is the
// console admin, so a successful login grants every remote CP operation
// locally (overview, provision, grant, revoke, agents, portal).
//
// Kept independent of internal/cli so the TUI can use it without the
// cli<->tui import cycle the running-mode flows deliberately avoid.
package oplogin

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	"freehold/orchestrator/internal/config"
	"freehold/orchestrator/internal/console"
	"freehold/orchestrator/internal/crypto"
	"freehold/orchestrator/internal/flows"
	"freehold/orchestrator/internal/wire"
)

// Dir returns the durable operator identity dir (the operator's own nsec).
func Dir() string {
	home := os.Getenv("FREEHOLD_HOME")
	if home == "" {
		home = "~/.freehold"
		if h := os.Getenv("HOME"); h != "" {
			home = h + "/.freehold"
		}
	}
	return filepath.Join(home, "control-plane", "operator")
}

// SecretHex returns the persisted operator nsec hex, or an error when none is
// recorded yet.
func SecretHex() (string, error) {
	id, err := flows.LoadIdentity(Dir())
	if err != nil {
		return "", err
	}
	return id.NostrSecretHex, nil
}

// Save persists the operator's own nsec to the operator identity dir (0600
// file / 0700 dir). First-run-wins: an already-recorded key is never
// clobbered. Returns whether it wrote.
func Save(secret [32]byte) (bool, error) {
	if _, err := SecretHex(); err == nil {
		return false, nil
	}
	if err := wire.EnsurePrivateDir(Dir()); err != nil {
		return false, err
	}
	enc := make([]byte, 32)
	if _, err := rand.Read(enc); err != nil {
		return false, err
	}
	doc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(secret[:]),
		"enc_secret_hex":   hex.EncodeToString(enc),
	}
	if err := wire.WriteJSON0600(filepath.Join(Dir(), "identity.json"), doc); err != nil {
		return false, err
	}
	return true, nil
}

// NsecToSecret accepts nsec1<bech32> or bare 64-hex -> 32-byte secret.
func NsecToSecret(s string) ([32]byte, error) { return crypto.NsecToSecret(s) }

// Login establishes the operator's NIP-98 console session for base.
func Login(base string, secret [32]byte) (*console.Client, error) {
	defer zero32(&secret)
	return console.Login(base, secret[:], 15*time.Second)
}

// Interactive is `freehold --login`: reuse the persisted operator nsec if
// present, else prompt on stdin, persist it, and establish the console session
// against the recorded CP URL. Prints operator pubkey + session cookie.
func Interactive() error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if cfg == nil || cfg.CPURL == "" {
		return fmt.Errorf("no config with a CP URL — run `freehold build` to bring the world up first")
	}

	secret := [32]byte{}
	have := false
	if hexStr, herr := SecretHex(); herr == nil {
		if b, derr := hex.DecodeString(hexStr); derr == nil && len(b) == 32 {
			copy(secret[:], b)
			have = true
		}
	}
	if !have {
		raw, err := readNsec()
		if err != nil {
			return err
		}
		if raw == "" {
			return fmt.Errorf("no nsec provided")
		}
		secret, err = NsecToSecret(raw)
		if err != nil {
			return fmt.Errorf("bad nsec: %w", err)
		}
	}

	pk, err := crypto.PubkeyFromSecret(secret[:])
	if err != nil {
		return err
	}
	c, err := Login(cfg.CPURL, secret)
	if err != nil {
		return fmt.Errorf("login to %s failed: %w", cfg.CPURL, err)
	}
	// Only a successful login is persisted as "the operator" — a mistyped key
	// never poisons the auto-login ledger.
	if !have {
		if _, err := Save(secret); err != nil {
			return err
		}
	}
	cookie := c.Cookie()
	if len(cookie) > 12 {
		cookie = cookie[:12] + "…"
	}
	fmt.Printf("logged in as %s (session cookie %s)\n", pk, cookie)
	return nil
}

// readNsec prompts for the operator nsec WITHOUT echo when stdin is a terminal
// (the key must never appear on screen); piped input (scripts/tests) reads a
// plain line. Mirrors installer::ask_nsec. The read bytes are zeroed on return.
func readNsec() (string, error) {
	fmt.Fprint(os.Stderr, "operator nsec (nsec1… or 64-hex): ")
	if term.IsTerminal(os.Stdin.Fd()) {
		b, err := term.ReadPassword(os.Stdin.Fd())
		fmt.Fprintln(os.Stderr) // the suppressed Enter
		if err != nil && len(b) == 0 {
			return "", fmt.Errorf("no input")
		}
		s := strings.TrimSpace(string(b))
		for i := range b {
			b[i] = 0
		}
		return s, nil
	}
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", fmt.Errorf("no input")
	}
	return strings.TrimSpace(sc.Text()), nil
}

func zero32(s *[32]byte) {
	for i := range s {
		s[i] = 0
	}
}
