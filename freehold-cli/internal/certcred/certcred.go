// Package certcred holds the DNS provider credential engine shared by the
// `build` and `dns-cred` commands: sealing/reusing per-slot credentials, the
// interactive provider picker, and the pre-verify.
package certcred

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"freehold/contract/crypto"
	"freehold/platform/provisioning/box"
	cert "freehold/platform/services/certificates/letsencrypt"
)

// Engine carries the I/O and consent a credential prompt needs. Build and
// dns-cred each construct one from their own surface.
type Engine struct {
	Out    io.Writer
	Yes    bool
	Prompt func(label string) (string, error)
}

// CertCredPath is the sealed credential path for a slot.
func (e *Engine) CertCredPath(slot string) string {
	return filepath.Join(box.StateDir(), "dns-provider-"+slot+".json")
}

// CertIdent loads the ops identity used to seal credentials.
func (e *Engine) CertIdent() (*box.Identity, []byte, []byte, error) {
	id, err := box.LoadIdentity(box.OpsDir())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("no ops identity for cert storage: %w", err)
	}
	secret, err := hex.DecodeString(id.EncSecretHex)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ops identity enc secret: %w", err)
	}
	pub, err := crypto.X25519PublicKey(secret)
	if err != nil {
		return nil, nil, nil, err
	}
	return id, secret, pub, nil
}

