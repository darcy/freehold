package agenttools

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"freehold/contract/console"
	"freehold/control-plane/api/agent"
)

// Server is the freehold-agent-tools MCP server: JSON-RPC 2.0 over HTTP POST
// at /mcp (the same protocol shape the runner serves). tools/call is
// authorized with the shared signed-header scheme; each tool handler calls an
// in-process agent.Tools method (direct call, not a proxy, not exec).
type Server struct {
	// Audience is this server's own pubkey — the caller's signature binds to
	// it, closing cross-audience replay, exactly like a runner's pubkey.
	Audience string
	// Grants returns the CURRENT whitelist of caller pubkeys allowed to call
	// this server, read fresh for every tools/call. It is the server's own
	// relay roster (39002 channel membership), read live per call and
	// fail-closed on relay error — the same grant model a runner uses. Seeded
	// at bootstrap with the operator/build identity, and revocable without a
	// restart (revocation is a roster change, not a process state change).
	Grants func() ([]string, error)

	// Tools holds the bound agent-management actions (Console + the deploy
	// path). create_agent / grant_agent / manage_agent dispatch here.
	Tools *agent.Tools

	// Facts is the durable world-facts store (plane/certs/domains registered at
	// build). nil = the world-facts surface is unregistered.
	Facts *FactsStore

	// IsAgent reports whether a caller pubkey is a REGISTRY agent (a row in
	// the CP's agent registry). Scope rule: registry agents get the
	// create/grant/manage toolset only; operator callers (roster members NOT
	// in the registry — the admin/seed grants minted at bootstrap) get the
	// full toolset including the world_* actions. Keyed on the registry, read
	// fresh per call, so a revoked registry row loses world access on the
	// next request. nil = everyone is an operator (no registry filtering).
	IsAgent func(callerPubkey string) bool

	// AgentName resolves a caller pubkey to its registry agent NAME (the
	// exposure tools' scope check: exposure is the NETWORK department's
	// capability — the caller must be that agent, an operator, or refused).
	// nil = no name resolution (only operators may expose).
	AgentName func(callerPubkey string) (name string, ok bool)

	// AgentGrants reads the CP's agent-grant mode fresh per call ("confirm" =
	// the default, the CPA's provision_runner flow is up and the confirmation
	// discipline lives in the granting skill; "auto" = grants land without
	// confirmation; "off" = the server denies provision_runner outright —
	// the kill switch). nil = "confirm".
	AgentGrants func() string

	// ConsolePeer is the console identity's pubkey — the CP's session-authed
	// operator surface, which proxies world_migrate HERE (the registry lock
	// lives in this process). It authenticates by the same signed-header
	// scheme but never by the roster (the console is deliberately not a
	// channel member), and it may call world_migrate ONLY. Empty = no peer.
	ConsolePeer string

	// OperatorPeer is the operator identity's pubkey (--owner-pubkey): the
	// seed's break-glass caller, full operator scope (the dispatch gates a
	// roster-member operator gets — create/grant/manage + the world tools,
	// the IsAgent check still denying registry agents). Peer, not member: the
	// roster is the agent surface (the CPA), while the CP's own identities
	// authenticate by signature at boot — which also keeps a stale CLI's
	// operator-signed migration sweep working across a version jump. Empty =
	// no peer.
	OperatorPeer string

	// Jobs is the scheduled-jobs store (this process owns jobs.json + runs
	// the scheduler). nil = the job tools refuse (not bound).
	Jobs *JobsStore
	// ResolveChannel maps a channel name (or passes through an id) to the
	// h-tag id the fire posts to, signed by the CONSOLE identity — the same
	// credential that fires the job, so what resolves here is what can post
	// later. nil = job creation needs a raw channel id.
	ResolveChannel func(name string) (string, error)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	raw := string(body)

	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		s.rpcError(w, req.ID, -32700, "parse error")
		return
	}
	if req.Method == "initialize" {
		if req.Params == nil {
			s.rpcError(w, req.ID, -32602, "initialize requires params")
			return
		}
		s.rpcResult(w, req.ID, map[string]interface{}{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "freehold-agent-tools", "version": "0.1.0"},
		})
		return
	}
	// tools/list is public (listing tool names reveals nothing), like the runner.
	if req.Method == "tools/list" {
		s.rpcResult(w, req.ID, map[string]interface{}{"tools": ToolList()})
		return
	}

	// Everything else is authorized with the shared signed-header scheme. The
	// whitelist is this server's roster, read fresh per call (a revocation is
	// a relay roster change and lands on the very next request); a relay
	// outage fails closed (empty grants => deny all).
	grants, gerr := s.Grants()
	if gerr != nil {
		s.rpcError(w, req.ID, -32001, "authorization unreadable (roster): "+gerr.Error())
		return
	}
	caller, aerr := VerifyRequest(grants, s.Audience,
		r.Header.Get(PubkeyHeader), r.Header.Get(SigHeader), r.Header.Get(TSHeader), raw)
	if aerr != nil {
		// Local peers authenticate by signature, never by roster: the console
		// (the world_migrate proxy) and the operator (the seed's break-glass
		// caller — a stale CLI's sweep must survive a version jump). Each
		// verifies with itself as the grant, so the signature check is
		// unchanged; the peer's IDENTITY is the authorization.
		self := ""
		if s.ConsolePeer != "" && r.Header.Get(PubkeyHeader) == s.ConsolePeer {
			self = s.ConsolePeer
		} else if s.OperatorPeer != "" && r.Header.Get(PubkeyHeader) == s.OperatorPeer {
			self = s.OperatorPeer
		}
		if self != "" {
			caller, aerr = VerifyRequest([]string{self}, s.Audience,
				r.Header.Get(PubkeyHeader), r.Header.Get(SigHeader), r.Header.Get(TSHeader), raw)
		}
	}
	if aerr != nil {
		// Server-side detail the client error deliberately omits: WHO claimed
		// to call and WHO this server is. An audience drift (a pod signing a
		// stale agent-tools identity after a durable-state re-mint) reads here
		// as "signature does not verify" with two different pubkeys.
		log.Printf("freehold-agent-tools: rejected caller %s: %v (audience %s)",
			ShortHex(r.Header.Get(PubkeyHeader)), aerr, ShortHex(s.Audience))
		s.rpcError(w, req.ID, -32001, aerr.Error())
		return
	}

	switch req.Method {
	case "tools/call":
		s.dispatch(w, req.ID, req.Params, caller)
	default:
		s.rpcError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// ToolList is the semantic toolset — create/grant/manage. Deliberately NOT
// exec: exec stays the runner's funnel. Package-level (it uses no server
// state) so the pod-side stdio bridge can pin its mirrored schemas against
// this one — the drift guard lives in cmd/freehold-agent-tools.
func ToolList() []map[string]interface{} {
	i := func(props map[string]interface{}, req []string) map[string]interface{} {
		return map[string]interface{}{"type": "object", "properties": props, "required": req}
	}
	return []map[string]interface{}{
		{
			"name": "create_agent", "description": "Create a new conversational agent (name + one-line purpose + the channel(s) to add it to; each channel is created if it doesn't exist, the operator is added, and the CPA is added to every channel). model (optional) picks the LiteLLM alias the agent reasons on — Code for coding agents, ExtraThinking for deep architecture/thinking work, General (the default) otherwise. Returns the new agent's pubkey.",
			"inputSchema": i(map[string]interface{}{
				"name":     map[string]interface{}{"type": "string"},
				"purpose":  map[string]interface{}{"type": "string"},
				"channel":  map[string]interface{}{"type": "string"},
				"channels": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				"private":  map[string]interface{}{"type": "boolean"},
				"model":    map[string]interface{}{"type": "string", "enum": agent.CustomLiteLLMModels},
			}, []string{"name"}),
		},
		{
			"name": "grant_agent", "description": "Bind agent pubkeys to a runner's whitelist.",
			"inputSchema": i(map[string]interface{}{
				"runner":  map[string]interface{}{"type": "string"},
				"pubkeys": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			}, []string{"runner", "pubkeys"}),
		},
		{
			"name": "provision_runner", "description": "Stage a NEW capability runner on the fly and grant the named agents onto its roster (the grant-giving flow: new capability = new runner, named <target>-<protocol>-<identity>). The tool takes NO credential: kind=ssh mints the runner's own keypair and returns the public key to install on the target; api-class kinds ship EMPTY — DM the operator the returned door page link and they fill the credential in the console web UI. api-class kinds REQUIRE probe — the door's verify arm as data, since you know the API: \"<METHOD> <path> [auth] [want] [insecure]\" (e.g. \"GET /user/tokens/verify bearer\"; auth one of bearer (default) | basic | json-body — the credential IS the POST body, unifi-style | none; want a 3-digit status, default 200; the literal token \"insecure\" composes curl -k for a private-CA target like a k3s API) and optionally probe_body (a literal JSON request body alongside the credential — kubernetes' SelfSubjectReview; kind=kubernetes injects its own verify arm — never send a probe or probe_body on a kube door). The runner composes the curl itself; no rebuild is ever needed for a new kind. unifi doors: the operator fills username+password in the console and the exec env carries UNIFI_API_ADMIN as a JSON object with the keys username and password — POST it to <controller>/api/auth/login, take the session token from the response, and call the API with it; there is no X-API-KEY on this door (probe: \"POST /api/auth/login json-body\"). hosted=\"self\" (kind=local) enrolls a runner RESIDENT on the target instead of staging one on the CP guest: the runner-client was installed on the box and `runner enroll` printed its pubkeys — pass them (pubkey, enc_pubkey) plus host (the box's PINNED NAME — a bare host, no port; the CP allocates the port); the CP records the identity, starts nothing, and the target runs its own unit. The operator must confirm the enrollment on the door page (verifying the pubkeys against the guest's own enroll output) before the credential fill unlocks. kind=kubernetes is a KUBE SLOT — a namespace-scoped door for an agent's own workloads, NEVER cluster scope (the cluster itself is Compute's, kube-api-root): name it kube-api-<slot>, pass ns (the slot's namespace — a fresh DNS label, not a platform namespace (kube-*, default, caddy, litellm, agents)) and optionally quota (\"cpu=4,memory=8Gi,pods=32\"), and the slot must be CARVED FIRST by Compute through kube-api-root — ask Compute in conversation to apply the slot manifest (a missing slot's error carries it) — then this tool verifies the slot, reads its SA token and seals it CP-side: the door is live immediately, no console fill, no door page. The record re-creates the slot and re-seals the token on every rebuild (a k3s rebuild rotates the CA); the ns is fixed at first provision, and one slot per namespace — share the door via grant_to instead of re-slicing it. Grants land live; the grantees' pods are re-applied with the new coords. address is the target endpoint (user@host[:port] for ssh, the base URL for api-class) — REFUSED for kind=kubernetes (the CP derives the k3s API route; an agent-stated endpoint would hand the sealed token to a server the agent controls).",
			"inputSchema": i(map[string]interface{}{
				"name":       map[string]interface{}{"type": "string"},
				"kind":       map[string]interface{}{"type": "string"},
				"address":    map[string]interface{}{"type": "string"},
				"grant_to":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				"probe":      map[string]interface{}{"type": "string"},
				"probe_body": map[string]interface{}{"type": "string"},
				"hosted":     map[string]interface{}{"type": "string"},
				"host":       map[string]interface{}{"type": "string"},
				"pubkey":     map[string]interface{}{"type": "string"},
				"enc_pubkey": map[string]interface{}{"type": "string"},
				"ns":         map[string]interface{}{"type": "string"},
				"quota":      map[string]interface{}{"type": "string"},
			}, []string{"name", "kind", "grant_to"}),
		},
		{
			"name": "revoke_runner", "description": "Take a capability away — the counterpart of provision_runner. With revoke_from set, those agent NAMES lose their grant on the door while it keeps serving the rest of its roster; with revoke_from empty the WHOLE door is retired (roster cleared, credential erased from the CP, its unit stopped where freehold hosts it, its record dropped). Every leg reports whether it was VERIFIED: the roster is re-read from the relay (what the runner checks per call), the unit is asked if it is still active and its port probed, the sealed package is re-opened. Anything unverified is named — a revoked door that is still running is a known state, never a silent one, so relay the unverified legs to the operator instead of claiming a clean teardown. The door's audit channel is KEPT read-only so the revocation stays auditable. Only doors provision_runner gave are touchable: build-time capability runners, the cloudflare-api- doors, and console-provisioned ones stay operator-scoped.",
			"inputSchema": i(map[string]interface{}{
				"name":        map[string]interface{}{"type": "string"},
				"revoke_from": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			}, []string{"name"}),
		},
		{
			"name": "expose_app", "description": "Make a service reachable by the people it serves: a public TLS vhost on the edge (DNS pointed, cert issued, Caddy config applied live), gated by the member login. name is a DNS label — the app's hostname is <name>.<the cp host's own hostname> (a subdomain OF the cp host, the domain the member cookie is scoped to). SCOPE: exposure is Network's capability — only the network department (or the operator) may call this; other agents are refused, so route the ask through Network in conversation. group is the relay CHANNEL the access rides (name or id; default general — the everyone-channel): the requester must ALREADY be in it — access narrows to groups the requester belongs to and never widens; the gate enforces the channel per request (IsMemberAuth), so removing someone from the channel revokes them within the cache TTL. requester is the pubkey of whoever asked (default: you). target is the service's internal host:port (the edge proxies plain HTTP to it). visibility: family (public DNS + gate — the default) | public (NO gate — operator-only) | lan (no public DNS). auth: gate (the member gate — the default) | app (the app has its own auth) | none (operator-only). The record survives rebuilds; a failed apply is re-ensured by the next build and the report names which legs landed.",
			"inputSchema": i(map[string]interface{}{
				"name":       map[string]interface{}{"type": "string"},
				"target":     map[string]interface{}{"type": "string"},
				"group":      map[string]interface{}{"type": "string"},
				"requester":  map[string]interface{}{"type": "string"},
				"visibility": map[string]interface{}{"type": "string", "enum": []string{"family", "public", "lan"}},
				"auth":       map[string]interface{}{"type": "string", "enum": []string{"gate", "app", "none"}},
			}, []string{"name", "target"}),
		},
		{
			"name": "unexpose_app", "description": "Take an exposed app down: the record is removed, the edge config re-applied without its vhost, its A record best-effort removed. The cert's durable mirror entry is kept (harmless). Network's capability — same scope as expose_app.",
			"inputSchema": i(map[string]interface{}{
				"name": map[string]interface{}{"type": "string"},
			}, []string{"name"}),
		},
		{
			"name": "update_agent", "description": "Update an existing agent you created: replace its purpose (the one-liner its system prompt is rendered from — live on the agent's next spawn), switch its litellm model, replace its channel list (private applies only then), or rename it. A rename moves the durable identity dir, workspace, pod objects and registry row to the new name while KEEPING the agent's pubkey — chat history, grants and memory follow. Absent fields keep the row's current values. Core identities (the CPA and the four departments) are refused — their prompts live in the repo.",
			"inputSchema": i(map[string]interface{}{
				"name":     map[string]interface{}{"type": "string"},
				"rename":   map[string]interface{}{"type": "string"},
				"purpose":  map[string]interface{}{"type": "string"},
				"channel":  map[string]interface{}{"type": "string"},
				"channels": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				"private":  map[string]interface{}{"type": "boolean"},
				"model":    map[string]interface{}{"type": "string", "enum": agent.CustomLiteLLMModels},
			}, []string{"name"}),
		},
		{
			"name": "manage_agent", "description": "List registered agents, or (remove=<name>) retire one's pod + derived k8s objects and drop its registry row (the durable workspace dir is kept — it is data).",
			"inputSchema": i(map[string]interface{}{
				"remove": map[string]interface{}{"type": "string"},
			}, []string{}),
		},
		{
			"name": "create_job", "description": "Schedule a job: at its schedule, the control plane posts prompt as a mention to agent in channel, and agent's in-channel reply is the delivery (the job runs on the named agent — usually you; pass another agent's name to run it there). cron is a standard 5-field expression or an @every/@hourly descriptor; tz the IANA zone (default UTC) — resolve the asker's actual clock before writing either; or pass at (a unix timestamp) INSTEAD of cron/at for a one-shot reminder. owner is the npub of the person who asked (omit for your own job). label is a short human name for it. The prompt is REDACTED from every console viewer who is not the owner — including the operator. Confirm the schedule with the asker before creating.",
			"inputSchema": i(map[string]interface{}{
				"prompt":  map[string]interface{}{"type": "string"},
				"cron":    map[string]interface{}{"type": "string"},
				"at":      map[string]interface{}{"type": "integer"},
				"tz":      map[string]interface{}{"type": "string"},
				"channel": map[string]interface{}{"type": "string"},
				"agent":   map[string]interface{}{"type": "string"},
				"owner":   map[string]interface{}{"type": "string"},
				"label":   map[string]interface{}{"type": "string"},
			}, []string{"prompt", "channel"}),
		},
		{
			"name": "list_jobs", "description": "List scheduled jobs. With owner set, only that npub's jobs (what to show a user who asks about their own); without, all jobs. Returns id, owner, agent, channel, schedule, next run, last run status — and the prompt only on jobs you created or that name you as agent.",
			"inputSchema": i(map[string]interface{}{
				"owner": map[string]interface{}{"type": "string"},
			}, []string{}),
		},
		{
			"name": "delete_job", "description": "Delete a scheduled job by id (the way a user stops a job).",
			"inputSchema": i(map[string]interface{}{
				"id": map[string]interface{}{"type": "string"},
			}, []string{"id"}),
		},
		{
			"name": "pause_job", "description": "Pause or resume a scheduled job by id (paused=true pauses, false resumes).",
			"inputSchema": i(map[string]interface{}{
				"id":     map[string]interface{}{"type": "string"},
				"paused": map[string]interface{}{"type": "boolean"},
			}, []string{"id", "paused"}),
		},
		{
			"name": "world_status", "description": "What the CP currently manages (its agent registry) — the box's post-login trigger surface.",
			"inputSchema": i(map[string]interface{}{}, []string{}),
		},
		{
			"name": "world_teardown", "description": "Run the CP-owned world teardown (the inverse of build: relay/k3s + agent-tools process + internal DNS; the CP, its runner, and the agent registry survive).",
			"inputSchema": i(map[string]interface{}{}, []string{}),
		},
		{
			"name": "world_migrate", "description": "Run the CP's pending one-time migration scripts (Omarchy-style <epoch>.sh: ascending order, a failure stops the queue and stays pending).",
			"inputSchema": i(map[string]interface{}{}, []string{}),
		},
		{
			"name": "world_build", "description": "Run the CP-owned world-build/reconcile stages through the CP's co-located runner (the box's login + trigger).",
			"inputSchema": i(map[string]interface{}{}, []string{}),
		},
		{
			"name": "world_exec", "description": "Run a command through the CP's co-located runner (operator-scoped drive-through-CP exec, so a thin login box has the build box's full operational surface).",
			"inputSchema": i(map[string]interface{}{
				"target":    map[string]interface{}{"type": "string"},
				"cmd":       map[string]interface{}{"type": "string"},
				"timeout_s": map[string]interface{}{"type": "integer"},
				"secrets":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			}, []string{"cmd"}),
		},
		{
			"name": "world_authorize_door", "description": "Append an operator box's public door key to the host door through the co-located runner (DOOR_SPEC).",
			"inputSchema": i(map[string]interface{}{
				"pubkey": map[string]interface{}{"type": "string"},
			}, []string{"pubkey"}),
		},
		{
			"name": "world_revoke_door", "description": "Remove an operator box's public door key from the host door.",
			"inputSchema": i(map[string]interface{}{
				"pubkey": map[string]interface{}{"type": "string"},
			}, []string{"pubkey"}),
		},
		{
			"name": "world_register_facts", "description": "Register the deployer-side world facts (plane/storage layout, canonical domains, cert metadata) so a management box renders DATA/Certs from the CP.",
			"inputSchema": i(map[string]interface{}{
				"facts": map[string]interface{}{"type": "object"},
			}, []string{"facts"}),
		},
	}
}

type createAgentArgs struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	// Channel is the single-channel form (the CPA's toolset); Channels is the
	// multi-channel form (a custom agent created into several channels). Both
	// are accepted; Channel is prepended to Channels when both are present.
	Channel  string   `json:"channel"`
	Channels []string `json:"channels"`
	// Private makes an explicitly created channel visibility=private. The
	// default freehold channel is always private.
	Private bool `json:"private"`
	// Model is the optional litellm alias the agent's harness reasons on (one
	// of the agent package's CustomLiteLLMModels; empty = the General
	// default). Freehold is core-only — the deploy path pins it for the CPA +
	// departments regardless of what arrives here.
	Model string `json:"model"`
}
type grantAgentArgs struct {
	Runner  string   `json:"runner"`
	Pubkeys []string `json:"pubkeys"`
}
type provisionRunnerArgs struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Address string   `json:"address"`
	GrantTo []string `json:"grant_to"`
	Hosted  string   `json:"hosted"`
	Host    string   `json:"host"`
	Pubkey  string   `json:"pubkey"`
	EncPub  string   `json:"enc_pubkey"`
	Probe   string   `json:"probe"`
	// ProbeBody is the probe's optional literal JSON request body.
	ProbeBody string `json:"probe_body"`
	// NS/Quota are the kube-slot fields (kind=kubernetes): the slot's
	// namespace (Compute carved it through kube-api-root) and its optional
	// ResourceQuota hard spec.
	NS    string `json:"ns"`
	Quota string `json:"quota"`
}
type revokeRunnerArgs struct {
	Name string `json:"name"`
	// RevokeFrom are the agent NAMES to drop from the door's roster; empty =
	// retire the whole door. The server never interprets it — the flow does.
	RevokeFrom []string `json:"revoke_from"`
}

