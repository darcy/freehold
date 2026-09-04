package deploy

import (
	"strings"
	"testing"
)

func TestRenderCaddyfile(t *testing.T) {
	out := RenderCaddyfile("relay.example.test", "192.168.30.8:3000", "cp.example.test", "192.168.30.9:8080")
	for _, want := range []string{
		"auto_https off",
		"relay.example.test {",
		"cp.example.test {",
		"tls /data/tls/relay/fullchain.pem /data/tls/relay/key.pem",
		"tls /data/tls/cp/fullchain.pem /data/tls/cp/key.pem",
		"reverse_proxy 192.168.30.8:3000",
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

func TestCaddyManifest(t *testing.T) {
	out := CaddyManifest(RenderCaddyfile("d.example", "10.0.0.9:3000", "cp.d.example", "10.0.0.8:8080"))
	for _, want := range []string{
		"kind: PersistentVolumeClaim",
		"name: caddy-data",
		"storageClassName: local-path",
		"kind: ConfigMap",
		"name: caddy-caddyfile",
		"hostNetwork: true",
		"image: caddy:2.8",
		"kind: Service",
		"nodePort: 30443",
		"  Caddyfile: |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CaddyManifest missing %q", want)
		}
	}
	// The rendered Caddyfile must be indented under the block scalar (>= 2
	// spaces; we emit 4) and NOT appear flattned (a 0-indent site block would
	// break YAML).
	if !strings.Contains(out, "\n    d.example {") {
		t.Errorf("Caddyfile not block-indented:\n%s", out)
	}
}

func TestYamlBlockIndent(t *testing.T) {
	if got := yamlBlockIndent("a\nb\n"); got != "    a\n    b\n    " {
		t.Errorf("yamlBlockIndent = %q", got)
	}
}
