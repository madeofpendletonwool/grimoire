package knowledge

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

func TestSummarizeFourBuckets(t *testing.T) {
	s, fx, kx := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// A public fact the party has no awareness of: unknown to them.
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	festival, err := cs.CreateFact(ctx, cid, fx.Duke, "attended", "", "the harvest festival",
		"The Duke attended the harvest festival.", campaign.ConfidenceCanon, campaign.VisibilityPublic, "keeper",
		[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored}})
	if err != nil {
		t.Fatal(err)
	}
	// A secret fact nobody but the DM holds: the vampire truth, canon now.
	coffin, err := cs.CreateFact(ctx, cid, fx.Duke, "keeps", "", "a locked coffin",
		"The Duke keeps a locked coffin beneath the keep.", campaign.ConfidenceCanon, campaign.VisibilitySecret, "keeper",
		[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored}})
	if err != nil {
		t.Fatal(err)
	}

	// What the party knows about the Duke:
	//   confirmed  — he owns the mines (the charter)
	//   suspected  — (party holds no suspects on the Duke; Thalia's is her own)
	//   incorrect  — the "never travels" account, which they believe is false
	//   unknown    — the festival attendance (public, ungranted)
	// and never the coffin, and never the proposal.
	sum, err := s.Summarize(ctx, ScopeParty, cid, fx.Duke)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(sum.Confirmed) != 1 || sum.Confirmed[0].ID != fx.FactMinesOwned {
		t.Fatalf("confirmed: %+v", sum.Confirmed)
	}
	if len(sum.Suspected) != 0 {
		t.Fatalf("party suspects nothing about the duke: %+v", sum.Suspected)
	}
	if len(sum.Incorrect) != 1 || sum.Incorrect[0].ID != fx.FactDukeNever {
		t.Fatalf("incorrect: %+v", sum.Incorrect)
	}
	// Unknown at the party scope: the festival (public, ungranted) and the
	// ledger visit — Thalia suspects it, but the party as a whole has taken
	// no position, so for the party it is unknown.
	if len(sum.Unknown) != 2 {
		t.Fatalf("party unknown should be the festival and the visit: %+v", sum.Unknown)
	}
	for _, f := range sum.Unknown {
		if f.ID != festival.ID && f.ID != fx.FactDukeVisited {
			t.Fatalf("unexpected party unknown: %+v", f)
		}
	}
	for _, bucket := range [][]campaign.Fact{sum.Confirmed, sum.Suspected, sum.Incorrect, sum.Unknown} {
		for _, f := range bucket {
			if f.Visibility == campaign.VisibilitySecret {
				t.Fatalf("secret surfaced in party summary: %+v", f)
			}
			if f.Confidence == campaign.ConfidenceProposed {
				t.Fatalf("proposal surfaced in party summary: %+v", f)
			}
		}
	}

	// Thalia's summary folds her own suspicion in.
	sum, err = s.Summarize(ctx, ScopeCharacter(fx.Thalia), cid, fx.Duke)
	if err != nil {
		t.Fatalf("summarize thalia: %v", err)
	}
	if len(sum.Suspected) != 1 || sum.Suspected[0].ID != fx.FactDukeVisited {
		t.Fatalf("thalia suspects the visit: %+v", sum.Suspected)
	}
	// And her unknowns shrink accordingly: the visit left her unknown set.
	for _, f := range sum.Unknown {
		if f.ID == fx.FactDukeVisited {
			t.Fatalf("a suspected fact is not unknown: %+v", f)
		}
	}

	// The DM's complement: everything the party has not met, secrets
	// included. Summarize plus this is the full panel.
	unknown, err := s.Unknown(ctx, ScopeDM, cid, campaign.PartyKnower, fx.Duke)
	if err != nil {
		t.Fatalf("unknown: %v", err)
	}
	ids := factIDs(unknown)
	if !slices.Contains(ids, coffin.ID) {
		t.Fatalf("dm unknown must include the coffin secret: %v", ids)
	}
	if !slices.Contains(ids, festival.ID) || !slices.Contains(ids, fx.FactDukeVisited) {
		t.Fatalf("dm unknown must include public ungranted facts too: %v", ids)
	}
	if slices.Contains(ids, fx.FactMinesOwned) || slices.Contains(ids, fx.FactDukeNever) {
		t.Fatalf("granted facts are not unknown: %v", ids)
	}
	if slices.Contains(ids, kx.FactDukeVampireID) {
		t.Fatal("proposed facts are never unknown — they are not retrievable at all")
	}

	// Unknown refuses non-DM scopes: the party cannot be shown the secrets
	// it does not know.
	if _, err := s.Unknown(ctx, ScopeParty, cid, campaign.PartyKnower, fx.Duke); !errors.Is(err, ErrScope) {
		t.Fatalf("party Unknown must be ErrScope: %v", err)
	}
	// The DM scope does not summarize: the DM has the campaign, not beliefs.
	if _, err := s.Summarize(ctx, ScopeDM, cid, fx.Duke); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dm Summarize must be refused: %v", err)
	}
}

func TestSummarizeMultiStanceGrantResolvesToStrongest(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// The party knows the mines; Mira also knows them. Resolution must
	// collapse to one confirmed row, not duplicate.
	if _, err := s.SetAwareness(ctx, cid, fx.Mira, fx.FactMinesOwned, StanceKnows, 1, "", ""); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summarize(ctx, ScopeCharacter(fx.Mira), cid, fx.Duke)
	if err != nil {
		t.Fatalf("summarize mira: %v", err)
	}
	if len(sum.Confirmed) != 1 {
		t.Fatalf("multi-knower grant must collapse: %+v", sum.Confirmed)
	}
}
