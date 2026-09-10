package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
)

// anchorStore indexes a corpus shaped like the real one: a glossary entry that
// names the rule defining a term, and a keyword rule whose body is the term.
func anchorStore(t *testing.T) *index.Store {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "anchors.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	recs := []data.Record{
		{Corpus: data.CorpusMTG, Number: "702.6", Title: "Keyword Abilities", Body: "Equip"},
		{Corpus: data.CorpusMTG, Number: "702.6a", Title: "Keyword Abilities", Body: "Equip is an activated ability of Equipment cards."},
		{Corpus: data.CorpusMTG, Number: "301.5", Title: "Artifacts", Body: `Some artifacts have the subtype "Equipment."`},
		{Corpus: data.CorpusMTG, Number: "104.1", Title: "Ending the Game", Body: "A game ends immediately when a player wins."},
		{Corpus: data.CorpusMTG, Number: "", Title: "Equipment", Body: `An artifact subtype. See rule 301, "Artifacts," and rule 702.6, "Equip."`},
	}
	ds := &data.Dataset{
		Records: recs,
		Meta:    map[data.Corpus]data.CorpusMeta{data.CorpusMTG: {Name: "Magic", Version: "t", SourceURL: "x", RecordCount: len(recs)}},
	}
	if err := store.Index(context.Background(), ds); err != nil {
		t.Fatalf("index: %v", err)
	}
	return store
}

// Keyword search finds the rules that share a question's words; it does not
// find the rules that define the question's terms, and those usually decide
// it. A question that says "Equipment" must reach 702.6 even though the word
// "equip" never appears in it.
func TestAnchorsSeedTheDefiningRules(t *testing.T) {
	s := &Server{store: anchorStore(t)}
	got := s.anchorNumbers(context.Background(), data.CorpusMTG,
		"If I move my Equipment, what happens?", nil, nil)

	want := map[string]bool{"301": true, "702.6": true}
	for _, n := range got {
		delete(want, n)
	}
	if len(want) > 0 {
		t.Errorf("anchors = %v, missing %v", got, want)
	}
}

// A card's own type line names mechanics the asker never typed. Resolving them
// is what lets "does this thing stop the spell?" reach the rules for the
// Equipment it is holding.
func TestAnchorsReadCardOracleText(t *testing.T) {
	s := &Server{store: anchorStore(t)}
	cards := []llm.CardDoc{{
		Name:       "Whispersilk Cloak",
		TypeLine:   "Artifact — Equipment",
		OracleText: "Equipped creature can't be blocked and has shroud.",
	}}
	got := s.anchorNumbers(context.Background(), data.CorpusMTG, "Does this stop it?", cards, nil)

	found := false
	for _, n := range got {
		if n == "702.6" {
			found = true
		}
	}
	if !found {
		t.Errorf("anchors = %v, want the equip rule from the card's type line", got)
	}
}

// A rule number the asker typed is the strongest anchor there is.
func TestAnchorsHonourAnExplicitRuleNumber(t *testing.T) {
	s := &Server{store: anchorStore(t)}
	got := s.anchorNumbers(context.Background(), data.CorpusMTG, "what does 702.6a actually say?", nil, nil)
	if len(got) == 0 || got[0] != "702.6a" {
		t.Errorf("anchors = %v, want 702.6a first", got)
	}
}

// A citation the corpus no longer holds must never reach the model as
// grounding: a glossary entry can outlive the rule it points at.
func TestAnchorSeedsDropRulesTheIndexLacks(t *testing.T) {
	s := &Server{store: anchorStore(t)}
	seeds := s.anchorSeeds(context.Background(), data.CorpusMTG, []string{"702.6", "999.9"}, nil)
	if len(seeds) != 1 || seeds[0].Number != "702.6" {
		t.Fatalf("seeds = %v, want only 702.6", numbersOfResults(seeds))
	}
	if !seeds[0].Anchor {
		t.Error("an anchored seed must be marked as one, or expansion cannot weight it")
	}
}

func numbersOfResults(rs []index.Result) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Number)
	}
	return out
}
