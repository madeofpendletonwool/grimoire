package intent

// The trigger proposal's tests (MAD-335): the gate over the model's
// reply (vocabulary, phrase, confidence, the honest NONE), the
// already-registered refusal, the no-model refusal, and the card
// resolution through the game's universe. The model proposes; nothing
// here ever writes the registry.

import (
	"errors"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

func fencedTrigger(kind, effect string) string {
	return "```json\n{\"kind\":\"" + kind + "\",\"effect\":\"" + effect + "\",\"confidence\":0.9}\n```"
}

func TestProposeTriggerGatesTheReply(t *testing.T) {
	p, err := GateTrigger("Rhystic Study", fencedTrigger("OPPONENT_CASTS_SPELL", "may draw a card"))
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if p.Event != string(engine.TriggerOppCasts) || p.Effect != "may draw a card" {
		t.Fatalf("proposal = %+v", p)
	}
	if p.Card != "Rhystic Study" {
		t.Fatalf("card = %q", p.Card)
	}
	// The model's confidence is capped below the manual bar.
	if p.Confidence > 0.9 {
		t.Fatalf("confidence = %f", p.Confidence)
	}
	// NONE is the honest refusal, not an error reply.
	if _, err := GateTrigger("Glorious Anthem", "```json\n{\"kind\":\"NONE\"}\n```"); !errors.Is(err, ErrNone) {
		t.Fatalf("NONE err = %v", err)
	}
	// A kind outside the vocabulary is refused.
	if _, err := GateTrigger("X", fencedTrigger("WHENEVER_ANYTHING", "nope")); err == nil {
		t.Error("unknown kind accepted")
	}
	// So is an empty effect, a paragraph, and unfenced garbage.
	if _, err := GateTrigger("X", fencedTrigger("UPKEEP", "   ")); err == nil {
		t.Error("empty effect accepted")
	}
	if _, err := GateTrigger("X", fencedTrigger("UPKEEP", strings.Repeat("word ", 100))); err == nil {
		t.Error("a paragraph effect accepted")
	}
	if _, err := GateTrigger("X", "I think it draws cards sometimes?"); err == nil {
		t.Error("unparseable reply accepted")
	}
	// Newlines in an effect collapse to one line of table voice.
	multiline := "```json\n{\"kind\":\"UPKEEP\",\"effect\":\"lose 1 life,\\n\\tdraw a card\"}\n```"
	p, err = GateTrigger("X", multiline)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if p.Effect != "lose 1 life, draw a card" {
		t.Fatalf("effect = %q", p.Effect)
	}
}

func TestProposeTriggerThroughThePipeline(t *testing.T) {
	f := newFixture(t, &fakeModel{t: t, responses: []string{
		fencedTrigger("OPPONENT_CASTS_SPELL", "may draw a card"),
	}})
	p, err := f.pipeline.ProposeTrigger(f.ctx, f.game, 1, "Rhystic Study")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if p.Card != "Rhystic Study" || p.Event != string(engine.TriggerOppCasts) {
		t.Fatalf("proposal = %+v", p)
	}
	// Nothing was written: the registry row appears only on the confirm
	// tap, which this test never made.
	rows, err := f.engine.TriggerRows(f.ctx, "Rhystic Study")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("registry = %+v, want empty — the proposal never writes", rows)
	}
}

func TestProposeTriggerRefusesRegisteredAndModelless(t *testing.T) {
	f := newFixture(t, &fakeModel{t: t, responses: []string{}})
	if _, err := f.engine.RegisterTrigger(f.ctx, "Rhystic Study",
		engine.TriggerOppCasts, "may draw a card", "declared", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pipeline.ProposeTrigger(f.ctx, f.game, 1, "Rhystic Study"); err == nil {
		t.Error("an already-registered card was proposed again")
	}
	// A grammar-only install (model nil) still registers by hand; the
	// propose affordance is simply not there.
	bare := newFixture(t, nil)
	if _, err := bare.pipeline.ProposeTrigger(bare.ctx, bare.game, 1, "Sol Ring"); !errors.Is(err, ErrNoModel) {
		t.Fatalf("modelless err = %v, want ErrNoModel", err)
	}
	if _, err := bare.pipeline.ProposeTrigger(bare.ctx, bare.game, 1, "  "); err == nil {
		t.Error("an empty card name was proposed")
	}
}

func TestProposeTriggerResolvesThroughTheUniverse(t *testing.T) {
	// A mumbled name resolves against the seat's deck before the prompt
	// is built, so the proposal registers once under the canonical
	// spelling.
	f := newFixture(t, &fakeModel{t: t, responses: []string{
		fencedTrigger("UPKEEP", "lose 1 life, draw a card"),
	}})
	p, err := f.pipeline.ProposeTrigger(f.ctx, f.game, 1, "rhystic")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if p.Card != "Rhystic Study" {
		t.Fatalf("card = %q, want the canonical spelling", p.Card)
	}
}
