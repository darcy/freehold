package proxmox

import (
	"os"
	"strings"
	"testing"
)

func TestSSHArgsShape(t *testing.T) {
	args := sshArgs("192.168.30.224", "/tmp/k", `pct exec 100 -- sh -c 'echo hi'`)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-i /tmp/k",
		"-o BatchMode=yes",
		"-o StrictHostKeyChecking=accept-new",
		"root@192.168.30.224",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh args missing %q: %v", want, args)
		}
	}
	if args[len(args)-1] != `pct exec 100 -- sh -c 'echo hi'` {
		t.Errorf("the command must be the last (single) ssh argument, got %q", args[len(args)-1])
	}
}

func TestWriteTempKeyIs0600AndCleansUp(t *testing.T) {
	pem := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n")
	path, cleanup, err := WriteTempKey(pem)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 600", info.Mode().Perm())
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(pem) {
		t.Errorf("key file content mismatch")
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup must remove the key file")
	}
}
