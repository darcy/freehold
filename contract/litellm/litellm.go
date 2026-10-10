// Package litellm is the curated provider table the build's gateway setup
// draws from: every entry is a LiteLLM provider with single-API-key auth (the
// picker offers no Azure/Bedrock/Vertex — those need multi-field credentials),
// its litellm_params model prefix, and a maintained default model the operator
// can override at the prompt. One provider + one model is chosen at first
// build and backs ALL the gateway's aliases; the AI department retargets or
// adds providers later through the litellm-api-admin door.
package litellm

// Provider is one curated LiteLLM provider.
type Provider struct {
	// Name is the picker label (litellm's provider slug).
	Name string `json:"name"`
	// Prefix is the litellm_params.model prefix (`<prefix>/<model>`).
	Prefix string `json:"prefix"`
	// DefaultModel is the model id pre-filled at the prompt (everything after
	// the prefix).
	DefaultModel string `json:"default_model"`
	// Desc is the one-line picker description.
	Desc string `json:"desc"`
	// MaxOutputTokens is the provider's per-request output ceiling the agent
	// harness must stay under (BUZZ_AGENT_MAX_OUTPUT_TOKENS) — the harness
	// default (65536) exceeds some providers' hard caps (Anthropic rejects
	// >64000 with a 400 every agent's first LLM call). 0 = no override (the
	// harness default rides).
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

// providers is the curated table. Verify a DefaultModel (and the prefix's
// exact spelling) against docs.litellm.ai before changing an entry — the
// prompt pre-fills it and the gateway registers it verbatim.
var providers = []Provider{
	{
		Name:         "fireworks_ai",
		Prefix:       "fireworks_ai",
		DefaultModel: "accounts/fireworks/models/glm-5p3-flash",
		Desc:         "Fireworks AI (the freehold default)",
	},
	{
		Name:         "openai",
		Prefix:       "openai",
		DefaultModel: "gpt-4o",
		Desc:         "OpenAI",
	},
	{
		Name:            "anthropic",
		Prefix:          "anthropic",
		DefaultModel:    "claude-sonnet-4-5",
		Desc:            "Anthropic",
		MaxOutputTokens: 64000,
	},
	{
		Name:         "gemini",
		Prefix:       "gemini",
		DefaultModel: "gemini-2.5-flash",
		Desc:         "Google AI Studio (Gemini)",
	},
	{
		Name:         "groq",
		Prefix:       "groq",
		DefaultModel: "llama-3.3-70b-versatile",
		Desc:         "Groq",
	},
	{
		Name:         "deepseek",
		Prefix:       "deepseek",
		DefaultModel: "deepseek-chat",
		Desc:         "DeepSeek",
	},
	{
		Name:         "mistral",
		Prefix:       "mistral",
		DefaultModel: "mistral-large-latest",
		Desc:         "Mistral",
	},
	{
		Name:         "together_ai",
		Prefix:       "together_ai",
		DefaultModel: "meta-llama/Llama-3.3-70B-Instruct-Turbo",
		Desc:         "Together AI",
	},
	{
		Name:         "openrouter",
		Prefix:       "openrouter",
		DefaultModel: "openai/gpt-4o",
		Desc:         "OpenRouter (one key, hundreds of models)",
	},
	{
		Name:         "xai",
		Prefix:       "xai",
		DefaultModel: "grok-3",
		Desc:         "xAI (Grok)",
	},
}

// Providers returns the curated table (a copy — callers may not reorder it).
func Providers() []Provider {
	out := make([]Provider, len(providers))
	copy(out, providers)
	return out
}

// Get returns the provider by slug name, or nil.
func Get(name string) *Provider {
	for _, p := range providers {
		if p.Name == name {
			p := p
			return &p
		}
	}
	return nil
}
