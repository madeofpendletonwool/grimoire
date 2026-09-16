package engine

// Judges, spectators and the ruling log (MAD-338, stage 6 of MAD-321):
// the non-playing participant. An observer is an account with a role row
// in mtg_observers — judge or spectator — redeemed through the same join
// code a seat is, except an observer may join a live game, because a
// dispute is exactly when a judge arrives. Both roles read the public
// stream only: the zero Viewer (PublicViewer below) is already that
// entitlement, so the same SQL WHERE clause that scopes a seat scopes
// the observer, and the 6a leak gate covers the judge's surfaces for
// free.
//
// The ruling log is the other half: mtg_rulings, one row per ruling,
// anchored to the event ordinal it concerns. A ruling is a human record
// about the game, not an event of the game — it never rides the fold,
// never consumes an ordinal, and survives a rewind, because a judge's
// record that the truncate below it erased would be a record the game
// never kept (the model doc's contract, mirrored in RewindTo).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// The observer roles. A judge reads the public game and records
// rulings; a spectator reads the public game. Nothing else differs at
// the store's level — the ruling write is the one gate the server adds.
const (
	RoleJudge     = "judge"
	RoleSpectator = "spectator"
)

// ValidObserverRole reports whether role names an observer role.
func ValidObserverRole(role string) bool {
	return role == RoleJudge || role == RoleSpectator
}

// Observer is one mtg_observers row: an account's non-playing place at
// a game.
type Observer struct {
	ID       string `json:"id"`
	GameID   string `json:"game_id"`
	UserID   string `json:"user_id"`
	Role     string `json:"role"`
	Name     string `json:"name"`
	JoinedAt int64  `json:"joined_at"`
}

