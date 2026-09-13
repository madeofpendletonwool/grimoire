// Package intent is the Magic table's model layer (MAD-331, stage 4 of
// MAD-321): natural language in, a candidate typed Action out — and
// nothing else. It never writes game state itself (the engine's Submit
// is the only writer, called only for rungs that apply), and it never
// decides a rules question.
//
// The pipeline is grammar-first, model-second, exactly as ADR 10 orders
// it: the deterministic grammar (MAD-330) answers instantly and offline;
// only what it refuses reaches the model fallback, which is prompted
// with the current game state and the known-card universe and gated
// onto the same deterministic lookups the grammar trusts. Every
// candidate then walks the confirmation ladder from
// docs/table/interaction.md:
//
//	auto    applied immediately, one-tap undo (life, tap/untap, draw,
//	        land drops, damage, flow).
//	confirm applied optimistically, highlighted with accept / fix.
//	ask     not applied. One question with tappable answers — or,
//	        when the one-tap rule cannot be met, a row in the
//	        unresolved tray and play continues.
//
// The hard rule the clarification component obeys: every clarification
// is answerable in one tap or one spoken word and never blocks the log.
// A modal that stops the game is worse than a wrong board state,
// because a wrong board state costs one tap to fix and a stopped game
// cannot be un-stopped.
package intent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/grammar"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// modelTimeout bounds one fallback call. A table utterance wants an
// answer in seconds; a parser that takes minutes is a stopped game by
// another name, and the one-tap rule forbids stopped games.
const modelTimeout = 30 * time.Second

/* ---------- the model seam ---------- */

// Completion is one model response with its token accounting — the same
// shape the canon engine, the director and the homebrew linter use, so
// the same adapters serve all of them.
type Completion struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// ModelClient is the slice of the LLM surface the fallback needs: one
// non-streaming prompt exchange. The production adapter wraps the
// shared internal/llm client; tests replay fixture responses.
type ModelClient interface {
	// ModelName names the model for the response record.
	ModelName() string
	// Complete answers one system+user exchange.
	Complete(ctx context.Context, system, user string) (Completion, error)
}

// llmModel adapts the shared client to ModelClient.
type llmModel struct{ c *llm.Client }

// NewLLMModel adapts the shared internal/llm client to the intent
// pipeline's ModelClient. The client's own provider failover applies
// underneath.
func NewLLMModel(c *llm.Client) ModelClient { return llmModel{c: c} }

func (m llmModel) ModelName() string { return m.c.Model() }

func (m llmModel) Complete(ctx context.Context, system, user string) (Completion, error) {
	text, usage, err := m.c.AnswerPromptUsage(ctx, system, user)
	if err != nil {
		return Completion{}, err
	}
	return Completion{Text: text, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}, nil
}

/* ---------- the resolver seam ---------- */

// Resolver is the identification half the pipeline leans on: the
// universe store's cached tier walk plus the two cache writers (a
// human's correction, the model's identification). *universe.Store
// satisfies it.
type Resolver interface {
	ResolveGame(ctx context.Context, st *engine.State, gameID string, seat int, spoken string) (universe.Resolution, error)
	Record(ctx context.Context, gameID, spoken, card string) error
	RecordLLM(ctx context.Context, gameID, spoken, card string, confidence float64) error
}

/* ---------- the reply ---------- */

// Reply is one utterance's outcome: what was understood, how it was
// dispositioned, what was applied, and the question if there is one.
// Parsed false with a Note is a clean no-parse — never a guess.
type Reply struct {
	Parsed      bool           `json:"parsed"`
	From        string         `json:"from,omitempty"` // grammar | llm — which layer answered
	Disposition Disposition    `json:"disposition,omitempty"`
	Action      *engine.Action `json:"action,omitempty"`  // the candidate as stamped
	Applied     bool           `json:"applied,omitempty"` // auto/confirm were submitted
	Events      []engine.Event `json:"events,omitempty"`
	State       *engine.State  `json:"state,omitempty"`
	Question    *Question      `json:"question,omitempty"` // ask: not applied
	Note        string         `json:"note,omitempty"`
}

