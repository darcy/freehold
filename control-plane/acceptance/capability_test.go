package acceptance

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"freehold/contract/crypto"
	provisioner "freehold/control-plane/secret-management"
	"freehold/control-plane/api/agenttools"
	"freehold/control-plane/state"
)

// TestAgentProvisionedCapabilityLifecycle drives the on-the-fly grant flow's
// mechanics against the real runner binary: a capability runner provisioned
// for a grantee (the RTX3090 shape), recorded as a dynamic capability
// (rebuild-safe), starts, and enforces its roster — the granted caller execs,
// an outsider fails closed. (The full provision_runner staging — channel sync,
// systemd unit, pod re-apply — needs a live world; the pieces it composes are
// each proven here and in the cpbuild/server unit tests.)
func TestAgentProvisionedCapabilityLifecycle(t *testing.T) {
	base := t.TempDir()
	cpDir := filepath.Join(base, "cp")
	store := openStore(t, cpDir)
	runnerDir := filepath.Join(base, "runner")

	granteeSec := make([]byte, 32)
	if _, err := rand.Read(granteeSec); err != nil {
		t.Fatal(err)
	}
	grantee, err := crypto.PubkeyFromSecret(granteeSec)
	if err != nil {
		t.Fatal(err)
	}
	outsiderSec := make([]byte, 32)
	outsiderSec[0] = 0x42
	outsider, err := crypto.PubkeyFromSecret(outsiderSec)
	if err != nil {
		t.Fatal(err)
	}

	// Provision + record: the record is what makes the door rebuild-safe
	// (staging re-asserts it every build) and marks it agent-provisioned.
	if _, err := provisioner.ProvisionRunner(store, &provisioner.ProvisionRequest{
		Name: "rtx3090-local-probe", Kind: "local", Address: "local",
		Secret: []byte("local"), RunnerDir: runnerDir, Grants: []string{grantee},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCapability("rtx3090-local-probe", state.CapabilityRecord{
		Kind: "local", Address: "local", Port: 8800,
		Rosters: []string{"ai"}, CreatedAt: 5,
	}); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, cpDir)
	rec, ok := reopened.GetCapability("rtx3090-local-probe")
	if !ok || rec.Kind != "local" || rec.Port != 8800 || len(rec.Rosters) != 1 {
		t.Fatalf("capability record must survive reopen: %+v", rec)
	}

	addr := startRunner(t, runnerDir)
	rpk, _ := reopened.GetRunner("rtx3090-local-probe")

	exec := func(sec []byte, caller string) string {
		t.Helper()
		raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec","arguments":{"cmd":"printf probe-ok","target":"rtx3090-local-probe","secrets":["rtx3090-local-probe"]}}}`)
		ts := time.Now().Unix()
		canonical := rpk.NostrPubkey + "|" + strconv.FormatInt(ts, 10) + "|" + raw
		digest := sha256.Sum256([]byte(canonical))
		sig, err := crypto.SignBIP340(sec, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", bytes.NewBufferString(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(agenttools.PubkeyHeader, caller)
		req.Header.Set(agenttools.SigHeader, hex.EncodeToString(sig))
		req.Header.Set(agenttools.TSHeader, strconv.FormatInt(ts, 10))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body strings.Builder
		for {
			buf := make([]byte, 1024)
			n, rerr := resp.Body.Read(buf)
			body.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		return body.String()
	}

	if out := exec(granteeSec, grantee); !strings.Contains(out, "probe-ok") {
		t.Fatalf("the granted caller must exec through the dynamic door: %s", out)
	}
	if out := exec(outsiderSec, outsider); strings.Contains(out, "probe-ok") || !strings.Contains(out, "error") {
		t.Fatalf("an ungranted caller must fail closed: %s", out)
	}
}
