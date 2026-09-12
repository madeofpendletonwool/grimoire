package engine

// Amend (MAD-328): rewind, substitute a corrected action, re-apply —
// as one writer transaction. Rewind alone is a truncate (RewindTo);
// amend is the same truncate with the correction applied to the
// surviving prefix before the log ever holds an intermediate state, so
// no attached client observes a half-corrected game and a rejected
// correction leaves the log byte-identical. This file is the
// easy-adjust contract's engine half; the two-tap surface that drives
// it lives in the play window.

import (
	"context"
	"database/sql"
	"fmt"
)

// AmendAt truncates the log at the batch of events containing ord —
// everything one Submit produced, and everything after it — and applies
// the corrected action in its place. The ordinal must exist and the
// correction must validate; otherwise nothing changes, by the
// transaction's construction. Ordinals stay contiguous: the correction's
// rows start exactly where the amended batch started.
func (s *Store) AmendAt(ctx context.Context, gameID string, ord int64, action Action) ([]Event, *State, error) {
	if _, err := s.GetGame(ctx, gameID); err != nil {
		return nil, nil, err
	}
	if ord < 1 {
		return nil, nil, fmt.Errorf("%w: amend ordinal %d is not a log row", ErrInvalid, ord)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("amend tx: %w", err)
	}
	defer tx.Rollback()

	evs, err := eventsInTx(ctx, tx, gameID)
	if err != nil {
		return nil, nil, err
	}
	lo, _, ok := batchBounds(evs, ord)
	if !ok {
		return nil, nil, fmt.Errorf("%w: no event at ordinal %d", ErrInvalid, ord)
	}
	from := evs[lo].Ord
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM mtg_events WHERE game_id = ? AND ord >= ?`, gameID, from); err != nil {
		return nil, nil, fmt.Errorf("truncate events: %w", err)
	}

	// The correction validates against the surviving prefix, inside the
	// transaction: a rejection rolls the truncate back with it.
	prefix := evs[:lo]
	state := Fold(prefix)
	fresh, err := Apply(state, action)
	if err != nil {
		return nil, nil, err
	}
	out, hasStart, hasEnd, err := s.writeEvents(ctx, tx, gameID, action, fresh, from)
	if err != nil {
		return nil, nil, err
	}

	// The lifecycle is re-derived from everything that survives plus
	// everything the correction produced — amending past GAME_STARTED
	// with a non-start correction leaves an honest setup game, the same
	// posture a rewind to that point would hold.
	for i := range prefix {
		switch prefix[i].Kind {
		case EventGameStarted:
			hasStart = true
		case EventGameEnded:
			hasEnd = true
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mtg_games SET status = ?, updated_at = ?,
			started_at = CASE WHEN ? THEN started_at ELSE NULL END,
			ended_at   = CASE WHEN ? THEN ended_at ELSE NULL END
		WHERE id = ?`,
		lifecycle(hasStart, hasEnd).String(), s.now().UnixMilli(), hasStart, hasEnd, gameID); err != nil {
		return nil, nil, fmt.Errorf("restate game: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("amend commit: %w", err)
	}
	s.notify(gameID)
	return out, state.FoldInto(out), nil
}

// lifecycle derives the game row's status from what the log holds.
func lifecycle(hasStart, hasEnd bool) Status {
	switch {
	case hasEnd:
		return StatusFinished
	case hasStart:
		return StatusActive
	default:
		return StatusSetup
	}
}

// String keeps the status a plain SQL argument.
func (s Status) String() string { return string(s) }

// batchBounds returns the half-open index range [lo, hi) of the
// contiguous run of events one Submit produced around the event at
// ordinal ord — the rows amend rewrites and undo removes. The batch
// stamp is the authority; rows from before the stamp (or a
// hand-assembled log) fall back to cause equality over contiguous
// ordinals, which is the best a repeating cause allows.
func batchBounds(evs []Event, ord int64) (lo, hi int, ok bool) {
	idx := -1
	for i := range evs {
		if evs[i].Ord == ord {
			idx = i
			break
		}
	}
	if idx < 0 {
		return 0, 0, false
	}
	same := func(a, b Event) bool {
		if a.Ord+1 != b.Ord {
			return false
		}
		// A stamp on one side and not the other is a boundary: one of
		// the two rows was written by a Submit that stamped, the other
		// was not.
		if (a.Batch == "") != (b.Batch == "") {
			return false
		}
		if a.Batch != "" {
			return a.Batch == b.Batch
		}
		return a.Cause != "" && a.Cause == b.Cause
	}
	lo, hi = idx, idx+1
	for lo > 0 && same(evs[lo-1], evs[lo]) {
		lo--
	}
	for hi < len(evs) && same(evs[hi-1], evs[hi]) {
		hi++
	}
	return lo, hi, true
}

// eventsInTx reads the whole log inside the caller's transaction — the
// fold's input, read where the truncate is about to run so the bounds
// and the delete cannot disagree.
func eventsInTx(ctx context.Context, tx *sql.Tx, gameID string) ([]Event, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, ord, kind, actor_seat, source, cause, payload, visibility, visible_seat, created_at
		FROM mtg_events WHERE game_id = ? ORDER BY ord`, gameID)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}