// Option is one tappable answer. Applying it submits Action — already
// stamped with the human-tapped confidence the ladder respects.
type Option struct {
	Label  string        `json:"label"`
	Action engine.Action `json:"action"`
}

// Question is a clarification under the one-tap rule: Options present
// means it was asked — every answer is one tap, the log never waits.
// Options empty means the one-tap rule could not be met and the
// question is parked in the unresolved tray instead, play continuing
// while it stays open.
type Question struct {
	Text    string   `json:"text"`
	Options []Option `json:"options,omitempty"`
	// Spoken is the name span the options disambiguate, recorded so an
	// answer becomes the game's cached resolution of that span.
	Spoken string `json:"spoken,omitempty"`
	// ID is the pending row's id when the question was persisted.
	ID string `json:"id,omitempty"`
}

// maxOptions is the one-tap rule's small set: a question with more
// answers than this is not one tap, it is a menu, and it parks.
const maxOptions = 3

/* ---------- the store ---------- */

// Store runs the intent pipeline over live games.
type Store struct {
	db      *sql.DB
	model   ModelClient // nil: grammar-only install, the fallback is absent
	games   *engine.Store
	resolve Resolver
	now     func() time.Time
}

// New builds the pipeline. model may be nil — the grammar still answers
// and the ladder still runs; what the grammar refuses is then a clean
// no-parse rather than a bill.
func New(db *sql.DB, model ModelClient, games *engine.Store, resolve Resolver) (*Store, error) {
	if db == nil {
		return nil, errors.New("intent: nil database handle")
	}
	if games == nil {
		return nil, errors.New("intent: nil engine store")
	}
	if resolve == nil {
		return nil, errors.New("intent: nil resolver")
	}
	return &Store{db: db, model: model, games: games, resolve: resolve, now: time.Now().UTC}, nil
}

// ModelName names the model behind the fallback, "" when none is wired.
func (s *Store) ModelName() string {
	if s.model == nil {
		return ""
	}
	return s.model.ModelName()
}

// cachedNames is the grammar's name seam over the per-game cache and
// the tier walk: the grammar resolves names through the same resolver
// the resolve endpoint uses, so a cached answer is the grammar's answer
// too and no name is re-inferred behind the grammar's back.
type cachedNames struct {
	resolve Resolver
	gameID  string
	st      *engine.State
}

func (n cachedNames) ResolveName(ctx context.Context, seat int, spoken string) grammar.NameInfo {
	r, err := n.resolve.ResolveGame(ctx, n.st, n.gameID, seat, spoken)
	if err != nil || !r.Resolved() {
		return grammar.NameInfo{}
	}
	return grammar.NameInfo{Card: r.Card, Confidence: r.Confidence}
}

