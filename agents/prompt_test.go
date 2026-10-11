package agents

import (
	"strings"
	"testing"
)

func TestCPASystemPromptEmbedded(t *testing.T) {
	if !strings.Contains(CPASystemPrompt("", ""), "Control Plane Agent") {
		t.Fatalf("CPASystemPrompt does not look like the CPA prompt")
	}
}

// TestCPASystemPromptCarriesSkills: the CPA's composed prompt carries every
// skill — the granting rules and the unifi access runbook (the env contract +
// the live-network caution) must survive a refactor of the composition.
func TestCPASystemPromptCarriesSkills(t *testing.T) {
	p := CPASystemPrompt("", "")
	for _, want := range []string{"granting skill", "access-unifi", "UNIFI_API_ADMIN"} {
		if !strings.Contains(p, want) {
			t.Fatalf("CPASystemPrompt is missing %q", want)
		}
	}
	// The other departments do NOT carry the CPA's skills — they are the
	// CPA's runbooks (compute carries its own create-lxc runbook instead).
	if strings.Contains(SystemPrompt("network", "", "", ""), "UNIFI_API_ADMIN") {
		t.Fatalf("network's prompt must not carry the CPA's unifi runbook")
	}
}

// TestComputePromptCarriesCreateLxc: compute's composed prompt carries its
// create-lxc runbook — the required-name rule and the door handoff must
// survive a refactor of the composition.
func TestComputePromptCarriesCreateLxc(t *testing.T) {
	p := SystemPrompt("compute", "", "", "")
	for _, want := range []string{"create-lxc", "No name, no create", "provision_runner"} {
		if !strings.Contains(p, want) {
			t.Fatalf("compute's prompt is missing %q", want)
		}
	}
}

// TestOrientationOnEveryPrompt: the CPA, every department, AND custom agents
// carry the shared system orientation — the conversation-flow rules (plumbing
// in #freehold, plain-language status in user channels) live in one place. The
// raw custom renderer stays bare.
func TestOrientationOnEveryPrompt(t *testing.T) {
	const sentinel = "System orientation"
	if got := CPASystemPrompt("", ""); !strings.Contains(got, sentinel) {
		t.Errorf("CPA prompt is missing the shared orientation block")
	}
	for _, name := range DepartmentNames() {
		if got := SystemPrompt(name, "", "", ""); !strings.Contains(got, sentinel) {
			t.Errorf("department %q prompt is missing the shared orientation block", name)
		}
	}
	if got := SystemPrompt("waldo", "look after the garden", "", ""); !strings.Contains(got, sentinel) {
		t.Errorf("custom agent prompt must carry the system orientation block")
	}
	if got := AgentSystemPrompt("waldo", ""); strings.Contains(got, sentinel) {
		t.Errorf("AgentSystemPrompt (the raw renderer) must not prepend the orientation block")
	}
}

