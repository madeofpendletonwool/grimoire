package engine

// The store: the single writer. It owns the mtg_* rows the migration
// defined and creates no tables of its own, following the pattern
// internal/campaign set — schema ships as migrations, packages ship
// behaviour. Submit folds the log from the database, applies the action,
// and appends the events with contiguous per-game ordinals in one
// transaction with the games.updated_at bump: that transaction is the
// whole multiplayer story (ADR 9) — one writer, ordinal replication, no
// conflict resolution.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/madeofpendletonwool/grimoire/internal/pubsub"
)

// Store persists games, seats and the event log.
type Store struct {
	db     *sql.DB
	now    func() time.Time
	broker *pubsub.Broker
}

// New builds a store on an open database handle. The store carries its
// own broker: game writes wake the game's SSE readers (MAD-326), and no
// campaign topic ever collides with a game id.
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("engine: nil database handle")
	}
	return &Store{db: db, now: time.Now().UTC, broker: pubsub.New()}, nil
}

// Subscribe wakes when anything writes this game — an append, a setup
// change or a rewind. The returned cancel must be called when the
// reader goes away.
func (s *Store) Subscribe(gameID string) (<-chan struct{}, func()) {
	return s.broker.Subscribe(gameID)
}

// notify pings the game's readers. Pings carry no data — a wake just
// means "re-query", so the push channel cannot widen what a reader sees.
func (s *Store) notify(gameID string) {
	s.broker.Notify(gameID)
}

// DB exposes the handle for callers wiring transactions around store
// operations — the same escape hatch the campaign store carries.
func (s *Store) DB() *sql.DB { return s.db }

// Game is one mtg_games row: the container. Lifecycle metadata only —
// current turn, phase, priority, who is alive are folds, never stored.
type Game struct {
	ID           string
	OwnerID      string
	Name         string
	Format       string
	StartingLife int
	Status       Status
	CreatedAt    time.Time
	UpdatedAt    time.Time
	StartedAt    *time.Time
	EndedAt      *time.Time
}

// CreateGame writes a setup game. Format defaults to commander and life
// to 40: the engine underneath is format-agnostic, the defaults are the
// product's first table.
func (s *Store) CreateGame(ctx context.Context, owner, name, format string, startingLife int) (*Game, error) {
	if format == "" {
		format = "commander"
	}
	if startingLife == 0 {
		startingLife = 40
	}
	now := s.now()
	g := &Game{ID: uuid.NewString(), OwnerID: owner, Name: name, Format: format,
		StartingLife: startingLife, Status: StatusSetup, CreatedAt: now, UpdatedAt: now}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mtg_games (id, owner_id, name, format, starting_life, status, settings, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '{}', ?, ?)`,
		g.ID, g.OwnerID, g.Name, g.Format, g.StartingLife, string(g.Status),
		g.CreatedAt.UnixMilli(), g.UpdatedAt.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert mtg game: %w", err)
	}
	s.notify(g.ID)
	return g, nil
}

// GetGame reads one game row.
func (s *Store) GetGame(ctx context.Context, id string) (*Game, error) {
	g := &Game{ID: id}
	var (
		status           string
		created, updated int64
		started, ended   sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT owner_id, name, format, starting_life, status, created_at, updated_at, started_at, ended_at
		  FROM mtg_games WHERE id = ?`, id).
		Scan(&g.OwnerID, &g.Name, &g.Format, &g.StartingLife, &status, &created, &updated, &started, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("mtg game %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get mtg game: %w", err)
	}
	g.Status = Status(status)
	g.CreatedAt, g.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	if started.Valid {
		t := time.UnixMilli(started.Int64).UTC()
		g.StartedAt = &t
	}
	if ended.Valid {
		t := time.UnixMilli(ended.Int64).UTC()
		g.EndedAt = &t
	}
	return g, nil
}

// ErrNotFound marks a game id that does not exist for the caller.
var ErrNotFound = errors.New("mtg game not found")