// Interpret turns one utterance by one seat into an applied action, an
// asked question, or a clean no-parse. source is the entry channel
// ("grammar" for typed talk, "voice" for push-to-talk); which layer
// answered is reported separately in Reply.From.
func (s *Store) Interpret(ctx context.Context, gameID string, seat int, utterance string, source string) (*Reply, error) {
	utterance = strings.TrimSpace(utterance)
	if utterance == "" {
		return nil, fmt.Errorf("%w: nothing was said", engine.ErrInvalid)
	}
	if source == "" {
		source = "grammar"
	}
	st, err := s.games.State(ctx, gameID)
	if err != nil {
		return nil, err
	}
	if _, ok := st.Seats[seat]; !ok {
		return nil, fmt.Errorf("%w: seat %d is not in this game", engine.ErrInvalid, seat)
	}

	// Layer 1: the grammar, over the cached resolver.
	names := cachedNames{resolve: s.resolve, gameID: gameID, st: st}
	from := "grammar"
	var action engine.Action
	if res := grammar.Parse(ctx, seat, utterance, st, names); res.OK {
		action = res.Action
		action.Source = source
	} else if s.model == nil {
		return &Reply{Parsed: false, From: "grammar",
			Note: "the grammar did not recognize that and no model is configured"}, nil
	} else {
		// Layer 2: the model fallback, gated.
		u := universe.FromState(st)
		cctx, cancel := context.WithTimeout(ctx, modelTimeout)
		comp, err := s.model.Complete(cctx, SystemPrompt(), UserMessage(st, seat, utterance, u))
		cancel()
		if err != nil {
			return &Reply{Parsed: false, From: "llm", Note: fmt.Sprintf("the model did not answer: %v", err)}, nil
		}
		gated, ident, err := Gate(st, seat, utterance, u, comp.Text)
		if errors.Is(err, ErrNone) {
			return &Reply{Parsed: false, From: "llm", Note: "not an action anyone could parse"}, nil
		}
		if err != nil {
			return &Reply{Parsed: false, From: "llm", Note: err.Error()}, nil
		}
		gated.Source = "llm"
		if source == "voice" {
			gated.Source = "voice"
		}
		action = gated
		from = "llm"
		// The model's identification joins the per-game cache: the same
		// mumble is never re-inferred, never re-billed (MAD-329's
		// contract, extended to the llm tier).
		if ident != nil {
			_ = s.resolve.RecordLLM(ctx, gameID, ident.Spoken, ident.Card, ident.Confidence)
		}
	}

	disposition := Assign(action)
	action.Disposition = string(disposition)
	reply := &Reply{Parsed: true, From: from, Disposition: disposition, Action: &action}

	switch disposition {
	case Auto, Confirm:
		evs, state, err := s.games.Submit(ctx, gameID, action)
		if err != nil {
			// A rejected action wrote nothing — the engine's invariant.
			// Surface it as the reply's note rather than a transport
			// error: the table hears "not applied, because", not a 500.
			reply.Applied = false
			reply.Note = err.Error()
			return reply, nil
		}
		reply.Applied = true
		reply.Events = evs
		reply.State = state
		return reply, nil
	default: // Ask: not applied, by construction.
		q := s.buildQuestion(ctx, gameID, st, seat, utterance, action)
		reply.Question = q
		return reply, nil
	}
}

// buildQuestion reduces an ask to the one-tap contract: candidates from
// the known-card universe become tappable options (each carrying the
// action with that card, at the confidence a human's tap earns), and a
// question that cannot be so reduced parks in the unresolved tray with
// play continuing. Either way the log is untouched and nothing waits.
func (s *Store) buildQuestion(ctx context.Context, gameID string, st *engine.State, seat int, utterance string, action engine.Action) *Question {
	spoken := action.Card // the unresolved span rides the card field
	text := spoken
	if text == "" {
		text = utterance
	}
	q := &Question{Text: fmt.Sprintf("%q — which card?", text), Spoken: spoken}
	u := universe.FromState(st)
	if spoken != "" && u.Attached() > 0 {
		for _, cand := range u.Candidates(seat, spoken, maxOptions) {
			optionAction := action
			optionAction.Card = cand
			optionAction.Base = nil // the unresolved span never earned types
			optionAction.Confidence = universe.ConfManual
			optionAction.Disposition = string(Assign(optionAction))
			q.Options = append(q.Options, Option{Label: cand, Action: optionAction})
		}
	}
	if len(q.Options) < 2 {
		// One option is not a question; zero is not a tap. Park it.
		q.Options = nil
		q.Text = fmt.Sprintf("What did %s say — %q?", seatName(st, seat), text)
	}
	if id, err := s.park(ctx, gameID, st, q, action); err == nil {
		q.ID = id
	}
	return q
}

