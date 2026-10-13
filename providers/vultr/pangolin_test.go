package vultr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"freehold/platform/provisioning"
)

// TestPangolinInstallScriptRendersTheWorld pins the payload: the stack
// config serves the WORLD domain (the base_domain the app siblings ride),
// the integration API is on, the server secret is generated at install
// (never a fixed value — a known secret lets anyone forge sessions), and
// the whole thing carries no apostrophes (the script rides a single-quoted
// sh -c).
func TestPangolinInstallScriptRendersTheWorld(t *testing.T) {
	script := PangolinInstallScript("librem.example.test", "darcy@freehold.technology")
	if !strings.Contains(script, `base_domain: "librem.example.test"`) {
		t.Fatal("the stack must serve the world domain as its base")
	}
	if !strings.Contains(script, "enable_integration_api: true") {
		t.Fatal("the integration API must be on (the world's CP drives it)")
	}
	if strings.Contains(script, "CHANGE_ME") || !strings.Contains(script, "$(openssl rand -hex 32)") {
		t.Fatal("the server secret must be generated at install, never a fixed placeholder")
	}
	if strings.Contains(script, "'") {
		t.Fatal("the script must carry no apostrophes (it rides a single-quoted sh -c)")
	}
	if !strings.Contains(script, "PANGOLIN_ROOT_KEY=") {
		t.Fatal("the script must print the root key once")
	}
	if !strings.Contains(script, "/srv/pangolin/root-key.env") {
		t.Fatal("the key must persist on the VPS (a re-run re-prints, never re-mints)")
	}
}

// TestPreparePangolinMintsAndInstalls pins the created-host shape: the
// mint records the billed handle FIRST (OnCreated before the install can
// fail), the door key born-with, the pangolin payload installed (never
// PVE), and the re-adopt path skips the create entirely.
func TestPreparePangolinMintsAndInstalls(t *testing.T) {
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/os"):
			_, _ = w.Write([]byte(`{"os":[{"id":1743,"name":"Debian 12 x64","family":"debian","arch":"x64"}]}`))
		case r.URL.Path == "/v2/regions" || strings.HasSuffix(r.URL.Path, "/regions"):
			_, _ = w.Write([]byte(`{"regions":[{"id":"ewr"}]}`))
		case r.URL.Path == "/v2/plans" || strings.HasSuffix(r.URL.Path, "/plans") || strings.Contains(r.URL.Path, "plans"):
			_, _ = w.Write([]byte(`{"plans":[{"id":"vc2-2c-4gb"}]}`))
		case strings.HasPrefix(r.URL.Path, "/v2/ssh-keys"):
			_, _ = w.Write([]byte(`{"ssh_key":{"id":"k1"}}`))
		case r.URL.Path == "/v2/instances" && r.Method == http.MethodPost:
			created = true
			_, _ = w.Write([]byte(`{"instance":{"id":"pang1"}}`))
		case strings.Contains(r.URL.Path, "/instances") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"instance":{"id":"pang1","status":"active","main_ip":"203.0.113.50"}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	var installed []string
	s := &provisioning.HostSession{
		Answers:  map[string]string{"VULTR_API_KEY": "tok", "region": "ewr", "plan": "vc2-2c-4gb"},
		DoorLine: "ssh-ed25519 AAA door",
		World:    "librem",
	}
	s.Print = func(string, ...any) {}
	s.ExecOnHost = func(script string, _ uint64) error {
		installed = append(installed, script)
		return nil
	}
	clientForBuilder = func(s *provisioning.HostSession) (*Client, error) {
		return &Client{Token: "tok", BaseURL: srv.URL}, nil
	}
	t.Cleanup(func() { clientForBuilder = defaultClientFor })

	host, err := PreparePangolin(context.Background(), s, "", "librem.example.test", "darcy@freehold.technology")
	if err != nil {
		t.Fatal(err)
	}
	if host.ID != "pang1" || host.IP != "203.0.113.50" {
		t.Fatalf("the mint returns the handle: %+v", host)
	}
	if !created {
		t.Fatal("the instance must have been created")
	}
	var install string
	for _, s := range installed {
		if strings.Contains(s, "base_domain: \"librem.example.test\"") {
			install = s
		}
	}
	if install == "" {
		t.Fatalf("the pangolin payload must install (not PVE): %.200s", installed)
	}

	// The re-adopt: the recorded id answers alive — no re-create, and the
	// (idempotent) install script re-runs to revive a dead stack, the same
	// re-ensure the PVE payload's Prepare does.
	created = false
	installed = nil
	host, err = PreparePangolin(context.Background(), s, "pang1", "librem.example.test", "darcy@freehold.technology")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("a re-adopt must not re-create the instance")
	}
	install = ""
	for _, s := range installed {
		if strings.Contains(s, "base_domain: \"librem.example.test\"") {
			install = s
		}
	}
	if install == "" {
		t.Fatalf("a re-adopt re-ensures the payload (the script short-circuits on a live stack): %.200s", installed)
	}
	_ = host
}

// TestPangolinInstallScriptParsesClean pins the payload's SHELL validity:
// the rendered script (the heredocs filled) must parse under sh -n — the
// mint is a ROOT script on a billed host; a parse error strands everything.
func TestPangolinInstallScriptParsesClean(t *testing.T) {
	script := PangolinInstallScript("librem.example.test", "darcy@freehold.technology")
	f := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-n", f).CombinedOutput()
	if err != nil {
		t.Fatalf("the rendered payload must parse as shell: %v: %.200s", err, out)
	}
	// The key parse must produce BARE values: the JSON fragment's field name
	// and the data envelope never survive the cut+tr chain.
	if !strings.Contains(script, `| cut -d: -f2 | tr -dc "a-z0-9"`) {
		t.Fatal("the key parse must strip to the value after the colon")
	}
}
