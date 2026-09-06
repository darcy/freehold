// Package prompts embeds the runtime prompt files the orchestrator ships to
// agent pods. The canonical CPA definition lives here as CPA_SYSTEM_PROMPT.md
// and is glued into the freehold-orchestrator binary at compile time, so the
// binary is self-contained regardless of CWD — a host-side os.ReadFile of a
// relative path would silently break staging depending on the working
// directory.
package prompts

import _ "embed"

// CPASystemPrompt is the CPA's purpose, embedded from CPA_SYSTEM_PROMPT.md in
// this directory. stageCpa ships it verbatim into the CPA pod's <pod>-prompt
// ConfigMap; the pod re-reads its mounted copy at
// /srv/freehold/CPA_SYSTEM_PROMPT.md on every spawn — never cached.
//
//go:embed CPA_SYSTEM_PROMPT.md
var CPASystemPrompt string

// AgentSystemPrompt is the default system prompt for agents the CPA creates
// (Chunk 4 Phase E): purely conversational, with no skill and no target —
// holding its own relay-persisted memory, exactly like the CPA at this stage.
// The purpose given at create time is included verbatim so the agent knows its
// reason to exist.
func AgentSystemPrompt(name, purpose string) string {
	var purposeLine string
	if purpose != "" {
		purposeLine = "\nYour purpose, set by the operator via the control plane agent: " + purpose
	}
	return `You are ` + name + ` — a member of the freehold community, running on the same buzz harness as the control plane agent. You hold real, reasoned conversations in Buzz rooms and DMs.` + purposeLine + `
You have no tools and no privileged commands yet — you are conversation-only. Never pretend to run a command, provision a target, deploy a service, or create an agent; if asked for something you cannot do, say so plainly and describe precisely what was requested. You see no secrets and never reference credentials beyond their names. Everything you say is relay-audited, so never route around it. Answer directly, warmly, and honestly; own what you do not know.`
}
