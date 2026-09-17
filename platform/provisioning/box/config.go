package box

import "freehold/contract/config"

// ApplyConfigDefaults fills any omitted flag from the stored config so a
// rebuild/build is smooth: it reuses the recorded operator key, relay/CP hosts,
// thin-pool, agent name, and proxy IP instead of forcing re-entry. An explicit
// flag always wins; the config only fills blanks.
func ApplyConfigDefaults(f *Flags, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	if f.Name == "" {
		f.Name = cfg.Name
	}
	if f.OperatorPubkey == "" {
		f.OperatorPubkey = cfg.OperatorPubkey
	}
	if f.Target == "" && cfg.Runner.Target != "" {
		f.Target = cfg.Runner.Target
	}
	if f.RelayDomain == "" {
		f.RelayDomain = cfg.RelayHost()
	}
	if f.CpDomain == "" {
		f.CpDomain = cfg.CPHost()
	}
	if f.ThinPool == "" && cfg.Plane.ThinPool != nil {
		f.ThinPool = *cfg.Plane.ThinPool
	}
	if f.AgentName == "" && cfg.CPAName != "" {
		f.AgentName = cfg.CPAName
	}
	if f.ProxyIP == "" && cfg.Proxy.Ip != nil {
		f.ProxyIP = *cfg.Proxy.Ip
	}
	if !f.ManageDNSExplicit && !f.ManageDNS && cfg.Dns.Manager != nil && cfg.Dns.Manager.Managed {
		f.ManageDNS = true
	}
	return nil
}
