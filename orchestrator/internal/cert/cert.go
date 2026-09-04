// Package cert wraps go-acme/lego's embedded ACME engine for freehold's core
// TLS edge (roadmap/CORE_TLS.md F3): issuing + renewing the relay wildcard
// certificate via DNS-01 using the operator's chosen provider.
//
// lego runs IN-PROCESS (no shipped binary). The provider is addressed BY NAME
// from lego's full registry (providers_gen.go) and its credentials are supplied
// as an env map that lego's challenge providers read — never plaintext in
// freehold (the env values ride the ciphertext runner-secret path). The issued
// files are written into the Caddy durable PVC at /data/tls.
//
// The providers and their env-var names are GENERATED from lego's own registry
// (go run ./internal/cert/genproviders), so the dropdown tracks every provider
// lego supports — not a curated shortlist.
package cert

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns"
	"github.com/go-acme/lego/v4/registration"
)

// Providers returns every DNS-01 provider lego supports (drives the dropdown).
func Providers() []string {
	out := make([]string, len(providerNames))
	copy(out, providerNames)
	return out
}

// ProviderEnvNames returns the env-var names the named provider reads (for the
// credential prompt). Empty when unknown — the caller falls back to freeform
// KEY=VAL entry (or none for auto-detecting providers).
func ProviderEnvNames(name string) []string {
	return append([]string(nil), providerEnvNames[name]...)
}

// IsProvider reports whether name is a real lego DNS-01 provider.
func IsProvider(name string) bool {
	for _, n := range providerNames {
		if n == name {
			return true
		}
	}
	return false
}

// Issued is the result of a successful issuance.
type Issued struct {
	Fullchain []byte // leaf + intermediates (PEM)
	Key       []byte // private key (PEM)
	NotAfter  time.Time
}

// user implements registration.User with an ephemeral account key.
type user struct {
	key  *rsa.PrivateKey
	reg  *registration.Resource
	mail string
}

func (u *user) GetEmail() string                        { return u.mail }
func (u *user) GetRegistration() *registration.Resource { return u.reg }
func (u *user) GetPrivateKey() crypto.PrivateKey        { return u.key }

// envScope temporarily exports the provider credentials to the process env
// (what lego's challenge providers read at construction), restoring afterwards.
type envScope struct {
	prev map[string]*string
}

func (s *envScope) Start(env map[string]string) {
	s.prev = map[string]*string{}
	for k, v := range env {
		old, had := os.LookupEnv(k)
		if had {
			s.prev[k] = &old
		} else {
			s.prev[k] = nil
		}
		os.Setenv(k, v)
	}
}

func (s *envScope) Stop() {
	for k, v := range s.prev {
		if v == nil {
			os.Unsetenv(k)
		} else {
			os.Setenv(k, *v)
		}
	}
}

// buildProvider constructs lego's challenge provider by name with the given env.
func buildProvider(providerName string, env map[string]string) (challenge.Provider, error) {
	scope := &envScope{}
	scope.Start(env)
	defer scope.Stop()
	p, err := dns.NewDNSChallengeProviderByName(providerName)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", providerName, err)
	}
	return p, nil
}

// Verify runs a throwaway TXT present+cleanup through lego to prove the chosen
// provider's credentials "work" before they are saved. It exercises the provider
// API (create+remove a challenge record) but never the public resolver.
func Verify(domain, providerName string, env map[string]string) error {
	p, err := buildProvider(providerName, env)
	if err != nil {
		return err
	}
	token := strings.ReplaceAll(time.Now().UTC().Format("20060102150405"), ":", "")
	keyAuth := token + "." + "freehold-verify"
	if err := p.Present(domain, token, keyAuth); err != nil {
		_ = p.CleanUp(domain, token, keyAuth)
		return fmt.Errorf("%s: TXT present failed (credentials bad/missing?): %w", providerName, err)
	}
	if err := p.CleanUp(domain, token, keyAuth); err != nil {
		return fmt.Errorf("%s: TXT cleanup failed: %w", providerName, err)
	}
	return nil
}

