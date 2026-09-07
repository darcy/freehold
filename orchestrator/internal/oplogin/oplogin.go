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

// identityFile is the operator identity ledger file name within Dir().
const identityFile = "identity.json"

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

// Interactive is `freehold login` (root-free): prompt for the CP address + CP
// pubkey + operator nsec, establish the NIP-98 console session (authorizing this
// operator against that CP), then seed a local connection/desire profile so the
// operator can just run `freehold` afterwards. It ends the command — build /
// teardown are separate (root-requiring) commands that never ride login.
//
// This is the fresh-box recovery path: nothing that lived only on a lost box is
// needed — only CP address + pubkey (where the world is) + the operator's own
// nsec (who the operator is). The CP's world summary seeds relay/CP coordinates.
func Interactive() error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	// ONE buffered reader over stdin (AGENTS.md discipline): a fresh reader per
	// prompt reads ahead past the first newline and breaks back-to-back prompts.
	stdin := bufio.NewReader(os.Stdin)

	cpURL := cfg.CPURL
	if cpURL == "" {
		cpURL = strings.TrimSpace(promptLine(stdin, "CP address (https://cp-<domain>): "))
	}
	if cpURL == "" {
		return fmt.Errorf("no CP address provided — where is the control plane?")
	}

	cpPubkey := cfg.CpPubkey
	if cpPubkey == "" {
		cpPubkey = strings.TrimSpace(promptLine(stdin, "CP pubkey (64-hex): "))
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
		raw, err := readNsec(stdin)
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
	defer zero32(&secret)

	pk, err := crypto.PubkeyFromSecret(secret[:])
	if err != nil {
		return err
	}
	c, err := Login(cpURL, secret)
	if err != nil {
		return fmt.Errorf("login to %s failed: %w", cpURL, err)
	}
	// Only a successful login is persisted as "the operator" — a mistyped key
	// never poisons the auto-login ledger.
	if !have {
		if _, err := Save(secret); err != nil {
			return err
		}
	}
	// Seed a local desire profile so `freehold` is immediately runnable: CP coords
	// the operator just authorized against + the relay/CP identities the CP itself
	// reports. Degrades gracefully if the CP predates /api/world — the CP the box
	// dialed + the operator key are always recorded.
	var relayURL, relayWS, relayPubkey, worldCPPub string
	if w, werr := c.World(); werr == nil {
		relayURL, relayWS, relayPubkey, worldCPPub = w.RelayURL, w.RelayWsURL, w.RelayPubkey, w.CPPubkey
	} else {
		fmt.Fprintf(os.Stderr, "  (note: world summary not available — %v)\n", werr)
	}
	// The CP pubkey is the box's trust anchor for a CP it has never met: NIP-98
	// proves the OPERATOR's identity to whatever answers at cpURL, never the CP
	// back — so cross-check the operator-supplied pubkey against the CP's own
	// /api/world report (hard error on mismatch), and fall back to that report
	// only when the operator offered none. A bogus/hijacked anchor never seeds.
	anchor, err := resolveCPPubkey(cpPubkey, worldCPPub)
	if err != nil {
		return err
	}
	if err := seed(cfg, cpURL, anchor, pk, relayURL, relayWS, relayPubkey); err != nil {
		return err
	}
	fmt.Printf("logged in as %s against %s — run `freehold` to operate the world\n", pk, cpURL)
	return nil
}

// resolveCPPubkey establishes the recorded CP trust anchor. A user-supplied
// pubkey must match what the CP reports about itself; a blank one is accepted
// only when the CP offers one. Neither source yields an anchor -> error (the
// box must not trust a CP it cannot identify).
func resolveCPPubkey(user, world string) (string, error) {
	if user != "" && world != "" && user != world {
		return "", fmt.Errorf("CP pubkey mismatch: you supplied %.10s…, but the CP reported %.10s… — aborting to avoid trusting the wrong control plane", user, world)
	}
	if user != "" {
		return user, nil
	}
	if world != "" {
		return world, nil
	}
	return "", fmt.Errorf("no CP pubkey provided and the CP reported none — cannot establish a trust anchor")
}

// seed writes the logged-in connection/desire profile back to the config path,
// preserving any surviving local facts (plane, runners, coords) the box already
// holds and filling the connection coordinates login just established.
func seed(cfg *config.Config, cpURL, cpPubkey, operatorPK, relayURL, relayWS, relayPubkey string) error {
	cfg.CPURL = cpURL
	if cpPubkey != "" {
		cfg.CpPubkey = cpPubkey
	}
	cfg.OperatorPubkey = operatorPK
	if relayURL != "" {
		cfg.RelayURL = relayURL
	}
	if relayWS != "" {
		cfg.RelayWsURL = relayWS
	}
	if relayPubkey != "" {
		cfg.RelayPubkey = &relayPubkey
	}
	return cfg.Save(config.DefaultPath())
}

// Logout drops the local login ledger (the operator identity the box kept from a
// previous login) so a fresh login (or a different operator's nsec) starts clean.
// It does NOT touch the CP, relay, or any world resources — this box only.
func Logout() error {
	idp := filepath.Join(Dir(), identityFile)
	if err := os.Remove(idp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("logout: remove operator identity: %w", err)
	}
	return nil
}

// promptLine reads a single plain (non-secret) line through the shared buffered
// reader (never a fresh reader — see AGENTS.md's ONE-reader discipline).
func promptLine(in *bufio.Reader, prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	line, err := in.ReadString('\n')
	if err != nil && len(line) == 0 {
		return ""
	}
	return strings.TrimSpace(line)
}

// readNsec prompts for the operator nsec WITHOUT echo when stdin is a terminal
// (the key must never appear on screen); piped input (scripts/tests) reads a
// plain line. Mirrors installer::ask_nsec. The read bytes are zeroed on return.
// Reads through the caller's ONE shared buffered reader (`in`).
func readNsec(in *bufio.Reader) (string, error) {
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
	line, err := in.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", fmt.Errorf("no input")
	}
	return strings.TrimSpace(line), nil
}

func zero32(s *[32]byte) {
	for i := range s {
		s[i] = 0
	}
}
