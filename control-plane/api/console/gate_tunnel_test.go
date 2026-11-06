package console

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/control-plane/api/cpbuild"
)

// attachTunnelBuilder gives the fixture a build executor with a gateway + a
// console identity, so the edge token derives (the Caddyfile renders the same
// value) and the gateway's internal address is known.
func attachTunnelBuilder(t *testing.T, f *memberFixture) string {
	t.Helper()
	enc := secret32(0x33)
	idJSON, _ := json.Marshal(map[string]string{
		"nostr_secret_hex": hex.EncodeToString(secret32(0x22)),
		"enc_secret_hex":   hex.EncodeToString(enc),
	})
	// The Spec's StateDir is the console root; cpGuestDirs resolves it back
	// from StateDir's parent (the same shape the console deploy uses).
	root := filepath.Join(f.dir, "control-plane")
	if err := os.MkdirAll(filepath.Join(root, "console"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "console", "identity.json"), idJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := cpbuild.NewSpec(cpbuild.Coords{
		CpHost: "cp.example.com", GatewayCIDR: "10.78.0.0/24",
	}, secret32(0x44), "aud")
	spec.StateDir = root
	f.s.Builder = spec
	return f.s.edgeTunnelToken()
}

func tunnelVerify(f *memberFixture, token, clientIP, remoteUser string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/auth/verify", nil)
	req.Header.Set("X-Freehold-Tunnel-Token", token)
	req.Header.Set("X-Original-Client-IP", clientIP)
	req.Header.Set("Remote-User", remoteUser)
	req.Header.Set("X-Original-Host", "yuvomi.cp.example.com")
	req.Header.Set("X-Original-Uri", "/")
	rec := httptest.NewRecorder()
	f.s.ServeHTTP(rec, req)
	return rec
}

// TestGateTrustsTunnelIdentity: the Pangolin public path's per-request
// enforcement. The edge authenticated at the VPS and forwards the member's
// pubkey; the gate trusts it ONLY with the edge token AND from the gateway's
// address, then checks the relay roster like any member session — so channel
// removal revokes immediately, without a Pangolin session expiry.
func TestGateTrustsTunnelIdentity(t *testing.T) {
	f := newMemberFixture(t)
	token := attachTunnelBuilder(t, f)

	if rec := tunnelVerify(f, token, "10.78.0.1", f.memberPK); rec.Code != http.StatusNoContent {
		t.Fatalf("a tunneled member must pass: %d", rec.Code)
	}
	// Not in the app's channel: denied (the roster is the ACL).
	if rec := tunnelVerify(f, token, "10.78.0.1", strings.Repeat("d", 64)); rec.Code != http.StatusForbidden {
		t.Fatalf("a non-member tunnel identity must be denied, got %d", rec.Code)
	}
	// A LAN client reaches the vhost directly: the tunnel identity is NOT
	// trusted (it would be a spoofable header), so the cookie path applies.
	if rec := tunnelVerify(f, token, "192.168.30.9", f.memberPK); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a non-gateway client must fall to the cookie gate, got %d", rec.Code)
	}
	// A wrong token: the client-IP assertion is not Caddy's, so nothing is
	// trusted.
	if rec := tunnelVerify(f, "deadbeef", "10.78.0.1", f.memberPK); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong tunnel token must not be trusted, got %d", rec.Code)
	}
	// Token + gateway but no member identity (Pangolin did not authenticate):
	// fail closed, never fall through to a redirect.
	if rec := tunnelVerify(f, token, "10.78.0.1", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a tunnel request without Remote-User must be denied, got %d", rec.Code)
	}
}
