package cpdeploy

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"freehold/contract/client"
	"freehold/contract/crypto"
	"freehold/contract/wire"
)

// fakeTransport models the guest filesystem + the host's root authorized_keys,
// recognizing the exact commands rotateAdoptedSubstrate issues.
type fakeTransport struct {
	guestFiles map[string]string
	hostKeys   []string
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{guestFiles: map[string]string{}}
}

var (
	reCat      = regexp.MustCompile(`cat ([^ ]+) 2>/dev/null`)
	reWrite    = regexp.MustCompile(`printf %s "([A-Za-z0-9+/=]+)" \| base64 -d > ([^ ]+)`)
	reAppend   = regexp.MustCompile(`echo '([^']+)' >> /root/\.ssh/authorized_keys`)
	reGrepBody = regexp.MustCompile(`grep -vF '([^']+)' /root/\.ssh/authorized_keys`)
)

func (f *fakeTransport) Exec(cmd string, timeoutS uint64) (*client.ExecOutcome, error) {
	code := 0
	switch {
	case reCat.MatchString(cmd):
		p := reCat.FindStringSubmatch(cmd)[1]
		if v, ok := f.guestFiles[p]; ok {
			return &client.ExecOutcome{Stdout: v, ExitCode: &code}, nil
		}
		return &client.ExecOutcome{Stdout: "", ExitCode: &code}, nil // absent -> nil-ish
	case reWrite.MatchString(cmd):
		m := reWrite.FindStringSubmatch(cmd)
		b, _ := base64.StdEncoding.DecodeString(m[1])
		f.guestFiles[m[2]] = string(b)
		return &client.ExecOutcome{ExitCode: &code}, nil
	case strings.Contains(cmd, "grep -vF"):
		body := reGrepBody.FindStringSubmatch(cmd)[1]
		var keep []string
		for _, k := range f.hostKeys {
			if !strings.Contains(k, body) {
				keep = append(keep, k)
			}
		}
		f.hostKeys = keep
		return &client.ExecOutcome{ExitCode: &code}, nil
	case strings.Contains(cmd, ">> /root/.ssh/authorized_keys"):
		line := reAppend.FindStringSubmatch(cmd)[1]
		for _, k := range f.hostKeys {
			if k == line {
				return &client.ExecOutcome{ExitCode: &code}, nil
			}
		}
		f.hostKeys = append(f.hostKeys, line)
		return &client.ExecOutcome{ExitCode: &code}, nil
	}
	return &client.ExecOutcome{ExitCode: &code}, nil
}

func (f *fakeTransport) Upload(localPath, remotePath string, timeoutS uint64) (uint64, error) {
	return 0, nil
}

func TestRotateAdoptedSubstrate(t *testing.T) {
	encSecret := make([]byte, 32)
	for i := range encSecret {
		encSecret[i] = byte(i + 1)
	}
	encPub, err := crypto.X25519PublicKey(encSecret)
	if err != nil {
		t.Fatal(err)
	}
	const target = "proxmox-box"
	runnerDir := "/srv/data/cp/control-plane/runner/" + target

	// The plane's existing package: an OLD ssh key sealed to its enc key.
	oldPEM, oldPub, err := crypto.GenerateSSHKeypair(target)
	if err != nil {
		t.Fatal(err)
	}
	sealedOld, err := crypto.Seal(encPub, []byte(target), oldPEM)
	if err != nil {
		t.Fatal(err)
	}
	idDoc, _ := json.Marshal(map[string]string{"enc_secret_hex": hex.EncodeToString(encSecret)})
	pkg := &wire.SecretPackage{
		Secrets: map[string]string{target: hex.EncodeToString(sealedOld)},
		Targets: map[string]wire.TargetMeta{target: {Kind: "ssh", Address: "root@host", Secret: target}},
		Grants:  []string{"console"},
	}
	pkgJSON, _ := pkg.Bytes()

	f := newFakeTransport()
	f.guestFiles[runnerDir+"/identity.json"] = string(idDoc)
	f.guestFiles[runnerDir+"/secrets.json"] = string(pkgJSON)
	f.hostKeys = []string{oldPub}

	spec := &DeployCpSpec{StateDir: "/srv/data/cp/control-plane"}
	if err := rotateAdoptedSubstrate(f, spec, runnerDir); err != nil {
		t.Fatal(err)
	}

	// The package now holds a NEW key that decrypts with the plane's enc secret.
	var got wire.SecretPackage
	if err := json.Unmarshal([]byte(f.guestFiles[runnerDir+"/secrets.json"]), &got); err != nil {
		t.Fatal(err)
	}
	sealed, _ := hex.DecodeString(got.Secrets[target])
	newPEM, err := crypto.Open(encSecret, []byte(target), sealed)
	if err != nil {
		t.Fatalf("rotated secret does not open with the plane enc key: %v", err)
	}
	newPub, err := crypto.ExtractED25519PublicKeyLine(newPEM)
	if err != nil {
		t.Fatal(err)
	}
	if newPub == oldPub {
		t.Fatal("rotation must produce a new key")
	}
	// Identity + grants preserved.
	if got.Targets[target].Kind != "ssh" || len(got.Grants) != 1 || got.Grants[0] != "console" {
		t.Errorf("identity/grants not preserved: %+v", got)
	}
	// Host authorized_keys: new key in, old key out.
	var hasNew, hasOld bool
	for _, k := range f.hostKeys {
		if k == newPub {
			hasNew = true
		}
		if k == oldPub {
			hasOld = true
		}
	}
	if !hasNew {
		t.Errorf("new key not authorized on the host: %v", f.hostKeys)
	}
	if hasOld {
		t.Errorf("old key must be removed from the host: %v", f.hostKeys)
	}
}
