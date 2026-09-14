package engine

// The trigger registry's store half (MAD-335): the mtg_trigger_registry
// rows are install-wide card knowledge, like the rules corpora — one
// registration per (card, event kind), cached across every game, so the
// second time anyone plays the card it is automatic. The writer is the
// registration surface (a human's declaration, or a model proposal a
// human confirmed); the reader is Submit, which loads the whole corpus
// as data for the pure Apply.

import (
	"context"
	"fmt"
	"strings"
)

// TriggerRow is one mtg_trigger_registry row as the surfaces read it.
type TriggerRow struct {
	Card   string       `json:"card"`
	Event  TriggerEvent `json:"event_kind"`
	Effect string       `json:"effect"`
	Origin string       `json:"origin"`
}

// Trigger effect phrases are table-voice prose — a dozen words a player
// would say. Longer than this is a paragraph, not a trigger.
const maxTriggerEffect = 240

// RegisterTrigger upserts one registration. origin is "declared" (a
// human typed it) or "confirmed" (a model proposed it, a human
// confirmed); confirmedBy records who did the confirming — the
// declared-vs-simulated line again (ADR 11).
func (s *Store) RegisterTrigger(ctx context.Context, card string, kind TriggerEvent, effect, origin, confirmedBy string) (*TriggerRow, error) {
	card = strings.TrimSpace(card)
	effect = strings.TrimSpace(effect)
	if card == "" {
		return nil, fmt.Errorf("%w: a trigger needs a card name", ErrInvalid)
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: %q is not a registry event kind", ErrInvalid, kind)
	}
	if effect == "" {
		return nil, fmt.Errorf("%w: a trigger needs its effect said", ErrInvalid)
	}
	if len(effect) > maxTriggerEffect {
		return nil, fmt.Errorf("%w: the effect is longer than %d characters", ErrInvalid, maxTriggerEffect)
	}
	if origin != "declared" && origin != "confirmed" {
		return nil, fmt.Errorf("%w: origin is declared|confirmed", ErrInvalid)
	}
	now := s.now().UnixMilli()
	var by any
	if origin == "confirmed" && confirmedBy != "" {
		by = confirmedBy
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mtg_trigger_registry (card_name, event_kind, effect, origin, confirmed_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (card_name, event_kind) DO UPDATE SET
			effect = excluded.effect, origin = excluded.origin,
			confirmed_by = excluded.confirmed_by, updated_at = excluded.updated_at`,
		card, string(kind), effect, origin, by, now, now); err != nil {
		return nil, fmt.Errorf("register trigger: %w", err)
	}
	return &TriggerRow{Card: card, Event: kind, Effect: effect, Origin: origin}, nil
}

// DeleteTrigger removes one registration — the correction path for a
// registration that over-fires. A missing row answers ErrNotFound.
func (s *Store) DeleteTrigger(ctx context.Context, card string, kind TriggerEvent) error {
	card = strings.TrimSpace(card)
	if card == "" || !kind.Valid() {
		return fmt.Errorf("%w: deleting needs the card and its event kind", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM mtg_trigger_registry WHERE card_name = ? AND event_kind = ?`, card, string(kind))
	if err != nil {
		return fmt.Errorf("delete trigger: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s has no %s registration", ErrNotFound, card, kind)
	}
	return nil
}

// TriggerRows reads the registry: one card's registrations when card is
// set, the whole corpus otherwise. Card name, then kind, ascending.
func (s *Store) TriggerRows(ctx context.Context, card string) ([]TriggerRow, error) {
	q := `SELECT card_name, event_kind, effect, origin FROM mtg_trigger_registry`
	args := []any{}
	if card = strings.TrimSpace(card); card != "" {
		q += ` WHERE card_name = ?`
		args = append(args, card)
	}
	q += ` ORDER BY card_name, event_kind`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	defer rows.Close()
	var out []TriggerRow
	for rows.Next() {
		var r TriggerRow
		var kind string
		if err := rows.Scan(&r.Card, &kind, &r.Effect, &r.Origin); err != nil {
			return nil, err
		}
		r.Event = TriggerEvent(kind)
		out = append(out, r)
	}
	return out, rows.Err()
}

// TriggerRegistry loads the whole corpus as the pure Apply's data: one
// small read per write, invalid kinds skipped by the builder — a corpus
// may outlive any one build's vocabulary.
func (s *Store) TriggerRegistry(ctx context.Context) (TriggerRegistry, error) {
	rows, err := s.TriggerRows(ctx, "")
	if err != nil {
		return nil, err
	}
	specs := make([]TriggerSpec, 0, len(rows))
	for _, r := range rows {
		specs = append(specs, TriggerSpec{Card: r.Card, Event: r.Event, Effect: r.Effect})
	}
	return NewTriggerRegistry(specs), nil
}

// Nudges reads the game's state and the registry and derives the
// current-action pane's don't-forget reminders (MAD-335) — the same
// deterministic query the pure State.Nudges answers, with the corpus
// attached.
func (s *Store) Nudges(ctx context.Context, gameID string) ([]Nudge, error) {
	if _, err := s.GetGame(ctx, gameID); err != nil {
		return nil, err
	}
	st, err := s.State(ctx, gameID)
	if err != nil {
		return nil, err
	}
	reg, err := s.TriggerRegistry(ctx)
	if err != nil {
		return nil, err
	}
	return st.Nudges(reg), nil
}
