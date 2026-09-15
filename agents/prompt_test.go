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
