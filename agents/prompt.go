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
	"sort"
	"strings"
	"text/template"
)

// UpstreamRepoURL is the default source repository the shared system-orientation
// block points agents at. A deployment may override it (the CP threads its own
// URL through create); an empty value resolves here.
const UpstreamRepoURL = "https://github.com/darcy/freehold"

// cpaPrompt is the freehold named agent's purpose, embedded from
// freehold/prompt.md. The shared orientation block is prepended at render time
// (CPASystemPrompt); create_agent ships the composed result verbatim into the
// CPA pod's <pod>-prompt ConfigMap, and the pod re-reads its mounted copy at
// /srv/freehold/SYSTEM_PROMPT.md on every spawn — never cached.
//
//go:embed freehold/prompt.md
var cpaPrompt string

// grantingSkill is the freehold agent's first skill (skills/granting.md): the
// rules + guardrails for giving away capability — the unit of grant (the
// runner), the naming convention, who may hold what, and the
// revocation/audit discipline. Composed onto the CPA's prompt (the only
// grant-capable identity) until the skill-schema work ships proper
// skill-serving; the file stays the canonical text.
//
//go:embed freehold/skills/granting.md
var grantingSkill string

// orientationTmplSrc is the shared system-orientation block (repo knowledge,
// read-on-boot + periodic re-check, the be-loud escalation discipline) prepended
// to every non-custom prompt: the CPA and the four departments. Custom agents
// (agents the CPA creates on the fly) do NOT get it.
//
//go:embed common/orientation.md
var orientationTmplSrc string

// customSystemPrompt is the template for agents the CPA creates (Chunk 4 Phase
// E): purely conversational, with no skill and no target — holding its own
// relay-persisted memory, exactly like the CPA at this stage. The purpose given
// at create time is rendered in verbatim so the agent knows its reason to exist.
//
//go:embed custom/prompt.md
var customSystemPrompt string

var (
	customPromptTmpl = template.Must(template.New("custom").Parse(customSystemPrompt))
	orientationTmpl  = template.Must(template.New("orientation").Parse(orientationTmplSrc))
)

// repoURLOr returns url, or the upstream default when url is empty/whitespace.
func repoURLOr(url string) string {
	if u := strings.TrimSpace(url); u != "" {
		return u
	}
	return UpstreamRepoURL
}

// renderOrientation renders the shared system-orientation block for a repo URL
// (empty = the upstream default). The template is fixed at build time and its
// only field is provided, so Execute cannot fail at runtime.
func renderOrientation(repoURL string) string {
	var b strings.Builder
	_ = orientationTmpl.Execute(&b, struct{ RepoURL string }{repoURLOr(repoURL)})
	return strings.TrimRight(b.String(), "\n")
}

// CPASystemPrompt is the freehold named agent's full purpose: the shared system
// orientation plus freehold/prompt.md, plus the granting skill. repoURL empty
// = the upstream default.
func CPASystemPrompt(repoURL string) string {
	return renderOrientation(repoURL) + "\n\n" + strings.TrimRight(cpaPrompt, "\n") +
		"\n\n" + strings.TrimRight(grantingSkill, "\n")
}

// Department prompts: the four departments the CPA delegates to (see AGENTS.md
// "Locked model"). Each is an agent definition like freehold/ and custom/,
// embedded from <department>/prompt.md. Deployment is lazy/on-demand, so a
// prompt can exist before any department pod does. Service lifecycle is not a
// department: whichever agent created a service owns it, ad hoc and unvetted.
//
//go:embed network/prompt.md
var networkPrompt string

//go:embed data/prompt.md
var dataPrompt string

//go:embed compute/prompt.md
var computePrompt string

//go:embed ai/prompt.md
var aiPrompt string

// departmentPrompts maps a reserved department identity name to its embedded
// system prompt. The names are reserved: a create_agent naming one of them
// selects that department's prompt rather than the custom template.
var departmentPrompts = map[string]string{
	"network": networkPrompt,
	"data":    dataPrompt,
	"compute": computePrompt,
	"ai":      aiPrompt,
}

// DepartmentNames returns the reserved department identity names, sorted.
func DepartmentNames() []string {
	names := make([]string, 0, len(departmentPrompts))
	for n := range departmentPrompts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// DepartmentPrompt returns the embedded system prompt for a reserved department
// identity name, and whether name is a department.
func DepartmentPrompt(name string) (string, bool) {
	p, ok := departmentPrompts[name]
	return p, ok
}

// departmentPurposes is the one-line purpose recorded on each department's
// registry row (and threaded through reconcile). The prompt is the department's
// real definition; this is metadata for the agent inventory.
var departmentPurposes = map[string]string{
	"network": "the network surface — exposure, DNS, remote access, and continuous verification",
	"data":    "the data plane — backup, DR, and restore verification",
	"compute": "the box itself — CPU/RAM/disk, LXC/kube and remote provisioning, and its monitoring",
	"ai":      "models, providers, AI hardware, and the agents themselves",
}

// DepartmentPurpose returns the one-line purpose for a reserved department name.
func DepartmentPurpose(name string) (string, bool) {
	p, ok := departmentPurposes[name]
	return p, ok
}

// DepartmentChannels returns the channels a department identity joins: the
// shared freehold channel plus its own per-department channel, both prefixed
// `freehold-` so they read as one system (#freehold, #freehold-network, …). Nil
// for a non-department name. create_agent resolves each by name (creating it,
// owned by the department, when absent) and adds the CPA to every channel.
func DepartmentChannels(name string) []string {
	if _, ok := departmentPrompts[name]; !ok {
		return nil
	}
	return []string{"#freehold", "#freehold-" + name}
}

// SystemPrompt returns the system prompt to ship for a create: a reserved
// department name selects that department's embedded prompt, prefixed with the
// shared system orientation; any other name renders the custom template with the
// purpose supplied at create time (and NO orientation — custom agents are exempt
// from the repo/escalation block). This is the single selection point for which
// definition a create uses. repoURL empty = the upstream default.
func SystemPrompt(name, purpose, repoURL string) string {
	if p, ok := DepartmentPrompt(name); ok {
		return renderOrientation(repoURL) + "\n\n" + strings.TrimRight(p, "\n")
	}
	return AgentSystemPrompt(name, purpose)
}

// AgentSystemPrompt renders the created-agent prompt for a given name and
// purpose — the custom template, with no system orientation. An empty purpose
// omits the purpose paragraph entirely.
func AgentSystemPrompt(name, purpose string) string {
	var b strings.Builder
	// The template is fixed at build time and all referenced fields are
	// provided, so this cannot fail at runtime.
	_ = customPromptTmpl.Execute(&b, struct{ Name, Purpose string }{name, purpose})
	return strings.TrimRight(b.String(), "\n")
}
