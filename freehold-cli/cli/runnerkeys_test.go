package cli

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"freehold/contract/config"
	"freehold/contract/crypto"
	"freehold/contract/wire"
)

// fakeHostExec answers the CP `pct exec ... cat` reads with canned files.
type fakeHostExec struct{ files map[string]string }

func (f fakeHostExec) Exec(cmd string) (bool, string) {
	for name, body := range f.files {
		if strings.Contains(cmd, name) {
			return true, body
		}
	}
	return false, ""
}

// TestPlaneSubstratePubLine guards the decrypt-and-extract path (the CP package
// uses the same identity.json schema as the box: `enc_secret_hex`).
func TestPlaneSubstratePubLine(t *testing.T) {
	encSecret := make([]byte, 32)
	for i := range encSecret {
		encSecret[i] = byte(i + 7)
	}
	encPub, err := crypto.X25519PublicKey(encSecret)
	if err != nil {
		t.Fatal(err)
	}
	pem, pub, err := crypto.GenerateSSHKeypair("proxmox-box")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := crypto.Seal(encPub, []byte("proxmox-box"), pem)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := json.Marshal(map[string]string{"enc_secret_hex": hex.EncodeToString(encSecret)})
	pkg, _ := wire.New(
		map[string]string{"proxmox-box": hex.EncodeToString(sealed)},
		map[string]wire.TargetMeta{"proxmox-box": {Kind: "ssh", Address: "root@host", Secret: "proxmox-box"}},
		nil,
	).Bytes()

	cp := uint32(101)
	cfg := &config.Config{Runner: config.RunnerRef{Target: "proxmox-box"}, Lxc: config.LxcSpec{Cp: config.LxcGuest{Vmid: &cp}}}
	r := fakeHostExec{files: map[string]string{"identity.json": string(id), "secrets.json": string(pkg)}}
	if got := planeSubstratePubLine(cfg, r); got != pub {
		t.Errorf("planeSubstratePubLine = %q, want %q", got, pub)
	}
}
