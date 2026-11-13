package cpbuild

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"freehold/contract/crypto"
	"freehold/contract/relay"
	"freehold/control-plane/api/agenttools"
)

// groupMetaEvent is a kind-39000 discovery event the fake relay serves for
// FindChannelAuth (d = channel id, name = display name).
func groupMetaEvent(id, name string) map[string]interface{} {
	return map[string]interface{}{
		"kind":       float64(39000),
		"created_at": float64(1),
		"pubkey":     "relay",
		"tags": []interface{}{
			[]interface{}{"d", id},
			[]interface{}{"name", name},
		},
		"content": "",
		"sig":     "",
	}
}

// memberEvent is a kind-9000 put-user (or 9001 remove-user) membership event.
func pruneMemberEvent(kind int, channelID, pubkey string) map[string]interface{} {
	return map[string]interface{}{
		"kind":       float64(kind),
		"created_at": float64(2),
		"pubkey":     "signer",
		"tags": []interface{}{
			[]interface{}{"h", channelID},
			[]interface{}{"p", pubkey},
		},
		"content": "",
		"sig":     "",
	}
}

func TestStageScheduledJobsChannel(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "agent-tools")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The console identity lives under the CP root's console/ dir.
	consoleSec := make([]byte, 32)
	consoleSec[0] = 0x42
	consolePK, err := crypto.PubkeyFromSecret(consoleSec)
	if err != nil {
		t.Fatal(err)
	}
	consoleDir := filepath.Join(root, "control-plane", "console")
	if err := os.MkdirAll(consoleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	idJSON := fmt.Sprintf(`{"nostr_secret_hex":%q}`, hex.EncodeToString(consoleSec))
	if err := os.WriteFile(filepath.Join(consoleDir, "identity.json"), []byte(idJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	agentPK := strings.Repeat("ab", 32)
	operatorPK := strings.Repeat("cd", 32)
	reg, err := agenttools.OpenRegistry(filepath.Join(stateDir, agenttools.RegistryFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RegisterAgent("cpa", agentPK, "#freehold"); err != nil {
		t.Fatal(err)
	}

	f := newFakeRelay(t)
	generalID := relay.ChannelIDFromName("#general")
	f.events = append(f.events,
		groupMetaEvent(relayFreeholdChannel, "#freehold"),
		groupMetaEvent(generalID, "#general"),
		// The console currently holds a seat in #freehold (the old shape).
		pruneMemberEvent(9000, relayFreeholdChannel, consolePK),
	)

	spec := &Spec{StateDir: stateDir, RelayURL: f.srv.URL, OwnerPub: operatorPK}
	report, err := spec.stageScheduledJobsChannel(nil)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(report) == 0 || !strings.Contains(report[0], "scheduled-jobs channel ensured") {
		t.Errorf("report = %v, want the jobs-channel note", report)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var created, seated, removed bool
	for _, ev := range f.events {
		kind, _ := ev["kind"].(float64)
		tags := tagsOfTest(ev)
		switch int(kind) {
		case 9007:
			if tagValueTest(tags, "h") == agenttools.ScheduledJobsChannelID {
				created = true
			}
		case 9000:
			if tagValueTest(tags, "h") == agenttools.ScheduledJobsChannelID {
				pk := tagValueTest(tags, "p")
				if pk == operatorPK || pk == agentPK {
					seated = true
				}
			}
		case 9001:
			if tagValueTest(tags, "h") == relayFreeholdChannel && tagValueTest(tags, "p") == consolePK {
				removed = true
			}
		}
	}
	if !created {
		t.Error("jobs channel was not created (no kind-9007 with its h id)")
	}
	if !seated {
		t.Error("operator/agent were not seated in the jobs channel")
	}
	if !removed {
		t.Error("console was not removed from #freehold")
	}
}

func tagsOfTest(ev map[string]interface{}) [][]string {
	raw, _ := ev["tags"].([]interface{})
	out := make([][]string, 0, len(raw))
	for _, t := range raw {
		parts, _ := t.([]interface{})
		row := make([]string, 0, len(parts))
		for _, p := range parts {
			s, _ := p.(string)
			row = append(row, s)
		}
		out = append(out, row)
	}
	return out
}

func tagValueTest(tags [][]string, key string) string {
	for _, t := range tags {
		if len(t) > 1 && t[0] == key {
			return t[1]
		}
	}
	return ""
}