// ListGames reads one owner's games, most recently updated first — the
// account's game list. Another owner's games are absent from the rows,
// not filtered after the fact.
func (s *Store) ListGames(ctx context.Context, owner string) ([]*Game, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, owner_id, name, format, starting_life, status, created_at, updated_at, started_at, ended_at
		  FROM mtg_games WHERE owner_id = ? ORDER BY updated_at DESC, id`, owner)
	if err != nil {
		return nil, fmt.Errorf("list mtg games: %w", err)
	}
	defer rows.Close()
	var out []*Game
	for rows.Next() {
		g := &Game{}
		var (
			status           string
			created, updated int64
			started, ended   sql.NullInt64
		)
		if err := rows.Scan(&g.ID, &g.OwnerID, &g.Name, &g.Format, &g.StartingLife, &status,
			&created, &updated, &started, &ended); err != nil {
			return nil, err
		}
		g.Status = Status(status)
		g.CreatedAt, g.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
		if started.Valid {
			t := time.UnixMilli(started.Int64).UTC()
			g.StartedAt = &t
		}
		if ended.Valid {
			t := time.UnixMilli(ended.Int64).UTC()
			g.EndedAt = &t
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// LatestOrd reads the log head — the ordinal a stream resumes from when
// it has no cursor of its own.
func (s *Store) LatestOrd(ctx context.Context, gameID string) (int64, error) {
	var ord int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ord), 0) FROM mtg_events WHERE game_id = ?`, gameID).Scan(&ord); err != nil {
		return 0, fmt.Errorf("latest ord: %w", err)
	}
	return ord, nil
}

// EventIDAt reads one row's id at an ordinal — the stream's rewind
// sentinel: the row a cursor points at must still be the row the reader
// last saw, or the log was rewound and re-appended past it.
func (s *Store) EventIDAt(ctx context.Context, gameID string, ord int64) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM mtg_events WHERE game_id = ? AND ord = ?`, gameID, ord).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("mtg event %d: %w", ord, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("read event id: %w", err)
	}
	return id, nil
}

// SeatPlayer writes a seat row: position in turn order, a display name,
// optionally a bound user, an attached deck and the denormalized
// commander. Setup only — once the log begins, seats are what
// GAME_STARTED echoed.
func (s *Store) SeatPlayer(ctx context.Context, gameID string, position int, name, userID, deckID, commander string, startingLife *int) error {
	if position < 1 {
		return fmt.Errorf("%w: seat position must be positive", ErrInvalid)
	}
	g, err := s.GetGame(ctx, gameID)
	if err != nil {
		return err
	}
	if g.Status != StatusSetup {
		return fmt.Errorf("%w: cannot seat players once the game is %s", ErrInvalid, g.Status)
	}
	// A taken position is a rejected request, not a constraint violation
	// surfacing as a broken pipe — the API maps it onto ErrInvalid.
	var taken string
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM mtg_seats WHERE game_id = ? AND position = ?`, gameID, position).Scan(&taken)
	if err == nil {
		return fmt.Errorf("%w: seat %d is already taken", ErrInvalid, position)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load seat: %w", err)
	}
	// A seated deck with no explicit commander adopts the deck's own:
	// commander damage and tax bookkeeping need it on every cast.
	if commander == "" && deckID != "" {
		err := s.db.QueryRowContext(ctx,
			`SELECT commander FROM decks WHERE id = ?`, deckID).Scan(&commander)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: deck %s does not exist", ErrInvalid, deckID)
		}
		if err != nil {
			return fmt.Errorf("load deck: %w", err)
		}
	}
	var life any
	if startingLife != nil {
		life = *startingLife
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mtg_seats (id, game_id, position, user_id, name, deck_id, commander, starting_life, joined_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), gameID, position, nullString(userID), name, nullString(deckID),
		commander, life, s.now().UnixMilli()); err != nil {
		return fmt.Errorf("seat player: %w", err)
	}
	s.notify(gameID)
	return nil
}

