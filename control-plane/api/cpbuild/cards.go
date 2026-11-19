package cpbuild

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"freehold/contract/agentcard"
	"freehold/contract/agentsurface"
	"freehold/contract/avatar"
	"freehold/control-plane/api/agenttools"
)

// The starter showcase is deliberately small: freehold (the CPA), with a short
// public prompt. The full repo prompts (the four departments) are the
// appliance's IP and stay private.
const showcaseFreeholdPrompt = "You are freehold, the control plane agent. You talk with the operator and their team in plain language, reason about what they want, and delegate expert work to the Network, Data, Compute, and AI departments. You create and manage agents when asked, and you never pretend to have done work you have not verified."

type showcaseCard struct {
	slug     string
	persona  agentcard.Persona
	avatarBy string // registry name whose pubkey supplies the face
}

func showcaseCards() []showcaseCard {
	return []showcaseCard{
		{
			slug: "freehold",
			persona: agentcard.Persona{
				DisplayName:  "freehold",
				SystemPrompt: showcaseFreeholdPrompt,
				ACPCommand:   "buzz-acp",
				Runtime:      "buzz-agent",
				RespondTo:    "owner-only",
				Parallelism:  1,
				Description:  "the control plane agent — freehold's main reasoning touchpoint",
			},
			avatarBy: "freehold",
		},
	}
}

// publishShowcaseCards publishes the starter marketplace cards, canary first.
// A relay refusal or a read-back mismatch fails the build loudly: a card that
// cannot be imported is worse than no card.
func (s *Spec) publishShowcaseCards(reg *agenttools.Registry) error {
	ownerSec, err := s.ownerKey()
	if err != nil {
		return fmt.Errorf("showcase cards: no owner key to sign personas: %w", err)
	}
	cpHost := strings.TrimSpace(s.CpHost)
	if cpHost == "" {
		return fmt.Errorf("showcase cards: no CP host to derive the hosted avatar URL")
	}
	rows, err := reg.Agents()
	if err != nil {
		return fmt.Errorf("showcase cards: read registry: %w", err)
	}
	byName := map[string]string{}
	for _, row := range rows {
		byName[row.Name] = row.Pubkey
	}
	for _, card := range showcaseCards() {
		p := card.persona
		avatarPk := byName[card.avatarBy]
		if card.slug == "freehold" {
			avatarPk = byName[s.cpaNameOrDefault()]
		}
		if avatarPk == "" {
			avatarPk = avatarSeed(card.slug)
		}
		p.AvatarURL = avatar.URL(cpHost, avatarPk)
		p.Description = agentsurface.ClampAbout(p.Description)
		if err := s.ensureShowcaseCard(ownerSec, p, card.slug); err != nil {
			return fmt.Errorf("showcase card %s: %w", card.slug, err)
		}
	}
	return nil
}

func (s *Spec) ensureShowcaseCard(ownerSec []byte, p agentcard.Persona, slug string) error {
	authURL := s.RelayAuthURL
	if authURL == "" {
		authURL = s.RelayURL
	}
	content, tags, headAt, err := agentcard.QueryPersonaAuth(s.relayDial(), authURL, ownerSec, s.OwnerPub, slug)
	if err != nil {
		return fmt.Errorf("read current head: %w", err)
	}
	if content != "" && agentcard.PersonaMatches(content, tags, p, slug) {
		return nil
	}
	ts := time.Now().Unix()
	if headAt >= ts {
		ts = headAt + 1
	}
	if err := agentcard.PublishPersonaAt(s.relayDial(), authURL, ownerSec, p, slug, ts); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	content, tags, _, err = agentcard.QueryPersonaAuth(s.relayDial(), authURL, ownerSec, s.OwnerPub, slug)
	if err != nil {
		return fmt.Errorf("verify published head: %w", err)
	}
	if !agentcard.PersonaMatches(content, tags, p, slug) {
		return fmt.Errorf("relay accepted the card but the head does not carry the expected body + shared tag")
	}
	return nil
}

// avatarSeed is a deterministic 64-hex stand-in for a card whose registry row
// does not exist yet (a showcase persona that is not a pod agent).
func avatarSeed(slug string) string {
	sum := sha256.Sum256([]byte("freehold-agentcard:" + slug))
	return hex.EncodeToString(sum[:])
}
