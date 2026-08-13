package resolver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/cards"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
)

// fakeLooker satisfies cards.Looker from a name→oracle map. Names not in the
// map return ErrNotFound so the resolver reports them as unresolved.
type fakeLooker map[string]*cards.Card

func (f fakeLooker) Lookup(_ context.Context, name string) (*cards.Card, error) {
	if c, ok := f[strings.ToLower(name)]; ok {
		return c, nil
	}
	return nil, cards.ErrNotFound
}

func mustStore(t *testing.T) *index.Store {
	t.Helper()
	s, err := index.Open(filepath.Join(t.TempDir(), "resolve.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// indexRules loads MTG numbered rules into a store so grounding has something
// real to retrieve and to pull wholesale by chapter.
func indexRules(t *testing.T, s *index.Store, rules []data.Record) {
	t.Helper()
	ds := &data.Dataset{Records: rules, Meta: map[data.Corpus]data.CorpusMeta{
		data.CorpusMTG: {Name: "Magic: The Gathering", Version: "t", SourceURL: "x", RecordCount: len(rules)},
	}}
	if err := s.Index(context.Background(), ds); err != nil {
		t.Fatalf("index: %v", err)
	}
}

func sampleRules() []data.Record {
	return []data.Record{
		{Corpus: data.CorpusMTG, Number: "117.1", Title: "Timing and Priority", Body: "A player may take an action when they have priority."},
		{Corpus: data.CorpusMTG, Number: "117.4", Title: "Timing and Priority", Body: "If all players pass in succession, the top object of the stack resolves."},
		{Corpus: data.CorpusMTG, Number: "603.2", Title: "Triggered Abilities", Body: "Whenever a game event matches a triggered ability's trigger event, the ability triggers."},
		{Corpus: data.CorpusMTG, Number: "603.3b", Title: "Triggered Abilities", Body: "If multiple abilities have triggered since the last time a player received priority, the active player puts their abilities on the stack first, then each other player in turn order."},
		{Corpus: data.CorpusMTG, Number: "613.1", Title: "Interaction of Continuous Effects", Body: "The values of an object's characteristics are determined by applying layers."},
		{Corpus: data.CorpusMTG, Number: "616.1", Title: "Interaction of Replacement/Prevention Effects", Body: "If an event would occur and one or more replacement effects apply, the affected player chooses one to apply."},
		{Corpus: data.CorpusMTG, Number: "702.2", Title: "Deathtouch", Body: "Deathtouch is a static ability."},
		{Corpus: data.CorpusMTG, Number: "704.5f", Title: "State-Based Actions", Body: "If a creature has toughness 0 or less, it is put into its owner's graveyard."},
	}
}

/* ---------- Parsing ---------- */

func TestParseBoard_ControllerAndState(t *testing.T) {
	b := ParseInput("Opp: Blood Artist # tapped\nMe: Figure of Destiny # +1/+1 counters: 2\n", "", "").Board
	if got := len(b.Permanents); got != 2 {
		t.Fatalf("permanents = %d, want 2", got)
	}
	first := b.Permanents[0]
	if first.Controller != "Opp" || first.Name != "Blood Artist" || !first.Tapped {
		t.Errorf("first = %+v", first)
	}
	second := b.Permanents[1]
	if second.Controller != "Me" || second.Name != "Figure of Destiny" || second.Tapped {
		t.Errorf("second = %+v", second)
	}
	if second.Counters == "" {
		t.Errorf("counters should be captured from note: %+v", second)
	}
}

func TestParseBoard_DefaultController(t *testing.T) {
	b := ParseInput("Lightning Bolt", "", "").Board
	if len(b.Permanents) != 1 || b.Permanents[0].Controller != "You" {
		t.Errorf("default controller = %+v", b.Permanents)
	}
}

func TestParseBoard_SkipsBlanksAndComments(t *testing.T) {
	b := ParseInput("\n# a comment\n// a note\nOpp: Blood Artist\n", "", "").Board
	if len(b.Permanents) != 1 {
		t.Errorf("permanents = %d, want 1 (blanks/comments skipped)", len(b.Permanents))
	}
}

func TestParseSequence_StripsNumberingAndController(t *testing.T) {
	s := ParseInput("", "1. Opp casts Wrath of God\n2) You activate Scavenging Ooze\n3. Opp: sacrifices a creature\n", "").Sequence
	if len(s.Steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(s.Steps))
	}
	// No colon: the actor stays in the prose, controller left empty.
	if s.Steps[0].Controller != "" || s.Steps[0].Text != "Opp casts Wrath of God" {
		t.Errorf("step 0 = %+v", s.Steps[0])
	}
	if s.Steps[1].Controller != "" || s.Steps[1].Text != "You activate Scavenging Ooze" {
		t.Errorf("step 1 = %+v", s.Steps[1])
	}
	// A colon prefix is honored when the user does write one.
	if s.Steps[2].Controller != "Opp" || s.Steps[2].Text != "sacrifices a creature" {
		t.Errorf("step 2 = %+v", s.Steps[2])
	}
}

func TestParseInput_NotePreserved(t *testing.T) {
	in := ParseInput("You: Blood Artist", "1. Opp casts Wrath of God", "It is the opponent's turn.")
	if in.Note != "It is the opponent's turn." {
		t.Errorf("note = %q", in.Note)
	}
}

/* ---------- Grounding ---------- */

func TestGround_ResolvesBoardAndSequenceCards(t *testing.T) {
	looker := fakeLooker{
		"blood artist": {Name: "Blood Artist", TypeLine: "Creature", OracleText: "Whenever Blood Artist or another creature dies, you may have target player lose 1 life."},
		"wrath of god": {Name: "Wrath of God", TypeLine: "Sorcery", OracleText: "Destroy all creatures. They can't be regenerated."},
	}
	in := ParseInput("You: Blood Artist", "1. Opp casts Wrath of God", "")
	g := Ground(context.Background(), Deps{Cards: looker}, in)
	names := cardNames(g.Cards)
	if !names["Blood Artist"] || !names["Wrath of God"] {
		t.Errorf("expected Blood Artist + Wrath of God resolved, got %v", names)
	}
}

func TestGround_ReportsUnresolved(t *testing.T) {
	looker := fakeLooker{"blood artist": {Name: "Blood Artist", OracleText: "x"}}
	in := ParseInput("You: Blood Artist\nYou: Fake Card Here", "1. Opp casts Wrath of God", "")
	g := Ground(context.Background(), Deps{Cards: looker}, in)
	if len(g.Unresolved) == 0 {
		t.Errorf("a multi-word miss must be reported unresolved, not dropped")
	}
}

func TestGround_PullsCoreChaptersAndRetrieves(t *testing.T) {
	s := mustStore(t)
	indexRules(t, s, sampleRules())
	in := ParseInput("You: Blood Artist", "1. Opp casts Wrath of God\n2. creatures die", "")
	g := Ground(context.Background(), Deps{Store: s}, in)

	numbers := map[string]bool{}
	for _, d := range g.Docs {
		numbers[d.Number] = true
	}
	for _, want := range []string{"117.1", "117.4", "603.2", "603.3b", "613.1", "616.1"} {
		if !numbers[want] {
			t.Errorf("grounding missing core chapter rule %s; have %v", want, numbers)
		}
	}
	// FTS retrieval over the sequence surfaces state-based actions, which the
	// wholesale chapters do not include.
	if len(g.Sources) == 0 {
		t.Errorf("expected FTS retrieve seeds as sources, got none")
	}
}

func TestGround_NilDadsAreGraceful(t *testing.T) {
	in := ParseInput("You: Blood Artist", "1. Opp casts Wrath of God", "")
	// No cards, no store: still produces a prompt with no docs/cards and no panic.
	g := Ground(context.Background(), Deps{}, in)
	if len(g.Cards) != 0 || len(g.Docs) != 0 {
		t.Errorf("nil deps should yield empty grounding, got %+v", g)
	}
}

/* ---------- Prompt ---------- */

func TestSystemPrompt_CoversResolutionOrder(t *testing.T) {
	got := SystemPrompt()
	for _, want := range []string{
		"603.3b",         // APNAP
		"616.1",          // replacement dependency/timestamp
		"613.1",          // layers
		"117.4",          // stack resolves top-down
		"201.4",          // self-reference
		"assistant, not", // honesty caveat
	} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt missing %q:\n%s", want, got)
		}
	}
}

