package intent

// The pipeline end to end (MAD-331's acceptance): ambiguous table talk
// produces either a correct optimistic application or a one-tap
// question — never a silent wrong write, never a blocking modal. Every
// test asserts on the Action produced and the disposition assigned;
// the store is only consulted where the contract itself is a row (the
// unresolved tray, the identity cache).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

func TestGrammarAutoAppliesImmediately(t *testing.T) {
	f := newFixture(t, nil)
	before := f.latestOrd()
	reply := f.talk(1, "-3")
	a := mustReplyAction(t, reply)
	if a.Kind != engine.ActionChangeLife || a.TargetSeat != 1 || a.Delta != -3 {
		t.Fatalf("action = %+v, want Collin −3 life", a)
	}
	if reply.Disposition != Auto || !reply.Applied {
		t.Fatalf("disposition/applied = %s/%v, want auto applied", reply.Disposition, reply.Applied)
	}
	if reply.From != "grammar" {
		t.Errorf("from = %q, want grammar", reply.From)
	}
	if f.state().Seats[1].Life != 37 {
		t.Errorf("life = %d, want 37", f.state().Seats[1].Life)
	}
	if f.latestOrd() <= before {
		t.Error("auto wrote no events")
	}
	// One-tap undo is the log's own: the batch the action wrote is
	// addressable by its ordinal, the correction contract's business.
}

func TestGrammarConfirmAppliesOptimistically(t *testing.T) {
	f := newFixture(t, nil)
	reply := f.talk(1, "cast Rhystic Study")
	a := mustReplyAction(t, reply)
	if a.Kind != engine.ActionCast || a.Card != "Rhystic Study" {
		t.Fatalf("action = %+v, want a cast of Rhystic Study", a)
	}
	if reply.Disposition != Confirm || !reply.Applied {
		t.Fatalf("disposition/applied = %s/%v, want confirm applied optimistically", reply.Disposition, reply.Applied)
	}
	if a.Confidence < 0.95 {
		t.Errorf("confidence = %.2f, want the deck-exact band", a.Confidence)
	}
	// The disposition rides the cause: the pane highlights what was
	// applied optimistically straight from the log entry.
	var cause struct {
		Disposition string `json:"disposition"`
		Source      string `json:"source"`
	}
	evs, _ := f.engine.Events(f.ctx, f.game, 0, 0)
	if len(evs) == 0 {
		t.Fatal("no events written")
	}
	if err := json.Unmarshal([]byte(evs[len(evs)-1].Cause), &cause); err != nil {
		t.Fatalf("cause: %v", err)
	}
	if cause.Disposition != "confirm" || cause.Source != "grammar" {
		t.Errorf("cause = %+v, want confirm/grammar", cause)
	}
}

// The acceptance case, exactly: an unresolved name is never silently
// written. With no candidates it parks; with candidates it asks, and
// the question is answerable in one tap — and the log never waited.
func TestAskParksWhenNoCandidates(t *testing.T) {
	f := newFixture(t, nil)
	before := f.latestOrd()
	reply := f.talk(1, "cast blorptidious")
	a := mustReplyAction(t, reply)
	if a.Kind != engine.ActionCast || a.Card != "blorptidious" {
		t.Fatalf("action = %+v, want the spoken span carried honestly", a)
	}
	if reply.Disposition != Ask {
		t.Fatalf("disposition = %s, want ask", reply.Disposition)
	}
	if reply.Applied || reply.Events != nil {
		t.Fatal("an ask applied something — the one failure this design cannot tolerate")
	}
	if f.latestOrd() != before {
		t.Error("the ask moved the log")
	}
	if reply.Question == nil || len(reply.Question.Options) != 0 {
		t.Fatalf("question = %+v, want parked (no options)", reply.Question)
	}
	open, err := f.pipeline.Open(f.ctx, f.game)
	if err != nil || len(open) != 1 || open[0].ID != reply.Question.ID {
		t.Fatalf("open = %+v err %v, want the parked row", open, err)
	}

	// The answer arrives as free text: recorded, and when it names a
	// card, the span's resolution is cached — the next "cast
	// blorptidious" is the grammar's to answer, alone.
	ans, err := f.pipeline.AnswerPending(f.ctx, f.game, reply.Question.ID, 1, "Rhystic Study")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if ans.Applied {
		t.Error("a parked answer applied an action — parked rows carry nothing safe to finish")
	}
	if res, ok := f.cachedResolution("blorptidious"); !ok || res.Card != "Rhystic Study" || res.Method != universe.MethodManual {
		t.Fatalf("cache = %+v ok %v, want the manual resolution", res, ok)
	}
	again := f.talk(1, "cast blorptidious")
	if again.From != "grammar" || again.Disposition != Confirm || !again.Applied {
		t.Fatalf("second ask = %+v, want the grammar answering from the cache", again)
	}
	if got := mustReplyAction(t, again).Card; got != "Rhystic Study" {
		t.Errorf("cached card = %q, want Rhystic Study", got)
	}
}

