package agents

import (
	"strings"
	"testing"
)

func TestCPASystemPromptEmbedded(t *testing.T) {
	if !strings.Contains(CPASystemPrompt, "Control Plane Agent") {
		t.Fatalf("CPASystemPrompt does not look like the CPA prompt")
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
	want := []string{"agent-ops", "gatekeeper", "provisioner", "services", "vault"}
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
