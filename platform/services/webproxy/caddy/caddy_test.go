package deploy

import (
	"strings"
	"testing"
)

func TestRenderCaddyfile(t *testing.T) {
	out := RenderCaddyfile("relay.example.test", "192.168.30.8:3000", "192.168.30.250:5000",
		"cp.example.test", "192.168.30.9:8080", "192.168.30.9:8089")
	for _, want := range []string{
		"auto_https off",
		"relay.example.test {",
		"cp.example.test {",
		"tls /data/tls/relay/fullchain.pem /data/tls/relay/key.pem",
		"tls /data/tls/cp/fullchain.pem /data/tls/cp/key.pem",
		"handle /pair* {",
		"reverse_proxy 192.168.30.250:5000",
		"reverse_proxy 192.168.30.8:3000",
		"handle /mcp {",
		"reverse_proxy 192.168.30.9:8089",
		"reverse_proxy 192.168.30.9:8080",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderCaddyfile missing %q:\n%s", want, out)
		}
	}
	// The /pair route must sit INSIDE the relay vhost, ahead of its catch-all.
	if i := strings.Index(out, "handle /pair*"); i == -1 || i < strings.Index(out, "relay.example.test {") {
		t.Errorf("/pair handle not inside the relay vhost:\n%s", out)
	}
	if strings.Contains(out, "\t") {
		t.Error("Caddyfile must not contain tabs (embeds in a YAML block scalar)")
	}
}

func TestRenderCaddyfileOmitsPairRouteWithoutUpstream(t *testing.T) {
	out := RenderCaddyfile("relay.example.test", "192.168.30.8:3000", "",
		"cp.example.test", "192.168.30.9:8080", "192.168.30.9:8089")
	if strings.Contains(out, "/pair") {
		t.Errorf("no pair upstream — the /pair route must be omitted:\n%s", out)
	}
	if !strings.Contains(out, "reverse_proxy 192.168.30.8:3000") {
		t.Errorf("relay catch-all lost:\n%s", out)
	}
}