// ClearStoredDNSCreds forgets stored DNS provider credentials.
func ClearStoredDNSCreds() {
	dir := box.StateDir()
	for _, name := range []string{"dns-provider-relay.json", "dns-provider-cp.json", "dns-provider.json"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// CPSecretBlob builds a sealed credential blob for a CP secret. The scratch
// file is created with a random name (O_EXCL) so a predictable /tmp path can't
// be pre-created as a symlink by a local attacker.
func CPSecretBlob(name, provider string, env map[string]string, seal cert.Sealer, pub []byte) (json.RawMessage, error) {
	f, err := os.CreateTemp("", "fh-cp-"+name+"-*.json")
	if err != nil {
		return nil, err
	}
	p := f.Name()
	_ = f.Close()
	defer os.Remove(p)
	if err := cert.SaveCreds(p, provider, env, seal, pub, name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// DNSZone returns a host's registrable-ish zone: everything after the first
// label (e.g. "cp.example.com" -> "example.com"). "" for a bare host.
func DNSZone(host string) string {
	if i := strings.Index(host, "."); i >= 0 && i < len(host)-1 {
		return host[i+1:]
	}
	return ""
}

// PromptSecret reads a credential value WITHOUT echo when stdin is a terminal,
// falling back to the ordinary prompt for a piped stdin.
func (e *Engine) PromptSecret(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return e.Prompt(label)
	}
	fmt.Fprintf(e.Out, "%s: ", label)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(e.Out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// PromptDNSCred returns the provider + env for a slot, reusing a sealed
// credential or collecting (and pre-verifying) a fresh one.
func (e *Engine) PromptDNSCred(slot, host, reuseFrom string) (string, map[string]string, error) {
	path := e.CertCredPath(slot)
	_, secret, pub, err := e.CertIdent()
	if err != nil {
		return "", nil, err
	}
	seal := func(pub, aad, plain []byte) ([]byte, error) { return crypto.Seal(pub, aad, plain) }
	open := func(secret, aad, blob []byte) ([]byte, error) { return crypto.Open(secret, aad, blob) }

	// Migrate the legacy single-file credential into the relay slot.
	if slot == "relay" && !cert.CredExists(path) {
		if legacy := filepath.Join(box.StateDir(), "dns-provider.json"); cert.CredExists(legacy) {
			if provider, env, lerr := cert.LoadCreds(legacy, open, secret); lerr == nil {
				if serr := cert.SaveCreds(path, provider, env, seal, pub, "cert-dns-"+slot); serr == nil {
					fmt.Fprintf(e.Out, "  · migrated your stored DNS credential (%s) into the %s slot\n", provider, slot)
				}
			}
		}
	}

	if cert.CredExists(path) {
		provider, env, err := cert.LoadCreds(path, open, secret)
		if err != nil {
			return "", nil, fmt.Errorf("reusing %s DNS credential: %w", slot, err)
		}
		fmt.Fprintf(e.Out, "  · reusing sealed %s DNS credential (%s)\n", slot, provider)
		return provider, env, nil
	}

	if reuseFrom != "" {
		fromPath := e.CertCredPath(reuseFrom)
		if cert.CredExists(fromPath) {
			reuse := e.Yes
			if !e.Yes {
				ans, err := e.Prompt(fmt.Sprintf("%s has no DNS credential — reuse the %s one? (y/n)", slot, reuseFrom))
				if err != nil {
					return "", nil, err
				}
				reuse = strings.EqualFold(strings.TrimSpace(ans), "y")
			}
			if reuse {
				provider, env, err := cert.LoadCreds(fromPath, open, secret)
				if err != nil {
					return "", nil, err
				}
				if err := cert.SaveCreds(path, provider, env, seal, pub, "cert-dns-"+slot); err != nil {
					return "", nil, fmt.Errorf("storing %s DNS credential: %w", slot, err)
				}
				fmt.Fprintf(e.Out, "  · %s reuses the %s DNS credential (%s) — copies kept separate\n", slot, reuseFrom, provider)
				return provider, env, nil
			}
		}
	}

	if e.Yes {
		return "", nil, fmt.Errorf("no %s DNS provider credential stored and interactive collection is disabled (--non-interactive); run without --non-interactive once to store it", slot)
	}

	provider, err := e.PromptProvider()
	if err != nil {
		return "", nil, err
	}
	env, err := e.PromptProviderEnv(provider)
	if err != nil {
		return "", nil, err
	}
	if host != "" {
		fmt.Fprintf(e.Out, "  · pre-verifying %s credentials (throwaway TXT round-trip)…\n", provider)
		if err := cert.Verify(host, provider, env); err != nil {
			return "", nil, fmt.Errorf("DNS provider pre-verify failed — fix the credential and try again: %w", err)
		}
	}
	if err := cert.SaveCreds(path, provider, env, seal, pub, "cert-dns-"+slot); err != nil {
		return "", nil, fmt.Errorf("storing DNS credential: %w", err)
	}
	return provider, env, nil
}

// PromptProvider runs the interactive provider picker over lego's registry.
func (e *Engine) PromptProvider() (string, error) {
	return RunProviderPicker(cert.Providers())
}

// PromptProviderEnv collects the provider's credential fields.
func (e *Engine) PromptProviderEnv(provider string) (map[string]string, error) {
	names := cert.ProviderEnvNames(provider)
	env := map[string]string{}
	if len(names) == 0 {
		fmt.Fprintln(e.Out, "  this provider has no enumerated env fields — paste KEY=VAL entries (one per line; empty line to finish):")
		for {
			line, err := e.Prompt("KEY=VAL (or blank to finish)")
			if err != nil {
				return nil, err
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) == "" {
				fmt.Fprintln(e.Out, "  expected KEY=VAL")
				continue
			}
			env[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		return env, nil
	}
	required := map[string][]string{
		"route53":    {"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_HOSTED_ZONE_ID"},
		"cloudflare": {"CLOUDFLARE_DNS_API_TOKEN"},
	}[provider]
	var requiredSet []string
	for _, n := range required {
		if containsStr(names, n) {
			requiredSet = append(requiredSet, n)
		}
	}
	fmt.Fprintln(e.Out, "  enter the REQUIRED credential fields:")
	for _, n := range requiredSet {
		v, err := e.PromptSecret(n + " (required)")
		if err != nil {
			return nil, err
		}
		if v = strings.TrimSpace(v); v == "" {
			return nil, fmt.Errorf("%s is required for the %s credential", n, provider)
		}
		env[n] = v
	}
	var rest []string
	for _, n := range names {
		if !containsStr(requiredSet, n) {
			rest = append(rest, n)
		}
	}
	if len(rest) > 0 {
		fmt.Fprintln(e.Out, "  optional fields (blank = unset):")
		for _, n := range rest {
			v, err := e.PromptSecret(n + " (optional)")
			if err != nil {
				return nil, err
			}
			if v = strings.TrimSpace(v); v != "" {
				env[n] = v
			}
		}
	}
	return env, nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
