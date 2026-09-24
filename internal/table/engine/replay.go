package engine

// Replay (MAD-339, stage 7 of MAD-321): the viewer over data the log
// already holds. State is a fold over an immutable event log, so the
// board at any past ordinal is fold(events[:n]) — the prefix property
// MAD-323 tests — and nothing new is stored, exactly the shape the
// D&D session replay (internal/replay) established on its side. The
// scoped half is ADR 13's rule unchanged: a seat scrubs its own
// stream, selected in SQL, and the owner folds everything.

import (
	"context"
	"fmt"
	"strings"
)

// StateAt folds the viewer's stream up to an ordinal, inclusive — the
// board as it stood the moment that row landed. `at` past the head
// reads the present (the same forgiveness the stream's reconnect
// gives); at zero folds the empty state, which is the honest answer
// for "before anything happened". The fold is the engine's own; the
// scoping is EventsFor's WHERE clause with its upper bound, never a
// post-filter.
func (s *Store) StateAt(ctx context.Context, gameID string, v Viewer, at int64) (*State, error) {
	if at < 0 {
		return nil, fmt.Errorf("%w: replay ordinal %d is negative", ErrInvalid, at)
	}
	if v.Owner {
		evs, err := s.Events(ctx, gameID, 0, 0)
		if err != nil {
			return nil, err
		}
		return Fold(upto(evs, at)), nil
	}
	q := fmt.Sprintf(`SELECT %s FROM mtg_events WHERE game_id = ? AND ord <= ?`, eventColumns)
	args := []any{gameID, at}
	if len(v.Seats) == 0 {
		q += ` AND visibility = 'public'`
	} else {
		ph := make([]string, len(v.Seats))
		for i, seat := range v.Seats {
			ph[i] = "?"
			args = append(args, seat)
		}
		q += ` AND (visibility = 'public' OR visible_seat IN (` + strings.Join(ph, ",") + `))`
	}
	q += ` ORDER BY ord`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read events up to ordinal: %w", err)
	}
	defer rows.Close()
	evs, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	return RedactFor(Fold(evs), v), nil
}

// upto keeps the rows at or before an ordinal, in place.
func upto(evs []Event, at int64) []Event {
	out := evs[:0]
	for _, e := range evs {
		if e.Ord > at {
			break
		}
		out = append(out, e)
	}
	return out
}

// ExpectedTriggerFires answers what the trigger registry would have
// fired over one batch of rows against the state as it stood before
// the batch landed — the deterministic half of the post-game coach's
// missed-trigger derivation (MAD-339): replay the log batch by batch
// with today's registry and diff against the TRIGGER_FIRED rows the
// log actually holds. Read-only: the returned rows are expectations,
// never folded, never written.
func ExpectedTriggerFires(base *State, batch []Event, reg TriggerRegistry) []Event {
	return matchRegistryTriggers(base, batch, reg)
}
