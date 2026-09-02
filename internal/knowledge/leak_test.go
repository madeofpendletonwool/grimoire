package knowledge

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/*
The leak test (MAD-304's hard gate, ADR 6 layer 2).

It enumerates every exported retrieval function on *campaign.Store and
*knowledge.Store by reflection — selection is structural, not a hand-written
list: any exported method that takes a Scope and is not obviously a mutation
counts. A retrieval function added later that forgets its scope filter fails
this test rather than shipping.

Each enumerated function is called with a non-DM scope (party, a character,
an npc) over the seeded campaign, with argument values aimed at the fixture's
planted rows — the secret Silver Key fact, the proposed vampire fact, the
party-known mines fact — and every returned value is walked for anything
fact-shaped. On top of the seed, plantAdversarial adds the two states the
write path permits and a lazier filter would leak: awareness granted on the
proposed vampire fact, and a retcon that supersedes the mines fact after the
party learned it. The rules:

  - No secret-visibility fact and no proposed fact may appear in any result
    at any non-DM scope. (The seed grants no non-DM knower a secret, so a
    surfaced secret here is always a leak.)
  - No retconned fact may appear at any non-DM scope either — superseded
    history is not current truth, learned or not.
  - No proposed fact may appear at the DM scope either, through the knowledge
    package — proposals are invisible to every retrieval path (ADR 3).
  - The campaign package's own retrieval must refuse non-DM scopes outright
    with ErrScope; its reads are the DM's raw store.

Calls that return an error are fine — ErrNotFound for an ungranted fact is
the architecture working. Only successful calls carrying rows can fail.
*/

// mutationPrefixes name exported Store methods that write rather than read;
// everything else carrying a Scope is presumed retrieval and enumerated.
var mutationPrefixes = []string{
	"Create", "Add", "Delete", "Remove", "Update", "Set", "Record",
	"Supersede", "Register", "ResolveContradiction", "Transition", "Link",
	"Seed", "PlayerViewOf", "New",
}

