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
