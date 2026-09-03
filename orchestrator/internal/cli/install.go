// install.go — `freehold install`: the standalone interactive front-end over
// the rebuild engine. Go port of the former Rust freehold-install binary
// (installer/src/main.rs: dialoguer collect() + pipeline handoff) — the Rust
// installer crate is deleted; this command is its replacement.
//
// The Rust crate split the bring-up stages (lib.rs) from the dialoguer
// front-end (main.rs); the Go port keeps that split: every stage lives ONCE
// in rebuildEngine (rebuild.go), and install only collects the answers,
// builds the SAME rebuildFlags rebuildCmd uses, and hands the engine over.
// No parallel pipeline. The ONE buffered stdin reader built here is handed
// to the engine so its mid-pipeline prompts (thin-pool placement, door gate)
// never lose bytes to a second bufio.Reader.

package cli

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"freehold/orchestrator/internal/crypto"
	"freehold/orchestrator/internal/drive"
	"freehold/orchestrator/internal/wire"
)

const installBanner = `
  ╭──────────────────────────────────────────────────────────────╮
  │                     Welcome to Freehold                      │
  │                                                              │
  │   Reclaim the future we were promised — one command.         │
  │                                                              │
  │   This installer brings up your appliance end to end:        │
  │     • provisions the door into your Proxmox host            │
  │     • starts the runner (your agent's hands on the host)    │
  │     • boots + deploys the Buzz relay (LXC)                  │
  │     • boots + deploys the control plane (its own LXC)       │
  │                                                              │
  │   You'll be asked a handful of questions with defaults.      │
  │   Ctrl-C at any time aborts cleanly; stages are idempotent.  │
  ╰──────────────────────────────────────────────────────────────╯
`

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Bring up the appliance end to end, interactively: a few questions with defaults, then the full rebuild pipeline (the freehold-install port)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runInstall(os.Stdin, os.Stdout)
	},
}

func runInstall(in io.Reader, out io.Writer) error {
	return runInstallWith(in, out, newRebuildEngine)
}

// runInstallWith is runInstall with the engine constructor injected — the
// test seam (newRebuildEngine resolves real sibling binaries).
func runInstallWith(in io.Reader, out io.Writer, newEngine func(rebuildFlags) (*rebuildEngine, error)) error {
	ui := &installerUI{out: out, raw: in, in: bufio.NewReader(in)}
	fmt.Fprint(out, installBanner)

	f, err := collectAnswers(ui)
	if err != nil {
		return err
	}

	consent := "no"
	if f.confirmStorage {
		consent = "yes"
	}
	fmt.Fprintf(out, `
  ───────────────── Setting up ─────────────────
  host:            %s
  runner:          %s @ %s
  domain:          %s
  relay LXC:       auto vmid (dhcp ip)
  control-plane:   auto vmid (dhcp ip)
  k3s LXC:         auto vmid (dhcp ip)
  operator pk:     %s
  storage consent: %s (applies only if no backend is detected)
  ──────────────────────────────────────────────
`, f.host, f.target, f.addr, f.domain, f.operatorPubkey, consent)

	proceed, err := ui.confirm("Proceed?", true)
	if err != nil {
		return err
	}
	if !proceed {
		fmt.Fprintln(out, "aborted.")
		return nil
	}

	eng, err := newEngine(f)
	if err != nil {
		return err
	}
	// Share the ONE buffered reader — see package comment above.
	eng.stdin = ui.in
	return eng.run()
}

