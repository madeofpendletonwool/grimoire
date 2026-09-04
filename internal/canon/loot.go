package canon

/*
The loot generator's placement (MAD-384, stage 5.3 of MAD-317).

Generating a hoard is a pure read — coin and items rolled from the DMG's
tables and the catalog, nothing persisted. What canon owns is the one
moment a hoard touches the campaign graph: the generated items become
`item` entities and the hand-out becomes an event, staged together as one
proposal batch behind the review gate like every other generated write.
Nothing is written until the DM approves the batch (ADR 3). Rerolling,
overruling a concentration warning, or walking away writes nothing at all.
*/

import (
	"context"
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// HoardPlaceItem is one generated item line to stage. Narrative lines —
// the act's own item — are not placed: they are already campaign
// entities, and the hoard only carried them.
type HoardPlaceItem struct {
	// Key is the hoard line's stable key ("item-1"), the batch item's
	// identity among its siblings.
	Key string
	// EntityID is set only for the narrative line's existing entity, which
	// rides the event's summary and is never staged twice.
	EntityID string
	Slug     string
	Name     string
	Summary  string
	Doc      string
	Rarity   string
	Homebrew bool
	// Reason is why the generator chose the item — recorded on the
	// proposal so the review queue shows the same arithmetic the hoard
	// did. Never empty: an unexplained placement is a bug, not a default.
	Reason string
}

// HoardPlaceInput is one hoard placement: the item lines and the hand-out
// that dates them.
type HoardPlaceInput struct {
	CampaignID string
	// Summary is the hand-out event's summary — the hoard as the party
	// found it, in the DM's words or the generator's.
	Summary string
	Items   []HoardPlaceItem
	// ParticipantIDs are the pc entity ids who were there.
	ParticipantIDs []string
	CreatedBy      string
}

// PlaceHoard stages the hoard: one proposed entity per generated item,
// then one proposed event (the hand-out) that depends on all of them, in
// a single batch. Every reference resolves inside the batch, so an accept
// cannot half-land. Nothing is written here.
func (s *Store) PlaceHoard(ctx context.Context, in HoardPlaceInput) (*Batch, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Summary) == "" {
		return nil, fmt.Errorf("%w: a hoard placement needs the hand-out's summary", ErrInvalid)
	}
	if len(in.Items) == 0 {
		return nil, fmt.Errorf("%w: a hoard placement needs at least one item", ErrInvalid)
	}
	seenPC := map[string]bool{}
	var participants []map[string]any
	for _, id := range in.ParticipantIDs {
		id = strings.TrimSpace(id)
		if id == "" || seenPC[id] {
			continue
		}
		seenPC[id] = true
		participants = append(participants, map[string]any{"entity": id, "role": "present"})
	}

	var items []BatchItemInput
	var staged []string // keys of the entity items the event depends on
	for _, line := range in.Items {
		if line.EntityID != "" {
			continue // the act's own item: already an entity, never staged twice
		}
		if strings.TrimSpace(line.Name) == "" {
			return nil, fmt.Errorf("%w: hoard item %q has no name", ErrInvalid, line.Key)
		}
		if strings.TrimSpace(line.Reason) == "" {
			return nil, fmt.Errorf("%w: hoard item %q carries no reason — an unexplained placement is not stageable", ErrInvalid, line.Key)
		}
		summary := strings.TrimSpace(line.Summary)
		if summary == "" {
			summary = hoardItemSummary(line)
		}
		entityPayload := map[string]any{}
		if line.Slug != "" {
			entityPayload["slug"] = line.Slug
		}
		if line.Doc != "" {
			entityPayload["doc"] = line.Doc
		}
		if line.Rarity != "" {
			entityPayload["rarity"] = line.Rarity
		}
		if line.Homebrew {
			entityPayload["homebrew"] = true
		}
		items = append(items, BatchItemInput{
			ID:      line.Key,
			Kind:    "entity",
			Subject: line.Name,
			Summary: summary,
			Payload: map[string]any{
				"local_id": line.Key, "kind": campaign.KindItem,
				"name": line.Name, "summary": summary,
				// The generator's reason rides the entity payload, so the
				// "why" survives the batch into the graph.
				"chosen_reason": line.Reason,
				"payload":       entityPayload,
			},
		})
		staged = append(staged, line.Key)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: every item in the hoard is already a campaign entity — nothing to place", ErrInvalid)
	}
	items = append(items, BatchItemInput{
		ID:      "handout",
		Kind:    "event",
		Subject: in.Summary,
		Payload: map[string]any{
			"local_id":     "handout",
			"summary":      in.Summary,
			"participants": participants,
		},
		DependsOn: staged,
	})

	prompt := fmt.Sprintf("the hoard %s — %d generated item(s) behind one hand-out", in.Summary, len(staged))
	return s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceLoot,
		Prompt: prompt, CreatedBy: in.CreatedBy,
		Items: items,
	})
}

// hoardItemSummary is the one line a placed item carries when the DM did
// not write one: rarity, homebrew-ness, and the slug that names it.
func hoardItemSummary(line HoardPlaceItem) string {
	parts := []string{"A"}
	if line.Homebrew {
		parts = append(parts, "homebrew")
	}
	if line.Rarity != "" {
		parts = append(parts, line.Rarity)
	}
	parts = append(parts, "magic item")
	if line.Slug != "" {
		parts = append(parts, "("+line.Slug+")")
	}
	joined := strings.Join(parts, " ")
	return joined + "."
}
