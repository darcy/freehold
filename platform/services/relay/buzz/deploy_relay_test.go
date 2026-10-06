package deploy

import (
	"strings"
	"testing"
)

func TestInstallCmdPairingAdvertisedOnDomainDeploys(t *testing.T) {
	domain := "relay.example.test"
	cmd := InstallCmd(&RelayDeploySpec{
		RelayName:      "relay",
		DeployDir:      "/srv/data/relay",
		HTTPPort:       3000,
		BuzzRef:        DefaultBufRef,
		OwnerPubkey:    strings.Repeat("a", 64),
		RelayURL:       "https://" + domain,
		OperatorPubkey: strings.Repeat("b", 64),
		Domain:         &domain,
	})
	for _, want := range []string{
		"BUZZ_PAIRING_RELAY_URL=wss://relay.example.test/pair",
		"grep -q \"^BUZZ_PAIRING_RELAY_URL=\" .env",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("InstallCmd missing %q:\n%s", want, cmd)
		}
	}
}

func TestInstallCmdNoPairingWithoutDomain(t *testing.T) {
	cmd := InstallCmd(&RelayDeploySpec{
		RelayName:      "relay",
		DeployDir:      "/srv/data/relay",
		HTTPPort:       3000,
		BuzzRef:        DefaultBufRef,
		OwnerPubkey:    strings.Repeat("a", 64),
		RelayURL:       "http://192.168.30.8:3000",
		OperatorPubkey: strings.Repeat("b", 64),
	})
	if strings.Contains(cmd, "BUZZ_PAIRING_RELAY_URL") {
		t.Errorf("IP-anchored deploy must not advertise pairing:\n%s", cmd)
	}
}

// The pinned bundle's .env.example ships a placeholder CORS allowlist
// (BUZZ_CORS_ORIGINS=https://buzz.example.com). Left in place it rejects the
// desktop webview's preflight — the invite mint/claim fetches are
// webview-issued and CORS-bound, so the invite link silently fails while
// everything else works. InstallCmd must neutralize the var (empty = the
// relay's permissive default).
func TestInstallCmdNeutralizesPlaceholderCors(t *testing.T) {
	cmd := InstallCmd(&RelayDeploySpec{
		RelayName:      "relay",
		DeployDir:      "/srv/data/relay",
		HTTPPort:       3000,
		BuzzRef:        DefaultBufRef,
		OwnerPubkey:    strings.Repeat("a", 64),
		RelayURL:       "http://192.168.30.8:3000",
		OperatorPubkey: strings.Repeat("b", 64),
	})
	for _, want := range []string{
		"grep -q \"^BUZZ_CORS_ORIGINS=\" .env",
		"sed -i \"s/^BUZZ_CORS_ORIGINS=.*/BUZZ_CORS_ORIGINS=/\" .env",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("InstallCmd missing %q:\n%s", want, cmd)
		}
	}
}

// The payload rides a single-quoted `sh -c` (LxcExec) — a single quote in the
// emitted command breaks the transport.
func TestInstallCmdSingleQuoteFree(t *testing.T) {
	domain := "relay.example.test"
	for _, spec := range []*RelayDeploySpec{
		{
			RelayName:      "relay",
			DeployDir:      "/srv/data/relay",
			HTTPPort:       3000,
			BuzzRef:        DefaultBufRef,
			OwnerPubkey:    strings.Repeat("a", 64),
			RelayURL:       "https://" + domain,
			OperatorPubkey: strings.Repeat("b", 64),
			Domain:         &domain,
		},
		{
			RelayName:      "relay",
			DeployDir:      "/srv/data/relay",
			HTTPPort:       3000,
			BuzzRef:        DefaultBufRef,
			OwnerPubkey:    strings.Repeat("a", 64),
			RelayURL:       "http://192.168.30.8:3000",
			OperatorPubkey: strings.Repeat("b", 64),
		},
	} {
		if cmd := InstallCmd(spec); strings.ContainsRune(cmd, '\'') {
			t.Errorf("InstallCmd emitted a single quote (breaks the sh -c wrapper):\n%s", cmd)
		}
	}
}
