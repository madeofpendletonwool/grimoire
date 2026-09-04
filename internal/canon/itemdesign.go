package canon

/*
The item designer's placement (MAD-383, stage 5.2 of MAD-317).

Designing is a model-free operation — the designer's honesty is
comparative, not generative, and it lives entirely in internal/items.
What canon owns is the one moment a designed item touches the campaign
graph: placing it as an `item` entity through the proposal batch, behind
the review gate like every other generated write. Designing without
placing writes nothing.
*/

import (
	"context"
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// ItemPlaceInput is one placement request: a saved homebrew item, staged
// into the campaign as an item entity.
type ItemPlaceInput struct {
	CampaignID string
	HomebrewID string
	Name       string
	// Summary is the entity's one-line description — the notes' first
	// sentence, or the rarity-and-type line when there are no notes.
	Summary string
	// Rarity is the DM's own label, for the batch's prompt record. It is
	// evidence of what the DM claims, not of what the server computed —
	// the server computed nothing.
	Rarity    string
	Notes     string
	CreatedBy string
}

// PlaceItem stages the item's placement: one item entity, behind the
// review gate like every other generated write. Nothing is written to the
// graph here; the entity exists when the batch is accepted.
func (s *Store) PlaceItem(ctx context.Context, in ItemPlaceInput) (*Batch, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: a placement needs the item's name", ErrInvalid)
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		summary = fmt.Sprintf("A homebrew %s.", strings.TrimSpace(in.Rarity+" magic item"))
	}
	prompt := fmt.Sprintf("the homebrew item %s placed as an item", name)
	if in.Rarity != "" {
		prompt += " (rarity: " + in.Rarity + ", by the DM's own label)"
	}
	if in.Notes != "" {
		prompt += " — notes: " + in.Notes
	}
	return s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceItem,
		Prompt: prompt, CreatedBy: in.CreatedBy,
		Items: []BatchItemInput{{
			ID: "item", Kind: "entity", Subject: name, Summary: summary,
			Payload: map[string]any{
				"local_id": "item", "homebrew_id": in.HomebrewID,
				"kind": campaign.KindItem, "name": name, "summary": summary,
			},
		}},
	})
}