func isMutation(name string) bool {
	for _, p := range mutationPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

const maxCombos = 400

// candidateArgs builds the argument sets for one method: the context, the
// scope, the campaign id in its position, then a bounded product of fixture
// values aimed at the planted secret, proposal and their subjects. rumorIDs
// are the leak fixture's planted rumours, aimed the same way.
func candidateArgs(m reflect.Method, scope Scope, cid string, fx *campaign.Fixture, kx *KnowledgeFixture, rumorIDs []string) [][]any {
	ft := m.Type
	if ft.NumIn() < 3 || ft.NumIn() > 6 {
		return nil
	}
	// param 0 receiver, 1 ctx, 2 scope, 3+ the rest. By convention every
	// scoped retrieval in both stores takes the campaign id immediately
	// after the scope, so position 3 is always the campaign id and the
	// aimed candidate values start at the first parameter after it.
	if ft.In(3).Kind() != reflect.String {
		return nil
	}
	strPositions := []any{
		fx.Duke, fx.FactKeyOpensCrypt, kx.FactDukeVampireID, fx.Mira,
		fx.Elara, fx.EventAmbush, fx.QuestID, fx.ContradictionID,
		campaign.PartyKnower, "silver key", "npc", fx.Key,
		fx.FactMinesOwned,
	}
	strPositions = append(strPositions, anySlice(rumorIDs)...)
	positions := [][]any{{cid}}
	for i := 4; i < ft.NumIn(); i++ {
		in := ft.In(i)
		switch {
		case in.Kind() == reflect.String:
			positions = append(positions, strPositions)
		case in == reflect.TypeOf(FactFilter{}):
			positions = append(positions, []any{FactFilter{}, FactFilter{SubjectEntity: fx.Duke}, FactFilter{Stance: StanceKnows}})
		case in == reflect.TypeOf(campaign.FactFilter{}):
			positions = append(positions, []any{campaign.FactFilter{}, campaign.FactFilter{SubjectEntity: fx.Duke}})
		case in == reflect.TypeOf(RumorFilter{}):
			positions = append(positions, []any{
				RumorFilter{},
				RumorFilter{About: fx.Duke},
				RumorFilter{Status: campaign.RumorStatusCirculating},
			})
		case in.Kind() == reflect.Int || in.Kind() == reflect.Int64:
			positions = append(positions, []any{20})
		case in.Kind() == reflect.Float64:
			positions = append(positions, []any{1.0})
		default:
			return nil // a param shape the test does not know how to aim
		}
		if len(positions) > 0 && len(positions[len(positions)-1]) > 1 && len(positions) > 3 {
			return nil // too many varying positions to aim reliably
		}
	}
	total := 1
	for _, p := range positions {
		total *= len(p)
	}
	if total == 0 || total > maxCombos {
		return nil
	}
	var out [][]any
	var build func(i int, acc []any)
	build = func(i int, acc []any) {
		if i == len(positions) {
			full := append([]any{}, acc...)
			out = append(out, full)
			return
		}
		for _, v := range positions[i] {
			build(i+1, append(acc, v))
		}
	}
	build(0, nil)
	return out
}

// scanFacts walks a returned value for anything fact-shaped — a campaign.Fact
// or a ProseHit — and reports visibility/confidence pairs.
func scanFacts(v reflect.Value, depth int, found *[]struct{ vis, conf string }) {
	if depth > 4 {
		return
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			scanFacts(v.Elem(), depth+1, found)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			scanFacts(v.Index(i), depth+1, found)
		}
	case reflect.Struct:
		t := v.Type()
		if t.Name() == "Fact" || t.Name() == "ProseHit" {
			vis, conf := "", ""
			for i := 0; i < t.NumField(); i++ {
				switch t.Field(i).Name {
				case "Visibility":
					vis = v.Field(i).String()
				case "Confidence":
					conf = v.Field(i).String()
				}
			}
			*found = append(*found, struct{ vis, conf string }{vis, conf})
			return
		}
		for i := 0; i < t.NumField(); i++ {
			scanFacts(v.Field(i), depth+1, found)
		}
	}
}

// scanRumors walks a returned value for anything rumour-shaped — a
// campaign.Rumor — and reports what a leak would carry: the truth value,
// the fact link, the DM-only marker, the id.
func scanRumors(v reflect.Value, depth int, found *[]rumorSighting) {
	if depth > 4 {
		return
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			scanRumors(v.Elem(), depth+1, found)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			scanRumors(v.Index(i), depth+1, found)
		}
	case reflect.Struct:
		t := v.Type()
		if t.Name() == "Rumor" {
			var r rumorSighting
			for i := 0; i < t.NumField(); i++ {
				switch t.Field(i).Name {
				case "ID":
					r.id = v.Field(i).String()
				case "Truth":
					r.truth = v.Field(i).String()
				case "FactID":
					r.factID = v.Field(i).String()
				case "DMOnly":
					r.dmOnly = v.Field(i).Bool()
				}
			}
			*found = append(*found, r)
			return
		}
		for i := 0; i < t.NumField(); i++ {
			scanRumors(v.Field(i), depth+1, found)
		}
	}
}

type rumorSighting struct {
	id, truth, factID string
	dmOnly            bool
}

