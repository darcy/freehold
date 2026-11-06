package cpbuild

import (
	"encoding/json"
	"testing"
	"time"
)

// rebootKilled must match exactly the reboot-casualty footprint: a container
// termination recorded at/after the k3s guest's boot time — clean OR NOT,
// because the harness exits 0 when the relay/model gateway are still booting
// (the casaq incident: chef/ai/coach/compute froze Completed 19 seconds after
// a host reboot) — plus wedged ContainerStatusUnknown terminations whose
// FinishedAt the kubelet never records (the CPA's 39h zombie: exit 137,
// finishedAt null, invisible to the revive). Freehold-managed pods only.
// Clean exits predating the boot stay terminal (I5).
func TestRebootKilled(t *testing.T) {
	boot := time.Date(2026, 10, 7, 2, 3, 34, 0, time.UTC)
	after := boot.Add(2 * time.Second).Format(time.RFC3339)
	before := boot.Add(-1 * time.Hour).Format(time.RFC3339)

	podsJSON := `{"items":[
		{"metadata":{"labels":{"app.kubernetes.io/managed-by":"freehold"},"annotations":{"freehold.fh/agent-name":"freehold"}},
		 "status":{"containerStatuses":[{"state":{"terminated":{"exitCode":255,"finishedAt":"` + after + `"}}}]}},
		{"metadata":{"labels":{"app.kubernetes.io/managed-by":"freehold"},"annotations":{"freehold.fh/agent-name":"chef"}},
		 "status":{"containerStatuses":[{"state":{"running":{}}}]}},
		{"metadata":{"labels":{"app.kubernetes.io/managed-by":"freehold"},"annotations":{"freehold.fh/agent-name":"coach"}},
		 "status":{"containerStatuses":[{"state":{"terminated":{"exitCode":0,"finishedAt":"` + after + `"}}}]}},
		{"metadata":{"labels":{"app.kubernetes.io/managed-by":"freehold"},"annotations":{"freehold.fh/agent-name":"dj"}},
		 "status":{"containerStatuses":[{"state":{"terminated":{"exitCode":137,"finishedAt":"` + before + `"}}}]}},
		{"metadata":{"labels":{},"annotations":{"freehold.fh/agent-name":"sneaky"}},
		 "status":{"containerStatuses":[{"state":{"terminated":{"exitCode":255,"finishedAt":"` + after + `"}}}]}},
		{"metadata":{"labels":{"app.kubernetes.io/managed-by":"freehold"},"annotations":{"freehold.fh/agent-name":"data"}},
		 "status":{"containerStatuses":[{"state":{"waiting":{"reason":"ContainerCreating"}},"lastState":{"terminated":{"exitCode":255,"finishedAt":"` + after + `"}}}]}},
		{"metadata":{"labels":{"app.kubernetes.io/managed-by":"freehold"},"annotations":{"freehold.fh/agent-name":"ai"}},
		 "status":{"containerStatuses":[{"state":{"waiting":{"reason":"ContainerCreating"}},"lastState":{"terminated":{"exitCode":255,"finishedAt":"` + before + `"}}}]}},
		{"metadata":{"labels":{"app.kubernetes.io/managed-by":"freehold"},"annotations":{"freehold.fh/agent-name":"network"}},
		 "status":{"containerStatuses":[{"state":{"terminated":{"exitCode":137}}}]}},
		{"metadata":{"labels":{"app.kubernetes.io/managed-by":"freehold"},"annotations":{"freehold.fh/agent-name":"puravida"}},
		 "status":{"containerStatuses":[{"state":{"terminated":{"exitCode":0}}}]}},
		{"metadata":{"labels":{"app.kubernetes.io/managed-by":"freehold"},"annotations":{"freehold.fh/agent-name":"homelab"}},
		 "status":{"containerStatuses":[{"state":{"terminated":{"exitCode":0,"finishedAt":"` + before + `"}}}]}}
	]}`
	var pods podList
	if err := json.Unmarshal([]byte(podsJSON), &pods); err != nil {
		t.Fatal(err)
	}
	got := rebootKilled(pods, boot.Unix())
	want := []string{"freehold", "coach", "data", "network"}
	if len(got) != len(want) {
		t.Fatalf("rebootKilled = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rebootKilled = %v, want %v", got, want)
		}
	}
}