func TestOrientationRepoURLOverride(t *testing.T) {
	const repo = "https://github.com/example/fork"
	if got := CPASystemPrompt(repo, ""); !strings.Contains(got, repo) {
		t.Errorf("CPA prompt must render the supplied repo URL")
	}
	if got := CPASystemPrompt("", ""); !strings.Contains(got, UpstreamRepoURL) {
		t.Errorf("CPA prompt must fall back to the upstream repo URL")
	}
	if got := SystemPrompt("data", "", repo, ""); !strings.Contains(got, repo) {
		t.Errorf("department prompt must render the supplied repo URL")
	}
	if got := SystemPrompt("data", "", "", ""); !strings.Contains(got, UpstreamRepoURL) {
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

// TestProvisioningAsksFreehold: every composed prompt — the CPA, all four
// departments, and the custom template — carries the workroom rule: door
// staging is freehold-only, asked in #freehold (never a DM), with the plumbing
// kept out of user channels.
func TestProvisioningAsksFreehold(t *testing.T) {
	prompts := map[string]string{
		"cpa":    CPASystemPrompt("", ""),
		"custom": SystemPrompt("waldo", "look after the garden", "", ""),
	}
	for _, name := range DepartmentNames() {
		prompts[name] = SystemPrompt(name, "", "", "")
	}
	for name, p := range prompts {
		for _, want := range []string{"never a DM", "plumbing"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt is missing %q", name, want)
			}
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
		want := []string{"#freehold"}
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
	got := SystemPrompt("network", "look after the garden", "", "")
	dept, _ := DepartmentPrompt("network")
	if !strings.Contains(got, strings.TrimRight(dept, "\n")) {
		t.Errorf("SystemPrompt(network) must select the department prompt")
	}
	if strings.Contains(got, "look after the garden") {
		t.Errorf("a department prompt must not embed the create-time purpose")
	}
}

// TestDataPromptComposesSnapshotSkill: data's prompt carries the snapshot
// runbook (the verbs' door + the rollback discipline) — the identity that
// executes the skill is the one it renders onto.
func TestDataPromptComposesSnapshotSkill(t *testing.T) {
	got := SystemPrompt("data", "the data plane", "", "")
	for _, want := range []string{"cp-local-root", "snapshot rollback", "The snapshot skill"} {
		if !strings.Contains(got, want) {
			t.Errorf("data's composed prompt lacks %q", want)
		}
	}
	// The other departments must not carry it.
	if other, _ := DepartmentPrompt("compute"); strings.Contains(other, "cp-local-root") {
		t.Errorf("compute's prompt must not carry data's snapshot skill")
	}
}

// TestDataPromptComposesBackblazeSkill: data's prompt also carries the
// off-site runbook — the restic leg, the timer render, and the rule that
// init/credentials never pass through the agent.
func TestDataPromptComposesBackblazeSkill(t *testing.T) {
	got := SystemPrompt("data", "the data plane", "", "")
	for _, want := range []string{"The backblaze skill", "backup run", "install-timer", "never runs from you", "restore latest"} {
		if !strings.Contains(got, want) {
			t.Errorf("data's composed prompt lacks %q", want)
		}
	}
	if other, _ := DepartmentPrompt("ai"); strings.Contains(other, "install-timer") {
		t.Errorf("ai's prompt must not carry data's backblaze skill")
	}
}

func TestSystemPromptFallsBackToCustomTemplate(t *testing.T) {
	got := SystemPrompt("waldo", "look after the garden", "", "")
	if !strings.Contains(got, "You are waldo") || !strings.Contains(got, "look after the garden") {
		t.Errorf("a non-department name must render the custom template: %q", got)
	}
}

// TestOrientationOperatorTZ: the timezone note renders only when the operator
// timezone is set — every agent learns the zone (custom agents included, the
// orientation block is shared); an unset setting renders no note at all (the
// pods-run-UTC default, unchanged shape). The raw custom renderer stays bare.
func TestOrientationOperatorTZ(t *testing.T) {
	got := CPASystemPrompt("", "America/Chicago")
	if !strings.Contains(got, "America/Chicago") || !strings.Contains(got, "Timezones") {
		t.Errorf("CPASystemPrompt with a timezone lacks the timezone note: %q", got[:200])
	}
	plain := CPASystemPrompt("", "")
	if strings.Contains(plain, "Timezones") || strings.Contains(plain, "America/Chicago") {
		t.Errorf("CPASystemPrompt without a timezone must render no note")
	}
	dept := SystemPrompt("network", "", "", "Europe/Berlin")
	if !strings.Contains(dept, "Europe/Berlin") {
		t.Errorf("SystemPrompt(department) must carry the timezone note")
	}
	custom := SystemPrompt("waldo", "", "", "Europe/Berlin")
	if !strings.Contains(custom, "Europe/Berlin") {
		t.Errorf("SystemPrompt(custom) must carry the timezone note")
	}
	raw := AgentSystemPrompt("waldo", "")
	if strings.Contains(raw, "Timezones") {
		t.Errorf("AgentSystemPrompt (raw) renders no orientation, timezone note included")
	}
}

// TestAIPromptComposesLitellmUiUserSkill pins ai's composed prompt: the
// litellm-ui-user runbook (the /user/new call through the
// litellm-api-admin door) rides ai's identity — no other department's.
func TestAIPromptComposesLitellmUiUserSkill(t *testing.T) {
	got := SystemPrompt("ai", "the AI domain", "", "")
	for _, want := range []string{"litellm", "user/new", "app_owner", "LITELLM_API_ADMIN_URL"} {
		if !strings.Contains(got, want) {
			t.Errorf("ai's composed prompt lacks %q", want)
		}
	}
	if other, _ := DepartmentPrompt("network"); strings.Contains(other, "user/new") {
		t.Errorf("network's prompt must not carry ai's litellm skill")
	}
}
