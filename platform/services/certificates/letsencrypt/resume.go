package cert

import (
	"crypto"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/acme"
	acmeapi "github.com/go-acme/lego/v4/acme/api"
	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
)

// Resume drives a RESUMABLE DNS-01 issuance: it creates the ACME order and
// PLACES the challenge TXT at build start, persists the order identity (sealed)
// to a state file, and a later build can RESUME that same order — so a re-run
// after a timeout verifies the already-placed challenge (Let's Encrypt validates
// it via its own authoritative resolvers) instead of re-challenging from scratch
// and losing the propagation wait.
//
// The account key + kid + order URL are what make an order resumable; they are
// the ONLY ACME identifiers that survive between processes. lego drives them via
// its exported acme/api.Core, which we reconstruct from the persisted key+kid.
const ResumeUserAgent = "freehold-resume"

// ErrAuthInvalid is the terminal per-order ACME failure: Let's Encrypt validated
// the challenge and marked this order's authorization INVALID. Resuming it can
// never succeed, so it is the ONLY Resolve failure that justifies discarding
// the pending resumable state; every other error (a polling timeout, a transient
// network error) should keep the order so the next run RESUMES it (and its
// already-placed challenge), not start a fresh one.
var ErrAuthInvalid = errors.New("acme: authorization invalid")

// pendingResume is the persisted on-disk order state (sealed acct key).
type pendingResume struct {
	Domain        string `json:"domain"`
	Wildcard      bool   `json:"wildcard"`
	AcctKeySealed string `json:"acct_key_sealed"` // hex ciphertext (sealed to ops identity)
	Kid           string `json:"kid"`
	OrderURL      string `json:"order_url"`
	ChallengeName string `json:"challenge_name"`
	CreatedAt     string `json:"created_at"` // RFC3339; orders expire — resume only while fresh
}

// Resume is a resumable issuance controller for one cert-domain.
type Resume struct {
	Domain   string // the LE cert-domain (may be "*.base")
	Wildcard bool   // legoDomain was a wildcard "*.base"
	Provider challenge.Provider
	Seal     Sealer
	Open     Opener
	SealPub  []byte // recipient pubkey for sealing the acct key
	OpenSec  []byte // recipient secret for reopening it
	Path     string // state-file path

	HTTPClient *http.Client
}

// pendingOrder is an in-memory resume handle (acct key material reopened).
type pendingOrder struct {
	acctKey  crypto.PrivateKey
	kid      string
	orderURL string
}

// PendingOrder is the opaque handle returned by Begin/TryLoad and consumed by
// Resolve/Cleanup; rebuild holds it on a run to resume/complete the issuance.
type PendingOrder = pendingOrder

// TryLoad returns the persisted order handle when a (still-fresh) one exists,
// else nil. A state file whose order has expired is treated as absent (a fresh
// issuance then replaces it).
func (r *Resume) TryLoad() (*pendingOrder, bool, error) {
	raw, err := os.ReadFile(r.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var st pendingResume
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, false, fmt.Errorf("resume state parse: %w", err)
	}
	if st.OrderURL == "" || st.Kid == "" || st.AcctKeySealed == "" {
		// Malformed/unusable state (e.g. a prior crash before persist completed) —
		// forget it so a fresh order is begun rather than resuming a broken one.
		_ = os.Remove(r.Path)
		return nil, false, nil
	}
	// ACME orders are short-lived; only resume while plausibly fresh.
	if created, cerr := time.Parse(time.RFC3339, st.CreatedAt); cerr == nil {
		if time.Since(created) > 7*24*time.Hour {
			return nil, false, nil
		}
	}
	blob, err := hex.DecodeString(st.AcctKeySealed)
	if err != nil {
		return nil, false, fmt.Errorf("resume acct key blob: %w", err)
	}
	// aad = the state file's path (defence against swapping state files).
	plain, err := r.Open(r.OpenSec, []byte(r.Path), blob)
	if err != nil {
		return nil, false, fmt.Errorf("resume acct key unseal: %w", err)
	}
	key, err := certcrypto.ParsePEMPrivateKey(plain)
	if err != nil {
		return nil, false, fmt.Errorf("resume acct key parse: %w", err)
	}
	return &pendingOrder{acctKey: key, kid: st.Kid, orderURL: st.OrderURL}, true, nil
}

