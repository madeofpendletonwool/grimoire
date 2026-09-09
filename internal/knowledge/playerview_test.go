package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// The PlayerView's guarantee is two-layered: player-facing code cannot reach
// the wide store (a compile property — the handlers hold the interface), and
// every method on the interface drops secret-visibility and proposed rows
// even when an awareness grant exists.
func TestPlayerViewExcludesGrantedSecrets(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// The party discovers the crypt secret. On the wide store the grant
	// flows — this is the party's own knowledge.
	if _, err := s.SetAwareness(ctx, cid, campaign.PartyKnower, fx.FactKeyOpensCrypt, StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	wide, err := s.Facts(ctx, ScopeParty, cid, FactFilter{})
	if err != nil {
		t.Fatalf("wide party facts: %v", err)
	}
	found := false
	for _, f := range wide {
		if f.ID == fx.FactKeyOpensCrypt {
			found = true
		}
	}
	if !found {
		t.Fatal("wide store must serve the granted secret to its knower")
	}

	// Through the player view it does not exist at all.
	pv, err := s.PlayerViewOf(ScopeParty)
	if err != nil {
		t.Fatalf("player view: %v", err)
	}
	player, err := pv.Facts(ctx, cid, FactFilter{})
	if err != nil {
		t.Fatalf("player facts: %v", err)
	}
	for _, f := range player {
		if f.ID == fx.FactKeyOpensCrypt || f.Visibility == campaign.VisibilitySecret {
			t.Fatalf("player view served a secret: %+v", f)
		}
	}
	if _, err := pv.Fact(ctx, cid, fx.FactKeyOpensCrypt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("granted secret by id through the player view must be missing: %v", err)
	}

	// Prose search through the view stays clean of the secret's text too.
	hits, err := pv.SearchProse(ctx, cid, "crypt beneath greyfall", 20)
	if err != nil {
		t.Fatalf("player prose: %v", err)
	}
	for _, h := range hits {
		if h.RefID == fx.FactKeyOpensCrypt || h.Visibility == campaign.VisibilitySecret {
			t.Fatalf("player prose served a secret: %+v", h)
		}
	}
}

func TestPlayerViewSummarizeDropsGrantedSecrets(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	if _, err := s.SetAwareness(ctx, cid, campaign.PartyKnower, fx.FactKeyOpensCrypt, StanceKnows, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	pv, err := s.PlayerViewOf(ScopeCharacter(fx.Mira))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := pv.Summarize(ctx, cid, fx.Key)
	if err != nil {
		t.Fatalf("player summarize: %v", err)
	}
	for _, bucket := range [][]campaign.Fact{sum.Confirmed, sum.Suspected, sum.Incorrect, sum.Unknown} {
		for _, f := range bucket {
			if f.Visibility == campaign.VisibilitySecret {
				t.Fatalf("player summary served a secret: %+v", f)
			}
		}
	}
}

func TestPlayerViewDiscoveriesDropSecretTrails(t *testing.T) {
	s, fx, kx := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	pv, err := s.PlayerViewOf(ScopeParty)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := pv.Discoveries(ctx, cid, "")
	if err != nil {
		t.Fatalf("player discoveries: %v", err)
	}
	if len(ds) != 1 || ds[0].ID != kx.DiscoveryCharterID {
		t.Fatalf("party view sees only the charter discovery: %+v", ds)
	}
	_ = fx
}

func TestPlayerViewBindsOnlyPlayerScopes(t *testing.T) {
	s, fx, _ := seeded(t)
	if _, err := s.PlayerViewOf(ScopeDM); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dm player view: %v", err)
	}
	if _, err := s.PlayerViewOf(ScopeNPC(fx.Elara)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("npc player view: %v", err)
	}
	if _, err := s.PlayerViewOf(ScopeParty); err != nil {
		t.Fatalf("party player view: %v", err)
	}
	if _, err := s.PlayerViewOf(ScopeCharacter(fx.Thalia)); err != nil {
		t.Fatalf("character player view: %v", err)
	}
}

// A draft handout is invisible to a player by construction (MAD-490):
// the view's own query refuses every status but published, so neither the
// list nor the single read can carry it — the handler has no good
// behaviour to forget. Retiring takes a published handout back off the
// party's list the same way.
func TestPlayerViewHandoutsCannotSeeDrafts(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	draft, err := s.CreateHandout(ctx, cid, HandoutInput{
		Kind: campaign.HandoutKindHandout, Title: "The unfinished letter",
		Body: "Do not show the party this yet.",
	})
	if err != nil {
		t.Fatal(err)
	}
	published, err := s.CreateHandout(ctx, cid, HandoutInput{
		Kind: campaign.HandoutKindHandout, Title: "The charter", Body: "Be it known…",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHandoutStatus(ctx, cid, published.ID, campaign.HandoutStatusPublished); err != nil {
		t.Fatal(err)
	}

	pv, err := s.PlayerViewOf(ScopeParty)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pv.Handouts(ctx, cid)
	if err != nil {
		t.Fatalf("player handouts: %v", err)
	}
	if len(list) != 1 || list[0].ID != published.ID {
		t.Fatalf("player handout list = %+v; want the published charter alone", list)
	}
	if _, err := pv.Handout(ctx, cid, draft.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("draft by id through the view must be missing: %v", err)
	}
	got, err := pv.Handout(ctx, cid, published.ID)
	if err != nil || got.Title != "The charter" {
		t.Fatalf("published by id through the view: %v %+v", err, got)
	}

	// Retire: off the party's list, kept for the DM's history.
	if _, err := s.SetHandoutStatus(ctx, cid, published.ID, campaign.HandoutStatusRetired); err != nil {
		t.Fatal(err)
	}
	after, err := pv.Handouts(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("retired handout still on the player list: %+v", after)
	}
	dmList, err := s.Handouts(ctx, ScopeDM, cid, HandoutFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dmList) != 2 {
		t.Fatalf("the dm keeps the history: %d rows, want 2", len(dmList))
	}
}
