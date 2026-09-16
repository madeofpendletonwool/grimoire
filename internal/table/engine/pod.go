package engine

// The multiplayer pod's store half (MAD-337, stage 6 of MAD-321): the
// scoped reads ADR 13 names, the join that binds a participant to a
// seat, and the private per-seat scratch mtg_seat_notes has waited
// for. The write path is unchanged — one writer, append-only ordinals
// — because multiplayer was never a write problem. Everything here is
// the WHERE clause: a seat's view of the log is selected in SQL, never
// post-filtered, never entrusted to a prompt.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrNotEntitled marks a request the viewer may not make inside a game
// they can otherwise see: acting as a seat they do not hold, reading a
// zone that is not theirs. Distinct from ErrNotFound on purpose — the
// game exists and the viewer may play at it; this one thing is not
// theirs. The API maps it onto 403.
var ErrNotEntitled = fmt.Errorf("engine: viewer is not entitled to that")

// eventColumns is the SELECT every log read shares, so the scoped and
// unscoped queries cannot drift apart.
const eventColumns = `id, ord, kind, actor_seat, source, cause, payload, visibility, visible_seat, created_at`

// EventsFor reads the log past an ordinal for one viewer — ADR 13's
// WHERE clause made real: public rows for everyone, seat-visible rows
// for the seat named on the row, in SQL. The owner's viewer short
// circuits to the unscoped read; a viewer holding no seat sees the
// public stream only.
func (s *Store) EventsFor(ctx context.Context, gameID string, v Viewer, after int64, limit int) ([]Event, error) {
	if v.Owner {
		return s.Events(ctx, gameID, after, limit)
	}
	q := fmt.Sprintf(`SELECT %s FROM mtg_events WHERE game_id = ? AND ord > ?`, eventColumns)
	args := []any{gameID, after}
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
	if limit > 0 {
		q += ` ORDER BY ord LIMIT ?`
		args = append(args, limit)
	} else {
		q += ` ORDER BY ord`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read events for viewer: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// StateFor folds the viewer's own stream: the scoped rows, then the
// redaction pass. Folding scoped rows is the honest half — a seat's
// fold physically lacks the rows it must not see, so no downstream
// prompt assembly can quote what is not there. The redaction pass is
// the belt over legacy rows (games whose public GAME_STARTED carried
// decks before the DECK_KNOWN split): it zeroes hidden-zone fields on
// seats the viewer does not hold, whatever row they rode in on.
func (s *Store) StateFor(ctx context.Context, gameID string, v Viewer) (*State, error) {
	if v.Owner {
		return s.State(ctx, gameID)
	}
	evs, err := s.EventsFor(ctx, gameID, v, 0, 0)
	if err != nil {
		return nil, err
	}
	return RedactFor(Fold(evs), v), nil
}

// RedactFor zeroes every hidden-zone field on seats the viewer does not
// hold: HandKnown, Deck, LibraryComp, LibraryExact. Hand and library
// counts stay — sizes are public information in Magic. It mutates the
// state it is handed; call it on a fold you own (StateFor folds fresh,
// handlers re-scope freshly returned states).
func RedactFor(st *State, v Viewer) *State {
	if st == nil || v.Owner {
		return st
	}
	for seat, p := range st.Seats {
		if p == nil || v.SeesSeat(seat) {
			continue
		}
		p.HandKnown = nil
		p.Deck = nil
		p.LibraryComp = nil
		p.LibraryExact = false
	}
	return st
}

// SeatsForUser lists the seats an account holds in a game — the
// entitlement a scoped read hangs on. Empty means the account is not
// in the game; the caller decides whether that is a 404.
func (s *Store) SeatsForUser(ctx context.Context, gameID, user string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT position FROM mtg_seats WHERE game_id = ? AND user_id = ? ORDER BY position`, gameID, user)
	if err != nil {
		return nil, fmt.Errorf("load seats for user: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var seat int
		if err := rows.Scan(&seat); err != nil {
			return nil, err
		}
		out = append(out, seat)
	}
	return out, rows.Err()
}

/* ---------- joining ---------- */

// joinAlphabet is the non-ambiguous alphabet a spoken-at-a-table code
// needs: no 0/O, no 1/I, nothing two people read differently over a
// noisy room.
const joinAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// joinCodeLen is six characters — 30^6 ≈ 7×10^8 codes; plenty of
// entropy for a game night and still sayable in one breath.
const joinCodeLen = 6

// mintJoinCode draws six characters from the join alphabet.
func mintJoinCode() string {
	b := make([]byte, joinCodeLen)
	max := big.NewInt(int64(len(joinAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			// A broken CSPRNG has no honest fallback; a game join code
			// from a weak source is still fine, so degrade rather than
			// refuse the whole game creation.
			n = big.NewInt(int64(time.Now().UnixNano() % int64(len(joinAlphabet))))
		}
		b[i] = joinAlphabet[n.Int64()]
	}
	return string(b)
}

// NormalizeJoinCode canonicalizes a code as typed: trimmed, uppercased,
// dashes and spaces dropped. "abc-123" and "ABC123" are the same code.
func NormalizeJoinCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, "-", "")
	code = strings.ReplaceAll(code, " ", "")
	return code
}

// JoinGame redeems a join code: the participant is bound to a seat —
// their existing seat if they already hold one (idempotent), else a
// fresh seat appended after the table's last position, carrying their
// display name. Setup only: seating is what GAME_STARTED echoes, and a
// game already under way has no seat left to add. Games without a code
// (legacy rows from before the pod) are owner-only forever and answer
// not-found here, which is the honest reading of that fact.
func (s *Store) JoinGame(ctx context.Context, rawCode, userID, name string) (*Game, int, error) {
	if userID == "" {
		return nil, 0, fmt.Errorf("%w: joining needs a session", ErrInvalid)
	}
	code := NormalizeJoinCode(rawCode)
	if len(code) != joinCodeLen {
		return nil, 0, fmt.Errorf("%w: a join code is six characters", ErrInvalid)
	}
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM mtg_games WHERE join_code = ?`, code).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("mtg game %s: %w", code, ErrNotFound)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("find game by code: %w", err)
	}
	g, err := s.GetGame(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if g.Status != StatusSetup {
		return nil, 0, fmt.Errorf("%w: that game is already %s — seats close when play begins", ErrInvalid, g.Status)
	}
	// The owner joining their own game is a no-op that still answers
	// the game: they hold everything already.
	if g.OwnerID == userID {
		return g, 0, nil
	}
	// Idempotent: a seat already held is the join.
	if seats, err := s.SeatsForUser(ctx, g.ID, userID); err == nil && len(seats) > 0 {
		return g, seats[0], nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("join tx: %w", err)
	}
	defer tx.Rollback()
	// Re-check under the writer's lock: two tabs joining at once must
	// not mint two seats for one account.
	var held sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(position) FROM mtg_seats WHERE game_id = ? AND user_id = ?`, g.ID, userID).Scan(&held); err != nil {
		return nil, 0, fmt.Errorf("recheck held seat: %w", err)
	}
	if held.Valid {
		if err := tx.Commit(); err != nil {
			return nil, 0, fmt.Errorf("join commit: %w", err)
		}
		return g, int(held.Int64), nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mtg_seats (id, game_id, position, user_id, name, joined_at)
		VALUES (?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM mtg_seats WHERE game_id = ?), ?, ?, ?)`,
		uuid.NewString(), g.ID, g.ID, userID, name, s.now().UnixMilli()); err != nil {
		return nil, 0, fmt.Errorf("join seat: %w", err)
	}
	// A seat outranks an observer row (MAD-338): a spectator who takes a
	// chair stops being one, or the roster would count them twice.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM mtg_observers WHERE game_id = ? AND user_id = ?`, g.ID, userID); err != nil {
		return nil, 0, fmt.Errorf("drop observer row: %w", err)
	}
	var seat int
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(position) FROM mtg_seats WHERE game_id = ? AND user_id = ?`, g.ID, userID).Scan(&seat); err != nil {
		return nil, 0, fmt.Errorf("read joined seat: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("join commit: %w", err)
	}
	s.notify(g.ID)
	return g, seat, nil
}

/* ---------- private per-seat notes ---------- */

// noteCap bounds one seat's scratch — notes are a pad beside the game,
// not a document store. 64 KiB is a small novel of table talk.
const noteCap = 64 << 10

// SeatNote reads one seat's private scratch. mtg_seat_notes carries no
// audience column because it has no audience: the account bound to the
// seat, and — for a local seat no account holds — the owner whose
// client runs it. Not even the game's owner reads a seated player's
// notes; ADR 13's "the owner is entitled to everything" is about the
// game's hidden zones, and a player's pad is not a zone of the game.
func (s *Store) SeatNote(ctx context.Context, gameID string, seat int) (string, error) {
	if err := s.seatExists(ctx, gameID, seat); err != nil {
		return "", err
	}
	var body sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT body FROM mtg_seat_notes WHERE game_id = ? AND seat = ?`, gameID, seat).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read seat note: %w", err)
	}
	return body.String, nil
}

// SetSeatNote writes one seat's scratch, latest-wins. Never notified,
// never an event: nothing about a note is the game's, and the wake
// channel must not tell the table a seat is writing.
func (s *Store) SetSeatNote(ctx context.Context, gameID string, seat int, body string) error {
	if err := s.seatExists(ctx, gameID, seat); err != nil {
		return err
	}
	if len(body) > noteCap {
		return fmt.Errorf("%w: a seat note is capped at %d bytes", ErrInvalid, noteCap)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mtg_seat_notes (game_id, seat, body, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (game_id, seat) DO UPDATE SET body = excluded.body, updated_at = excluded.updated_at`,
		gameID, seat, body, s.now().UnixMilli()); err != nil {
		return fmt.Errorf("write seat note: %w", err)
	}
	return nil
}

// seatExists validates a seat row before anything hangs off it.
func (s *Store) seatExists(ctx context.Context, gameID string, seat int) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM mtg_seats WHERE game_id = ? AND position = ?`, gameID, seat).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: seat %d is not in this game", ErrInvalid, seat)
	}
	if err != nil {
		return fmt.Errorf("load seat: %w", err)
	}
	return nil
}