// AttachDeck binds a deck to a seat — the known-card universe (MAD-329)
// and the library composition. When the seat has no explicit commander
// the deck's own commander is adopted, because commander damage and tax
// bookkeeping need it on every cast.
func (s *Store) AttachDeck(ctx context.Context, gameID string, position int, deckID string) error {
	g, err := s.GetGame(ctx, gameID)
	if err != nil {
		return err
	}
	if g.Status != StatusSetup {
		return fmt.Errorf("%w: cannot attach a deck once the game is %s", ErrInvalid, g.Status)
	}
	var seatCommander string
	err = s.db.QueryRowContext(ctx,
		`SELECT commander FROM mtg_seats WHERE game_id = ? AND position = ?`, gameID, position).Scan(&seatCommander)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: seat %d is not in game %s", ErrInvalid, position, gameID)
	}
	if err != nil {
		return fmt.Errorf("load seat: %w", err)
	}
	if seatCommander == "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT commander FROM decks WHERE id = ?`, deckID).Scan(&seatCommander)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: deck %s does not exist", ErrInvalid, deckID)
		}
		if err != nil {
			return fmt.Errorf("load deck: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE mtg_seats SET deck_id = ?, commander = ? WHERE game_id = ? AND position = ?`,
		deckID, seatCommander, gameID, position); err != nil {
		return fmt.Errorf("attach deck: %w", err)
	}
	s.notify(gameID)
	return nil
}

// deckEntry is one card line of a decks row's cards JSON — read here so
// the store never imports the deck package for one struct's shape.
type deckEntry struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Board string `json:"board"`
}

// StartGame loads the seat table, attaches each deck's composition, and
// submits the START_GAME action whose GAME_STARTED echo makes the log
// self-describing: seats, life, commanders, and what every library held
// when play began.
func (s *Store) StartGame(ctx context.Context, gameID string) ([]Event, *State, error) {
	g, err := s.GetGame(ctx, gameID)
	if err != nil {
		return nil, nil, err
	}
	if g.Status != StatusSetup {
		return nil, nil, fmt.Errorf("%w: game is already %s", ErrInvalid, g.Status)
	}
	seats, err := s.loadSeatConfigs(ctx, gameID)
	if err != nil {
		return nil, nil, err
	}
	action := Action{Kind: ActionStartGame, Source: "system", Seats: seats,
		Format: g.Format, StartingLife: g.StartingLife}
	return s.Submit(ctx, gameID, action)
}

// Seats reads the seat rows as the play surface's setup pane renders
// them: position, name, bound user, attached deck and commander. Setup
// facts only — once the game is active the GAME_STARTED echo in the fold
// is the seating, and this read is for rebuilding a setup in progress.
func (s *Store) Seats(ctx context.Context, gameID string) ([]SeatConfig, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT position, COALESCE(user_id, ''), name, COALESCE(deck_id, ''), commander, starting_life
		  FROM mtg_seats WHERE game_id = ? ORDER BY position`, gameID)
	if err != nil {
		return nil, fmt.Errorf("load seats: %w", err)
	}
	defer rows.Close()
	var out []SeatConfig
	for rows.Next() {
		var (
			sc           SeatConfig
			startingLife sql.NullInt64
		)
		if err := rows.Scan(&sc.Seat, &sc.UserID, &sc.Name, &sc.DeckID, &sc.Commander, &startingLife); err != nil {
			return nil, err
		}
		if startingLife.Valid {
			sc.StartingLife = int(startingLife.Int64)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// loadSeatConfigs reads the seat rows with their attached decks. Two
// passes on purpose: the app's database handle allows a single connection,
// so no query runs while another's rows are still open.
func (s *Store) loadSeatConfigs(ctx context.Context, gameID string) ([]SeatConfig, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT position, COALESCE(user_id, ''), name, COALESCE(deck_id, ''), commander, starting_life
		  FROM mtg_seats WHERE game_id = ? ORDER BY position`, gameID)
	if err != nil {
		return nil, fmt.Errorf("load seats: %w", err)
	}
	var out []SeatConfig
	for rows.Next() {
		var (
			sc           SeatConfig
			startingLife sql.NullInt64
		)
		if err := rows.Scan(&sc.Seat, &sc.UserID, &sc.Name, &sc.DeckID, &sc.Commander, &startingLife); err != nil {
			rows.Close()
			return nil, err
		}
		if startingLife.Valid {
			sc.StartingLife = int(startingLife.Int64)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].DeckID == "" {
			continue
		}
		comp, err := s.deckComposition(ctx, out[i].DeckID)
		if err != nil {
			return nil, err
		}
		out[i].Deck = comp
	}
	return out, nil
}

