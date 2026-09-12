package deploy

import (
	"strings"
	"testing"
)

func TestRenderCaddyfile(t *testing.T) {
	out := RenderCaddyfile("relay.example.test", "192.168.30.8:3000", "cp.example.test", "192.168.30.9:8080", "192.168.30.9:8089")
	for _, want := range []string{
		"auto_https off",
		"relay.example.test {",
		"cp.example.test {",
		"tls /data/tls/relay/fullchain.pem /data/tls/relay/key.pem",
		"tls /data/tls/cp/fullchain.pem /data/tls/cp/key.pem",
		"reverse_proxy 192.168.30.8:3000",
		"handle /mcp {",
		"reverse_proxy 192.168.30.9:8089",
		"reverse_proxy 192.168.30.9:8080",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderCaddyfile missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\t") {
		t.Error("Caddyfile must not contain tabs (embeds in a YAML block scalar)")
	}
}
