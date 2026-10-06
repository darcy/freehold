package vultr

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freehold/platform/provisioning"
)

// stubClientFor swaps the session client for one pointed at the test
// server (the provider builds its own client from the session's key).
func stubClientFor(t *testing.T, h http.Handler) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	orig := clientForBuilder
	clientForBuilder = func(s *provisioning.HostSession) (*Client, error) {
		return &Client{Token: s.Answers["VULTR_API_KEY"], BaseURL: srv.URL, HTTP: srv.Client()}, nil
	}
	t.Cleanup(func() { clientForBuilder = orig })
}

func recordingSession(printed *[]string) (*provisioning.HostSession, *[]string) {
	var scripts []string
	s := &provisioning.HostSession{
		Answers:  map[string]string{"VULTR_API_KEY": "tok", "region": "ewr", "plan": "vc2-4c-8gb", "label": "freehold-demo"},
		DoorLine: "ssh-ed25519 AAA door",
		Host:     "",
		ExecOnHost: func(script string, timeoutSecs uint64) error {
			scripts = append(scripts, script)
			return nil
		},
		Print: func(format string, args ...any) {
			if printed != nil {
				*printed = append(*printed, strings.TrimSpace(format))
			}
		},
	}
	return s, &scripts
}

func TestVultrNeedsDeclareKeyAndAnswers(t *testing.T) {
	needs := (HostProvider{}).Needs()
	var hasSecret, hasRegion, hasPlan bool
	for _, n := range needs {
		switch n.Name {
		case "VULTR_API_KEY":
			hasSecret = n.Secret
		case "region":
			hasRegion = n.Default != ""
		case "plan":
			hasPlan = n.Default != ""
		case provisioning.NeedHost, provisioning.NeedProxyIP, provisioning.NeedConfirmStorage:
			t.Errorf("a created-host provider must not need %q (it derives them)", n.Name)
		}
	}
	if !hasSecret || !hasRegion || !hasPlan {
		t.Fatalf("needs incomplete: %+v", needs)
	}
	if (HostProvider{}).HostsGateway() != true {
		t.Error("the vultr host IS the gateway")
	}
	if d := (HostProvider{}).Defaults(); d["storage"] != "local" {
		t.Errorf("the dir storage default missing: %v", d)
	}
}

func TestVultrPrepareMintRunsPVEInstall(t *testing.T) {
	creates := 0
	stubClientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/ssh-keys" && r.Method == http.MethodGet:
			w.Write([]byte(`{"ssh_keys":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/ssh-keys":
			w.Write([]byte(`{"ssh_key":{"id":"k1"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/instances":
			creates++
			w.Write([]byte(`{"instance":{"id":"i-9"}}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/instances/i-9"):
			w.Write([]byte(`{"instance":{"status":"active","main_ip":"203.0.113.9"}}`))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
	}))
	s, scripts := recordingSession(nil)
	host, err := (HostProvider{}).Prepare(context.Background(), s, "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if host.ID != "i-9" || host.IP != "203.0.113.9" {
		t.Fatalf("host: %+v", host)
	}
	if s.Host != "root@203.0.113.9" {
		t.Fatalf("session host not set for ExecOnHost: %q", s.Host)
	}
	if creates != 1 {
		t.Fatalf("expected one instance create, got %d", creates)
	}
	// The sshd wait + the PVE install both ride the session transport.
	if len(*scripts) != 2 || !strings.Contains((*scripts)[1], "pve-install-ok") {
		t.Fatalf("the PVE install must run through the session transport: %v", *scripts)
	}
	if s.CreatedID != "i-9" {
		t.Fatalf("the created handle must ride the session the moment create succeeds: %q", s.CreatedID)
	}
}

func TestVultrPrepareReAdoptAlive(t *testing.T) {
	stubClientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/instances/i-live") {
			w.Write([]byte(`{"instance":{"status":"active","main_ip":"203.0.113.5"}}`))
			return
		}
		t.Errorf("a live re-adopt must not touch the API beyond the verify: %s %s", r.Method, r.URL.Path)
	}))
	s, scripts := recordingSession(nil)
	host, err := (HostProvider{}).Prepare(context.Background(), s, "i-live")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if host.ID != "i-live" || host.IP != "203.0.113.5" {
		t.Fatalf("host: %+v", host)
	}
	if len(*scripts) != 0 {
		t.Fatalf("a live re-adopt must not re-install PVE: %v", *scripts)
	}
}

func TestVultrPrepareRefusesOnLookupFailure(t *testing.T) {
	stubClientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		t.Errorf("a failed verify must never create: %s %s", r.Method, r.URL.Path)
	}))
	s, _ := recordingSession(nil)
	if _, err := (HostProvider{}).Prepare(context.Background(), s, "i-x"); err == nil {
		t.Fatal("a rate-limited verify must fail loudly, not mint a second instance")
	}
}

func TestVultrPreparePostCreateFailureCarriesHandle(t *testing.T) {
	// The instance is created, then the PVE install fails (the transport
	// refuses): the session must STILL carry the created handle — the
	// caller records it, so a failed first mint never strands the bill.
	old := sshdWaitTimeout
	sshdWaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { sshdWaitTimeout = old })
	stubClientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/ssh-keys" && r.Method == http.MethodGet:
			w.Write([]byte(`{"ssh_keys":[]}`))
		case r.URL.Path == "/v2/ssh-keys" && r.Method == http.MethodPost:
			w.Write([]byte(`{"ssh_key":{"id":"k1"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/instances":
			w.Write([]byte(`{"instance":{"id":"i-stranded"}}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/instances/i-stranded"):
			w.Write([]byte(`{"instance":{"status":"active","main_ip":"203.0.113.11"}}`))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
	}))
	s, scripts := recordingSession(nil)
	s.ExecOnHost = func(script string, timeoutSecs uint64) error {
		*scripts = append(*scripts, script)
		return errors.New("ssh: connection refused")
	}
	_, err := (HostProvider{}).Prepare(context.Background(), s, "")
	if err == nil {
		t.Fatal("a failing transport must fail the PVE install")
	}
	if !strings.Contains(err.Error(), "i-stranded") {
		t.Fatalf("the failure must name the billed instance: %v", err)
	}
	if s.CreatedID != "i-stranded" {
		t.Fatalf("the handle must survive the failure: %q", s.CreatedID)
	}
}

func TestVultrInstallDoorKeyIdempotent(t *testing.T) {
	s, scripts := recordingSession(nil)
	if err := (HostProvider{}).InstallDoorKey(s, "ssh-ed25519 AAA substrate"); err != nil {
		t.Fatalf("install door: %v", err)
	}
	if len(*scripts) != 1 {
		t.Fatalf("expected one append command, got %v", *scripts)
	}
	cmd := (*scripts)[0]
	if !strings.Contains(cmd, "grep -qF 'ssh-ed25519 AAA substrate'") || !strings.Contains(cmd, "authorized_keys") {
		t.Fatalf("the append must be guarded (no duplicates): %s", cmd)
	}
	if err := (HostProvider{}).InstallDoorKey(s, ""); err != nil {
		t.Fatalf("an empty key is a no-op: %v", err)
	}
}

func TestVultrDestroyFailsClosedWithoutKey(t *testing.T) {
	s, _ := recordingSession(nil)
	s.Answers["VULTR_API_KEY"] = ""
	if err := (HostProvider{}).Destroy(context.Background(), s, "i-9"); err == nil {
		t.Fatal("destroy without a key must fail (a missed destroy keeps billing)")
	}
}
