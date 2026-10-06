// The HOST-provider seam: how a world's HOST comes to exist and what the
// operator must supply for it. The provider (proxmox, vultr, …) owns every
// substrate-specific need — credentials, plain answers, defaults, the door
// key's install UX, the create/destroy lifecycle — and the installer asks
// generically from Needs(); nothing about a substrate is hardcoded in the
// installer. A provider that creates hosts (api-vultr) implements Prepare;
// one that is reached (proxmox) verifies and its Prepare is a no-op.
package provisioning

import "context"

// Conventional need names the installer maps onto its own core inputs.
// Anything else a Need carries is provider-specific and rides the answers
// map into the session.
const (
	NeedHost           = "host"            // the runner-SSH address ("root@box")
	NeedProxyIP        = "proxy_ip"        // the edge's static address (CIDR)
	NeedConfirmStorage = "confirm_storage" // consent to create a backend on the host
)

// HostNeed is one input a host provider needs from the operator. Secret
// needs are prompted no-echo in the guided flow (never stored, never
// logged) and fall back to the env var named here; plain needs are asked
// with the default.
type HostNeed struct {
	Name    string
	Label   string
	Default string
	Secret  bool
	Bool    bool // consent-style ask (yes/no), not a text field
}

// HostSession is the provider's handle during host provisioning: the
// operator's answers (secrets included, in memory only) plus the
// installer-owned transports the provider drives. The provider decides WHAT
// runs; the installer owns HOW it executes. Host is the current root@host —
// preset from the host need (proxmox) or set by the provider once its
// created host has an address (vultr, before the PVE install runs).
type HostSession struct {
	Answers map[string]string
	// DoorLine is the DOOR key's authorized_keys line — the credential the
	// provider's host must carry (born-with for vultr, pasted for proxmox).
	DoorLine string
	// Host is "root@<ip>" — set before ExecOnHost may run.
	Host string
	// CreatedID is the instance handle the provider set the MOMENT its
	// create call succeeded — before any later step (the address wait, the
	// host's SSH, the PVE install) can fail. A Prepare error with
	// CreatedID set means a host EXISTS and bills: the caller records the
	// handle before surfacing the failure, so the stranded instance is
	// always recoverable by tooling.
	CreatedID string
	// ExecOnHost runs a script on the session's host over the DOOR key
	// (the installer wires root SSH; ExecOnHost fails when the host does
	// not answer). Never carries secrets other than the door key.
	ExecOnHost func(script string, timeoutSecs uint64) error
	// Print/Prompt are the interactive primitives (Prompt returns the
	// trimmed answer; "" on bare ENTER). Interactive is false on a headless
	// run — Print still works, Prompt must not be called.
	Print       func(format string, args ...any)
	Prompt      func(label string) (string, error)
	Interactive bool
}

// Host is a provisioned host's coordinates.
type Host struct {
	ID string // the provider's instance handle ("" when the host predates the world)
	IP string
}

// HostProvider is the host-provisioning seam (see package doc). The
// composition root (install/uninstall) resolves one from providers/registry
// and drives it; the pipeline's guest engine never sees it.
type HostProvider interface {
	// Name is the registry key ("proxmox", "vultr") — what --provider takes.
	Name() string
	// AccessMode is the recorded install-access strategy ("ssh-root-proxmox",
	// "api-vultr") — how this provider reaches/owns the host.
	AccessMode() string
	// HostsGateway reports whether the HOST itself is the world's gateway
	// (the hosted shape: host nftables, no gateway guest). A reached host
	// (proxmox on a LAN) runs the gateway as a guest instead.
	HostsGateway() bool
	// Needs is everything the operator must supply for this provider —
	// credentials and answers both. The installer asks exactly this.
	Needs() []HostNeed
	// Defaults are the provider's substrate defaults the installer applies
	// when the operator/flags left them unset (e.g. "storage": "local").
	Defaults() map[string]string
	// Prepare makes a host ready to drive: mint = create it (vultr: the
	// instance born with the door key + PVE installed); re-adopt = verify
	// the recorded one (ExistingID) or re-create it when definitively gone.
	// A reached host (proxmox) just resolves/verifies. The returned Host is
	// the world's host coordinates.
	Prepare(ctx context.Context, s *HostSession, existingID string) (*Host, error)
	// InstallDoorKey gets the substrate door key authorized on the host —
	// idempotent, called for fresh AND recovered keys ("" = recover from
	// the runner package is the caller's problem; a provider may no-op).
	// vultr appends via the born-with door key; proxmox shows the paste
	// gate (an actionable error when !Interactive).
	InstallDoorKey(s *HostSession, key string) error
	// Destroy removes a created host (its data dies with it) — the session
	// supplies the credential. A reached host is the operator's — a no-op.
	Destroy(ctx context.Context, s *HostSession, id string) error
}