func TestUserMessage_BoardSequenceOracleAndRules(t *testing.T) {
	looker := fakeLooker{
		"blood artist": {Name: "Blood Artist", ManaCost: "{1}{B}", TypeLine: "Creature — Vampire", OracleText: "Whenever Blood Artist or another creature dies, you may have target player lose 1 life."},
		"wrath of god": {Name: "Wrath of God", TypeLine: "Sorcery", OracleText: "Destroy all creatures."},
	}
	s := mustStore(t)
	indexRules(t, s, sampleRules())
	in := ParseInput("You: Blood Artist\nOpp: Doomed Traveler", "1. Opp casts Wrath of God", "It is the opponent's turn.")
	g := Ground(context.Background(), Deps{Cards: looker, Store: s}, in)
	msg := UserMessage(in, g)

	checks := map[string]bool{
		"board lists Blood Artist":     strings.Contains(msg, "[You] Blood Artist") || strings.Contains(msg, "Blood Artist"),
		"sequence lists the cast":      strings.Contains(msg, "casts Wrath of God"),
		"clarification passed through": strings.Contains(msg, "It is the opponent's turn."),
		"oracle text present":          strings.Contains(msg, "Whenever Blood Artist or another creature dies"),
		"rule text present":            strings.Contains(msg, "active player puts their abilities on the stack first"),
		"unresolved card reported":     strings.Contains(msg, "could not be looked up"),
	}
	for label, ok := range checks {
		if !ok {
			t.Errorf("%s: user message missing expected content", label)
		}
	}
}

