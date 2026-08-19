package deck

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
)

// fakeCards backs the engine + analyzer tests without SQLite.
type fakeCards map[string]*carddb.Card

func (f fakeCards) Candidates(ctx context.Context, allowedMask int, terms []string, exclude map[string]bool, limit int) ([]*carddb.Card, error) {
	var out []*carddb.Card
	for _, c := range f {
		if c.IsLand() || !c.CommanderLegal && false {
			continue
		}
		if allowedMask != 0 && !c.IdentityAllowed(allowedMask) {
			continue
		}
		if exclude != nil && exclude[strings.ToLower(c.Name)] {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (f fakeCards) lookup(name string) (*carddb.Card, bool) {
	c, ok := f[strings.ToLower(name)]
	return c, ok
}

func fixtureCards() fakeCards {
	return fakeCards{
		"kaalia of the vast": {Name: "Kaalia of the Vast", ManaCost: "{1}{R}{W}{B}", ManaValue: 4, TypeLine: "Legendary Creature — Angel Cleric", ColorIdentity: "BRW", CommanderLegal: true, EDHRECRank: 122},
		"sol ring":           {Name: "Sol Ring", ManaCost: "{1}", ManaValue: 1, TypeLine: "Artifact", OracleText: "{T}: Add {C}{C}.", ColorIdentity: "", EDHRECRank: 1},
		"counterspell":       {Name: "Counterspell", ManaCost: "{U}{U}", ManaValue: 2, TypeLine: "Instant", OracleText: "Counter target spell.", ColorIdentity: "U", EDHRECRank: 15},
		"rampant growth":     {Name: "Rampant Growth", ManaCost: "{1}{G}", ManaValue: 2, TypeLine: "Sorcery", OracleText: "Search your library for a basic land card, put it onto the battlefield.", ColorIdentity: "G", EDHRECRank: 120},
		"brainstorm":         {Name: "Brainstorm", ManaCost: "{U}", ManaValue: 1, TypeLine: "Instant", OracleText: "Draw a card.", ColorIdentity: "U", EDHRECRank: 40},
		"blood crypt":        {Name: "Blood Crypt", TypeLine: "Land — Mountain Swamp", ColorIdentity: "BR"},
		"plains":             {Name: "Plains", TypeLine: "Basic Land — Plains", ColorIdentity: "W", EDHRECRank: 30},
		"anger":              {Name: "Anger", ManaCost: "{3}{R}", ManaValue: 4, TypeLine: "Creature — Incarnation", ColorIdentity: "R", EDHRECSaltiness: 0.8},
		"armageddon":         {Name: "Armageddon", ManaCost: "{3}{W}", ManaValue: 4, TypeLine: "Sorcery", ColorIdentity: "W", EDHRECSaltiness: 2.9, EDHRECRank: 500},
		"lightning bolt":     {Name: "Lightning Bolt", ManaCost: "{R}", ManaValue: 1, TypeLine: "Instant", OracleText: "Lightning Bolt deals 3 damage to any target.", ColorIdentity: "R", EDHRECRank: 60},
	}
}

func TestParseDecklist(t *testing.T) {
	text := `Commander: Kaalia of the Vast
# a comment
2x Sol Ring
1 Blood Crypt
Lightning Bolt x3
Rampant Growth x 2
Counterspell

Sideboard
1 Anger`
	entries := ParseDecklist(text)
	want := []Entry{
		{Name: "Kaalia of the Vast", Count: 1, Board: "commander"},
		{Name: "Sol Ring", Count: 2, Board: "main"},
		{Name: "Blood Crypt", Count: 1, Board: "main"},
		{Name: "Lightning Bolt", Count: 3, Board: "main"},
		{Name: "Rampant Growth", Count: 2, Board: "main"},
		{Name: "Counterspell", Count: 1, Board: "main"},
		{Name: "Anger", Count: 1, Board: "sideboard"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %+v\nwant %+v", entries, want)
	}
}

func TestParseDecklistCommanderHeader(t *testing.T) {
	entries := ParseDecklist("Commander\nKaalia of the Vast\nSol Ring")
	if entries[0].Board != "commander" || entries[0].Name != "Kaalia of the Vast" {
		t.Fatalf("commander line = %+v", entries[0])
	}
	if entries[1].Board != "commander" {
		t.Fatalf("line after commander header = %+v", entries[1])
	}
}

func TestFormatRoundTrip(t *testing.T) {
	text := "Commander: Kaalia of the Vast\n\n2 Sol Ring\n1 Plains\n\nSideboard\n1 Anger"
	entries := ParseDecklist(text)
	out := FormatDecklist(entries)
	if !strings.Contains(out, "Commander: Kaalia of the Vast") || !strings.Contains(out, "2 Sol Ring") || !strings.Contains(out, "Sideboard") {
		t.Fatalf("format = %q", out)
	}
}

func TestManaCost(t *testing.T) {
	generic, pips := ManaCost("{2}{W}{W}{B}")
	if generic != 2 || pips["W"] != 2 || pips["B"] != 1 {
		t.Fatalf("kaalia cost = %d %v", generic, pips)
	}
	_, pips = ManaCost("{W/U}")
	if pips["W"] != 0.5 || pips["U"] != 0.5 {
		t.Fatalf("hybrid = %v", pips)
	}
	_, pips = ManaCost("{2/W}")
	if pips["W"] != 0.5 {
		t.Fatalf("2/W = %v", pips)
	}
	generic, pips = ManaCost("{X}{R}")
	if generic != 1 || pips["R"] != 1 {
		t.Fatalf("X cost = %d %v", generic, pips)
	}
	generic, _ = ManaCost("{10}{R}")
	if generic != 10 {
		t.Fatalf("two-digit generic = %d", generic)
	}
	// Split cards join cleanly.
	generic, pips = ManaCost("{1}{R} // {1}{U}")
	if generic != 2 || pips["R"] != 1 || pips["U"] != 1 {
		t.Fatalf("split = %d %v", generic, pips)
	}
}

func TestLandCounts(t *testing.T) {
	// Kaalia-ish: heavy W, some R, less B.
	needs := ManaNeeds{Pips: map[string]float64{"W": 30, "R": 20, "B": 10}, TotalPips: 60}
	counts := LandCounts(needs, 36)
	if got := counts["W"] + counts["R"] + counts["B"]; got != 36 {
		t.Fatalf("land counts sum = %d, want 36 (%v)", got, counts)
	}
	if counts["W"] <= counts["R"] || counts["R"] <= counts["B"] {
		t.Fatalf("order wrong: %v", counts)
	}
	if counts["W"] != 18 || counts["R"] != 12 || counts["B"] != 6 {
		t.Fatalf("exact split = %v", counts)
	}

	// No demand: nothing suggested.
	if got := LandCounts(ManaNeeds{Pips: map[string]float64{}}, 36); len(got) != 0 {
		t.Fatalf("empty demand = %v", got)
	}
}

func TestSuggestLandCount(t *testing.T) {
	cards := fixtureCards()
	entries := []Entry{
		{Name: "Kaalia of the Vast", Board: "commander"},
		{Name: "Sol Ring", Count: 1},
		{Name: "Counterspell", Count: 1},
		{Name: "Brainstorm", Count: 1},
		{Name: "Anger", Count: 1},
		{Name: "Blood Crypt", Count: 1},
	}
	n := SuggestLandCount(entries, cards.lookup)
	if n < 32 || n > 42 {
		t.Fatalf("land count = %d, want within 32-42", n)
	}
}

func TestAnalyze(t *testing.T) {
	cards := fixtureCards()
	entries := []Entry{
		{Name: "Kaalia of the Vast", Board: "commander"},
		{Name: "Counterspell", Count: 1}, // illegal in BRW!
		{Name: "Sol Ring", Count: 1},
		{Name: "Armageddon", Count: 1},
		{Name: "Anger", Count: 1},
		{Name: "Blood Crypt", Count: 1},
		{Name: "Plains", Count: 36},
	}
	a := Analyze("Kaalia of the Vast", entries, cards.lookup)

	if len(a.IdentityBad) != 1 || a.IdentityBad[0].Name != "Counterspell" {
		t.Fatalf("identity violations = %+v", a.IdentityBad)
	}
	if a.Identity != "BRW" {
		t.Fatalf("identity = %q", a.Identity)
	}
	if a.TotalMain != 41 {
		t.Fatalf("total = %d", a.TotalMain)
	}
	if a.Lands != 37 {
		t.Fatalf("lands = %d", a.Lands)
	}
	if len(a.Saltiest) == 0 || a.Saltiest[0].Name != "Armageddon" {
		t.Fatalf("saltiest = %+v", a.Saltiest)
	}
	found := false
	for _, w := range a.Warnings {
		if strings.Contains(w, "short of 99") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v", a.Warnings)
	}

	// Unresolved names warn instead of vanishing.
	a = Analyze("Kaalia of the Vast", []Entry{{Name: "Fake Card"}}, cards.lookup)
	if len(a.Warnings) == 0 || !strings.Contains(a.Warnings[0], "Fake Card") {
		t.Fatalf("unresolved warning = %v", a.Warnings)
	}
}

func TestDiffEntries(t *testing.T) {
	before := []Entry{{Name: "Sol Ring", Count: 1}, {Name: "Anger", Count: 2}}
	after := []Entry{{Name: "Sol Ring", Count: 2}, {Name: "Lightning Bolt", Count: 1}}
	d := DiffEntries(before, after)
	if len(d.Added) != 2 || d.Added[0].Name != "Lightning Bolt" || d.Added[1].Name != "Sol Ring" || d.Added[1].Count != 1 {
		t.Fatalf("added = %+v", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0].Name != "Anger" || d.Removed[0].Count != 2 {
		t.Fatalf("removed = %+v", d.Removed)
	}
}

func TestEngineCandidates(t *testing.T) {
	cards := fixtureCards()
	e := NewEngine(cards)
	cands := e.BuildCandidates(context.Background(), carddb.MaskForColors("BRW"), "angels and dragons aggressive", nil, 10)
	if len(cands) == 0 {
		t.Fatal("no candidates")
	}
	for _, c := range cands {
		if c.Name == "Counterspell" || c.Name == "Rampant Growth" || c.Name == "Brainstorm" {
			t.Errorf("out-of-identity candidate: %s", c.Name)
		}
		if c.IsLand() {
			t.Errorf("land candidate: %s", c.Name)
		}
	}
	// Sol Ring (rank 1) should top the staples ranking.
	if cands[0].Name != "Sol Ring" {
		t.Fatalf("top candidate = %s, want Sol Ring", cands[0].Name)
	}
	if len(cands[0].Reasons) == 0 {
		t.Fatal("candidate carries no reasons")
	}

	// Exclusions respected.
	cands = e.BuildCandidates(context.Background(), carddb.MaskForColors("BRW"), "", []string{"Sol Ring"}, 10)
	for _, c := range cands {
		if c.Name == "Sol Ring" {
			t.Fatal("excluded candidate returned")
		}
	}
}

func TestBoostByStats(t *testing.T) {
	cands := []Candidate{
		{Card: &carddb.Card{Name: "Sol Ring", EDHRECRank: 1}},
		{Card: &carddb.Card{Name: "Anger", EDHRECRank: 500}},
	}
	// Anger has +0.6 synergy with this commander: 1000/510 + 300 beats 1000/11.
	BoostByStats(cands, map[string]SynergyStat{"anger": {Synergy: 0.6}})
	if cands[0].Name != "Anger" {
		t.Fatalf("boost order = %s, %s", cands[0].Name, cands[1].Name)
	}
	if len(cands[0].Reasons) == 0 {
		t.Fatal("boost added no reason")
	}
}

func TestRoleOf(t *testing.T) {
	cards := fixtureCards()
	if got := RoleOf(cards["sol ring"]); got != "ramp" {
		t.Errorf("sol ring role = %s", got)
	}
	if got := RoleOf(cards["counterspell"]); got != "interaction" {
		t.Errorf("counterspell role = %s", got)
	}
	if got := RoleOf(cards["brainstorm"]); got != "draw" {
		t.Errorf("brainstorm role = %s", got)
	}
}

func TestThemeTerms(t *testing.T) {
	terms := ThemeTerms("I want an aggressive angels and dragons deck, mid budget")
	for _, bad := range []string{"want", "deck", "budget", "the", "and"} {
		for _, tok := range terms {
			if tok == bad {
				t.Errorf("stopword %q kept in %v", bad, terms)
			}
		}
	}
	if !contains(terms, "aggressive") || !contains(terms, "angels") {
		t.Fatalf("terms = %v", terms)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