// plantAdversarial adds the two states a filter regression would leak: an
// awareness row granted on the proposed vampire fact (the write path allows
// it — one DM write or a Stage-3 extraction grant away), and a retcon that
// supersedes the mines fact after the party already knows it. Both must stay
// invisible at every non-DM scope; the reflection sweep below is what asserts
// it, single-fetch calls included.
func plantAdversarial(t *testing.T, s *Store, cs *campaign.Store, fx *campaign.Fixture, kx *KnowledgeFixture) {
	t.Helper()
	ctx := context.Background()
	cid := fx.Campaign.ID
	if _, err := s.SetAwareness(ctx, cid, campaign.PartyKnower, kx.FactDukeVampireID, StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("plant awareness on proposed fact: %v", err)
	}
	replacement, err := cs.CreateFact(ctx, cid, fx.Duke, "owns", fx.Mines, "",
		"The Duke holds the Eastern Mines through a steward.",
		campaign.ConfidenceCanon, campaign.VisibilityPublic, "keeper",
		[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored, Quote: "the steward signs the ledgers"}})
	if err != nil {
		t.Fatalf("plant replacement fact: %v", err)
	}
	if err := cs.SupersedeFact(ctx, cid, fx.FactMinesOwned, replacement.ID); err != nil {
		t.Fatalf("supersede the party-known mines fact: %v", err)
	}
}

// plantRumors adds the mill states a lazy filter would leak: a true
// rumour attesting the secret key fact, a false one naming the fact it
// contradicts, a fact-less one, and a DM-only rumour. Returns the ids so
// the argument aiming can reach every planted row.
func plantRumors(t *testing.T, s *Store, fx *campaign.Fixture) []string {
	t.Helper()
	ctx := context.Background()
	cid := fx.Campaign.ID
	ids := make([]string, 0, 4)
	plant := func(in RumorInput) string {
		r, err := s.CreateRumor(ctx, cid, in)
		if err != nil {
			t.Fatalf("plant rumor: %v", err)
		}
		ids = append(ids, r.ID)
		return r.ID
	}
	plant(RumorInput{
		Statement: "They say the silver key opens the crypt under the monastery.",
		Truth:     campaign.RumorTruthTrue, AboutEntity: fx.Monastery, FactID: fx.FactKeyOpensCrypt,
		Spread: campaign.RumorSpreadRegional,
	})
	plant(RumorInput{
		Statement: "The steward was seen buying silver by the pound.",
		Truth:     campaign.RumorTruthFalse, AboutEntity: fx.Duke, FactID: fx.FactMinesOwned,
	})
	plant(RumorInput{
		Statement: "The miller's second son came back wrong.",
		Truth:     campaign.RumorTruthFalse, AboutEntity: fx.Blackwater,
	})
	plant(RumorInput{
		Statement: "The thing in the trees pays the reeve in old coin.",
		Truth:     campaign.RumorTruthTrue, AboutEntity: fx.Blackwater, DMOnly: true,
	})
	return ids
}