/* ---------- Canned puzzles ---------- */

func TestCannedInputs_AllParse(t *testing.T) {
	ins := CannedInputs()
	if len(ins) != len(Puzzles) {
		t.Fatalf("CannedInputs = %d, want %d", len(ins), len(Puzzles))
	}
	for i, in := range ins {
		if len(in.Board.Permanents) == 0 {
			t.Errorf("puzzle %d parsed no permanents", i)
		}
		if len(in.Sequence.Steps) == 0 {
			t.Errorf("puzzle %d parsed no steps", i)
		}
	}
}

// TestCannedPuzzles_PromptShape is the acceptance fixture: each canned puzzle
// grounds and renders into a prompt that carries its board, its sequence, and
// the core interaction rules the trace must cite. The model's trace itself is
// verified manually against Puzzle.Expected.
func TestCannedPuzzles_PromptShape(t *testing.T) {
	s := mustStore(t)
	indexRules(t, s, sampleRules())
	// A looker that resolves every named card in every puzzle from a small
	// oracle map, so the prompt carries real card text.
	looker := fakeLooker{
		"blood artist":       {Name: "Blood Artist", TypeLine: "Creature", OracleText: "Whenever Blood Artist or another creature dies, you may have target player lose 1 life."},
		"zulaport cutthroat": {Name: "Zulaport Cutthroat", TypeLine: "Creature", OracleText: "Whenever Zulaport Cutthroat or another creature you control dies, each opponent loses 1 life and you gain 1 life."},
		"doomed traveler":    {Name: "Doomed Traveler", TypeLine: "Creature", OracleText: "When Doomed Traveler dies, create a 1/1 white Spirit creature token with flying."},
		"grim haruspex":      {Name: "Grim Haruspex", TypeLine: "Creature", OracleText: "Whenever another nontoken creature you control dies, you may draw a card."},
		"fleshbag marauder":  {Name: "Fleshbag Marauder", TypeLine: "Creature", OracleText: "When Fleshbag Marauder enters the battlefield, each player sacrifices a creature."},
		"rest in peace":      {Name: "Rest in Peace", TypeLine: "Enchantment", OracleText: "If a card or token would be put into a graveyard from anywhere, exile it instead."},
		"wrath of god":       {Name: "Wrath of God", TypeLine: "Sorcery", OracleText: "Destroy all creatures. They can't be regenerated."},
	}
	for i, in := range CannedInputs() {
		g := Ground(context.Background(), Deps{Cards: looker, Store: s}, in)
		req := Prompt(in, g)
		if !strings.Contains(req.User, "BOARD:") || !strings.Contains(req.User, "SEQUENCE") {
			t.Errorf("puzzle %d (%s): prompt missing board/sequence", i, Puzzles[i].Name)
		}
		if !strings.Contains(req.User, "603.3b") {
			t.Errorf("puzzle %d (%s): prompt missing the APNAP rule from grounding", i, Puzzles[i].Name)
		}
		// Every named card in the puzzle should carry oracle text — none unresolved.
		if len(g.Unresolved) != 0 {
			t.Errorf("puzzle %d (%s): unexpected unresolved cards %v", i, Puzzles[i].Name, g.Unresolved)
		}
	}
}

func cardNames(cs []*cards.Card) map[string]bool {
	out := make(map[string]bool, len(cs))
	for _, c := range cs {
		out[c.Name] = true
	}
	return out
}