// exposeAppArgs / unexposeAppArgs are the exposure verbs' bodies. group is
// the relay channel (name or id — the record stores the ID); requester is
// whoever asked (their group membership bounds the grant); visibility/auth
// widen only at the operator's hand (dispatch-enforced).
type exposeAppArgs struct {
	Name       string `json:"name"`
	Target     string `json:"target"`
	Group      string `json:"group"`
	Requester  string `json:"requester"`
	Visibility string `json:"visibility"`
	Auth       string `json:"auth"`
}
type unexposeAppArgs struct {
	Name string `json:"name"`
}
type manageAgentArgs struct {
	Remove string `json:"remove"`
}

// updateAgentArgs mirrors createAgentArgs' shapes, with every field but name
// optional (absent = keep the row's current value). Rename is the new name.
type updateAgentArgs struct {
	Name     string   `json:"name"`
	Rename   string   `json:"rename"`
	Purpose  string   `json:"purpose"`
	Channel  string   `json:"channel"`
	Channels []string `json:"channels"`
	Private  bool     `json:"private"`
	Model    string   `json:"model"`
}

// createJobArgs is create_job's body. Agent defaults to the caller, owner to
// the caller (a self-scheduled job); the CPA passes the asker's npub as owner
// and another agent's name when the job runs there.
type createJobArgs struct {
	Prompt  string `json:"prompt"`
	Cron    string `json:"cron"`
	At      uint64 `json:"at"`
	TZ      string `json:"tz"`
	Channel string `json:"channel"`
	Agent   string `json:"agent"`
	Owner   string `json:"owner"`
	Label   string `json:"label"`
}

