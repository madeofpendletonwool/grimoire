package intent

// The trigger registry's proposal half (MAD-335): natural-language card
// knowledge in, ONE candidate trigger registration out — and nothing
// else. The model proposes; a human confirms; only then does the
// registration exist. This file never writes the registry and never
// writes game state: the confirmation tap is what carries the POST.
//
// The gate is the whole product: the proposal's event kind must be in
// the registry's fixed vocabulary, its effect a short table-voice
// phrase, its confidence capped. Anything else is a clean refusal, the
// same shape the action fallback's ErrGate answers with.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// ErrNoModel marks the propose affordance's absence: a grammar-only
// install (model nil) still registers triggers by hand, it just cannot
// propose them.
var ErrNoModel = errors.New("intent: no model is configured")

// TriggerProposal is one model-proposed registration, pre-confirmation:
// what the model thinks the card's trigger is, gated onto the
// vocabulary. The human's confirm tap turns it into an origin
// "confirmed" registration.
type TriggerProposal struct {
	Card       string  `json:"card"`
	Event      string  `json:"event_kind"`
	Effect     string  `json:"effect"`
	Confidence float64 `json:"confidence"`
}

// ProposeTrigger asks the model for one card's trigger registration.
// The card name is resolved against the game's universe when it can be
// (a proposal against the canonical spelling registers once and fires
// forever); an already-registered card answers ErrInvalid — there is
// nothing to propose. The proposal is returned, never written.
func (s *Store) ProposeTrigger(ctx context.Context, gameID string, seat int, card string) (*TriggerProposal, error) {
	card = strings.TrimSpace(card)
	if card == "" {
		return nil, fmt.Errorf("%w: a proposal needs the card's name", engine.ErrInvalid)
	}
	if s.model == nil {
		return nil, ErrNoModel
	}
	st, err := s.games.State(ctx, gameID)
	if err != nil {
		return nil, err
	}
	if res, err := s.resolve.ResolveGame(ctx, st, gameID, seat, card); err == nil && res.Resolved() {
		card = res.Card
	}
	rows, err := s.games.TriggerRows(ctx, card)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		return nil, fmt.Errorf("%w: %s is already registered", engine.ErrInvalid, card)
	}
	cctx, cancel := context.WithTimeout(ctx, modelTimeout)
	comp, err := s.model.Complete(cctx, TriggerSystemPrompt(), TriggerUserMessage(card))
	cancel()
	if err != nil {
		return nil, fmt.Errorf("the model did not answer: %w", err)
	}
	return GateTrigger(card, comp.Text)
}

// GateTrigger validates one model reply into a proposal: the same
// fence-tolerant JSON decode the action gate uses, then the vocabulary
// and phrase checks. {"kind":"NONE"} is ErrNone — the model's honest
// "this card has no trigger my vocabulary can name".
func GateTrigger(card, reply string) (*TriggerProposal, error) {
	var raw struct {
		Kind       string  `json:"kind"`
		Effect     string  `json:"effect"`
		Confidence float64 `json:"confidence"`
	}
	if err := decodeFencedJSON(reply, &raw); err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(raw.Kind), "NONE") {
		return nil, ErrNone
	}
	kind := engine.TriggerEvent(strings.TrimSpace(raw.Kind))
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: %q is not a registry event kind", ErrGate, raw.Kind)
	}
	effect := strings.Join(strings.Fields(raw.Effect), " ")
	if effect == "" {
		return nil, fmt.Errorf("%w: the proposal carries no effect", ErrGate)
	}
	if len(effect) > 240 {
		return nil, fmt.Errorf("%w: the effect phrase is too long", ErrGate)
	}
	conf := raw.Confidence
	if conf < 0 {
		conf = 0
	}
	if conf > universe.ConfLLM {
		conf = universe.ConfLLM
	}
	return &TriggerProposal{Card: card, Event: string(kind), Effect: effect, Confidence: conf}, nil
}

// TriggerSystemPrompt is the proposal prompt's standing instruction:
// one trigger, the fixed vocabulary with its scope semantics spelled,
// table-voice effect phrases, NONE when the card's best-known trigger
// does not fit. The prompt requests; GateTrigger enforces.
func TriggerSystemPrompt() string {
	var b strings.Builder
	b.WriteString(`You propose trigger registrations for a Magic: the Gathering companion's trigger registry. A HUMAN will confirm or reject what you propose — you are the proposal, never the registration.

Reply with exactly one fenced json block and nothing else — no prose, no explanation.

THE SHAPE:
` + "```json\n{\"kind\": \"...\", \"effect\": \"...\", \"confidence\": 0.0}\n```" + `

"kind" is one of these structural events, with the scope the registry fires:
- "LAND_PLAYED" — whenever a land enters under the card's controller (landfall)
- "CREATURE_ETB" — whenever a creature enters under the card's controller
- "UPKEEP" — at the card's controller's upkeep
- "OPPONENT_CASTS_SPELL" — whenever a player other than the card's controller casts a spell
- "ATTACKS" — whenever the card itself is declared as an attacker
- "DIES" — whenever a creature under the card's controller dies, or the card itself dies
- "END_STEP" — at the card's controller's end step
- "NONE" — the honest refusal

"effect" is the trigger's effect as a short table-voice phrase, at most 12 words. Prefer the common shapes when they fit: "draw a card", "create a Treasure", "deal 1 damage to each opponent", "create a 1/1", "investigate", "gain 1 life", "mill a card", "add one mana".

RULES YOU MAY NOT BREAK:
1. Propose the card's SINGLE most useful triggered ability that fits one of the kinds above. Static abilities (anthems, lords, cost reduction) are NOT triggers — emit NONE for them.
2. If the card's best-known trigger does not fit any kind (wrong scope, an activated ability, a replacement effect), emit {"kind":"NONE"} rather than stretching a kind to fit.
3. "effect" describes what the trigger DOES when it resolves, in the table's own words — never restates when it fires.
4. "confidence" is 0.0 to 1.0, your read on the mapping.`)
	return b.String()
}

// TriggerUserMessage carries the card name — the one thing the prompt
// is about. Kept pure for the tests.
func TriggerUserMessage(card string) string {
	return fmt.Sprintf("Propose one trigger registration for the Magic card %q.", card)
}