func TestNoLeakAcrossScopes(t *testing.T) {
	s, fx, kx := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	plantAdversarial(t, s, cs, fx, kx)
	rumorIDs := plantRumors(t, s, fx)
	dmOnlyRumorID := rumorIDs[len(rumorIDs)-1]

	nonDM := []Scope{ScopeParty, ScopeCharacter(fx.Thalia), ScopeNPC(fx.Elara)}
	scopeType := reflect.TypeOf(ScopeDM)

	stores := []struct {
		name  string
		value any
	}{
		{"campaign.Store", cs},
		{"knowledge.Store", s},
	}

	for _, st := range stores {
		tv := reflect.TypeOf(st.value)
		vv := reflect.ValueOf(st.value)
		scanned := 0
		rumorsScanned := 0
		for i := 0; i < tv.NumMethod(); i++ {
			m := tv.Method(i)
			if !m.IsExported() || isMutation(m.Name) {
				continue
			}
			ft := m.Type
			// receiver + ctx + scope minimum
			if ft.NumIn() < 3 || ft.In(2) != scopeType {
				continue
			}
			scopes := nonDM
			if st.name == "knowledge.Store" {
				scopes = append(scopes, ScopeDM) // DM: proposals must still be invisible
			}
			for _, scope := range scopes {
				for _, args := range candidateArgs(m, scope, cid, fx, kx, rumorIDs) {
					callArgs := []reflect.Value{vv, reflect.ValueOf(ctx), reflect.ValueOf(scope)}
					for _, a := range args {
						callArgs = append(callArgs, reflect.ValueOf(a))
					}
					if len(callArgs) != ft.NumIn() {
						continue
					}
					results := m.Func.Call(callArgs)
					errVal, _ := results[len(results)-1].Interface().(error)
					var found []struct{ vis, conf string }
					var rumFound []rumorSighting
					for _, r := range results[:len(results)-1] {
						scanFacts(r, 0, &found)
						scanRumors(r, 0, &rumFound)
					}
					if errVal != nil {
						// An errored call cannot leak — and for the campaign
						// store at non-DM scope it must BE an error.
						if st.name == "campaign.Store" && !scope.IsDM() && (len(found) > 0 || len(rumFound) > 0) {
							t.Fatalf("%s.%s(%s): rows despite error", st.name, m.Name, scope)
						}
						continue
					}
					for _, r := range rumFound {
						if !scope.IsDM() {
							if r.truth != "" {
								t.Fatalf("LEAK: %s.%s at %s returned a rumour carrying truth=%q",
									st.name, m.Name, scope, r.truth)
							}
							if r.factID != "" {
								t.Fatalf("LEAK: %s.%s at %s returned a rumour carrying fact_id=%q",
									st.name, m.Name, scope, r.factID)
							}
							if r.dmOnly {
								t.Fatalf("LEAK: %s.%s at %s returned a DM-only rumour (%s)",
									st.name, m.Name, scope, r.id)
							}
						}
					}
					rumorsScanned += len(rumFound)
					if st.name == "campaign.Store" && !scope.IsDM() {
						if len(found) > 0 {
							t.Fatalf("LEAK: campaign.Store.%s succeeded at %s and returned fact-shaped rows; campaign retrieval is DM-only",
								m.Name, scope)
						}
						continue
					}
					for _, f := range found {
						if !scope.IsDM() && f.vis == campaign.VisibilitySecret {
							t.Fatalf("LEAK: %s.%s at %s returned a secret-visibility fact", st.name, m.Name, scope)
						}
						if f.conf == campaign.ConfidenceProposed {
							t.Fatalf("LEAK: %s.%s at %s returned a proposed fact (invisible to every retrieval path)",
								st.name, m.Name, scope)
						}
						if !scope.IsDM() && f.conf == campaign.ConfidenceRetconned {
							t.Fatalf("LEAK: %s.%s at %s returned a retconned fact (superseded history is not current truth)",
								st.name, m.Name, scope)
						}
					}
					scanned += len(found)
				}
			}
		}
		if st.name == "knowledge.Store" && scanned == 0 {
			t.Fatalf("leak test scanned no rows; the reflection enumeration is broken")
		}
		if st.name == "knowledge.Store" && rumorsScanned == 0 {
			t.Fatalf("leak test scanned no rumour rows; the reflection enumeration is broken")
		}
		t.Logf("%s: leak test exercised %d fact-shaped and %d rumour-shaped results", st.name, scanned, rumorsScanned)
	}

	// Prove the fixture has teeth: the secret and the proposal exist, the
	// DM can read the secret through the wide store, and only the proposal
	// and the retconned row are withheld from the DM.
	dmFacts, err := s.Facts(ctx, ScopeDM, cid, FactFilter{})
	if err != nil {
		t.Fatal(err)
	}
	secretSeen := false
	for _, f := range dmFacts {
		if f.ID == fx.FactKeyOpensCrypt {
			secretSeen = true
		}
		if f.ID == kx.FactDukeVampireID {
			t.Fatal("fixture integrity: proposed fact readable at dm scope; the leak assertions above are vacuous")
		}
		if f.ID == fx.FactMinesOwned {
			t.Fatal("fixture integrity: retconned fact readable at dm scope; the leak assertions above are vacuous")
		}
	}
	if !secretSeen {
		t.Fatal("fixture integrity: secret fact missing from dm view; the leak assertions above are vacuous")
	}
	// The rumour fixture has teeth the same way: the DM reads truth, a
	// player scope reads none, and the DM-only row stays DM-side.
	dmRumors, err := s.Rumors(ctx, ScopeDM, cid, RumorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	dmTruth := 0
	for _, r := range dmRumors {
		if r.Truth != "" {
			dmTruth++
		}
	}
	if dmTruth != len(rumorIDs) {
		t.Fatalf("fixture integrity: dm sees truth on %d of %d rumours; the leak assertions above are vacuous",
			dmTruth, len(rumorIDs))
	}
	playerRumors, err := s.Rumors(ctx, ScopeParty, cid, RumorFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range playerRumors {
		if r.ID == dmOnlyRumorID {
			t.Fatal("fixture integrity: dm-only rumour readable at party scope; the leak assertions above are vacuous")
		}
		if r.Truth != "" || r.FactID != "" {
			t.Fatal("fixture integrity: party rumour read carries truth/fact; the leak assertions above are vacuous")
		}
	}
	if len(playerRumors) != len(rumorIDs)-1 {
		t.Fatalf("fixture integrity: party sees %d rumours, want %d", len(playerRumors), len(rumorIDs)-1)
	}
}

// TestLeakTestDetectsAMissingFilter is the test that keeps the leak test
// honest: it removes the scope filters from a scratch store's query path in
// the most total way — an unscoped SELECT — over a fixture that carries the
// secret, the proposal and the retcon, and asserts the scan catches all
// three. This proves scanFacts and the assertions fire on exactly the shapes
// a real leak produces.
func TestLeakTestDetectsAMissingFilter(t *testing.T) {
	s, fx, kx := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	plantAdversarial(t, s, cs, fx, kx)

	// Simulate the bug: a retrieval that forgot its scope filter and loaded
	// everything.
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+factCols+` FROM facts WHERE campaign_id = ?`, cid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var leaked []campaign.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			t.Fatal(err)
		}
		leaked = append(leaked, *f)
	}

	// The same scan the leak test applies must flag it.
	var found []struct{ vis, conf string }
	scanFacts(reflect.ValueOf(leaked), 0, &found)
	caughtSecret, caughtProposed, caughtRetconned := false, false, false
	for _, f := range found {
		if f.vis == campaign.VisibilitySecret {
			caughtSecret = true
		}
		if f.conf == campaign.ConfidenceProposed {
			caughtProposed = true
		}
		if f.conf == campaign.ConfidenceRetconned {
			caughtRetconned = true
		}
	}
	if !caughtSecret || !caughtProposed || !caughtRetconned {
		t.Fatalf("the leak scan must catch an unscoped read: secret=%v proposed=%v retconned=%v",
			caughtSecret, caughtProposed, caughtRetconned)
	}

	// The rumour shape too: an unscoped mill read carries truth, fact
	// links and the DM-only marker, and the rumour scan must catch all
	// three the way a real regression would serve them.
	plantRumors(t, s, fx)
	rows, err = s.db.QueryContext(ctx,
		`SELECT `+rumorColsDM+` FROM rumors WHERE campaign_id = ?`, cid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var leakedRumors []campaign.Rumor
	for rows.Next() {
		r, err := scanRumor(rows)
		if err != nil {
			t.Fatal(err)
		}
		leakedRumors = append(leakedRumors, *r)
	}
	var rumFound []rumorSighting
	scanRumors(reflect.ValueOf(leakedRumors), 0, &rumFound)
	caughtTruth, caughtFact, caughtDMOnly := 0, 0, 0
	for _, r := range rumFound {
		if r.truth != "" {
			caughtTruth++
		}
		if r.factID != "" {
			caughtFact++
		}
		if r.dmOnly {
			caughtDMOnly++
		}
	}
	if caughtTruth < 4 || caughtFact < 2 || caughtDMOnly != 1 {
		t.Fatalf("the rumour scan must catch an unscoped mill read: truth=%d fact=%d dm_only=%d",
			caughtTruth, caughtFact, caughtDMOnly)
	}
}
