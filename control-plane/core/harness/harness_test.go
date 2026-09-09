// Package harness drives the Rust oracle binary (freehold-harness-oracle)
// and asserts the Go port of each crypto/wire primitive is byte-identical.
// This is the phase-2 release gate: a primitive is not "done" until it passes
// here, and a harness failure blocks that primitive (and everything
// downstream of it).
package harness

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/crypto"
	"freehold/contract/wire"
)

var (
	seedS1 = "0000000000000000000000000000000000000000000000000000000000000007"
	seedS2 = "1111111111111111111111111111111111111111111111111111111111111111"
	nowTS  = int64(1720000000)
)

func findOracle(t *testing.T) string {
	t.Helper()
	for _, c := range []string{filepath.Join("..", "..", "..", "target", "debug", "freehold-harness-oracle")} {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	t.Fatalf("oracle binary not found (run `cargo build -p freehold-harness-oracle` from repo root)")
	return ""
}

func oracle(t *testing.T, req map[string]interface{}) map[string]interface{} {
	t.Helper()
	bin := findOracle(t)
	reqBytes, _ := json.Marshal(req)
	cmd := exec.Command(bin)
	cmd.Stdin = strings.NewReader(string(reqBytes) + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("oracle exec failed: %v\nstderr: %s", err, cmd.Stderr)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("oracle bad json: %v\nout: %s", err, out)
	}
	if e, ok := resp["error"]; ok {
		t.Fatalf("oracle error: %v", e)
	}
	return resp
}

func h2b(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// x25519PubOfSecret returns the X25519 pubkey bytes of a secret, derived
// byte-exactly by the Rust oracle (identity enc_pubkey_hex).
func x25519PubOfSecret(t *testing.T, secret []byte) []byte {
	t.Helper()
	pk := oracle(t, map[string]interface{}{"op": "enc_pubkey", "secret": hex.EncodeToString(secret)})["enc_pubkey"].(string)
	return h2b(t, pk)
}

func TestPubkeyMatchesOracle(t *testing.T) {
	got, err := crypto.PubkeyFromSecret(h2b(t, seedS1))
	if err != nil {
		t.Fatal(err)
	}
	want := oracle(t, map[string]interface{}{"op": "pubkey", "secret": seedS1})["pubkey"].(string)
	if got != want {
		t.Fatalf("pubkey mismatch:\n  go:   %s\n  rust: %s", got, want)
	}
}

func TestPubkeyRejectsInvalidScalar(t *testing.T) {
	if _, err := crypto.PubkeyFromSecret(make([]byte, 32)); err == nil {
		t.Fatal("expected error for zero scalar")
	}
	tooBig := h2b(t, "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364142")
	if _, err := crypto.PubkeyFromSecret(tooBig); err == nil {
		t.Fatal("expected error for scalar >= N")
	}
}

func TestSealedBoxCrossDirections(t *testing.T) {
	const aad = "db-pass"
	const plaintext = "hunter2-secret-value"

	// Consistent keypair: seedS1 IS the X25519 static secret; its X25519
	// pubkey (from the oracle) is what both sides seal to.
	recipPub := x25519PubOfSecret(t, h2b(t, seedS1))
	recipientPubStr := hex.EncodeToString(recipPub)

	// Rust seals; Go opens with seedS1.
	rustSealed := oracle(t, map[string]interface{}{
		"op": "seal", "recipientPub": recipientPubStr, "aad": aad, "plaintext": plaintext,
	})["blob"].(string)
	openRust, err := crypto.Open(h2b(t, seedS1), []byte(aad), h2b(t, rustSealed))
	if err != nil {
		t.Fatalf("Go open of Rust-sealed blob failed: %v", err)
	}
	if string(openRust) != plaintext {
		t.Fatalf("Go open mismatch: got %q want %q", openRust, plaintext)
	}

	// Go seals; Rust opens with seedS1.
	goBlob, err := crypto.Seal(recipPub, []byte(aad), []byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	want := oracle(t, map[string]interface{}{
		"op": "open", "secret": seedS1, "aad": aad, "blob": hex.EncodeToString(goBlob),
	})["plaintext"].(string)
	if want != plaintext {
		t.Fatalf("Rust open of Go-sealed blob mismatch: got %q want %q", want, plaintext)
	}
}

func TestSealedBoxSecondKeypair(t *testing.T) {
	recipPub := x25519PubOfSecret(t, h2b(t, seedS2))
	pubStr := hex.EncodeToString(recipPub)
	rustSealed := oracle(t, map[string]interface{}{
		"op": "seal", "recipientPub": pubStr, "aad": "ssh", "plaintext": "private-key",
	})["blob"].(string)
	out, err := crypto.Open(h2b(t, seedS2), []byte("ssh"), h2b(t, rustSealed))
	if err != nil || string(out) != "private-key" {
		t.Fatalf("open failed: err=%v out=%q", err, out)
	}
}

func TestSealedBoxFailClosed(t *testing.T) {
	recipPub := x25519PubOfSecret(t, h2b(t, seedS1))
	blob, err := crypto.Seal(recipPub, []byte("name"), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crypto.Open(h2b(t, seedS1), []byte("name"), blob); err != nil {
		t.Fatalf("self-open failed: %v", err)
	}
	if _, err := crypto.Open(h2b(t, seedS1), []byte("wrong-name"), blob); err == nil {
		t.Fatal("expected error for wrong AAD")
	}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := crypto.Open(h2b(t, seedS1), []byte("name"), tampered); err == nil {
		t.Fatal("expected error for tampered blob")
	}
}

func TestSignEventMatchesOracle(t *testing.T) {
	tags := [][]string{{"u", "http://relay:3000/events"}, {"method", "POST"}, {"nonce", "abc"}}
	pkGo, idGo, sigGo, err := wire.SignEvent(h2b(t, seedS1), 27235, nowTS, tags, "")
	if err != nil {
		t.Fatal(err)
	}
	resp := oracle(t, map[string]interface{}{
		"op": "sign_event", "secret": seedS1, "created_at": nowTS, "kind": 27235,
		"tags": tags, "content": "",
	})
	if want := resp["pubkey"].(string); pkGo != want {
		t.Fatalf("pubkey mismatch: go=%s rust=%s", pkGo, want)
	}
	if want := resp["id"].(string); idGo != want {
		t.Fatalf("id mismatch:\n  go:   %s\n  rust: %s", idGo, want)
	}
	if want := resp["sig"].(string); sigGo != want {
		t.Fatalf("sig mismatch:\n  go:   %s\n  rust: %s", sigGo, want)
	}
}

func TestSignEventEscaping(t *testing.T) {
	content := `{"cmd":"uname -a","target":"local","exit":0,"note":"a<b>c"}`
	_, idGo, sigGo, err := wire.SignEvent(h2b(t, seedS1), 48001, nowTS, [][]string{}, content)
	if err != nil {
		t.Fatal(err)
	}
	resp := oracle(t, map[string]interface{}{
		"op": "sign_event", "secret": seedS1, "created_at": nowTS, "kind": 48001,
		"tags": [][]string{}, "content": content,
	})
	if want := resp["id"].(string); idGo != want {
		t.Fatalf("escaping id mismatch:\n  go:   %s\n  rust: %s", idGo, want)
	}
	if want := resp["sig"].(string); sigGo != want {
		t.Fatalf("escaping sig mismatch:\n  go:   %s\n  rust: %s", sigGo, want)
	}
}

func TestVerifyEventCross(t *testing.T) {
	resp := oracle(t, map[string]interface{}{
		"op": "sign_event", "secret": seedS1, "created_at": nowTS, "kind": 9,
		"tags": [][]string{{"h", "abc"}}, "content": "hello",
	})
	pk := resp["pubkey"].(string)
	sig := resp["sig"].(string)
	got, err := wire.VerifyEvent(pk, nowTS, 9, [][]string{{"h", "abc"}}, "hello", sig)
	if err != nil {
		t.Fatalf("Go verify of Rust signature failed: %v", err)
	}
	if got != pk {
		t.Fatalf("verify returned %s want %s", got, pk)
	}
	if _, err := wire.VerifyEvent(pk, nowTS, 9, [][]string{{"h", "abc"}}, "hello!", sig); err == nil {
		t.Fatal("expected verify failure on tampered content")
	}
}

func TestNip98AuthMatchesOracle(t *testing.T) {
	url := "http://relay:3000/events"
	method := "POST"
	authGo, err := wire.Nip98Auth(h2b(t, seedS1), method, url, nowTS)
	if err != nil {
		t.Fatal(err)
	}
	authRust := oracle(t, map[string]interface{}{
		"op": "nip98_auth", "secret": seedS1, "method": method, "url": url, "now": nowTS,
	})["auth"].(string)

	for _, a := range []string{authGo, authRust} {
		if !strings.HasPrefix(a, "Nostr ") {
			t.Fatalf("bad auth shape: %q", a)
		}
	}
	decode := func(a string) map[string]interface{} {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, "Nostr "))
		if err != nil {
			t.Fatalf("bad base64: %v", err)
		}
		var ev map[string]interface{}
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("bad event json: %v", err)
		}
		return ev
	}
	evGo := decode(authGo)
	evRust := decode(authRust)
	if evGo["kind"].(float64) != 27235 || evRust["kind"].(float64) != 27235 {
		t.Fatalf("kind mismatch: go=%v rust=%v", evGo["kind"], evRust["kind"])
	}
	if evGo["pubkey"] != evRust["pubkey"] {
		t.Fatalf("pubkey mismatch: go=%v rust=%v", evGo["pubkey"], evRust["pubkey"])
	}
	if evGo["id"].(string) == "" || evGo["sig"].(string) == "" {
		t.Fatal("go auth event missing id/sig")
	}
	goPK, _ := crypto.PubkeyFromSecret(h2b(t, seedS1))
	if evGo["pubkey"].(string) != goPK {
		t.Fatalf("go auth pubkey %v != derived %s", evGo["pubkey"], goPK)
	}
}

func TestAuditSignMatchesOracle(t *testing.T) {
	content := `{"cmd":"uname -a","target":"local","exit":0}`
	goEv, err := wire.SignAuditEvent(h2b(t, seedS1), content)
	if err != nil {
		t.Fatal(err)
	}
	resp := oracle(t, map[string]interface{}{"op": "audit_sign", "secret": seedS1, "content": content})
	if goEv.Pubkey != resp["pubkey"].(string) {
		t.Fatalf("audit pubkey mismatch: go=%s rust=%s", goEv.Pubkey, resp["pubkey"])
	}
	if goEv.Sig != resp["sig"].(string) {
		t.Fatalf("audit sig mismatch:\n  go:   %s\n  rust: %s", goEv.Sig, resp["sig"])
	}
	if err := wire.VerifyAuditEvent(goEv); err != nil {
		t.Fatalf("go self-verify: %v", err)
	}
}

func TestNip44MatchesOracle(t *testing.T) {
	value := "remember this"
	sealedRust := oracle(t, map[string]interface{}{"op": "nip44_seal", "secret": seedS1, "value": value})["sealed"].(string)
	if goOpen, err := wire.OpenMemory(h2b(t, seedS1), sealedRust); err != nil || goOpen != value {
		t.Fatalf("Go open of Rust NIP-44 failed: err=%v open=%q", err, goOpen)
	}
	sealedGo, err := wire.SealMemory(h2b(t, seedS1), value)
	if err != nil {
		t.Fatal(err)
	}
	rustOpen := oracle(t, map[string]interface{}{"op": "nip44_open", "secret": seedS1, "sealed": sealedGo})["value"].(string)
	if rustOpen != value {
		t.Fatalf("Rust open of Go NIP-44 mismatch: got %q want %q", rustOpen, value)
	}
	if _, err := wire.OpenMemory(h2b(t, seedS2), sealedRust); err == nil {
		t.Fatal("expected error for wrong key")
	}
	if len(sealedRust) == 0 {
		t.Fatal("empty sealed")
	}
	last := sealedRust[len(sealedRust)-1]
	alt := byte('A')
	if last == 'A' {
		alt = 'B'
	}
	tampered := sealedRust[:len(sealedRust)-1] + string(alt)
	if _, err := wire.OpenMemory(h2b(t, seedS1), tampered); err == nil {
		t.Fatal("expected error for tampered payload")
	}
}

func TestSecretPackageMatchesOracle(t *testing.T) {
	pkgIn := map[string]interface{}{
		"secrets": map[string]interface{}{"z-pw": "00aabb", "a-pw": "001122"},
		"targets": map[string]interface{}{"t1": map[string]interface{}{"kind": "ssh", "address": "root@h:22", "secret": "z-pw"}},
		"grants":  []interface{}{"55aa", "11bb"},
	}
	rustJSON := oracle(t, map[string]interface{}{"op": "secretpackage_new", "package": pkgIn})["json"].(string)

	pkg := wire.New(
		map[string]string{"z-pw": "00aabb", "a-pw": "001122"},
		map[string]wire.TargetMeta{"t1": {Kind: "ssh", Address: "root@h:22", Secret: "z-pw"}},
		[]string{"55aa", "11bb"},
	)
	goJSON, err := pkg.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(goJSON) != rustJSON {
		t.Fatalf("SecretPackage JSON mismatch:\n  go:   %s\n  rust: %s", goJSON, rustJSON)
	}
}

func TestSecretPackageWriteLoad(t *testing.T) {
	dir := t.TempDir()
	pkg := wire.New(
		map[string]string{"db": "aabbcc"},
		map[string]wire.TargetMeta{"local": {Kind: "local", Address: "", Secret: "db"}},
		[]string{"feed"})
	if err := pkg.WriteToDir(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := wire.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := pkg.Bytes()
	back, _ := loaded.Bytes()
	if string(orig) != string(back) {
		t.Fatalf("write/load roundtrip mismatch:\n  %s\n  %s", orig, back)
	}
}

// TestSignEventParityProbe cross-verifies signatures for several seeds so the
// harness exercises BOTH pubkey parities (roughly half of random scalars give
// an odd-Y pubkey, which exercises the BIP-340 parity-negation path in the
// aux mask). The oracle is the byte-exact truth for whatever seed we pick, so
// a parity bug in the Go signer fails this even though the crypto tests work
// for seedS1 alone (IMPORTANT-4).
func TestSignEventParityProbe(t *testing.T) {
	seeds := []string{
		seedS1,
		seedS2,
		"2222222222222222222222222222222222222222222222222222222222222222",
		"3333333333333333333333333333333333333333333333333333333333333333",
		"0000000000000000000000000000000000000000000000000000000000000042",
		"8080808080808080808080808080808080808080808080808080808080808080",
		"0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20",
	}
	for _, seed := range seeds {
		if _, err := crypto.PubkeyFromSecret(h2b(t, seed)); err != nil {
			t.Fatalf("invalid seed %s: %v", seed, err)
		}
		pkGo, idGo, sigGo, err := wire.SignEvent(h2b(t, seed), 9007, nowTS, [][]string{{"h", "x"}}, "")
		if err != nil {
			t.Fatalf("seed %s: %v", seed, err)
		}
		resp := oracle(t, map[string]interface{}{
			"op": "sign_event", "secret": seed, "created_at": nowTS, "kind": 9007,
			"tags": [][]string{{"h", "x"}}, "content": "",
		})
		if pkGo != resp["pubkey"].(string) || idGo != resp["id"].(string) || sigGo != resp["sig"].(string) {
			t.Fatalf("seed %s: signature/event mismatch with oracle — parity path broken", seed)
		}
	}
}