// AnswerPending closes one open question: a tappable option submits its
// action (and the tapped card becomes the spoken span's cached
// resolution — the "no, Tithe" path); a parked row records the free
// text answer and applies nothing, because a parked question never
// carried an action anyone could safely finish.
func (s *Store) AnswerPending(ctx context.Context, gameID string, pendingID string, seat int, answer string) (*Reply, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil, fmt.Errorf("%w: an answer needs saying", engine.ErrInvalid)
	}
	row, err := s.loadPending(ctx, gameID, pendingID)
	if err != nil {
		return nil, err
	}
	if row.Status != pendingOpen {
		return nil, fmt.Errorf("%w: that question is already %s", engine.ErrInvalid, row.Status)
	}
	if len(row.Options) == 0 {
		// Parked: the free text is recorded, and when it names a card
		// the span's resolution is cached at manual confidence — the
		// next time that name is spoken, the grammar answers alone.
		if row.Spoken != "" {
			st, err := s.games.State(ctx, gameID)
			if err == nil {
				if res, rerr := s.resolve.ResolveGame(ctx, st, gameID, seat, answer); rerr == nil && res.Resolved() {
					_ = s.resolve.Record(ctx, gameID, row.Spoken, res.Card)
				}
			}
		}
		if err := s.setPendingStatus(ctx, gameID, pendingID, pendingAnswered, answer, seat); err != nil {
			return nil, err
		}
		return &Reply{Parsed: false, Note: "recorded"}, nil
	}
	for _, opt := range row.Options {
		if !strings.EqualFold(opt.Label, answer) {
			continue
		}
		action := opt.Action
		action.Disposition = string(Assign(action))
		reply := &Reply{Parsed: true, From: "grammar", Disposition: Assign(action), Action: &action}
		evs, state, err := s.games.Submit(ctx, gameID, action)
		if err != nil {
			reply.Note = err.Error()
			return reply, nil
		}
		if err := s.setPendingStatus(ctx, gameID, pendingID, pendingAnswered, opt.Label, seat); err != nil {
			return nil, err
		}
		// The tapped card is the human's word for the spoken span.
		if row.Spoken != "" {
			_ = s.resolve.Record(ctx, gameID, row.Spoken, opt.Action.Card)
		}
		reply.Applied = true
		reply.Events = evs
		reply.State = state
		return reply, nil
	}
	return nil, fmt.Errorf("%w: %q is not one of the answers", engine.ErrInvalid, answer)
}

// Dismiss closes an open question without answering it — the ✗ on the
// question strip.
func (s *Store) Dismiss(ctx context.Context, gameID, pendingID string) error {
	row, err := s.loadPending(ctx, gameID, pendingID)
	if err != nil {
		return err
	}
	if row.Status != pendingOpen {
		return fmt.Errorf("%w: that question is already %s", engine.ErrInvalid, row.Status)
	}
	return s.setPendingStatus(ctx, gameID, pendingID, pendingDismissed, "", 0)
}

// Open lists the game's open questions, oldest first — asked ones ride
// the current-action pane, parked ones the tray.
func (s *Store) Open(ctx context.Context, gameID string) ([]PendingRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, question, options, context, status, created_at FROM mtg_pending
		 WHERE game_id = ? AND status = 'open' ORDER BY created_at, id`, gameID)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}
	defer rows.Close()
	var out []PendingRow
	for rows.Next() {
		r, err := scanPending(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// DismissRewound closes every open question whose context ordinal was
// truncated away — the model doc's contract: a question about an entry
// that no longer exists is not waiting on anything. Called after every
// rewind and amend.
func (s *Store) DismissRewound(ctx context.Context, gameID string, head int64) error {
	open, err := s.Open(ctx, gameID)
	if err != nil {
		return err
	}
	for _, row := range open {
		if row.Ord > head {
			if err := s.setPendingStatus(ctx, gameID, row.ID, pendingDismissed, "", 0); err != nil {
				return err
			}
		}
	}
	return nil
}
