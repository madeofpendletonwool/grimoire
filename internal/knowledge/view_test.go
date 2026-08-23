package knowledge

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

func factIDs(fs []campaign.Fact) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.ID
	}
	return out
}

func TestScopedFacts(t *testing.T) {
	s, fx, kx := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// DM: every live fact — the four seeded ones (two now contested) and
	// nothing proposed.
	dm, err := s.Facts(ctx, ScopeDM, cid, FactFilter{})
	if err != nil {
		t.Fatalf("dm facts: %v", err)
	}
	ids := factIDs(dm)
	for _, want := range []string{fx.FactMinesOwned, fx.FactKeyOpensCrypt, fx.FactDukeVisited, fx.FactDukeNever} {
		if !slices.Contains(ids, want) {
			t.Fatalf("dm must read fact %s; has %v", want, ids)
		}
	}
	if slices.Contains(ids, kx.FactDukeVampireID) {
		t.Fatal("proposed facts are invisible to every scope, dm included")
	}

	// Party: exactly what the party's awareness grants — the mines the
	// charter gave them and the account they believe is false. Not the
	// secret, not the proposal, not the fact only Thalia suspects.
	party, err := s.Facts(ctx, ScopeParty, cid, FactFilter{})
	if err != nil {
		t.Fatalf("party facts: %v", err)
	}
	if got := factIDs(party); !slices.Equal(got, []string{fx.FactMinesOwned, fx.FactDukeNever}) &&
		!slices.Equal(got, []string{fx.FactDukeNever, fx.FactMinesOwned}) {
		t.Fatalf("party facts wrong: %v", got)
	}

	// Character scope folds in the party's rows plus the character's own:
	// Thalia additionally holds her suspicion of the ledger visit.
	thalia, err := s.Facts(ctx, ScopeCharacter(fx.Thalia), cid, FactFilter{})
	if err != nil {
		t.Fatalf("thalia facts: %v", err)
	}
	for _, want := range []string{fx.FactMinesOwned, fx.FactDukeNever, fx.FactDukeVisited} {
		if !slices.Contains(factIDs(thalia), want) {
			t.Fatalf("thalia must read %s: %v", want, factIDs(thalia))
		}
	}

	// Stance filtering works at non-DM scopes and is refused at the DM's.
	susp, err := s.Facts(ctx, ScopeCharacter(fx.Thalia), cid, FactFilter{Stance: StanceSuspects})
	if err != nil || len(susp) != 1 || susp[0].ID != fx.FactDukeVisited {
		t.Fatalf("stance filter: %v", susp)
	}
	if _, err := s.Facts(ctx, ScopeDM, cid, FactFilter{Stance: StanceKnows}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dm stance filter must be refused: %v", err)
	}

	// One fact by id, scoped: a secret the party is unaware of is simply
	// not there, and so is a proposal.
	if _, err := s.Fact(ctx, ScopeParty, cid, fx.FactKeyOpensCrypt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("party must not read the silver key secret: %v", err)
	}
	if _, err := s.Fact(ctx, ScopeDM, cid, kx.FactDukeVampireID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("proposed fact must be invisible even by id at dm scope: %v", err)
	}
	if f, err := s.Fact(ctx, ScopeParty, cid, fx.FactMinesOwned); err != nil || f.ID != fx.FactMinesOwned {
		t.Fatalf("party reads granted fact by id: %v %v", f, err)
	}
}

func TestGrantedSecretFlowsToItsKnowerOnly(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// Grant the crypt secret to Elara. She is in on it; the party is not.
	if _, err := s.SetAwareness(ctx, cid, fx.Elara, fx.FactKeyOpensCrypt, StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("grant elara: %v", err)
	}

	// Elara's scope reads her secret — that is what makes NPC simulation
	// possible: the Duke's confidante can be asked what she knows.
	elara, err := s.Facts(ctx, ScopeNPC(fx.Elara), cid, FactFilter{})
	if err != nil {
		t.Fatalf("elara facts: %v", err)
	}
	if !slices.Contains(factIDs(elara), fx.FactKeyOpensCrypt) {
		t.Fatalf("elara must read her granted secret: %v", factIDs(elara))
	}

	// The party still cannot, and neither can another character.
	for _, scope := range []Scope{ScopeParty, ScopeCharacter(fx.Thalia), ScopeNPC(fx.Tom)} {
		fs, err := s.Facts(ctx, scope, cid, FactFilter{})
		if err != nil {
			t.Fatalf("%s facts: %v", scope, err)
		}
		if slices.Contains(factIDs(fs), fx.FactKeyOpensCrypt) {
			t.Fatalf("%s must not read elara's secret", scope)
		}
	}
}

