package acceptance

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"freehold/contract/crypto"
	"freehold/control-plane/state"
)

// ---- fake relay ----

func spawnRelay(t *testing.T) (string, *RelayState) {
	t.Helper()
	rs := NewRelayState()
	ts := httptest.NewServer(rs.Handler())
	t.Cleanup(ts.Close)
	rs.SetBaseURL(ts.URL)
	return ts.URL, rs
}

// ---- CP state + console identity ----

func openStore(t *testing.T, dir string) *state.StateStore {
	t.Helper()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

// ensureConsoleIdentity writes <stateDir>/console/identity.json (the
// channel-owner credential cpstate.ConsoleSecret loads) and returns its
// (nostr secret, pubkey).
func ensureConsoleIdentity(t *testing.T, stateDir string) ([]byte, string) {
	t.Helper()
	sec := make([]byte, 32)
	if _, err := rand.Read(sec); err != nil {
		t.Fatal(err)
	}
	enc := make([]byte, 32)
	if _, err := rand.Read(enc); err != nil {
		t.Fatal(err)
	}
	pk, err := crypto.PubkeyFromSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	encPub, err := crypto.X25519PublicKey(enc)
	if err != nil {
		t.Fatal(err)
	}
	_ = encPub
	dir := filepath.Join(stateDir, "console")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := map[string]string{
		"nostr_secret_hex": hex.EncodeToString(sec),
		"enc_secret_hex":   hex.EncodeToString(enc),
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return sec, pk
}

// readRunnerIdentity reads a provisioned package's identity.json.
func readRunnerIdentity(t *testing.T, dir string) (nostrSecret, encSecret []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	var doc struct {
		NostrSecretHex string `json:"nostr_secret_hex"`
		EncSecretHex   string `json:"enc_secret_hex"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	nostrSecret, err = hex.DecodeString(doc.NostrSecretHex)
	if err != nil {
		t.Fatal(err)
	}
	encSecret, err = hex.DecodeString(doc.EncSecretHex)
	if err != nil {
		t.Fatal(err)
	}
	return nostrSecret, encSecret
}

func pubkeyOf(secret []byte) (string, error) {
	return crypto.PubkeyFromSecret(secret)
}

func hex32(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("not a 32-byte hex string: %q", s)
	}
	return b
}

func hex64(c byte) string {
	s := make([]byte, 64)
	for i := range s {
		s[i] = c
	}
	return string(s)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ---- runner subprocess ----

func findRunner(t *testing.T) string {
	t.Helper()
	// cwd is control-plane/acceptance; the cargo workspace root is two up.
	for _, c := range []string{
		filepath.Join("..", "..", "target", "debug", "runner"),
		filepath.Join("..", "..", "target", "release", "runner"),
	} {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	t.Fatalf("runner binary not found (run `cargo build --bin runner` from repo root)")
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startRunner serves a provisioned package in a real `runner serve` process
// (the exact code path the shipped runner runs) and returns its MCP addr.
func startRunner(t *testing.T, pkgDir string) string {
	t.Helper()
	bin := findRunner(t)
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cmd := exec.Command(bin, "serve", "--state-dir", pkgDir, "--addr", addr)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start runner: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("runner at %s did not start", addr)
	return ""
}