// collectAnswers is the Rust collect() port: the world details, the
// operator-identity select (have-key + optional nsec persist / generate),
// and the storage consent. Returns ready-to-run rebuildFlags.
func collectAnswers(ui *installerUI) (rebuildFlags, error) {
	fmt.Fprintln(ui.out, "  First, a few details about your world. Defaults are shown in [brackets].")
	fmt.Fprintln(ui.out)

	host, err := ui.ask("Proxmox host (address the runner will SSH into)", "root@192.168.30.224")
	if err != nil {
		return rebuildFlags{}, err
	}
	runner, err := ui.ask("Runner name", "proxmox-box")
	if err != nil {
		return rebuildFlags{}, err
	}
	serve, err := ui.ask("Runner MCP address (loopback)", "127.0.0.1:8787")
	if err != nil {
		return rebuildFlags{}, err
	}
	domain, err := ui.ask("Relay domain (must resolve to your host — the identity gate)", "freehold-test.darcydev.net")
	if err != nil {
		return rebuildFlags{}, err
	}
	// vmids + ips are auto-picked/assigned (stored in the config after boot).
	rootfs, err := ui.askUint32("LXC rootfs size (GB)", 16)
	if err != nil {
		return rebuildFlags{}, err
	}
	memory, err := ui.askUint32("LXC memory (MB)", 2048)
	if err != nil {
		return rebuildFlags{}, err
	}

	fmt.Fprintln(ui.out)
	pk, opDir, err := collectOperatorIdentity(ui)
	if err != nil {
		return rebuildFlags{}, err
	}

	// Rust asked this right before the storage stage; install gathers it up
	// front with the rest (same bool, same gate: it only applies when the
	// resolve stage detects NO backend — creating one is a destructive host
	// mutation, gated like teardown).
	fmt.Fprintln(ui.out)
	consent, err := ui.confirm(
		"No existing storage backend — create one (ZFS/LVM-thin)? this carves/relabels host storage", false)
	if err != nil {
		return rebuildFlags{}, err
	}

	return rebuildFlags{
		addr:             serve,
		target:           runner,
		host:             host,
		domain:           domain,
		operatorPubkey:   pk,
		operatorIdentity: opDir,
		sizeGB:           drive.TenantLVSizeGB,
		poolSizeGB:       drive.FreshPoolSizeGB,
		noK3s:            false,
		noLitellm:        false,
		rootfsGB:         rootfs,
		memoryMB:         memory,
		relayGw:          "192.168.30.1",
		configPath:       defaultConfigPath(),
		confirmStorage:   consent,
	}, nil
}

// ---- the operator identity (the one Rust-only piece Go didn't have) ------

// operatorDir is where a minted/persisted operator identity lands —
// freehold_home()/control-plane/operator (Rust operator_dir).
func operatorDir() string { return filepath.Join(rbStateDir(), "operator") }

// collectOperatorIdentity ports the dialoguer Select: have-key (parse npub/
// hex; optionally persist THEIR nsec so every local launch auto-logs-in) or
// generate (mint — or REUSE — the identity dir).
func collectOperatorIdentity(ui *installerUI) (pubkey, opDir string, err error) {
	fmt.Fprintln(ui.out, "  Operator identity:")
	fmt.Fprintln(ui.out, "    1) I have a Nostr key already (paste npub or hex)")
	fmt.Fprintln(ui.out, "    2) Generate one for me (an identity dir we keep for you)")
	for {
		choice, err := ui.ask("Choice", "1")
		if err != nil {
			return "", "", err
		}
		switch strings.TrimSpace(choice) {
		case "1":
			return collectHaveKey(ui)
		case "2":
			return collectGenerate(ui)
		}
		fmt.Fprintln(ui.out, "  (1 or 2)")
	}
}

func collectHaveKey(ui *installerUI) (string, string, error) {
	pk, err := ui.askPubkey()
	if err != nil {
		return "", "", err
	}

	// Optionally persist THEIR nsec: encoded into the same 0600 identity
	// file the generated path writes, so every local launch (TUI +
	// console-login) logs into the console automatically. The key never
	// leaves this machine.
	nsec, err := ui.askNsec()
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(nsec) == "" {
		return pk, "", nil
	}
	secret, err := crypto.NsecToSecret(strings.TrimSpace(nsec))
	if err != nil {
		return "", "", fmt.Errorf("invalid nsec: %w", err)
	}
	derived, err := crypto.PubkeyFromSecret(secret[:])
	if err != nil {
		return "", "", fmt.Errorf("invalid nsec: %w", err)
	}
	if derived != pk {
		return "", "", fmt.Errorf(
			"the nsec's pubkey %s does not match the pubkey you pasted (%s) — fix one", derived, pk)
	}
	dir := operatorDir()
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err == nil {
		return "", "", fmt.Errorf(
			"an operator identity already exists at %s — remove it or reuse that key", dir)
	}
	if err := writeOperatorIdentity(dir, secret[:]); err != nil {
		return "", "", err
	}
	fmt.Fprintln(ui.out)
	fmt.Fprintln(ui.out, "  Persisted your key (0600 — it stays on this machine):")
	fmt.Fprintf(ui.out, "    %s\n", dir)
	fmt.Fprintln(ui.out, "  Every local launch (TUI or console-login) now logs in automatically.")
	return pk, dir, nil
}

// writeOperatorIdentity mirrors core Identity::from_nostr_secret +
// write_to_dir: the operator's OWN nostr secret + a FRESH random enc
// keypair, persisted 0600 in a 0700 dir.
func writeOperatorIdentity(dir string, nostrSecret []byte) error {
	if err := wire.EnsurePrivateDir(dir); err != nil {
		return err
	}
	enc := make([]byte, 32)
	if _, err := rand.Read(enc); err != nil {
		return err
	}
	doc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(nostrSecret),
		"enc_secret_hex":   hex.EncodeToString(enc),
	}
	return wire.WriteJSON0600(filepath.Join(dir, "identity.json"), doc)
}