func TestScopedEntities(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// The party's world: the four pcs (co-participants of the ambush), the
	// Duke and the mines (carried by the granted charter fact), and
	// Blackwater (where the ambush happened). Nobody else.
	party, err := s.Entities(ctx, ScopeParty, cid, "")
	if err != nil {
		t.Fatalf("party entities: %v", err)
	}
	got := map[string]bool{}
	for _, e := range party {
		got[e.ID] = true
		if e.Payload != nil && len(e.Payload) != 0 {
			t.Fatalf("non-DM entities must drop payloads: %s", e.Name)
		}
	}
	for _, want := range []string{fx.Thalia, fx.Bran, fx.Keth, fx.Mira, fx.Duke, fx.Mines, fx.Blackwater} {
		if !got[want] {
			t.Fatalf("party must see %s; sees %v", want, got)
		}
	}
	for _, mustNot := range []string{fx.Elara, fx.Tom, fx.Venn, fx.Cult, fx.Monastery, fx.Key, fx.Verdant} {
		if got[mustNot] {
			t.Fatalf("party must not see %s", mustNot)
		}
	}

	// The DM sees everything, payloads attached.
	dm, err := s.Entities(ctx, ScopeDM, cid, "")
	if err != nil || len(dm) != 14 {
		t.Fatalf("dm sees all 14 entities: %d %v", len(dm), err)
	}
	for _, e := range dm {
		if e.Kind == campaign.KindPC && e.Payload["class"] == nil {
			t.Fatalf("dm entity payloads must be decoded: %+v", e)
		}
	}

	// By id, scoped.
	if _, err := s.Entity(ctx, ScopeParty, cid, fx.Elara); !errors.Is(err, ErrNotFound) {
		t.Fatalf("party must not read elara: %v", err)
	}
	if e, err := s.Entity(ctx, ScopeParty, cid, fx.Duke); err != nil || e.ID != fx.Duke {
		t.Fatalf("party reads the duke: %v %v", e, err)
	}

	// Cross-campaign scope ids read as missing.
	if _, err := s.Entities(ctx, ScopeCharacter("ghost"), cid, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ghost scope entity: %v", err)
	}
	if _, err := s.Entities(ctx, ScopeCharacter(fx.Duke), cid, ""); !errors.Is(err, ErrScope) {
		t.Fatalf("character scope on an npc must be refused: %v", err)
	}
}

func TestScopedTimeline(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// The party witnessed the ambush (all four pcs participated). The
	// flashback march into the hills has no participants, so no perspective
	// but the DM's holds it — it happened off-stage.
	party, err := s.Timeline(ctx, ScopeParty, cid)
	if err != nil {
		t.Fatalf("party timeline: %v", err)
	}
	if len(party) != 1 || party[0].ID != fx.EventAmbush {
		t.Fatalf("party timeline must be the ambush only: %+v", party)
	}
	if len(party[0].Participants) != 4 {
		t.Fatalf("participants attach: %d", len(party[0].Participants))
	}
	if party[0].Links != nil {
		t.Fatal("event links are DM material and must not attach at party scope")
	}

	dm, err := s.Timeline(ctx, ScopeDM, cid)
	if err != nil || len(dm) != 2 {
		t.Fatalf("dm timeline has both events: %d %v", len(dm), err)
	}
	// The one seeded link (survivors -> ambush) appears on both events,
	// the same behavior campaign.GetEvent has always had.
	var links int
	for _, e := range dm {
		links += len(e.Links)
	}
	if links != 2 {
		t.Fatalf("dm sees the causal link on both ends: %d", links)
	}
}

func TestScopedRelationships(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// The party's edges: only Mines located_in Blackwater — the open
	// structural edge between two entities it can see. Tom's identical edge
	// stays hidden (they have not met Tom); Elara's secretly_controls is
	// justified-by-provenance, which grants nothing; the cult's worship is
	// asserted DM structure.
	party, err := s.Relationships(ctx, ScopeParty, cid)
	if err != nil {
		t.Fatalf("party relationships: %v", err)
	}
	if len(party) != 1 || party[0].FromEntity != fx.Mines || party[0].RelType != "located_in" || party[0].ToEntity != fx.Blackwater {
		t.Fatalf("party relationships wrong: %+v", party)
	}

	dm, err := s.Relationships(ctx, ScopeDM, cid)
	if err != nil || len(dm) != 6 {
		t.Fatalf("dm reads every edge: %d %v", len(dm), err)
	}
}

