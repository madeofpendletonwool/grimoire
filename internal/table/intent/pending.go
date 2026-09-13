package intent

// The unresolved tray (MAD-331): mtg_pending, the table the stage-1
// migration defined and this stage fills. A clarification the one-tap
// rule could not reduce to tappable answers parks here open, and play
// continues — a question that stops the game is worse than a wrong
// board state. Asked questions (with options) are the same rows; the
// options column is what distinguishes them. On rewind, an open
// question whose context ordinal was truncated is auto-dismissed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// Pending statuses.
const (
	pendingOpen      = "open"
	pendingAnswered  = "answered"
	pendingDismissed = "dismissed"
)

// pendingContext is the JSON a row's context column carries: the spoken
// span it concerns, the seat that said it, and the log ordinal at ask
// time — the rewind contract keys on that ordinal.
type pendingContext struct {
	Spoken string `json:"spoken,omitempty"`
	Seat   int    `json:"seat,omitempty"`
	Ord    int64  `json:"ord,omitempty"`
}

// PendingRow is one tray row as the surface reads it: the question, its
// tappable options when it was asked, the spoken span, and the ordinal
// it was asked at.
type PendingRow struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Options  []Option `json:"options,omitempty"`
	Spoken   string   `json:"spoken,omitempty"`
	Ord      int64    `json:"ord,omitempty"`
	Status   string   `json:"status"`
}

// park writes one open row and returns its id. Asked questions carry
// their options; parked ones carry none.
func (s *Store) park(ctx context.Context, gameID string, st *engine.State, q *Question, action engine.Action) (string, error) {
	id := uuid.NewString()
	opts, err := json.Marshal(q.Options)
	if err != nil {
		return "", fmt.Errorf("encode options: %w", err)
	}
	contextJSON, err := json.Marshal(pendingContext{Spoken: q.Spoken, Seat: action.Seat, Ord: st.LastOrd})
	if err != nil {
		return "", fmt.Errorf("encode context: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mtg_pending (id, game_id, question, options, context, status, answer, created_at)
		VALUES (?, ?, ?, ?, ?, 'open', '', ?)`,
		id, gameID, q.Text, string(opts), string(contextJSON), s.now().UnixMilli()); err != nil {
		return "", fmt.Errorf("park question: %w", err)
	}
	return id, nil
}

// pendingScanner is the shared row shape for scanPending.
type pendingScanner interface {
	Scan(dest ...any) error
}

// scanPending decodes one row; options decode to nil when the column
// holds an empty array (a parked question).
func scanPending(row pendingScanner) (*PendingRow, error) {
	var (
		r       PendingRow
		opts    string
		context string
		status  string
		created int64
	)
	if err := row.Scan(&r.ID, &r.Question, &opts, &context, &status, &created); err != nil {
		return nil, fmt.Errorf("scan pending: %w", err)
	}
	r.Status = status
	if opts != "" && opts != "[]" && opts != "null" {
		if err := json.Unmarshal([]byte(opts), &r.Options); err != nil {
			return nil, fmt.Errorf("parse pending options: %w", err)
		}
	}
	if context != "" {
		var c pendingContext
		if err := json.Unmarshal([]byte(context), &c); err == nil {
			r.Spoken, r.Ord = c.Spoken, c.Ord
		}
	}
	return &r, nil
}

// loadPending reads one row of one game.
func (s *Store) loadPending(ctx context.Context, gameID, id string) (*PendingRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, question, options, context, status, created_at FROM mtg_pending
		 WHERE game_id = ? AND id = ?`, gameID, id)
	r, err := scanPending(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("mtg pending %s: %w", id, engine.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// setPendingStatus closes a row with its answer and answerer.
func (s *Store) setPendingStatus(ctx context.Context, gameID, id, status, answer string, seat int) error {
	var answeredAt any
	var answeredSeat any
	if status == pendingAnswered {
		answeredAt = s.now().UnixMilli()
		if seat != 0 {
			answeredSeat = seat
		}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE mtg_pending SET status = ?, answer = ?, answered_seat = ?, answered_at = ?
		 WHERE game_id = ? AND id = ? AND status = 'open'`,
		status, answer, answeredSeat, answeredAt, gameID, id)
	if err != nil {
		return fmt.Errorf("update pending: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: that question is no longer open", engine.ErrInvalid)
	}
	return nil
}