func collectGenerate(ui *installerUI) (string, string, error) {
	dir := operatorDir()
	// Rust mint_identity: an existing identity.json is LOADED, never
	// clobbered — only mint when absent.
	reused := false
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err == nil {
		reused = true
	} else if err := mintAgentIdentity(dir); err != nil {
		return "", "", err
	}
	pk, err := loadRPubkey(dir)
	if err != nil {
		return "", "", err
	}
	fmt.Fprintln(ui.out)
	if reused {
		fmt.Fprintln(ui.out, "  Reusing the identity already stored at:")
	} else {
		fmt.Fprintln(ui.out, "  Generated a fresh identity for you:")
	}
	fmt.Fprintf(ui.out, "    pubkey: %s\n", pk)
	fmt.Fprintf(ui.out, "    stored: %s (0600 — this IS your key, keep it safe)\n", dir)
	fmt.Fprintln(ui.out, "  You'll log into the console with it (no secrets on screen).")
	return pk, dir, nil
}

// ---- the dialoguer replacement --------------------------------------------

// installerUI prompts over ONE bufio.Reader. That reader is later handed to
// the rebuild engine, so no prompt anywhere in the pipeline may build a
// fresh bufio.Reader over stdin (a fresh one reads ahead past the newline
// and swallows bytes the operator already typed).
type installerUI struct {
	out io.Writer
	raw io.Reader // the underlying stream (terminal detection for no-echo nsec)
	in  *bufio.Reader
}

// ask prints "label [def]: " and returns the trimmed answer; blank = def.
// An empty default prints just "label: ".
func (u *installerUI) ask(label, def string) (string, error) {
	if def == "" {
		fmt.Fprintf(u.out, "  %s: ", label)
	} else {
		fmt.Fprintf(u.out, "  %s [%s]: ", label, def)
	}
	line, err := u.readLine()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(line) == "" {
		return def, nil
	}
	return strings.TrimSpace(line), nil
}

// askUint32 re-prompts until the answer parses (dialoguer's interact_text
// re-prompted on parse failure); blank takes the default.
func (u *installerUI) askUint32(label string, def uint32) (uint32, error) {
	for {
		s, err := u.ask(label, strconv.FormatUint(uint64(def), 10))
		if err != nil {
			return 0, err
		}
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil || n < 1 {
			fmt.Fprintln(u.out, "  (must be a number >= 1)")
			continue
		}
		return uint32(n), nil
	}
}

// confirm prints "label [Y/n]:"/"[y/N]:" and returns the answer; blank = def.
func (u *installerUI) confirm(label string, def bool) (bool, error) {
	suffix := "[y/N]"
	if def {
		suffix = "[Y/n]"
	}
	for {
		fmt.Fprintf(u.out, "  %s %s: ", label, suffix)
		line, err := u.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(u.out, "  (y or n)")
	}
}

// askPubkey loops until the answer parses as npub1… / 64-hex.
func (u *installerUI) askPubkey() (string, error) {
	for {
		s, err := u.ask("Your Nostr public key (npub1… or 64 hex)", "")
		if err != nil {
			return "", err
		}
		if s == "" {
			fmt.Fprintln(u.out, "  (required — the console admin + relay owner)")
			continue
		}
		pk, err := crypto.ParsePubkeyInput(s)
		if err != nil {
			fmt.Fprintf(u.out, "  (invalid pubkey: %v)\n", err)
			continue
		}
		return pk, nil
	}
}

// askNsec reads the optional nsec WITHOUT echo when stdin is a terminal
// (dialoguer's Password); piped input (tests, scripts) reads a plain line
// through the shared reader.
func (u *installerUI) askNsec() (string, error) {
	prompt := "  Your nsec (nsec1… — optional: persist your login key here so the TUI/console-login work with no pasting; empty = skip): "
	if f, ok := u.raw.(*os.File); ok && term.IsTerminal(f.Fd()) && u.in.Buffered() == 0 {
		fmt.Fprint(u.out, prompt)
		b, err := term.ReadPassword(f.Fd())
		fmt.Fprintln(u.out) // the suppressed Enter
		if err != nil && len(b) == 0 {
			return "", fmt.Errorf("no input (EOF)")
		}
		s := string(b)
		for i := range b {
			b[i] = 0 // don't let the pasted nsec linger
		}
		return s, nil
	}
	fmt.Fprint(u.out, prompt)
	return u.readLine()
}

func (u *installerUI) readLine() (string, error) {
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no input (EOF)")
	}
	return line, nil
}
