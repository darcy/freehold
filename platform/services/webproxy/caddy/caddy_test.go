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

// TestRenderAppVhosts pins the apps' site blocks: a gated app carries
// forward_auth to the console on the CP guest (uri /auth/verify), an
// ungated one doesn't, and the order is DETERMINISTIC (sorted by fqdn) —
// the build's terraform render and the expose verb's kubectl apply must
// produce identical config.
func TestRenderAppVhosts(t *testing.T) {
	apps := []AppVhost{
		{FQDN: "zeta.example", Upstream: "10.0.0.9:3000", Slot: "app-zeta", Gate: "10.78.0.12:8080"},
		{FQDN: "alpha.example", Upstream: "10.0.0.8:8080", Slot: "app-alpha"}, // no gate
	}
	out := RenderAppVhosts(apps)
	alpha := strings.Index(out, "alpha.example {")
	zeta := strings.Index(out, "zeta.example {")
	if alpha < 0 || zeta < 0 || alpha > zeta {
		t.Fatalf("app blocks must render sorted by fqdn:\n%s", out)
	}
	if !strings.Contains(out, "forward_auth 10.78.0.12:8080 {\n    uri /auth/verify\n  }") {
		t.Fatalf("the gated app must forward to the console:\n%s", out)
	}
	if !strings.Contains(out, "reverse_proxy 10.0.0.9:3000") {
		t.Fatalf("the app's upstream must ride:\n%s", out)
	}
	alphaBlock := out[alpha:zeta]
	if strings.Contains(alphaBlock, "forward_auth") {
		t.Fatalf("an ungated app must not carry forward_auth:\n%s", alphaBlock)
	}
	if !strings.Contains(out, "tls /data/tls/app-zeta/fullchain.pem /data/tls/app-zeta/key.pem") {
		t.Fatalf("each app presents its own slot's cert:\n%s", out)
	}
	// The compose: the base file plus the apps' blocks, one render for both
	// appliers.
	full := RenderCaddyfileApps("relay.example", "10.0.0.11:3000", "", "cp.example", "10.0.0.12:8080", "10.0.0.12:8089", apps)
	if !strings.Contains(full, "relay.example {") || !strings.Contains(full, "cp.example {") || !strings.Contains(full, "zeta.example {") {
		t.Fatal("the composed render must carry base + apps")
	}
}
