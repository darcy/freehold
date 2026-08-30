package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"freehold/orchestrator/internal/crypto"
)

// AuditKind is the NIP-01 audit event kind for exec records (Phase D5).
const AuditKind = 48001

// SignedEvent is a signed audit record: content is opaque JSON, sig is a
// BIP-340 signature over sha256(content) by pubkey. Mirrors
// core/src/audit.rs::SignedEvent.
type SignedEvent struct {
	Pubkey  string `json:"pubkey"`
	Content string `json:"content"`
	Sig     string `json:"sig"`
}

// SignAuditEvent signs content with the runner's Nostr secret key. The signed
// bytes are sha256(content) — matches core::audit::sign_event.
func SignAuditEvent(nostrSecret []byte, content string) (*SignedEvent, error) {
	digest := sha256.Sum256([]byte(content))
	sig, err := crypto.SignBIP340(nostrSecret, digest[:])
	if err != nil {
		return nil, err
	}
	pubkey, err := crypto.PubkeyFromSecret(nostrSecret)
	if err != nil {
		return nil, err
	}
	return &SignedEvent{
		Pubkey:  pubkey,
		Content: content,
		Sig:     hex.EncodeToString(sig),
	}, nil
}

// VerifyAuditEvent verifies a signed event: sig must be a valid BIP-340
// signature by pubkey over sha256(content). Matches core::audit::verify_event.
func VerifyAuditEvent(ev *SignedEvent) error {
	sig, err := hex.DecodeString(ev.Sig)
	if err != nil {
		return fmt.Errorf("invalid sig hex: %w", err)
	}
	if len(sig) != 64 {
		return fmt.Errorf("signature must be 64 bytes, got %d", len(sig))
	}
	digest := sha256.Sum256([]byte(ev.Content))
	return crypto.VerifyBIP340(ev.Pubkey, digest[:], sig)
}

// BuildAuditEvent builds a kind-N NIP-01 audit event (Phase D5): id over the
// canonical event serialization, BIP-340 signature over the id — the SAME
// event spooled locally AND published to the relay. Matches
// core::audit::Auditor::event.
func BuildAuditEvent(secret []byte, kind uint32, tags [][]string, content string, createdAt int64) (map[string]interface{}, error) {
	pubkey, id, sig, err := SignEvent(secret, kind, createdAt, tags, content)
	if err != nil {
		return nil, fmt.Errorf("audit event build failed: %w", err)
	}
	return map[string]interface{}{
		"id":         id,
		"pubkey":     pubkey,
		"created_at": createdAt,
		"kind":       kind,
		"tags":       tags,
		"content":    content,
		"sig":        sig,
	}, nil
}
