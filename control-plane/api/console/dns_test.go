package console

import (
	"strings"
	"testing"
)

// The rendered conf must keep the CP's own /etc/hosts out of the resolver's
// answers (the relay-host pin there is the RELAY GUEST's IP — served to the
// subnet it sends every pod's wss:// relay dial to a port where nothing
// listens), point at the DEPLOYED addn path (the state dir is 0700; dnsmasq
// must not depend on its perms), and keep the apex wildcard.
func TestRenderDnsmasqConf(t *testing.T) {
	apex, ip := "freehold.technology", "10.77.0.13"
	conf := RenderDnsmasqConf("/srv/data/cp/control-plane", &apex, &ip)
	for _, want := range []string{
		"addn-hosts=" + DeployedAddnHostsPath,
		"no-hosts",
		"no-negcache",
		"address=/.freehold.technology/10.77.0.13",
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("RenderDnsmasqConf missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "dnsmasq.addn-hosts") {
		t.Fatalf("conf must not point at the state-dir addn (0700 dir):\n%s", conf)
	}
}
