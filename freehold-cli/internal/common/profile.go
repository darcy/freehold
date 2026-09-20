package common

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"freehold/contract/config"
	oplogin "freehold/freehold-cli/login"
)

// NegotiateProfile picks the active tenant for a config-touching command. An
// explicit --config wins; otherwise, when the box has registered profiles the
// operator is asked to select one, scoping both the config path and the state
// dir via config.Current. Returns selected=false, err=nil when there are NO
// profiles — the caller decides (fail closed).
func NegotiateProfile(cmd *cobra.Command, what string) (bool, error) {
	if cmd.Flags().Changed("config") {
		p, _ := cmd.Flags().GetString("config")
		config.SetCurrent(config.ProfileForConfigPath(p))
		return true, nil
	}
	if len(config.List()) == 0 {
		return false, nil
	}
	_, err := oplogin.SelectProfile(what)
	if err != nil {
		return false, err
	}
	return config.Current() != nil, nil
}

// ProfileConfigPath is the effective config path after negotiation: the active
// profile's when one is negotiated AND the operator didn't pin --config
// explicitly, else the flag's own value.
func ProfileConfigPath(cmd *cobra.Command) string {
	if cmd.Flags().Changed("config") {
		p, _ := cmd.Flags().GetString("config")
		return p
	}
	return config.ConfigPath()
}

// ConfigPath resolves the default config path for the active profile.
func ConfigPath() string { return config.ConfigPath() }

// DefaultConfigPath mirrors the historical CLI default.
func DefaultConfigPath() string { return config.ConfigPath() }

// FreeholdHome is the active profile's scoped state dir (or the legacy
// ~/.freehold via FREEHOLD_HOME when no profile is negotiated).
func FreeholdHome() string { return config.StateDir() }

// ResolveExecProfile pins the tenant context for exec: an explicit --config
// wins; otherwise the single registered profile is used implicitly; multiple
// profiles without --config is ambiguous and fails closed.
func ResolveExecProfile(cmd *cobra.Command) error {
	if cmd.Flags().Changed("config") {
		p, _ := cmd.Flags().GetString("config")
		config.SetCurrent(config.ProfileForConfigPath(p))
		return nil
	}
	if l := config.List(); len(l) == 1 {
		config.SetCurrent(l[0])
		return nil
	} else if len(l) > 1 {
		names := make([]string, 0, len(l))
		for _, p := range l {
			names = append(names, p.Name)
		}
		return fmt.Errorf("multiple tenant profiles (%s) — pass --config <profile config> to pick one", strings.Join(names, ", "))
	}
	return nil
}
