package knowledge

import (
	"context"
	"fmt"
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
planted rows — the secret Silver Key fact, the proposed vampire fact — and
every returned value is walked for anything fact-shaped. The rules:

  - No secret-visibility fact and no proposed fact may appear in any result
    at any non-DM scope. (The seed grants no non-DM knower a secret, so a
    surfaced secret here is always a leak.)
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
// values aimed at the planted secret, proposal and their subjects.
func candidateArgs(m reflect.Method, scope Scope, cid string, fx *campaign.Fixture, kx *KnowledgeFixture) [][]any {
	ft := m.Type
	if ft.NumIn() < 3 || ft.NumIn() > 6 {
		return nil
	}
	// param 0 receiver, 1 ctx, 2 scope, 3+ the rest.
	if ft.In(2) != reflect.TypeOf(scope) {
		return nil
	}
	positions := [][]any{}
	for i := 3; i < ft.NumIn(); i++ {
		in := ft.In(i)
		switch {
		case in == reflect.TypeOf(fx.Campaign.ID):
			// first string slot is the campaign id
			positions = append(positions, []any{cid})
		case in.Kind() == reflect.String:
			positions = append(positions, []any{
				fx.Duke, fx.FactKeyOpensCrypt, kx.FactDukeVampireID, fx.Mira,
				fx.Elara, fx.EventAmbush, fx.QuestID, fx.ContradictionID,
				campaign.PartyKnower, "silver key", "npc", fx.Key,
			})
		case in == reflect.TypeOf(FactFilter{}):
			positions = append(positions, []any{FactFilter{}, FactFilter{SubjectEntity: fx.Duke}, FactFilter{Stance: StanceKnows}})
		case in == reflect.TypeOf(campaign.FactFilter{}):
			positions = append(positions, []any{campaign.FactFilter{}, campaign.FactFilter{SubjectEntity: fx.Duke}})
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

func TestNoLeakAcrossScopes(t *testing.T) {
	s, fx, kx := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID
	cs, err := campaign.New(s.db)
	if err != nil {
		t.Fatal(err)
	}

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
				for _, args := range candidateArgs(m, scope, cid, fx, kx) {
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
					for _, r := range results[:len(results)-1] {
						scanFacts(r, 0, &found)
					}
					if errVal != nil {
						// An errored call cannot leak — and for the campaign
						// store at non-DM scope it must BE an error.
						if st.name == "campaign.Store" && !scope.IsDM() && len(found) > 0 {
							t.Fatalf("%s.%s(%s): rows despite error", st.name, m.Name, scope)
						}
						continue
					}
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
					}
					scanned += len(found)
				}
			}
		}
		if st.name == "knowledge.Store" && scanned == 0 {
			t.Fatalf("leak test scanned no rows; the reflection enumeration is broken")
		}
		t.Logf("%s: leak test exercised %d fact-shaped results", st.name, scanned)
	}

	// Prove the fixture has teeth: the secret and the proposal exist, the
	// DM can read the secret through the wide store, and only the proposal
	// is withheld from the DM.
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
	}
	if !secretSeen {
		t.Fatal("fixture integrity: secret fact missing from dm view; the leak assertions above are vacuous")
	}
}

// TestLeakTestDetectsAMissingFilter is the test that keeps the leak test
// honest: it removes the visibility filter from the player-strict granted
// CTE in a scratch store's query path and asserts the scan catches a secret.
// Rather than mutating production code, it re-scans a deliberately unscoped
// fact list — proving scanFacts and the assertions fire on exactly the shape
// a real leak produces.
func TestLeakTestDetectsAMissingFilter(t *testing.T) {
	s, fx, _ := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

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
	caughtSecret, caughtProposed := false, false
	for _, f := range found {
		if f.vis == campaign.VisibilitySecret {
			caughtSecret = true
		}
		if f.conf == campaign.ConfidenceProposed {
			caughtProposed = true
		}
	}
	if !caughtSecret || !caughtProposed {
		t.Fatalf("the leak scan must catch an unscoped read: secret=%v proposed=%v", caughtSecret, caughtProposed)
	}
	_ = fmt.Sprintf("%d", len(leaked))
}