// deckComposition reads a deck's maindeck as a name → count multiset:
// the library's composition when play begins. Sideboard and commander
// entries stay off — the commander is in the command zone, and
// sideboards are not libraries.
func (s *Store) deckComposition(ctx context.Context, deckID string) (map[string]int, error) {
	var cards string
	err := s.db.QueryRowContext(ctx, `SELECT cards FROM decks WHERE id = ?`, deckID).Scan(&cards)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: deck %s does not exist", ErrInvalid, deckID)
	}
	if err != nil {
		return nil, fmt.Errorf("load deck cards: %w", err)
	}
	var entries []deckEntry
	if err := json.Unmarshal([]byte(cards), &entries); err != nil {
		return nil, fmt.Errorf("parse deck cards: %w", err)
	}
	comp := map[string]int{}
	for _, e := range entries {
		if e.Board == "sideboard" || e.Board == "commander" || e.Name == "" || e.Count <= 0 {
			continue
		}
		comp[e.Name] += e.Count
	}
	return comp, nil
}

// Submit is the writer: fold the log from the database, apply, append.
// A rejected action writes nothing — the transaction below only opens
// once Apply has produced events.
func (s *Store) Submit(ctx context.Context, gameID string, action Action) ([]Event, *State, error) {
	if _, err := s.GetGame(ctx, gameID); err != nil {
		return nil, nil, err
	}
	state, err := s.State(ctx, gameID)
	if err != nil {
		return nil, nil, err
	}
	evs, err := Apply(state, action)
	if err != nil {
		return nil, nil, err
	}
	persisted, err := s.append(ctx, gameID, action, evs)
	if err != nil {
		return nil, nil, err
	}
	s.notify(gameID)
	next := state.FoldInto(persisted)
	return persisted, next, nil
}

