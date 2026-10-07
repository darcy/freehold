// manual.go is the MANUAL DNS provider: a challenge.Provider for the operator
// who has no DNS API (or chose not to hand one over). Its Present hands the
// operator the exact record to create by FAILING the build with the
// instruction — the pending order is persisted anyway (resume.go's
// instructional contract), so the re-run RESUMES the same order and its
// per-order TXT value. CleanUp only notes the record is theirs to delete.
package cert

import (
	"fmt"
	"os"

	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
)

// ManualProviderName is the DNS provider name meaning "the operator creates
// the records by hand". It rides the same sealed credential record (provider +
// empty env) as every API provider, so the whole cert/door/teardown pipeline
// treats it uniformly. buildProvider shadows lego's own "manual" entry — that
// one waits on stdin, which a server-side CP does not have.
const ManualProviderName = "manual"

// ManualProvider is the manual challenge provider (buildProvider special-case).
func ManualProvider() challenge.Provider { return manualProvider{} }

// manualProvider hands the operator the DNS-01 record to create.
type manualProvider struct{}

// Instructional marks a provider whose Present returns the record as an
// INSTRUCTION error rather than placing anything — resume.go persists the
// order on such a failure so the retry resumes the same order (and thus the
// same TXT value the operator was shown).
func (manualProvider) Instructional() bool { return true }

func (manualProvider) Present(domain, token, keyAuth string) error {
	info := dns01.GetChallengeInfo(domain, keyAuth)
	return fmt.Errorf(
		"MANUAL DNS — create this TXT record in your DNS console:\n"+
			"    %s  TXT  \"%s\"\n"+
			"(Replace any earlier _acme-challenge TXT for this name — only the shown value validates.) "+
			"Then re-run `freehold build`: it resumes this exact order and completes the certificate.",
		info.EffectiveFQDN, info.Value)
}

func (manualProvider) CleanUp(domain, token, keyAuth string) error {
	if fqdn, value := dns01.GetRecord(domain, keyAuth); fqdn != "" {
		fmt.Fprintf(os.Stderr, "cert: manual DNS — you may now delete %s (TXT %s)\n", fqdn, value)
	}
	return nil
}
