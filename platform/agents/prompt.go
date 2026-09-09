// Package agents carries everything about a named agent — its prompt, creation
// spec, harness config, memory layout. The canonical CPA definition lives here
// as freehold/prompt.md and is embedded as a Go value so the modules that ship
// it (control-plane/api, cli) import the bytes without embedding across a Go
// module boundary (a package cannot //go:embed outside its own module).
package agents

import _ "embed"

// CPASystemPrompt is the CPA's purpose, embedded from freehold/prompt.md in
// this directory. create_agent ships it verbatim into the CPA pod's <pod>-prompt
// ConfigMap; the pod re-reads its mounted copy at
// /srv/freehold/CPA_SYSTEM_PROMPT.md on every spawn — never cached.
//
//go:embed freehold/prompt.md
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