func TestSearchProse(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// DM: prose search reaches entities, facts and events.
	dm, err := s.SearchProse(ctx, ScopeDM, cid, "silver key", 20)
	if err != nil {
		t.Fatalf("dm prose: %v", err)
	}
	kinds := map[string]string{}
	for _, h := range dm {
		kinds[h.Kind] = h.RefID
	}
	if kinds["entity"] != fx.Key {
		t.Fatalf("entity hit: %+v", kinds)
	}
	if kinds["fact"] != fx.FactKeyOpensCrypt {
		t.Fatalf("dm fact hit should be the (secret) key fact: %+v", kinds)
	}

	// Party: the key is not in their world — neither the entity nor the
	// secret statement may surface, not even as a snippet.
	party, err := s.SearchProse(ctx, ScopeParty, cid, "silver key", 20)
	if err != nil {
		t.Fatalf("party prose: %v", err)
	}
	for _, h := range party {
		if h.RefID == fx.Key || h.RefID == fx.FactKeyOpensCrypt {
			t.Fatalf("silver key leaked to party scope: %+v", h)
		}
		if h.Visibility == campaign.VisibilitySecret || h.Confidence == campaign.ConfidenceProposed {
			t.Fatalf("secret/proposed hit at party scope: %+v", h)
		}
	}

	// The charter is in their world: the mines statement surfaces.
	party, err = s.SearchProse(ctx, ScopeParty, cid, "holding charter", 20)
	if err != nil {
		t.Fatalf("party prose: %v", err)
	}
	found := false
	for _, h := range party {
		if h.Kind == "fact" && h.RefID == fx.FactMinesOwned {
			found = true
		}
	}
	if !found {
		t.Fatalf("party must find the charter fact: %+v", party)
	}

	// Proposed prose never surfaces, at any scope.
	for _, scope := range []Scope{ScopeDM, ScopeParty, ScopeCharacter(fx.Mira)} {
		hits, err := s.SearchProse(ctx, scope, cid, "vampire", 20)
		if err != nil {
			t.Fatalf("%s prose: %v", scope, err)
		}
		for _, h := range hits {
			if h.RefID == fx.Duke {
				t.Fatalf("proposed vampire fact surfaced at %s: %+v", scope, h)
			}
		}
	}

	// The empty query is a clean error, not a full scan.
	if _, err := s.SearchProse(ctx, ScopeDM, cid, "  ", 20); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("empty query: %v", err)
	}
}

func TestProseIndexTracksWrites(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}

	// A new fact is searchable the moment it lands.
	f, err := cs.CreateFact(ctx, cid, fx.Tom, "waters", "", "a dark ale called nightcap",
		"Tom keeps a dark ale called nightcap.", campaign.ConfidenceCanon, campaign.VisibilityPublic, "keeper",
		[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored}})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchProse(ctx, ScopeDM, cid, "nightcap", 20)
	if err != nil || len(hits) == 0 {
		t.Fatalf("new fact must be searchable: %v %v", hits, err)
	}

	// Reword it: the old text stops matching, the new one starts.
	if _, err := s.db.ExecContext(ctx, `UPDATE facts SET statement = 'a pale ale called daybreak' WHERE id = ?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if hits, err = s.SearchProse(ctx, ScopeDM, cid, "nightcap", 20); err != nil || len(hits) != 0 {
		t.Fatalf("old statement must leave the index: %v %v", hits, err)
	}
	if hits, err = s.SearchProse(ctx, ScopeDM, cid, "daybreak", 20); err != nil || len(hits) == 0 {
		t.Fatalf("new statement must enter the index: %v %v", hits, err)
	}

	// Soft-deleting an entity removes its prose.
	if err := cs.DeleteEntity(ctx, cid, fx.Tom); err != nil {
		t.Fatal(err)
	}
	if hits, err = s.SearchProse(ctx, ScopeDM, cid, "waystone", 20); err != nil || len(hits) != 0 {
		t.Fatalf("deleted entity prose must leave the index: %v %v", hits, err)
	}
}
