// Package agents carries everything about a named agent — its prompt, skills,
// and (later) creation spec, harness config, memory layout. This is the
// canonical, top-level home for agent definitions: freehold/ is the CPA,
// custom/ is the template for agents the CPA creates on the fly, and future
// named agents get their own <name>/ directory alongside freehold/.
//
// The package embeds its Markdown (a package cannot //go:embed outside its own
// module), so the modules that ship prompts import the bytes across the module
// boundary rather than re-embedding.
package agents

import (
	_ "embed"
	"strings"
	"text/template"
)

// CPASystemPrompt is the freehold named agent's purpose, embedded from
// freehold/prompt.md. create_agent ships it verbatim into the CPA pod's
// <pod>-prompt ConfigMap; the pod re-reads its mounted copy at
// /srv/freehold/CPA_SYSTEM_PROMPT.md on every spawn — never cached.
//
//go:embed freehold/prompt.md
var CPASystemPrompt string

// customSystemPrompt is the template for agents the CPA creates (Chunk 4 Phase
// E): purely conversational, with no skill and no target — holding its own
// relay-persisted memory, exactly like the CPA at this stage. The purpose given
// at create time is rendered in verbatim so the agent knows its reason to exist.
//
//go:embed custom/prompt.md
var customSystemPrompt string

var customPromptTmpl = template.Must(template.New("custom").Parse(customSystemPrompt))

// AgentSystemPrompt renders the created-agent prompt for a given name and
// purpose. An empty purpose omits the purpose paragraph entirely.
func AgentSystemPrompt(name, purpose string) string {
	var b strings.Builder
	// The template is fixed at build time and all referenced fields are
	// provided, so this cannot fail at runtime.
	_ = customPromptTmpl.Execute(&b, struct{ Name, Purpose string }{name, purpose})
	return strings.TrimRight(b.String(), "\n")
}