func TestAskWithOptionsIsOneTap(t *testing.T) {
	f := newFixture(t, &fakeModel{t: t, responses: []string{
		fenced(`{"kind":"CAST","card":"study","confidence":0.9}`),
	}})
	before := f.latestOrd()
	reply := f.talk(1, "put a study down")
	if reply.From != "llm" {
		t.Fatalf("from = %q, want the fallback to have answered", reply.From)
	}
	a := mustReplyAction(t, reply)
	if a.Kind != engine.ActionCast || a.Card != "study" {
		t.Fatalf("action = %+v, want the half-heard span carried at the ask rung", a)
	}
	if reply.Disposition != Ask || reply.Applied || f.latestOrd() != before {
		t.Fatalf("ask rung = %s applied %v, want nothing applied and an unmoved log", reply.Disposition, reply.Applied)
	}
	q := reply.Question
	if q == nil || len(q.Options) != 2 {
		t.Fatalf("question = %+v, want two tappable options", q)
	}
	if q.Options[0].Label != "Mystic Study" || q.Options[1].Label != "Rhystic Study" {
		t.Fatalf("options = %s / %s, want the deck's Study pair", q.Options[0].Label, q.Options[1].Label)
	}

	// One tap: the option applies its action at the confidence a
	// human's tap earns, and the tapped card becomes the span's cached
	// resolution.
	ans, err := f.pipeline.AnswerPending(f.ctx, f.game, q.ID, 1, "Rhystic Study")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if !ans.Applied || ans.Disposition != Confirm {
		t.Fatalf("answer reply = %+v, want the tapped cast applied optimistically", ans)
	}
	st := f.state()
	found := false
	for _, o := range st.Objects {
		if o.Zone == engine.ZoneStack && o.Identity.Card == "Rhystic Study" {
			found = true
		}
	}
	if !found {
		t.Error("the tapped option's cast never reached the stack")
	}
	if res, ok := f.cachedResolution("study"); !ok || res.Card != "Rhystic Study" {
		t.Fatalf("cache = %+v ok %v, want study → Rhystic Study", res, ok)
	}
}