// ObserveGame redeems a join code for a non-playing role (MAD-338):
// the judge's and the spectator's front door. Unlike JoinGame there is
// no status gate — observers join live games, which is the point — and
// no seat is minted. Idempotent: an observer redeeming again keeps (or
// switches) their role. A seated account is refused; their seat already
// carries more than any observer row would. The owner observes nothing
// — they already see everything — and the call is a no-op that still
// answers the game.
func (s *Store) ObserveGame(ctx context.Context, rawCode, userID, name, role string) (*Game, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: joining needs a session", ErrInvalid)
	}
	if !ValidObserverRole(role) {
		return nil, fmt.Errorf("%w: an observer joins as judge or spectator", ErrInvalid)
	}
	code := NormalizeJoinCode(rawCode)
	if len(code) != joinCodeLen {
		return nil, fmt.Errorf("%w: a join code is six characters", ErrInvalid)
	}
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM mtg_games WHERE join_code = ?`, code).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("mtg game %s: %w", code, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("find game by code: %w", err)
	}
	g, err := s.GetGame(ctx, id)
	if err != nil {
		return nil, err
	}
	if g.OwnerID == userID {
		return g, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("observe tx: %w", err)
	}
	defer tx.Rollback()
	// A seat dominates an observer row: the account already reads more
	// than the public stream as a player.
	var seat sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(position) FROM mtg_seats WHERE game_id = ? AND user_id = ?`, g.ID, userID).Scan(&seat); err != nil {
		return nil, fmt.Errorf("check held seat: %w", err)
	}
	if seat.Valid {
		return nil, fmt.Errorf("%w: you already hold seat %d — a seated player is never an observer", ErrInvalid, int(seat.Int64))
	}
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM mtg_observers WHERE game_id = ? AND user_id = ?`, g.ID, userID).Scan(&existing)
	switch {
	case err == nil:
		// Idempotent redeem, possibly a role switch: the row is the
		// member's place at the table, latest-wins on role and name.
		if _, err := tx.ExecContext(ctx,
			`UPDATE mtg_observers SET role = ?, name = ? WHERE id = ?`, role, name, existing); err != nil {
			return nil, fmt.Errorf("update observer: %w", err)
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mtg_observers (id, game_id, user_id, role, name, joined_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), g.ID, userID, role, name, s.now().UnixMilli()); err != nil {
			return nil, fmt.Errorf("insert observer: %w", err)
		}
	default:
		return nil, fmt.Errorf("load observer: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("observe commit: %w", err)
	}
	s.notify(g.ID)
	return g, nil
}

// Observers lists a game's judges and spectators in join order — the
// room's roster of non-players, public to everyone already entitled to
// the game.
func (s *Store) Observers(ctx context.Context, gameID string) ([]Observer, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, game_id, user_id, role, name, joined_at
		  FROM mtg_observers WHERE game_id = ? ORDER BY joined_at, id`, gameID)
	if err != nil {
		return nil, fmt.Errorf("load observers: %w", err)
	}
	defer rows.Close()
	var out []Observer
	for rows.Next() {
		var o Observer
		if err := rows.Scan(&o.ID, &o.GameID, &o.UserID, &o.Role, &o.Name, &o.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ObserverRole answers the account's observer role at a game — "" when
// they are not an observer. The server's read scope and its ruling gate
// both hang off this one lookup.
func (s *Store) ObserverRole(ctx context.Context, gameID, userID string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM mtg_observers WHERE game_id = ? AND user_id = ?`, gameID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load observer role: %w", err)
	}
	return role, nil
}

/* ---------- the ruling log ---------- */

// rulingCap bounds one ruling — a paragraph that settles a dispute, not
// a document. 8 KiB is a generous page of judge's reasoning.
const rulingCap = 8 << 10

// Ruling is one mtg_rulings row: a judge's record, anchored to the
// ordinal it concerns. Ruler is the display name resolved from users at
// read time — the row itself stores only the account id, the same
// denormalization discipline the seat rows keep.
type Ruling struct {
	ID        string `json:"id"`
	GameID    string `json:"game_id"`
	Ord       int64  `json:"ord"`
	RuledBy   string `json:"ruled_by"`
	Ruler     string `json:"ruler,omitempty"`
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"`
}

// RecordRuling writes one ruling anchored to an event ordinal that must
// exist. The write is a row insert, never an event: the fold is the
// game, and a ruling is about the game. notify wakes the streams so the
// table sees the ruling land live.
func (s *Store) RecordRuling(ctx context.Context, gameID string, ord int64, note, userID string) (*Ruling, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil, fmt.Errorf("%w: a ruling needs its text", ErrInvalid)
	}
	if len(note) > rulingCap {
		return nil, fmt.Errorf("%w: a ruling is capped at %d bytes", ErrInvalid, rulingCap)
	}
	head, err := s.LatestOrd(ctx, gameID)
	if err != nil {
		return nil, err
	}
	if ord < 1 || ord > head {
		return nil, fmt.Errorf("%w: a ruling anchors to an ordinal in the log (1–%d), not %d", ErrInvalid, head, ord)
	}
	r := &Ruling{ID: uuid.NewString(), GameID: gameID, Ord: ord,
		RuledBy: userID, Note: note, CreatedAt: s.now().UnixMilli()}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mtg_rulings (id, game_id, ord, ruled_by, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.GameID, r.Ord, r.RuledBy, r.Note, r.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert ruling: %w", err)
	}
	s.notify(gameID)
	return s.rulingByID(ctx, r.ID)
}

// Rulings reads the game's ruling log, oldest first — the history that
// carries its own rulings, newest last like the fold reads its rows.
func (s *Store) Rulings(ctx context.Context, gameID string) ([]Ruling, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.game_id, r.ord, r.ruled_by, COALESCE(u.username, r.ruled_by, ''), r.note, r.created_at
		  FROM mtg_rulings r LEFT JOIN users u ON u.id = r.ruled_by
		 WHERE r.game_id = ? ORDER BY r.created_at, r.id`, gameID)
	if err != nil {
		return nil, fmt.Errorf("load rulings: %w", err)
	}
	defer rows.Close()
	var out []Ruling
	for rows.Next() {
		var r Ruling
		if err := rows.Scan(&r.ID, &r.GameID, &r.Ord, &r.RuledBy, &r.Ruler, &r.Note, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// rulingByID reads one ruling back with its ruler's name.
func (s *Store) rulingByID(ctx context.Context, id string) (*Ruling, error) {
	var r Ruling
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.game_id, r.ord, r.ruled_by, COALESCE(u.username, r.ruled_by, ''), r.note, r.created_at
		  FROM mtg_rulings r LEFT JOIN users u ON u.id = r.ruled_by
		 WHERE r.id = ?`, id).
		Scan(&r.ID, &r.GameID, &r.Ord, &r.RuledBy, &r.Ruler, &r.Note, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("mtg ruling %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read ruling: %w", err)
	}
	return &r, nil
}
