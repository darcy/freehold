package console

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"

	"freehold/contract/crypto"
)

//go:embed index.html
var indexHTML string

// osWriteFile is the injected resolver write path (web.rs dns_write).
func osWriteFile(path string, body []byte) error {
	return os.WriteFile(path, body, 0o644)
}

// EnsureConsoleIdentity loads the console's own identity at
// <stateDir>/console/identity.json, minting it on first serve (the Rust
// Console::load_or_create behavior — a keypair is NEVER shipped, it is born on
// the box). Returns the secret + pubkey.
func EnsureConsoleIdentity(stateDir string) (secret []byte, pubkey string, err error) {
	dir := filepath.Join(stateDir, "console")
	file := filepath.Join(dir, "identity.json")
	if raw, err := os.ReadFile(file); err == nil {
		var id struct {
			NostrSecretHex string `json:"nostr_secret_hex"`
		}
		if err := json.Unmarshal(raw, &id); err != nil {
			return nil, "", err
		}
		sec, err := hex.DecodeString(id.NostrSecretHex)
		if err != nil {
			return nil, "", err
		}
		pk, err := crypto.PubkeyFromSecret(sec)
		if err != nil {
			return nil, "", err
		}
		return sec, pk, nil
	}
	nostrSecret := make([]byte, 32)
	encSecret := make([]byte, 32)
	if _, err := rand.Read(nostrSecret); err != nil {
		return nil, "", err
	}
	if _, err := rand.Read(encSecret); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	doc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(nostrSecret),
		"enc_secret_hex":   hex.EncodeToString(encSecret),
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		return nil, "", err
	}
	pk, err := crypto.PubkeyFromSecret(nostrSecret)
	if err != nil {
		return nil, "", err
	}
	return nostrSecret, pk, nil
}

// reloadDnsmasq ensures dnsmasq is installed, pointed at the state-dir conf
// (copied into /etc/dnsmasq.d/), and reloaded (web.rs dns_reload).
func reloadDnsmasq(stateDir string) error {
	ensure := "command -v dnsmasq >/dev/null 2>&1 || (export DEBIAN_FRONTEND=noninteractive; apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq dnsmasq >/dev/null 2>&1); mkdir -p /etc/dnsmasq.d"
	if _, err := runShell(ensure); err != nil {
		return err
	}
	conf, err := os.ReadFile(DnsmasqConfPath(stateDir))
	if err != nil {
		return err
	}
	if err := os.WriteFile("/etc/dnsmasq.d/freehold-names.conf", conf, 0o644); err != nil {
		return err
	}
	if _, err := runShell("systemctl enable dnsmasq >/dev/null 2>&1; systemctl restart dnsmasq >/dev/null 2>&1 || killall -HUP dnsmasq >/dev/null 2>&1; true"); err != nil {
		return err
	}
	return nil
}

func runShell(cmd string) (string, error) {
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	return string(out), err
}