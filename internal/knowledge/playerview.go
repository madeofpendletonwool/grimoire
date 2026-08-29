package knowledge

import (
	"context"
	"fmt"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// PlayerView is the store player-facing handlers are wired to (ADR 6): the
// same database as the DM's Grimoire, behind an interface with no method
// capable of returning a secret-visibility or proposed row at all.
//
// The guarantee is two-layered. Structurally, everything the portal can call
// is on this interface, and player-facing code never receives the wide
// *Store — adding a leaky method to a player handler is a compile error.
// Behaviorally, every implementation here applies the scope's authorization
// AND drops secret-visibility facts even when the party's awareness grants
// them: the portal renders the discovered, player-safe world, while the wide
// store's granted-secret flow serves the DM and NPC simulation. A discovered
// secret reaching a player surface is a product decision about visibility
// transitions, made deliberately in a later stage — not something this
// interface can do by accident.
//
// Proposed facts need no extra rule here: they are invisible to every
// retrieval path in the package, this interface included (ADR 3).
type PlayerView interface {
	// Facts and Fact return the party- or character-visible facts: granted
	// by awareness, never secret, never proposed.
	Facts(ctx context.Context, campaignID string, filter FactFilter) ([]campaign.Fact, error)
	Fact(ctx context.Context, campaignID, factID string) (*campaign.Fact, error)
	// Entities and Entity return the entities the scope has met, with
	// payloads dropped (they are DM structure).
	Entities(ctx context.Context, campaignID, kind string) ([]campaign.Entity, error)
	Entity(ctx context.Context, campaignID, id string) (*campaign.Entity, error)
	// Timeline returns the events the scope witnessed, in play order,
	// participants attached, causal links omitted.
	Timeline(ctx context.Context, campaignID string) ([]campaign.Event, error)
	// Relationships returns the edges the scope may read.
	Relationships(ctx context.Context, campaignID string) ([]campaign.Relationship, error)
	// SearchProse runs full-text search over campaign prose the scope may
	// read.
	SearchProse(ctx context.Context, campaignID, query string, limit int) ([]ProseHit, error)
	// SearchProseRelaxed is the ranked fallback when SearchProse's AND match
	// finds nothing: tokens OR-ed, best matches first, under the same
	// authorization.
	SearchProseRelaxed(ctx context.Context, campaignID, query string, limit int) ([]ProseHit, error)
	// Discoveries lists the scope's own discoveries of non-secret facts —
	// the journal's "how we learned it" view.
	Discoveries(ctx context.Context, campaignID, factID string) ([]Discovery, error)
	// Summarize is the four-bucket summary at this scope; its Unknown
	// bucket lists only public facts.
	Summarize(ctx context.Context, campaignID, subject string) (*Summary, error)
	// FactionFacade reads a faction's player-facing self-presentation —
	// the public face and the reputation from the payload's agent block,
	// and nothing else. PrivateTruth and the rest of the payload are DM
	// structure this interface cannot express.
	FactionFacade(ctx context.Context, campaignID, id string) (face, reputation string, err error)
	// QuestJournal is the player-visible quest read (MAD-369): public
	// quests only, the current state's label, the states already visited —
	// and never an unvisited branch, a state detail or an edge's requires.
	QuestJournal(ctx context.Context, campaignID string) ([]QuestJournalEntry, error)
}

// playerView is the PlayerView implementation: the wide store's scoped reads
// with playerStrict set, bound to one non-DM scope at construction.
type playerView struct {
	store *Store
	scope Scope
}

// PlayerViewOf binds a party or character scope to the narrow player store.
// NPC scopes are refused: NPC simulation is a DM feature and runs on the
// wide store, where granted secrets flow.
func (s *Store) PlayerViewOf(scope Scope) (PlayerView, error) {
	switch scope.Kind() {
	case campaign.ScopeKindParty, campaign.ScopeKindCharacter:
		return &playerView{store: s, scope: scope}, nil
	default:
		return nil, fmt.Errorf("%w: player view binds party or character scopes, not %s", ErrInvalid, scope)
	}
}

func (v *playerView) Facts(ctx context.Context, campaignID string, filter FactFilter) ([]campaign.Fact, error) {
	return v.store.facts(ctx, v.scope, campaignID, filter, true)
}

func (v *playerView) Fact(ctx context.Context, campaignID, factID string) (*campaign.Fact, error) {
	return v.store.fact(ctx, v.scope, campaignID, factID, true)
}

func (v *playerView) Entities(ctx context.Context, campaignID, kind string) ([]campaign.Entity, error) {
	return v.store.entities(ctx, v.scope, campaignID, kind, true)
}

func (v *playerView) Entity(ctx context.Context, campaignID, id string) (*campaign.Entity, error) {
	return v.store.entity(ctx, v.scope, campaignID, id, true)
}

func (v *playerView) Timeline(ctx context.Context, campaignID string) ([]campaign.Event, error) {
	return v.store.timeline(ctx, v.scope, campaignID, true)
}

func (v *playerView) Relationships(ctx context.Context, campaignID string) ([]campaign.Relationship, error) {
	return v.store.relationships(ctx, v.scope, campaignID, true)
}

func (v *playerView) SearchProse(ctx context.Context, campaignID, query string, limit int) ([]ProseHit, error) {
	return v.store.searchProse(ctx, v.scope, campaignID, query, limit, true, false)
}

func (v *playerView) SearchProseRelaxed(ctx context.Context, campaignID, query string, limit int) ([]ProseHit, error) {
	return v.store.searchProse(ctx, v.scope, campaignID, query, limit, true, true)
}

func (v *playerView) Discoveries(ctx context.Context, campaignID, factID string) ([]Discovery, error) {
	ds, err := v.store.Discoveries(ctx, v.scope, campaignID, factID)
	if err != nil {
		return nil, err
	}
	// Narrow to discoveries of non-secret facts: the trail behind a secret
	// the party has not accepted into their player-visible world is not
	// portal material, even when the awareness grant exists.
	out := ds[:0]
	for _, d := range ds {
		f, err := v.store.fact(ctx, v.scope, campaignID, d.FactID, true)
		if err != nil {
			continue // the strict view cannot read this fact; drop the trail to it
		}
		_ = f
		out = append(out, d)
	}
	return out, nil
}

func (v *playerView) Summarize(ctx context.Context, campaignID, subject string) (*Summary, error) {
	sum, err := v.store.Summarize(ctx, v.scope, campaignID, subject)
	if err != nil {
		return nil, err
	}
	// The wide Summarize's granted buckets can carry a granted secret (the
	// knower does hold it). The player view renders only the player-safe
	// world, so granted secrets move to the Unknown bucket's treatment:
	// removed, not shown — the party "does not know" it as far as any
	// player-facing surface can say.
	buckets := []*[]campaign.Fact{&sum.Confirmed, &sum.Suspected, &sum.Incorrect}
	for _, bucket := range buckets {
		kept := (*bucket)[:0]
		for _, f := range *bucket {
			if f.Visibility != campaign.VisibilitySecret {
				kept = append(kept, f)
			}
		}
		*bucket = kept
	}
	return sum, nil
}

func (v *playerView) FactionFacade(ctx context.Context, campaignID, id string) (string, string, error) {
	return v.store.FactionFacade(ctx, v.scope, campaignID, id)
}
