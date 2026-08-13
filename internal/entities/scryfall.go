package entities

import (
	"context"
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/data"
)

// Scryfall resolves MTG card mentions via the public Scryfall API. It implements
// data.EntityResolver (the neutral grounding contract) and additionally
// satisfies CardProjector so the server can route Magic through its dedicated
// rich card path — card images, faces, and the rulings layer — instead of the
// generic entity path used for D&D.
type Scryfall struct {
	cards *cards.Service
	dict  *cards.Dictionary
}

// NewScryfall builds a Scryfall resolver over a card lookup service and an
// optional card-name dictionary (powers lowercase/unquoted card detection,
// mirroring the chat's MTG behavior). Either may be nil, in which case Resolve
// resolves nothing — the same graceful degradation the card path already has.
func NewScryfall(svc *cards.Service, dict *cards.Dictionary) *Scryfall {
	return &Scryfall{cards: svc, dict: dict}
}

// CardProjector is implemented by entity resolvers that ground card-shaped
// entities (Scryfall/MTG). The server renders card-projector corpora through
// its existing card path — images, faces, official rulings — rather than the
// neutral entity path; resolvers for corpora without cards (Open5e/D&D) do not
// implement it. Defining the marker here keeps the routing contract with the
// producer of resolvers rather than the consumer.
type CardProjector interface {
	data.EntityResolver
	projectsCards()
}

// projectsCards marks Scryfall as a card-projecting resolver.
func (*Scryfall) projectsCards() {}

// Resolve extracts candidate card names from the question and looks each up via
// Scryfall, returning the resolved oracle text plus names it could not resolve.
// The output is a neutral projection (Kind "card"); the server uses the richer
// card path for the UI, but this satisfies the registry contract and is
// available to any caller that wants MTG grounding without card-shaped UI.
func (s *Scryfall) Resolve(ctx context.Context, question string) ([]data.Entity, []string, error) {
	if s == nil || s.cards == nil {
		return nil, nil, nil
	}
	res := cards.Resolve(ctx, s.cards, cards.ExtractCandidatesWithDict(question, s.dict))
	entities := make([]data.Entity, 0, len(res.Cards))
	for _, c := range res.Cards {
		entities = append(entities, data.Entity{Name: c.Name, Kind: "card", Body: formatCard(c)})
	}
	return entities, res.Unresolved, nil
}

// formatCard renders a card as grounding prose for the neutral entity shape:
// mana cost, type line, and oracle text (each face for double-faced cards). This
// mirrors the prompt section the MTG card path feeds the model, so a caller that
// resolves MTG through the interface sees equivalent text.
func formatCard(c *cards.Card) string {
	var b strings.Builder
	if c.ManaCost != "" {
		fmt.Fprintf(&b, "Mana cost: %s. ", c.ManaCost)
	}
	if c.TypeLine != "" {
		fmt.Fprintf(&b, "Type: %s.", c.TypeLine)
	}
	if len(c.Faces) > 0 {
		for _, f := range c.Faces {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			fmt.Fprintf(&b, "%s. %s", f.Name, strings.TrimSpace(f.OracleText))
		}
	} else if text := strings.TrimSpace(c.OracleText); text != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	return strings.TrimSpace(b.String())
}