// isChannelID reports whether s already looks like an h-tag channel id (a
// UUID) rather than a display name to resolve.
func isChannelID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch c {
		case '-':
			if i != 8 && i != 13 && i != 18 && i != 23 {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// createJob validates + records a job. The channel resolves to its h-tag id
// (a UUID passes through; a name resolves via the console identity — the same
// credential that will fire it, so an unresolvable channel is one the fire
// could not post into anyway).
func (s *Server) createJob(a createJobArgs, caller string) (*Job, error) {
	if s.Jobs == nil {
		return nil, fmt.Errorf("jobs are not bound on this server")
	}
	if a.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if a.Channel == "" {
		return nil, fmt.Errorf("channel is required (the channel the job fires into)")
	}
	channel := a.Channel
	if !isChannelID(channel) {
		if s.ResolveChannel == nil {
			return nil, fmt.Errorf("channel must be the channel id (a UUID); name resolution is not wired on this server")
		}
		resolved, err := s.ResolveChannel(channel)
		if err != nil {
			return nil, fmt.Errorf("channel %q: %w (a private channel the console identity cannot see cannot be fired into — use an open channel, or have the channel owner add the console identity)", a.Channel, err)
		}
		channel = resolved
	}
	// The running agent defaults to the caller; an explicit name must exist
	// (the row routes by the agent's PUBKEY — stable across renames — and
	// carries the name for display, refreshed per fire).
	agentName, agentPK := "", caller
	for _, ai := range mustAgents(s.Tools) {
		if ai.Pubkey == caller {
			agentName = ai.Name
			break
		}
	}
	if a.Agent != "" {
		agentName, agentPK = "", ""
		for _, ai := range mustAgents(s.Tools) {
			if ai.Name == a.Agent {
				agentName, agentPK = ai.Name, ai.Pubkey
				break
			}
		}
		if agentName == "" {
			return nil, fmt.Errorf("agent %q is not in the registry", a.Agent)
		}
	}
	if agentName == "" {
		return nil, fmt.Errorf("agent unknown: pass the name of a registered agent (the caller is not registered)")
	}
	if a.At == 0 {
		if _, err := ParseSchedule(a.Cron, a.TZ); err != nil {
			return nil, err
		}
	}
	nt := nextRunFor(a.Cron, a.TZ, a.At)
	owner := a.Owner
	if owner != "" && !isHex64(owner) {
		return nil, fmt.Errorf("owner must be the asker's 64-hex npub (or omitted for your own job)")
	}
	id, err := randomHex(6)
	if err != nil {
		return nil, err
	}
	job := &Job{
		ID: id, Owner: owner, Agent: agentName, AgentPubkey: agentPK, Channel: channel,
		Cron: a.Cron, At: a.At, TZ: a.TZ, Label: a.Label,
		Prompt: a.Prompt, NextRunAt: nt,
	}
	if err := s.Jobs.Create(job); err != nil {
		return nil, err
	}
	return job, nil
}

// mustAgents lists the registry (empty on error — validation then fails with
// the clearer per-field message).
func mustAgents(t *agent.Tools) []console.AgentInfo {
	if t == nil || t.Console == nil {
		return nil
	}
	agents, err := t.Console.Agents()
	if err != nil {
		return nil
	}
	return agents
}

// nextRunFor computes a new job's first NextRunAt: the one-shot timestamp, or
// the next occurrence after now (0 on an unparseable cron — Create validated
// it, and the first tick advances/re-records).
func nextRunFor(cronExpr, tz string, at uint64) uint64 {
	if at != 0 {
		return at
	}
	nt, _, err := nextAfter(cronExpr, tz, time.Now())
	if err != nil {
		return 0
	}
	return uint64(nt.Unix())
}

// isWorldTool reports whether a tool is an operator-scoped action: granting an
// agent onto a runner's whitelist, the world_* actions, and the door
// authorize/revoke all mutate what the operator owns. grant_agent is
// operator-only because a grant hands direct exec access to a runner's MCP
// surface — letting a prompt-reachable agent (e.g. the CPA) bind an arbitrary
// pubkey onto an arbitrary runner (incl. the CP's own co-located runner) would
// bypass this very boundary. The CPA's agent toolset is create + manage +
// provision_runner (the narrow carve-out: it stages NEW capability runners and
// grants only onto those; grants onto the build-time capability runners stay
// operator-only here).
//
// This is also the enforcement point for the two-tier agent org: a raw
// capability grant (proxy/backup/compute/model) attaches to the department
// identity that owns it, never to a custom agent. The provision_runner carve-out
// is bounded by the same discipline — the granting skill (the CPA's first
// skill) governs who may hold what; the server's part is the agent_grants
// kill switch. Since only an operator can grant onto a build-time runner, and
// a registry agent is denied here (-32003), a custom agent cannot self-serve a
// second, ungoverned path to a department-owned capability.
func isWorldTool(name string) bool {
	switch name {
	case "grant_agent", "world_status", "world_teardown", "world_migrate", "world_build",
		"world_exec", "world_authorize_door", "world_revoke_door", "world_register_facts":
		return true
	}
	return false
}

// isExposureTool reports whether name is an exposure verb — NETWORK's
// capability (external proxy/edge), never a custom agent's. The dispatch
// refuses everyone but the network department's identity and operator
// callers: a custom agent that self-serves exposure is a containment failure
// even when a grant would technically allow it — the ask routes through
// Network in conversation.
func isExposureTool(name string) bool {
	return name == "expose_app" || name == "unexpose_app"
}

// requireExposer enforces the exposure scope: the network department's
// identity or an operator. Returns the refusal error or nil.
func (s *Server) requireExposer(caller string) error {
	if s.AgentName != nil {
		if name, ok := s.AgentName(caller); ok && name == "network" {
			return nil
		}
	}
	if s.IsAgent != nil && s.IsAgent(caller) {
		return fmt.Errorf("exposure is Network's capability — ask Network in conversation (the network department executes exposure; a custom agent never holds it)")
	}
	return nil // operator caller (not a registry agent)
}

// agentGrantsMode returns the configured agent-grant mode ("confirm" default).
func (s *Server) agentGrantsMode() string {
	if s.AgentGrants == nil {
		return "confirm"
	}
	if mode := s.AgentGrants(); mode != "" {
		return mode
	}
	return "confirm"
}

func (s *Server) dispatch(w http.ResponseWriter, id json.RawMessage, params json.RawMessage, caller string) {
	if s.Tools == nil {
		s.rpcError(w, id, -32002, "freehold-agent-tools not bound (no agent actions)")
		return
	}
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		s.rpcError(w, id, -32602, "tools/call requires name")
		return
	}
	// Scope auth (per-channel tool visibility): the world_* actions and
	// grant_agent mutate what the operator owns — registry AGENTS are excluded
	// from them (conversation + create/manage only); only OPERATOR callers
	// (roster members not in the registry) drive them. The CPA's stdio bridge
	// already filters to create/manage; this is the same boundary enforced
	// server-side so it cannot be bypassed by calling the server directly.
	if isWorldTool(call.Name) && s.IsAgent != nil && s.IsAgent(caller) {
		s.rpcError(w, id, -32003, "unauthorized: registry agents cannot call "+call.Name+" (operator-scoped)")
		return
	}
	// The console peer's single tool (narrow by design — it exists to proxy
	// world_migrate, whose execution must stay in THIS process for the
	// registry lock). A console peer reaching anything else is a bug upstream.
	if s.ConsolePeer != "" && caller == s.ConsolePeer && call.Name != "world_migrate" {
		s.rpcError(w, id, -32003, "unauthorized: the console peer may call world_migrate only")
		return
	}
	switch call.Name {
	case "create_agent":
		var a createAgentArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "create_agent arguments: "+err.Error())
			return
		}
		if a.Model != "" && !slices.Contains(agent.CustomLiteLLMModels, a.Model) {
			s.rpcError(w, id, -32602, fmt.Sprintf("create_agent model must be one of [%s]", strings.Join(agent.CustomLiteLLMModels, ", ")))
			return
		}
		channels := append([]string(nil), a.Channels...)
		if strings.TrimSpace(a.Channel) != "" {
			channels = append([]string{a.Channel}, channels...)
		}
		pub, err := s.Tools.CreateAgent(a.Name, a.Purpose, channels, a.Private, a.Model)
		// Persist the purpose + the full channel list/private flag on the created
		// agent's registry row so a rebuild reconciler recreates its system prompt
		// verbatim and rejoins every channel (not just the primary) with the
		// right visibility. Registry-only (the console client has no such row).
		if err == nil {
			if reg, ok := s.Tools.Console.(*Registry); ok {
				_ = reg.SetPurpose(a.Name, a.Purpose)
				_ = reg.SetChannels(a.Name, channels, a.Private)
				if a.Model != "" {
					_ = reg.SetModel(a.Name, a.Model)
				}
			}
		}
		s.textResult(w, id, err, pub)
	case "grant_agent":
		var a grantAgentArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "grant_agent arguments: "+err.Error())
			return
		}
		err := s.Tools.GrantAgent(a.Runner, a.Pubkeys)
		s.textResult(w, id, err, "granted")
	case "provision_runner":
		// The grant-giving carve-out: registry agents (the CPA) may stage NEW
		// capability runners — unless the operator's kill switch is set. The
		// confirm/auto discipline (operator present in-thread vs. DM-confirm)
		// lives in the granting skill; the server cannot see threads.
		if mode := s.agentGrantsMode(); mode == "off" {
			s.rpcError(w, id, -32003, "unauthorized: agent_grants is off — provision_runner is disabled (the operator grants via the console)")
			return
		}
		var a provisionRunnerArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "provision_runner arguments: "+err.Error())
			return
		}
		report, err := s.Tools.ProvisionRunner(agent.ProvisionArgs{
			Name: a.Name, Kind: a.Kind, Address: a.Address, GrantTo: a.GrantTo,
			Hosted: a.Hosted, Host: a.Host, Pubkey: a.Pubkey, EncPubkey: a.EncPub,
			Probe: a.Probe, ProbeBody: a.ProbeBody,
			NS: a.NS, Quota: a.Quota,
		})
		s.textResult(w, id, err, report)
	case "revoke_runner":
		// The take-away carve-out, governed by the SAME kill switch as the
		// grant-giving one: an operator who turns agent grants off has said the
		// agent may neither hand capability out nor take it back — the console is
		// then the only revocation surface. Deliberately shared rather than a
		// second knob: a world where the agent can provision but not revoke (or
		// the reverse) is a half-governed capability plane, and two independent
		// switches is how a half-governed plane stays that way.
		if mode := s.agentGrantsMode(); mode == "off" {
			s.rpcError(w, id, -32003, "unauthorized: agent_grants is off — revoke_runner is disabled (the operator revokes via the console)")
			return
		}
		var a revokeRunnerArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "revoke_runner arguments: "+err.Error())
			return
		}
		report, err := s.Tools.RevokeRunner(agent.RetireArgs{Name: a.Name, RevokeFrom: a.RevokeFrom})
		s.textResult(w, id, err, report)
	case "expose_app":
		// NETWORK's capability (external proxy/edge): the network department's
		// identity or an operator — a custom agent asking directly is refused
		// here and routes through Network in conversation. Widening acts
		// (visibility public / auth none) are operator-only WITHIN the
		// allowed callers too: an agent narrows access to a group the
		// requester belongs to; it never opens the world.
		if err := s.requireExposer(caller); err != nil {
			s.rpcError(w, id, -32003, err.Error())
			return
		}
		var a exposeAppArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "expose_app arguments: "+err.Error())
			return
		}
		if s.IsAgent != nil && s.IsAgent(caller) && (a.Visibility == "public" || a.Auth == "none") {
			s.rpcError(w, id, -32003, "unauthorized: widening access (visibility public / auth none) is an operator-level act — ask the operator")
			return
		}
		// The requester is the VERIFIED caller when not named — the dispatch
		// is the only place that knows it (the flow's default would be the
		// toolset identity, which is neither the operator nor the agent that
		// asked).
		if a.Requester == "" {
			a.Requester = caller
		}
		report, err := s.Tools.ExposeApp(agent.ExposeArgs{
			Name: a.Name, Target: a.Target, Group: a.Group, Requester: a.Requester,
			Visibility: a.Visibility, Auth: a.Auth,
		})
		s.textResult(w, id, err, report)
	case "unexpose_app":
		if err := s.requireExposer(caller); err != nil {
			s.rpcError(w, id, -32003, err.Error())
			return
		}
		var a unexposeAppArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "unexpose_app arguments: "+err.Error())
			return
		}
		report, err := s.Tools.UnexposeApp(a.Name)
		s.textResult(w, id, err, report)
	case "update_agent":
		var a updateAgentArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "update_agent arguments: "+err.Error())
			return
		}
		if a.Model != "" && !slices.Contains(agent.CustomLiteLLMModels, a.Model) {
			s.rpcError(w, id, -32602, fmt.Sprintf("update_agent model must be one of [%s]", strings.Join(agent.CustomLiteLLMModels, ", ")))
			return
		}
		report, err := s.Tools.UpdateAgent(agent.UpdateArgs{
			Name: a.Name, Rename: a.Rename, Purpose: a.Purpose,
			Channel: a.Channel, Channels: a.Channels, Private: a.Private, Model: a.Model,
		})
		s.textResult(w, id, err, report)
	case "manage_agent":
		var a manageAgentArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "manage_agent arguments: "+err.Error())
			return
		}
		agents, err := s.Tools.ManageAgent(a.Remove)
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		b, _ := json.Marshal(agents)
		s.textResult(w, id, nil, string(b))
	case "create_job":
		var a createJobArgs
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			s.rpcError(w, id, -32602, "create_job arguments: "+err.Error())
			return
		}
		job, err := s.createJob(a, caller)
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		out, _ := json.Marshal(job)
		s.textResult(w, id, nil, "scheduled "+string(out))
	case "list_jobs":
		var a struct {
			Owner string `json:"owner"`
		}
		_ = json.Unmarshal(call.Arguments, &a)
		if s.Jobs == nil {
			s.textResult(w, id, fmt.Errorf("jobs are not bound on this server"), "")
			return
		}
		// The same privacy boundary the console API enforces: the prompt and
		// label ride only rows the caller OWNS or is the named agent of —
		// every other caller (agent or operator peer) gets metadata only.
		callerName := ""
		for _, ai := range mustAgents(s.Tools) {
			if ai.Pubkey == caller {
				callerName = ai.Name
				break
			}
		}
		jobs := s.Jobs.Snapshot()
		if a.Owner != "" {
			filtered := jobs[:0]
			for _, j := range jobs {
				if j.Owner == a.Owner {
					filtered = append(filtered, j)
				}
			}
			jobs = filtered
		}
		for i := range jobs {
			if jobs[i].Owner != caller && jobs[i].Agent != callerName {
				jobs[i].Prompt = ""
				jobs[i].Label = ""
			}
		}
		b, _ := json.Marshal(jobs)
		s.textResult(w, id, nil, string(b))
	case "delete_job":
		var a struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || a.ID == "" {
			s.rpcError(w, id, -32602, "delete_job arguments: id required")
			return
		}
		if s.Jobs == nil {
			s.textResult(w, id, fmt.Errorf("jobs are not bound on this server"), "")
			return
		}
		s.textResult(w, id, s.Jobs.Delete(a.ID), "deleted "+a.ID)
	case "pause_job":
		var a struct {
			ID     string `json:"id"`
			Paused *bool  `json:"paused"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || a.ID == "" || a.Paused == nil {
			s.rpcError(w, id, -32602, "pause_job arguments: id + paused required")
			return
		}
		if s.Jobs == nil {
			s.textResult(w, id, fmt.Errorf("jobs are not bound on this server"), "")
			return
		}
		s.textResult(w, id, s.Jobs.SetPaused(a.ID, *a.Paused), "paused "+a.ID)
	case "world_status":
		out, err := s.Tools.WorldStatus()
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		out["cp_pubkey"] = s.Audience
		b, _ := json.Marshal(out)
		s.textResult(w, id, nil, string(b))
	case "world_teardown":
		report, err := s.Tools.WorldTeardown()
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, report)
	case "world_migrate":
		res, err := s.Tools.WorldMigrate()
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		b, _ := json.Marshal(res)
		s.textResult(w, id, nil, string(b))
	case "world_build":
		out, err := s.Tools.WorldBuild()
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, out)
	case "world_exec":
		var a struct {
			Target   string   `json:"target"`
			Cmd      string   `json:"cmd"`
			TimeoutS uint64   `json:"timeout_s"`
			Secrets  []string `json:"secrets"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || a.Cmd == "" {
			s.rpcError(w, id, -32602, "world_exec arguments: cmd required")
			return
		}
		out, err := s.Tools.WorldExec(a.Target, a.Cmd, a.TimeoutS, a.Secrets...)
		if err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, out)
	case "world_authorize_door":
		var a struct {
			Pubkey string `json:"pubkey"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || a.Pubkey == "" {
			s.rpcError(w, id, -32602, "world_authorize_door arguments: pubkey required")
			return
		}
		if err := s.Tools.AuthorizeDoor(a.Pubkey); err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, "door key authorized")
	case "world_revoke_door":
		var a struct {
			Pubkey string `json:"pubkey"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || a.Pubkey == "" {
			s.rpcError(w, id, -32602, "world_revoke_door arguments: pubkey required")
			return
		}
		if err := s.Tools.RevokeDoor(a.Pubkey); err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, "door key revoked")
	case "world_register_facts":
		var a struct {
			Facts json.RawMessage `json:"facts"`
		}
		if err := json.Unmarshal(call.Arguments, &a); err != nil || len(a.Facts) == 0 {
			s.rpcError(w, id, -32602, "world_register_facts arguments: facts required")
			return
		}
		if s.Facts == nil {
			s.textResult(w, id, fmt.Errorf("world-register-facts: no facts store bound"), "")
			return
		}
		var facts WorldFacts
		if err := json.Unmarshal(a.Facts, &facts); err != nil {
			s.textResult(w, id, fmt.Errorf("world-register-facts: bad facts: %w", err), "")
			return
		}
		if err := s.Facts.Register(facts); err != nil {
			s.textResult(w, id, err, "")
			return
		}
		s.textResult(w, id, nil, "world facts registered")
	default:
		s.rpcError(w, id, -32601, "unknown tool: "+call.Name)
	}
}

func (s *Server) textResult(w http.ResponseWriter, id json.RawMessage, err error, text string) {
	if err != nil {
		s.rpcError(w, id, -32000, err.Error())
		return
	}
	s.rpcResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
	})
}

func (s *Server) rpcResult(w http.ResponseWriter, id json.RawMessage, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *Server) rpcError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]interface{}{"code": code, "message": msg},
	})
}

// isHex64 reports whether s is 64 hex chars (a npub).
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// randomHex returns n random bytes as hex.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