// IssueWildcard obtains a certificate covering the apex and its wildcard.
func IssueWildcard(domain, providerName string, env map[string]string) (*Issued, error) {
	p, err := buildProvider(providerName, env)
	if err != nil {
		return nil, err
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}
	u := &user{key: key, mail: ""}
	cfg := lego.NewConfig(u)
	cfg.Certificate.KeyType = certcrypto.RSA2048

	client, err := lego.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("acme client: %w", err)
	}
	if err := client.Challenge.SetDNS01Provider(p); err != nil {
		return nil, fmt.Errorf("DNS-01 provider: %w", err)
	}
	reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, fmt.Errorf("acme register: %w", err)
	}
	u.reg = reg

	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: []string{"*." + domain, domain},
		Bundle:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("obtain %s: %w", "*."+domain, err)
	}
	if res == nil || len(res.Certificate) == 0 {
		return nil, fmt.Errorf("obtain returned no certificate for %s", "*."+domain)
	}
	return &Issued{
		Fullchain: res.Certificate,
		Key:       res.PrivateKey,
		NotAfter:  leafNotAfter(res.Certificate),
	}, nil
}

// CertPaths are where the files land on the Caddy durable volume.
func CertPaths(tlsDir string) (fullchain, key string) {
	return tlsDir + "/fullchain.pem", tlsDir + "/key.pem"
}

// WriteTLS writes the issued chain/key atomically to the durable paths.
func WriteTLS(tlsDir string, cert *Issued) error {
	fc, key := CertPaths(tlsDir)
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		return err
	}
	fcTmp := fc + ".tmp"
	keyTmp := key + ".tmp"
	if err := os.WriteFile(fcTmp, cert.Fullchain, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(keyTmp, cert.Key, 0o600); err != nil {
		return err
	}
	if err := os.Rename(fcTmp, fc); err != nil {
		return err
	}
	return os.Rename(keyTmp, key)
}

// LoadExpiry parses the leaf's NotAfter from a fullchain PEM.
func LoadExpiry(fullchainPath string) (time.Time, error) {
	b, err := os.ReadFile(fullchainPath)
	if err != nil {
		return time.Time{}, err
	}
	for len(b) > 0 {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			continue
		}
		return cert.NotAfter, nil
	}
	return time.Time{}, fmt.Errorf("no certificate in %s", fullchainPath)
}

func leafNotAfter(chain []byte) time.Time {
	s, _ := LoadExpiryFromBytes(chain)
	return s
}

// LoadExpiryFromBytes parses the first PEM certificate's NotAfter.
func LoadExpiryFromBytes(chain []byte) (time.Time, error) {
	b := chain
	for len(b) > 0 {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			continue
		}
		return cert.NotAfter, nil
	}
	return time.Time{}, fmt.Errorf("no certificate in chain")
}

// ReuseIfValid reports whether the durable cert exists with at least
// minLifetime remaining (the reconcile-always reuse gate: skip DNS-01 entirely).
func ReuseIfValid(fullchainPath string, now time.Time, minLifetime time.Duration) (time.Time, bool) {
	exp, err := LoadExpiry(fullchainPath)
	if err != nil {
		return time.Time{}, false
	}
	return reuse(exp, now, minLifetime)
}

// ReuseIfValidBytes is ReuseIfValid for in-memory fullchain bytes (e.g. pulled
// out of the Caddy pod over the runner).
func ReuseIfValidBytes(fullchain []byte, now time.Time, minLifetime time.Duration) (time.Time, bool) {
	exp, err := LoadExpiryFromBytes(fullchain)
	if err != nil {
		return time.Time{}, false
	}
	return reuse(exp, now, minLifetime)
}

func reuse(exp time.Time, now time.Time, minLifetime time.Duration) (time.Time, bool) {
	if exp.IsZero() || exp.Before(now.Add(minLifetime)) {
		return time.Time{}, false
	}
	return exp, true
}
