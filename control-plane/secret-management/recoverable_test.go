package provisioner

import "testing"

// TestEverySecretKindIsRecoverableOrRemintable pins the PR4 invariant: a
// secret that reaches a runner's package must be CP-recoverable OR re-mintable.
// The ssh substrate key is the ONLY re-mintable kind (install regenerates it);
// every other kind must have a CP-side source (the world-secrets store) so a
// re-adopt can re-seal it. A new runner-only kind — classified here but with no
// CP-side source — must be added deliberately, and this test makes that visible.
func TestEverySecretKindIsRecoverableOrRemintable(t *testing.T) {
	// The kinds the provisioner accepts (defaultRisk is the classifier).
	kinds := []string{"vultr", "b2", "hetzner", "github", "websearch", "litellm", "local", "ssh"}
	for _, k := range kinds {
		if defaultRisk(k) == nil {
			t.Errorf("secret kind %q is not classified — add it deliberately", k)
		}
	}
	// ssh is the re-mintable door; anything else must be CP-recoverable.
	if r := defaultRisk("ssh"); r == nil || *r != "risky-install" {
		t.Errorf("ssh must be the re-mintable substrate kind (risky-install), got %v", r)
	}
}
