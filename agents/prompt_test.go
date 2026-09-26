package agents

import (
	"strings"
	"testing"
)

func TestCPASystemPromptEmbedded(t *testing.T) {
	if !strings.Contains(CPASystemPrompt(""), "Control Plane Agent") {
		t.Fatalf("CPASystemPrompt does not look like the CPA prompt")
	}
}

// TestCPASystemPromptCarriesSkills: the CPA's composed prompt carries every
// skill — the granting rules and the unifi access runbook (the env contract +
// the live-network caution) must survive a refactor of the composition.
func TestCPASystemPromptCarriesSkills(t *testing.T) {
	p := CPASystemPrompt("")
	for _, want := range []string{"granting skill", "access-unifi", "UNIFI_API_ADMIN"} {
		if !strings.Contains(p, want) {
			t.Fatalf("CPASystemPrompt is missing %q", want)
		}
	}
	// The other departments do NOT carry the CPA's skills — they are the
	// CPA's runbooks (compute carries its own create-lxc runbook instead).
	if strings.Contains(SystemPrompt("network", "", ""), "UNIFI_API_ADMIN") {
		t.Fatalf("network's prompt must not carry the CPA's unifi runbook")
	}
}

// TestComputePromptCarriesCreateLxc: compute's composed prompt carries its
// create-lxc runbook — the required-name rule and the door handoff must
// survive a refactor of the composition.
func TestComputePromptCarriesCreateLxc(t *testing.T) {
	p := SystemPrompt("compute", "", "")
	for _, want := range []string{"create-lxc", "No name, no create", "provision_runner"} {
		if !strings.Contains(p, want) {
			t.Fatalf("compute's prompt is missing %q", want)
		}
	}
}

// TestOrientationOnlyOnNonCustomPrompts: the CPA and every department carry the
// shared system orientation; the custom template (agents the CPA creates) does
// not — it is exempt from the repo/escalation block.
func TestOrientationOnlyOnNonCustomPrompts(t *testing.T) {
	const sentinel = "System orientation"
	if got := CPASystemPrompt(""); !strings.Contains(got, sentinel) {
		t.Errorf("CPA prompt is missing the shared orientation block")
	}
	for _, name := range DepartmentNames() {
		if got := SystemPrompt(name, "", ""); !strings.Contains(got, sentinel) {
			t.Errorf("department %q prompt is missing the shared orientation block", name)
		}
	}
	if got := SystemPrompt("waldo", "look after the garden", ""); strings.Contains(got, sentinel) {
		t.Errorf("custom agent prompt must NOT carry the system orientation block")
	}
	if got := AgentSystemPrompt("waldo", ""); strings.Contains(got, sentinel) {
		t.Errorf("AgentSystemPrompt (custom) must NOT carry the system orientation block")
	}
}

func TestOrientationRepoURLOverride(t *testing.T) {
	const repo = "https://github.com/example/fork"
	if got := CPASystemPrompt(repo); !strings.Contains(got, repo) {
		t.Errorf("CPA prompt must render the supplied repo URL")
	}
	if got := CPASystemPrompt(""); !strings.Contains(got, UpstreamRepoURL) {
		t.Errorf("CPA prompt must fall back to the upstream repo URL")
	}
	if got := SystemPrompt("data", "", repo); !strings.Contains(got, repo) {
		t.Errorf("department prompt must render the supplied repo URL")
	}
	if got := SystemPrompt("data", "", ""); !strings.Contains(got, UpstreamRepoURL) {
		t.Errorf("department prompt must fall back to the upstream repo URL")
	}
}

func TestAgentSystemPromptRendersNameAndPurpose(t *testing.T) {
	got := AgentSystemPrompt("waldo", "look after the garden")
	if !strings.Contains(got, "You are waldo") {
		t.Errorf("rendered prompt missing the agent name: %q", got)
	}
	if !strings.Contains(got, "look after the garden") {
		t.Errorf("rendered prompt missing the purpose")
	}
	if !strings.HasSuffix(got, "own what you do not know.") {
		t.Errorf("rendered prompt has unexpected trailing content: %q", got)
	}
}

func TestAgentSystemPromptOmitsEmptyPurpose(t *testing.T) {
	got := AgentSystemPrompt("waldo", "")
	if strings.Contains(got, "Your purpose") {
		t.Errorf("empty purpose must omit the purpose paragraph: %q", got)
	}
}

func TestDepartmentPromptsEmbeddedWithBoundary(t *testing.T) {
	want := []string{"ai", "compute", "data", "network"}
	got := DepartmentNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("DepartmentNames = %v, want %v", got, want)
	}
	for _, name := range want {
		p, ok := DepartmentPrompt(name)
		if !ok {
			t.Errorf("DepartmentPrompt(%q) not found", name)
			continue
		}
		if strings.TrimSpace(p) == "" {
			t.Errorf("department %q prompt is empty", name)
		}
		if !strings.Contains(p, "Ownership boundary") {
			t.Errorf("department %q prompt does not state an ownership boundary", name)
		}
		if !strings.Contains(p, "relay-audited") {
			t.Errorf("department %q prompt is missing the audit discipline", name)
		}
	}
}

func TestDepartmentPromptUnknownName(t *testing.T) {
	if _, ok := DepartmentPrompt("waldo"); ok {
		t.Fatalf("DepartmentPrompt(\"waldo\") must not resolve to a department")
	}
}

func TestDepartmentPurposeAndChannelsCoverEveryDepartment(t *testing.T) {
	for _, name := range DepartmentNames() {
		if p, ok := DepartmentPurpose(name); !ok || strings.TrimSpace(p) == "" {
			t.Errorf("department %q has no purpose", name)
		}
		want := []string{"#freehold", "#freehold-" + name}
		if got := DepartmentChannels(name); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("DepartmentChannels(%q) = %v, want %v", name, got, want)
		}
	}
	if _, ok := DepartmentPurpose("waldo"); ok {
		t.Errorf("DepartmentPurpose(\"waldo\") must not resolve")
	}
	if got := DepartmentChannels("waldo"); got != nil {
		t.Errorf("DepartmentChannels(\"waldo\") must be nil, got %v", got)
	}
}

func TestSystemPromptSelectsDepartmentByName(t *testing.T) {
	got := SystemPrompt("network", "look after the garden", "")
	dept, _ := DepartmentPrompt("network")
	if !strings.Contains(got, strings.TrimRight(dept, "\n")) {
		t.Errorf("SystemPrompt(network) must select the department prompt")
	}
	if strings.Contains(got, "look after the garden") {
		t.Errorf("a department prompt must not embed the create-time purpose")
	}
}

func TestSystemPromptFallsBackToCustomTemplate(t *testing.T) {
	got := SystemPrompt("waldo", "look after the garden", "")
	if !strings.Contains(got, "You are waldo") || !strings.Contains(got, "look after the garden") {
		t.Errorf("a non-department name must render the custom template: %q", got)
	}
}
