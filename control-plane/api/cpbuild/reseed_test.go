package cpbuild

import (
	"testing"

	"freehold/contract/wire"
)

// TestDropRetiredRunnerSecrets: the reseed's prune drops exactly the retired
// names (the provider key an older build shipped to the co-located runner)
// and keeps the requested litellm seeds + the runner's own target credential;
// a second pass is a no-op (idempotent — the restart re-runs the reseed).
func TestDropRetiredRunnerSecrets(t *testing.T) {
	pkg := wire.New(
		map[string]string{
			"litellm":      "aa",
			"postgres-pw":  "bb",
			"provider-key": "cc",
			"pve-ssh-root": "dd",
		},
		map[string]wire.TargetMeta{
			"pve-ssh-root": {Kind: "ssh", Address: "root@host", Secret: "pve-ssh-root"},
		},
		[]string{},
	)
	dropped := dropRetiredRunnerSecrets(pkg)
	if len(dropped) != 1 || dropped[0] != "provider-key" {
		t.Fatalf("dropped = %v, want [provider-key]", dropped)
	}
	for _, keep := range []string{"litellm", "postgres-pw", "pve-ssh-root"} {
		if _, ok := pkg.Secrets[keep]; !ok {
			t.Fatalf("%s was dropped", keep)
		}
	}
	if _, ok := pkg.Secrets["provider-key"]; ok {
		t.Fatal("provider-key kept")
	}
	if d := dropRetiredRunnerSecrets(pkg); len(d) != 0 {
		t.Fatalf("second pass dropped %v, want none", d)
	}
}

// TestDropRetiredRunnerSecretsNoStale: a fresh package (no retired names) is
// untouched — the reseed stays a no-op for it.
func TestDropRetiredRunnerSecretsNoStale(t *testing.T) {
	pkg := wire.New(
		map[string]string{"litellm": "aa", "postgres-pw": "bb", "pve-ssh-root": "dd"},
		map[string]wire.TargetMeta{
			"pve-ssh-root": {Kind: "ssh", Address: "root@host", Secret: "pve-ssh-root"},
		},
		[]string{},
	)
	if d := dropRetiredRunnerSecrets(pkg); len(d) != 0 {
		t.Fatalf("dropped %v from a clean package, want none", d)
	}
}
