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

// OpsDir is where the BOX's own provisioning identity lives — the same
// `agent-ops` identity `freehold build` / `teardown` sign with (rbOpsDir,
// internal/cli). Login materializes it so a fresh box is a durable, self-owned
// actor; it is box-local and excluded from any off-box backup.
func OpsDir() string {
	return filepath.Join(controlPlaneDir(), "agent-ops")
}

// controlPlaneDir is the box's local control-plane state area (FREEHOLD_HOME's
// `control-plane`, the parent of both the operator ledger and agent-ops).
func controlPlaneDir() string {
	return filepath.Join(identityHome(), "control-plane")
}

// identityHome is the box's freehold state root (FREEHOLD_HOME or ~/.freehold).
func identityHome() string {
	if home := os.Getenv("FREEHOLD_HOME"); home != "" {
		return home
	}
	home := "~/.freehold"
	if h := os.Getenv("HOME"); h != "" {
		home = h + "/.freehold"
	}
	return home
}

// EnsureOpsIdentity mints the box's own provisioning identity if missing
// (first-run-wins), returning its Nostr pubkey. idempotent + hermetic.
func EnsureOpsIdentity() (string, error) {
	if _, err := os.Stat(filepath.Join(OpsDir(), identityFile)); err == nil {
		return OpsPubkey()
	}
	if err := mintIdentity(OpsDir()); err != nil {
		return "", err
	}
	return OpsPubkey()
}

// OpsPubkey loads the box ops identity's Nostr pubkey.
func OpsPubkey() (string, error) {
	id, err := flows.LoadIdentity(OpsDir())
	if err != nil {
		return "", err
	}
	return id.NostrPubkeyHex()
}

// mintIdentity writes a fresh nostr + enc identity (identity.json 0600, dir
// 0700) at dir — the same shape `freehold build` mints for agent-ops.
func mintIdentity(dir string) error {
	if err := wire.EnsurePrivateDir(dir); err != nil {
		return err
	}
	nostrSecret := make([]byte, 32)
	encSecret := make([]byte, 32)
	if _, err := rand.Read(nostrSecret); err != nil {
		return err
	}
	if _, err := rand.Read(encSecret); err != nil {
		return err
	}
	doc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(nostrSecret),
		"enc_secret_hex":   hex.EncodeToString(encSecret),
	}
	return wire.WriteJSON0600(filepath.Join(dir, identityFile), doc)
}

// identityFile is the operator identity ledger file name within Dir().
const identityFile = "identity.json"

// Dir returns the durable operator identity dir (the operator's own nsec).
func Dir() string {
	return filepath.Join(controlPlaneDir(), "operator")
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

// Interactive is `freehold login` (root-free): prompt for the CP address + the
// operator nsec, establish the NIP-98 console session (authorizing this operator
// against that CP), then seed a local connection/desire profile so the operator
// can just run `freehold` afterwards. It ends the command — build / teardown are
// separate (root-requiring) commands that never ride login.
//
// This is the fresh-box recovery path: nothing that lived only on a lost box is
// needed — only CP address (where the world is) + the operator's own nsec (who
// the operator is). The CP's world summary seeds the relay/CP coordinates AND
// the CP's own identity (cp_pubkey): NIP-98 authorizes the OPERATOR to whatever
// answers at cp_url (the console only admits admin-minted operator pubkeys), so
// a legitimate box logging into its actual CP needs no separately-known CP
// pubkey — the anchor is simply the CP's own self-report. The trust boundary
// for a wrong-or-hijacked cp_url is TLS/DNS on that URL, not this recorded
// anchor, so the operator never needs to type or know the CP pubkey here.
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
	var relayURL, relayWS, relayPubkey, worldCPPub, atURL, atPubkey string
	if w, werr := c.World(); werr == nil {
		relayURL, relayWS, relayPubkey, worldCPPub = w.RelayURL, w.RelayWsURL, w.RelayPubkey, w.CPPubkey
		atURL, atPubkey = w.AgentToolsURL, w.AgentToolsPubkey
	} else {
		fmt.Fprintf(os.Stderr, "  (note: world summary not available — %v)\n", werr)
	}
	// The CP pubkey is the box's record of the CP's own identity — adopted, never
	// typed. NIP-98 only admits admin-minted operator pubkeys, so a legitimate box
	// logging into its actual CP never needs to know it up front; the anchor is
	// the CP's /api/world self-report, normalized to 64-hex. (The boundary for a
	// wrong-or-hijacked cp_url is TLS/DNS on that URL — a malicious server at the
	// wrong address can report any pubkey it likes — which is why this is an
	// informational anchor, not a substitute for verifying the CP endpoint.)
	anchor := resolveCPPubkey(worldCPPub)
	// Materialize the box's own provisioning identity (first-run-wins): this is
	// how the box is a durable, self-owned actor whose grant to the CP can live
	// locally — the "we only need to login once" property.
	if _, err := EnsureOpsIdentity(); err != nil {
		return err
	}
	if err := seed(cfg, cpURL, anchor, pk, relayURL, relayWS, relayPubkey, atURL, atPubkey); err != nil {
		return err
	}
	fmt.Printf("logged in as %s against %s — run `freehold` to operate the world\n", pk, cpURL)
	return nil
}

// resolveCPPubkey normalizes the CP's own self-reported pubkey (from /api/world)
// to lowercase 64-hex for the recorded trust anchor. Accepts npub1<bech32> or
// hex (plus trim/case) via the shared pubkey parser; a blank or unparseable
// report is returned as-is ("" / the raw value) — login still seeds the operator
// + CP coords, since the operator's NIP-98 admin session IS the authorization.
func resolveCPPubkey(world string) string {
	if world == "" {
		return ""
	}
	out, err := crypto.ParsePubkeyInput(world)
	if err != nil {
		return world
	}
	return out
}

// seed writes the logged-in connection/desire profile back to the config path,
// preserving any surviving local facts (plane, runners, coords) the box already
// holds and filling the connection coordinates login just established.
func seed(cfg *config.Config, cpURL, cpPubkey, operatorPK, relayURL, relayWS, relayPubkey, atURL, atPubkey string) error {
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
	// The agent-tools coords (MCP URL + roster audience) the CP reports —
	// a fresh box then drives the Agents view / create-grant-manage without
	// anything that lived on the box that deployed the toolset.
	if atURL != "" {
		cfg.AgentToolsURL = atURL
	}
	if atPubkey != "" {
		cfg.AgentToolsPubkey = atPubkey
	}
	// The operator identity dir records where this box's console-admin nsec
	// ledger lives (the same ledger the TUI auto-logs in from).
	opDir := Dir()
	cfg.OperatorIdentity = &opDir
	// [runner] is deliberately NOT touched: it is the DEPLOYED provisioning
	// runner's own identity, authored by `freehold build` and used as the
	// audience of every signed call — a box has no deployed runner at login,
	// fabricating one here would clobber a surviving local fact. The box's own
	// agent-ops identity is materialized on disk only (EnsureOpsIdentity);
	// opsPK is the box's caller identity, never the runner's.
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