// newCore reconstructs lego's ACME api.Core for the persisted account identity,
// which is what makes an order resumable (same account key + kid + order URL).
func (r *Resume) newCore(key crypto.PrivateKey, kid string) (*acmeapi.Core, error) {
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return acmeapi.New(httpClient, ResumeUserAgent, lego.LEDirectoryProduction, kid, key)
}

// Begin creates a FRESH order, places its DNS-01 challenge, and persists the
// order identity so a later run can resume it.
func (r *Resume) Begin() (*pendingOrder, error) {
	acctKey, err := certcrypto.GeneratePrivateKey(certcrypto.RSA2048)
	if err != nil {
		return nil, err
	}
	core, err := r.newCore(acctKey, "")
	if err != nil {
		return nil, err
	}
	// Register the (new) ACCOUNT — this is what sets the account kid.
	acc, err := core.Accounts.New(acme.Account{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, fmt.Errorf("acme register: %w", err)
	}
	core, err = r.newCore(acctKey, acc.Location)
	if err != nil {
		return nil, err
	}
	order, err := core.Orders.New([]string{r.Domain})
	if err != nil {
		return nil, fmt.Errorf("acme order: %w", err)
	}
	po := &pendingOrder{acctKey: acctKey, kid: acc.Location, orderURL: order.Location}

	// Place the DNS-01 challenge + persist the order identity.
	if err := r.place(po); err != nil {
		return nil, err
	}
	return po, nil
}

// place presents the DNS-01 challenge for a pending order and persists the
// resumable state.
func (r *Resume) place(po *pendingOrder) error {
	core, err := r.newCore(po.acctKey, po.kid)
	if err != nil {
		return err
	}
	order, err := core.Orders.Get(po.orderURL)
	if err != nil {
		return err
	}
	chlg, _, err := r.findDNSChallenge(core, order)
	if err != nil {
		return err
	}
	keyAuth, err := core.GetKeyAuthorization(chlg.Token)
	if err != nil {
		return err
	}
	if err := r.Provider.Present(r.Domain, chlg.Token, keyAuth); err != nil {
		return fmt.Errorf("present challenge: %w", err)
	}
	info := dns01.GetChallengeInfo(r.Domain, keyAuth)
	// Post-lego's own dns01 precheck: wait for the challenge TXT to be served at
	// the AUTHORITATIVE zone before the caller POSTs "ready" for LE to validate.
	// Without this, LE can read a stale/negative-cached resolver and mark the
	// authorization invalid before the record reaches it (the exact failure this
	// freehold world hit: relay auth invalid, record visible at the authz NS but
	// not at this box's recursive resolver).
	if err := waitAuthoritativePropagation(info.EffectiveFQDN, info.Value, 3*time.Minute); err != nil {
		return fmt.Errorf("dns-01 propagation: %w", err)
	}
	return r.persist(po, info.EffectiveFQDN)
}

// findDNSChallenge returns the dns-01 challenge from the order's (first)
// pending authorization.
func (r *Resume) findDNSChallenge(core *acmeapi.Core, order acme.ExtendedOrder) (acme.Challenge, string, error) {
	for _, url := range order.Authorizations {
		auth, err := core.Authorizations.Get(url)
		if err != nil {
			return acme.Challenge{}, "", err
		}
		if auth.Status == acme.StatusValid {
			continue
		}
		for _, c := range auth.Challenges {
			if c.Type == "dns-01" {
				return c, url, nil
			}
		}
	}
	return acme.Challenge{}, "", fmt.Errorf("no pending dns-01 challenge in order %s", order.Location)
}

// acceptChallenges POSTs "ready" for every outstanding dns-01 challenge. An
// authorization already VALID (e.g. validated by a prior attempt on resume) is
// skipped; an already-accepted-but-not-yet-valid challenge is re-accepted
// (harmless + idempotent) so Let's Encrypt re-checks the pre-placed record.
func (r *Resume) acceptChallenges(core *acmeapi.Core, order acme.ExtendedOrder) error {
	for _, url := range order.Authorizations {
		auth, err := core.Authorizations.Get(url)
		if err != nil {
			return err
		}
		if auth.Status == acme.StatusValid {
			continue
		}
		for _, c := range auth.Challenges {
			// Only a PENDING challenge is actionable via POST (accept). An
			// already-processing one (prior attempt on resume) must be left alone
			// for LE to finish validating — re-accepting returns 403.
			if c.Type == "dns-01" && c.Status == acme.StatusPending {
				if _, err := core.Challenges.New(c.URL); err != nil {
					return fmt.Errorf("accept challenge %s: %w", c.URL, err)
				}
			}
		}
	}
	return nil
}

// persist writes the sealed acct key + order identity to the state file.
func (r *Resume) persist(po *pendingOrder, challengeName string) error {
	acctKeyPEM := certcrypto.PEMEncode(po.acctKey)
	blob, err := r.Seal(r.SealPub, []byte(r.Path), acctKeyPEM)
	if err != nil {
		return fmt.Errorf("seal resume acct key: %w", err)
	}
	st := pendingResume{
		Domain:        r.Domain,
		Wildcard:      r.Wildcard,
		AcctKeySealed: hex.EncodeToString(blob),
		Kid:           po.kid,
		OrderURL:      po.orderURL,
		ChallengeName: strings.TrimSuffix(challengeName, "."),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(r.Path, raw, 0o600)
}

// Resolve completes a pending order (freshly begun OR resumed): accepts the
// dns-01 challenge so Let's Encrypt validates the (already-placed) record,
// finalizes, and downloads the certificate. Clears the state file on success.
// Plan reminder (CORE_TLS): on the fresh path the challenge was placed at build
// start and has had the whole pipeline to propagate before this runs.
func (r *Resume) Resolve(po *pendingOrder) (*Issued, error) {
	core, err := r.newCore(po.acctKey, po.kid)
	if err != nil {
		return nil, err
	}
	order, err := core.Orders.Get(po.orderURL)
	if err != nil {
		return nil, err
	}

	// Accept every outstanding (pending) dns-01 challenge, then wait for LE to
	// validate. On a resume where LE already validated an earlier attempt
	// everything may already be valid and there is nothing to accept/finalize.
	if err := r.acceptChallenges(core, order); err != nil {
		return nil, err
	}
	if err := waitFor("authorization", 5*time.Minute, r.authorizationValid(core, order)); err != nil {
		return nil, err
	}
	order, err = core.Orders.Get(po.orderURL)
	if err != nil {
		return nil, err
	}

	// Finalize ONLY when the order needs it. A prior resume may have already
	// finalized (Status valid/processing after a crash between finalize and
	// download); re-POSTing to the finalize URL of such an order returns 403, so
	// let those fall through to the certificate poll instead.
	certKey, err := certcrypto.GeneratePrivateKey(certcrypto.RSA2048)
	if err != nil {
		return nil, err
	}
	switch order.Status {
	case acme.StatusValid, acme.StatusProcessing:
		// already finalized (or in flight) — nothing to do here; poll below.
	case acme.StatusReady:
		// Finalize with a CSR for the SAN(s). Wildcard certs carry only the
		// wildcard SAN ("*.base"), single-name certs the host.
		san := []string{r.Domain}
		commonName := strings.TrimPrefix(r.Domain, "*.")
		csr, err := certcrypto.CreateCSR(certKey, certcrypto.CSROptions{Domain: commonName, SAN: san})
		if err != nil {
			return nil, err
		}
		order, err = core.Orders.UpdateForCSR(order.Finalize, csr)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("order %s in status %q (expected ready, valid, or processing)", po.orderURL, order.Status)
	}
	// Poll for the certificate URL. Covers both a freshly-finalized order and an
	// already-processing/valid one whose certificate is still to be served.
	if order.Certificate == "" {
		// lego's OrderService.Get does NOT populate Location, so poll by the order
		// URL we already hold, not order.Location.
		if err := waitFor("certificate", 5*time.Minute, r.certificateReady(core, po.orderURL, &order)); err != nil {
			return nil, err
		}
	}
	if order.Certificate == "" {
		return nil, errors.New("acme: order has no certificate URL after finalization")
	}

	certs, err := core.Certificates.GetAll(order.Certificate, true)
	if err != nil {
		return nil, err
	}
	rc, ok := certs[order.Certificate]
	if !ok || len(rc.Cert) == 0 {
		return nil, errors.New("acme: certificate download returned nothing")
	}
	// Fullchain = leaf + issuer chain (PEM), as Caddy expects.
	fullchain := append(append([]byte{}, rc.Cert...), rc.Issuer...)
	exp, _ := LoadExpiryFromBytes(fullchain)
	// Only clear the resumable state on SUCCESS; a failure keeps it so the next
	// run can resume this same order (and the already-placed challenge).
	r.DiscardState()
	return &Issued{
		Fullchain: fullchain,
		Key:       certcrypto.PEMEncode(certKey),
		NotAfter:  exp,
	}, nil
}

// authorizationValid returns a bool() polling fn that resolves true once every
// order authorization is valid (or fails on invalid).
func (r *Resume) authorizationValid(core *acmeapi.Core, order acme.ExtendedOrder) func() (bool, error) {
	return func() (bool, error) {
		for _, url := range order.Authorizations {
			auth, err := core.Authorizations.Get(url)
			if err != nil {
				return false, err
			}
			switch auth.Status {
			case acme.StatusValid:
				continue
			case acme.StatusInvalid:
				return false, fmt.Errorf("%w: %s", ErrAuthInvalid, url)
			default:
				return false, nil
			}
		}
		return true, nil
	}
}

// certificateReady polls the order until its certificate URL is populated.
func (r *Resume) certificateReady(core *acmeapi.Core, orderURL string, order *acme.ExtendedOrder) func() (bool, error) {
	return func() (bool, error) {
		o, err := core.Orders.Get(orderURL)
		if err != nil {
			return false, err
		}
		if o.Status == acme.StatusInvalid {
			return false, fmt.Errorf("order %s invalid: %w", orderURL, o.Err())
		}
		if o.Status == acme.StatusProcessing || o.Status == acme.StatusPending {
			return false, nil
		}
		if o.Certificate != "" {
			order.Certificate = o.Certificate
			return true, nil
		}
		return false, fmt.Errorf("order %s in unexpected status %q", orderURL, o.Status)
	}
}

// waitFor polls fn until it returns true or the deadline passes.
func waitFor(what string, timeout time.Duration, fn func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for %s", what)
}

// Cleanup removes a placed challenge for a pending order (best-effort; used
// when abandoning an issuance).
func (r *Resume) Cleanup(po *pendingOrder, token, keyAuth string) error {
	// find the challenge again to derive the fqdn for cleanup.
	core, err := r.newCore(po.acctKey, po.kid)
	if err != nil {
		return err
	}
	order, err := core.Orders.Get(po.orderURL)
	if err != nil {
		return err
	}
	chlg, _, err := r.findDNSChallenge(core, order)
	if err != nil {
		return err
	}
	return r.Provider.CleanUp(r.Domain, chlg.Token, keyAuth)
}

// DiscardState removes the persisted state file (e.g. after a confirmed issue).
func (r *Resume) DiscardState() { _ = os.Remove(r.Path) }
