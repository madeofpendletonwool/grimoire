package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// The relaxed prose search (MAD-311): the ranked OR fallback that keeps a
// conversational question ("where is the Black Sun headquarters?") grounding
// when the strict AND match starves. Authorization must be identical — the
// leak test enumerates SearchProseRelaxed like any other scoped read, and
// these tests pin the behaviours it can't aim at directly.

func TestSearchProseRelaxedFindsWhatStrictMisses(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// A conversational question whose tokens never co-occur in one row:
	// the strict AND finds nothing, the relaxed OR ranks the charter fact up.
	q := "Where is the Duke's holding charter kept?"
	strict, err := s.SearchProse(ctx, ScopeParty, cid, q, 20)
	if err != nil {
		t.Fatalf("strict: %v", err)
	}
	if len(strict) != 0 {
		t.Fatalf("fixture expects the strict AND to miss: %+v", strict)
	}
	relaxed, err := s.SearchProseRelaxed(ctx, ScopeParty, cid, q, 20)
	if err != nil {
		t.Fatalf("relaxed: %v", err)
	}
	found := false
	for _, h := range relaxed {
		if h.Kind == "fact" && h.RefID == fx.FactMinesOwned {
			found = true
		}
	}
	if !found {
		t.Fatalf("relaxed search must rank the party-known charter fact up: %+v", relaxed)
	}
}

func TestSearchProseRelaxedNeverLeaks(t *testing.T) {
	s, fx, kx := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// Plant the adversarial grant on the proposal, as the leak test does:
	// an awareness row on the proposed vampire fact must not surface it
	// under the wider match either.
	if _, err := s.SetAwareness(ctx, cid, campaign.PartyKnower, kx.FactDukeVampireID, StanceKnows, 1, "", ""); err != nil {
		t.Fatal(err)
	}

	for _, scope := range []Scope{ScopeParty, ScopeCharacter(fx.Thalia), ScopeNPC(fx.Elara)} {
		for _, q := range []string{
			"Where is the Black Sun headquarters?",
			"silver key crypt",
			"vampire duke mirrors",
		} {
			hits, err := s.SearchProseRelaxed(ctx, scope, cid, q, 20)
			if err != nil && !errors.Is(err, ErrEmptyQuery) {
				t.Fatalf("%s relaxed %q: %v", scope, q, err)
			}
			for _, h := range hits {
				if h.Visibility == campaign.VisibilitySecret || h.Confidence == campaign.ConfidenceProposed {
					t.Fatalf("LEAK: relaxed %q at %s surfaced %+v", q, scope, h)
				}
				if h.RefID == fx.FactKeyOpensCrypt || h.RefID == kx.FactDukeVampireID {
					t.Fatalf("LEAK: relaxed %q at %s surfaced a withheld fact", q, scope)
				}
			}
		}
	}

	// The DM's relaxed search may see the secret — the wide store's DM scope.
	dm, err := s.SearchProseRelaxed(ctx, ScopeDM, cid, "silver key crypt", 20)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, h := range dm {
		if h.RefID == fx.FactKeyOpensCrypt {
			seen = true
		}
	}
	if !seen {
		t.Fatal("fixture integrity: the DM's relaxed search cannot see the secret; the leak assertions above are vacuous")
	}
}

// TestPlayerViewSearchRelaxed pins the narrow view's fallback through the
// PlayerView interface — the path the campaign chat's player groundings ride.
func TestPlayerViewSearchRelaxed(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID
	view, err := s.PlayerViewOf(ScopeParty)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := view.SearchProseRelaxed(ctx, cid, "Who holds the Eastern Mines charter?", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Kind == "fact" && h.RefID == fx.FactMinesOwned {
			found = true
		}
		if h.Visibility == campaign.VisibilitySecret {
			t.Fatalf("LEAK: player view relaxed search surfaced a secret: %+v", h)
		}
	}
	if !found {
		t.Fatalf("player view relaxed search found nothing: %+v", hits)
	}
}

// TestProseTokensAreStopwordFilteredAndPossessiveSplit pins the tokenizer:
// questions are conversational, and "Duke's" must prefix-match "Duke".
func TestProseTokensAreStopwordFilteredAndPossessiveSplit(t *testing.T) {
	got := proseTokens("Where is the Duke's headquarters?")
	want := []string{"Duke", "headquarters"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokens = %v, want %v", got, want)
		}
	}
	if len(proseTokens("the of and")) != 0 {
		t.Fatal("a stopword-only query must tokenize to nothing")
	}
}