func TestFallbackStateAndUniversePrompted(t *testing.T) {
	model := &fakeModel{t: t, responses: []string{fenced(`{"kind":"NONE"}`)}}
	f := newFixture(t, model)
	if _, err := f.pipeline.Interpret(f.ctx, f.game, 1, "so what does the table think", ""); err != nil {
		t.Fatalf("interpret: %v", err)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(model.calls))
	}
	prompt := model.calls[0]
	for _, want := range []string{"Collin", "Bob", "Rhystic Study", "KNOWN CARDS", `so what does the table think`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

func TestFallbackDeclinesAndRefusalsAreCleanNoParses(t *testing.T) {
	for name, reply := range map[string]string{
		"declined":  fenced(`{"kind":"NONE"}`),
		"invented":  fenced(`{"kind":"CAST","card":"Black Lotus"}`),
		"malformed": `I think Collin should probably pass priority here`,
	} {
		model := &fakeModel{t: t, responses: []string{reply}}
		f := newFixture(t, model)
		before := f.latestOrd()
		r := f.talk(1, "so about that whole situation")
		if r.Parsed || r.Applied || r.Question != nil {
			t.Errorf("%s: reply = %+v, want a clean no-parse", name, r)
		}
		if r.Note == "" {
			t.Errorf("%s: no note explaining the no-parse", name)
		}
		if f.latestOrd() != before {
			t.Errorf("%s: the refusal moved the log", name)
		}
	}
}

func TestFallbackAppliesAtConfirmAtMost(t *testing.T) {
	// A word-number life payment the grammar does not cover; the model
	// claims 1.0 certainty and still lands confirm, never auto — the
	// auto tier is the deterministic layers' alone.
	model := &fakeModel{t: t, responses: []string{
		fenced(`{"kind":"CHANGE_LIFE","target":"Collin","delta":-3,"card":"Cultivate","confidence":1.0}`),
	}}
	f := newFixture(t, model)
	reply := f.talk(1, "i'll pay three life for that")
	a := mustReplyAction(t, reply)
	if a.Kind != engine.ActionChangeLife || a.TargetSeat != 1 || a.Delta != -3 {
		t.Fatalf("action = %+v, want Collin −3", a)
	}
	if a.SourceCard != "Cultivate" {
		t.Errorf("source = %q, want the canonical spelling", a.SourceCard)
	}
	if a.Confidence > 0.80 {
		t.Errorf("confidence = %.2f, want the model ceiling enforced", a.Confidence)
	}
	if reply.Disposition != Confirm || !reply.Applied {
		t.Fatalf("disposition/applied = %s/%v, want confirm applied", reply.Disposition, reply.Applied)
	}
	if f.state().Seats[1].Life != 37 {
		t.Errorf("life = %d, want 37", f.state().Seats[1].Life)
	}
}

func TestFallbackTargetsSeatByNameAndDamages(t *testing.T) {
	model := &fakeModel{t: t, responses: []string{
		fenced(`{"kind":"DEAL_DAMAGE","target":"Bob","amount":4,"card":"Lightning Bolt","confidence":0.8}`),
	}}
	f := newFixture(t, model)
	reply := f.talk(1, "can the bolt hit Bob for four?")
	a := mustReplyAction(t, reply)
	if a.Kind != engine.ActionDealDamage || a.TargetSeat != 2 || a.Amount != 4 {
		t.Fatalf("action = %+v, want Bob takes 4", a)
	}
	if !reply.Applied || reply.Disposition != Confirm {
		t.Fatalf("disposition/applied = %s/%v", reply.Disposition, reply.Applied)
	}
	if f.state().Seats[2].Life != 36 {
		t.Errorf("Bob's life = %d, want 36", f.state().Seats[2].Life)
	}
}

func TestModelIdentificationIsCachedPerGame(t *testing.T) {
	// The model names the card by its mumbled spelling; the gate
	// canonicalizes it and the cache records the llm-tier resolution,
	// so the same name is never re-inferred, never re-billed.
	model := &fakeModel{t: t, responses: []string{
		fenced(`{"kind":"CAST","card":"rhystic study","confidence":0.9}`),
	}}
	f := newFixture(t, model)
	reply := f.talk(1, "the rhystic study does its thing now")
	if !reply.Applied {
		t.Fatalf("reply = %+v, want the cast applied", reply)
	}
	if got := mustReplyAction(t, reply).Card; got != "Rhystic Study" {
		t.Errorf("card = %q, want the canonical spelling", got)
	}
	res, ok := f.cachedResolution("rhystic study")
	if !ok || res.Card != "Rhystic Study" || res.Method != universe.MethodLLM {
		t.Fatalf("cache = %+v ok %v, want the llm-tier row", res, ok)
	}
	if res.Confidence > 0.80 {
		t.Errorf("cached confidence = %.2f, want the ceiling preserved", res.Confidence)
	}
	// The grammar now answers the same mumble from the cache, with no
	// model anywhere near it — From is the proof of which layer spoke.
	r2 := f.talk(1, "cast rhystic study")
	if r2.From != "grammar" || !r2.Applied {
		t.Fatalf("cached cast = %+v, want the grammar's confirm", r2)
	}
}

func TestNoModelIsGrammarOnly(t *testing.T) {
	f := newFixture(t, nil)
	r := f.talk(1, "so about that whole situation")
	if r.Parsed || r.Note == "" {
		t.Fatalf("reply = %+v, want a clean no-parse with a note", r)
	}
}

func TestRewindDismissesOpenQuestions(t *testing.T) {
	f := newFixture(t, nil)
	ord := f.latestOrd()
	if _, err := f.pipeline.Interpret(f.ctx, f.game, 1, "cast blorptidious", ""); err != nil {
		t.Fatalf("interpret: %v", err)
	}
	open, err := f.pipeline.Open(f.ctx, f.game)
	if err != nil || len(open) != 1 {
		t.Fatalf("open = %+v err %v", open, err)
	}
	// Rewind past the ordinal the question was asked at: the entry it
	// concerns no longer exists, and the question closes itself.
	if _, err := f.engine.RewindTo(f.ctx, f.game, ord-1); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if err := f.pipeline.DismissRewound(f.ctx, f.game, ord-1); err != nil {
		t.Fatalf("dismiss rewound: %v", err)
	}
	open, err = f.pipeline.Open(f.ctx, f.game)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("open after rewind = %+v, want empty", open)
	}
}