// append assigns ids, ords and timestamps, writes the rows, and derives
// the game row's lifecycle transitions from the events themselves — in
// one transaction, so ordinals are contiguous by construction and a gap
// can only mean corruption.
func (s *Store) append(ctx context.Context, gameID string, action Action, evs []Event) ([]Event, error) {
	if len(evs) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("append tx: %w", err)
	}
	defer tx.Rollback()

	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ord), 0) + 1 FROM mtg_events WHERE game_id = ?`, gameID).Scan(&next); err != nil {
		return nil, fmt.Errorf("next ord: %w", err)
	}
	out, hasStart, hasEnd, err := s.writeEvents(ctx, tx, gameID, action, evs, next)
	if err != nil {
		return nil, err
	}

	now := s.now().UnixMilli()
	sets, args := "updated_at = ?", []any{now}
	if hasEnd {
		sets += ", status = ?, started_at = COALESCE(started_at, ?), ended_at = ?"
		args = append(args, string(StatusFinished), now, now)
	} else if hasStart {
		sets += ", status = ?, started_at = COALESCE(started_at, ?), ended_at = NULL"
		args = append(args, string(StatusActive), now)
	}
	args = append(args, gameID)
	if _, err := tx.ExecContext(ctx, `UPDATE mtg_games SET `+sets+` WHERE id = ?`, args...); err != nil {
		return nil, fmt.Errorf("bump game: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("append commit: %w", err)
	}
	return out, nil
}

// writeEvents stamps ids, ords, timestamps, cause and batch onto the
// reducer's rows and inserts them inside the caller's transaction — the
// shared half of Submit and AmendAt. The batch stamp, one uuid per
// Submit, is what makes "the events one action produced" exactly
// addressable afterwards: cause strings repeat for identical
// consecutive actions, stamps never do.
func (s *Store) writeEvents(ctx context.Context, tx *sql.Tx, gameID string, action Action, evs []Event, firstOrd int64) ([]Event, bool, bool, error) {
	cause, err := json.Marshal(action)
	if err != nil {
		return nil, false, false, fmt.Errorf("encode cause: %w", err)
	}
	batch := uuid.NewString()
	now := s.now().UnixMilli()
	out := make([]Event, 0, len(evs))
	hasStart, hasEnd := false, false
	for i := range evs {
		e := evs[i]
		e.ID = uuid.NewString()
		e.Ord = firstOrd + int64(i)
		e.CreatedAt = now
		e.Batch = batch
		if e.Cause == "" {
			e.Cause = string(cause)
		}
		switch e.Kind {
		case EventGameStarted:
			hasStart = true
		case EventGameEnded:
			hasEnd = true
		}
		payload, err := json.Marshal(e)
		if err != nil {
			return nil, false, false, fmt.Errorf("encode event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mtg_events (id, game_id, ord, kind, actor_seat, source, cause, payload, visibility, visible_seat, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ID, gameID, e.Ord, string(e.Kind), nullInt(e.ActorSeat), sourceOrDefault(e.Source),
			e.Cause, string(payload), string(e.Visibility), nullInt(e.VisibleSeat), e.CreatedAt); err != nil {
			return nil, false, false, fmt.Errorf("insert event %d: %w", e.Ord, err)
		}
		out = append(out, e)
	}
	return out, hasStart, hasEnd, nil
}

// Events reads the log past an ordinal, oldest first. After 0 with
// limit 0 reads the whole log — the fold's input and the SSE replay's.
func (s *Store) Events(ctx context.Context, gameID string, after int64, limit int) ([]Event, error) {
	q := `SELECT id, ord, kind, actor_seat, source, cause, payload, visibility, visible_seat, created_at
		FROM mtg_events WHERE game_id = ? AND ord > ?`
	args := []any{gameID, after}
	if limit > 0 {
		q += ` ORDER BY ord LIMIT ?`
		args = append(args, limit)
	} else {
		q += ` ORDER BY ord`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// scanEvents decodes event rows — the read half every log query shares.
func scanEvents(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var (
			e              Event
			kind, vis      string
			actor, visible sql.NullInt64
			cause          string
			payload        string
		)
		if err := rows.Scan(&e.ID, &e.Ord, &kind, &actor, &e.Source, &cause, &payload, &vis, &visible, &e.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return nil, fmt.Errorf("parse event %d: %w", e.Ord, err)
		}
		e.Kind = EventKind(kind)
		if actor.Valid {
			e.ActorSeat = int(actor.Int64)
		}
		e.Cause = cause
		e.Visibility = Visibility(vis)
		if visible.Valid {
			e.VisibleSeat = int(visible.Int64)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// State folds the whole log — the engine's own view, seat-visible rows
// included, because it must, to serve a seat its own view. Which seat
// may see what is the query's business (ADR 13), never the fold's.
func (s *Store) State(ctx context.Context, gameID string) (*State, error) {
	evs, err := s.Events(ctx, gameID, 0, 0)
	if err != nil {
		return nil, err
	}
	return Fold(evs), nil
}

// RewindTo truncates the log past an ordinal and returns the refolded
// state: undo as truncate-and-refold, the whole correction story. The
// game row's lifecycle is re-derived from what survives — rewinding
// before GAME_STARTED hands the game back to setup — and mtg_rulings
// rows survive untouched, because a human record is never clobbered by
// a truncate.
func (s *Store) RewindTo(ctx context.Context, gameID string, ord int64) (*State, error) {
	if ord < 0 {
		return nil, fmt.Errorf("%w: rewind ordinal %d is negative", ErrInvalid, ord)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("rewind tx: %w", err)
	}
	defer tx.Rollback()
	var max int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ord), 0) FROM mtg_events WHERE game_id = ?`, gameID).Scan(&max); err != nil {
		return nil, fmt.Errorf("max ord: %w", err)
	}
	if ord > max {
		return nil, fmt.Errorf("%w: rewind ordinal %d is past the log head %d", ErrInvalid, ord, max)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM mtg_events WHERE game_id = ? AND ord > ?`, gameID, ord); err != nil {
		return nil, fmt.Errorf("truncate events: %w", err)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT kind FROM mtg_events WHERE game_id = ?`, gameID)
	if err != nil {
		return nil, fmt.Errorf("scan kinds: %w", err)
	}
	hasStart, hasEnd := false, false
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			rows.Close()
			return nil, err
		}
		switch EventKind(kind) {
		case EventGameStarted:
			hasStart = true
		case EventGameEnded:
			hasEnd = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mtg_games SET status = ?, updated_at = ?,
			started_at = CASE WHEN ? THEN started_at ELSE NULL END,
			ended_at   = CASE WHEN ? THEN ended_at ELSE NULL END
		WHERE id = ?`,
		lifecycle(hasStart, hasEnd).String(), s.now().UnixMilli(), hasStart, hasEnd, gameID); err != nil {
		return nil, fmt.Errorf("restate game: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("rewind commit: %w", err)
	}
	s.notify(gameID)
	return s.State(ctx, gameID)
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func sourceOrDefault(src string) string {
	if src == "" {
		return "system"
	}
	return src
}
