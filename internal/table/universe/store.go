package universe

// The cache-backed resolver: one db handle over mtg_name_resolutions,
// the per-game identity cache the model doc defines ("the same mumble is
// never re-inferred, never re-billed"). The table ships in migration
// 0014; this package owns only its behaviour, the pattern every mtg_*
// store follows — schema as migrations, behaviour as packages.
//
// The cache sits in front of resolution, not beside it: a hit answers
// without rebuilding the universe or touching the global index, and a
// miss resolves through the tiers and persists what it found. The cache
// survives rewind untouched (Store.RewindTo never deletes from it) — a
// corrected card is still the resolution of that mumble — and unresolved
// names are never cached, so a later, better-equipped attempt can try
// again.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// Store resolves spoken names for live games, with the cache in front.
type Store struct {
	db     *sql.DB
	global Global
	now    func() time.Time
}

// NewStore builds the resolver. global may be nil — an install without
// the card index loses the last tier only, exactly the way an install
// without decks loses the first ones: the game still works.
func NewStore(db *sql.DB, global Global) (*Store, error) {
	if db == nil {
		return nil, errors.New("universe: nil database handle")
	}
	return &Store{db: db, global: global, now: time.Now().UTC}, nil
}

// ErrInvalid marks a rejected request — nothing spoken, or a correction
// missing half of what it needs. Callers map it onto their 400s.
var ErrInvalid = errors.New("universe: invalid request")

// ResolveGame resolves one spoken name for one seat of a live game: the
// cache first, then the tiers. The universe is rebuilt from the fold on
// every miss — it is cheap, derived, and always current with the log.
func (s *Store) ResolveGame(ctx context.Context, st *engine.State, gameID string, seat int, spoken string) (Resolution, error) {
	spoken = strings.TrimSpace(spoken)
	if spoken == "" {
		return Resolution{}, fmt.Errorf("%w: nothing spoken", ErrInvalid)
	}
	key := carddb.NormalizeName(spoken)
	if r, ok := s.Cached(ctx, gameID, spoken); ok {
		return r, nil
	}
	res := FromState(st).Resolve(ctx, seat, spoken, s.global)
	if res.Resolved() && key != "" {
		if err := s.write(ctx, gameID, key, res.Card, res.Method, res.Confidence); err != nil {
			return res, err // the resolution stands; the cache is an optimization
		}
	}
	return res, nil
}

// Cached reads one cached resolution back, normalized spoken → card.
func (s *Store) Cached(ctx context.Context, gameID, spoken string) (Resolution, bool) {
	key := carddb.NormalizeName(spoken)
	if key == "" {
		return Resolution{}, false
	}
	var (
		card       string
		method     string
		confidence float64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT card_name, method, confidence FROM mtg_name_resolutions WHERE game_id = ? AND spoken = ?`,
		gameID, key).Scan(&card, &method, &confidence)
	if err != nil {
		return Resolution{}, false
	}
	r := Resolution{Spoken: strings.TrimSpace(spoken), Card: card,
		Method: Method(method), Confidence: confidence, Scope: ScopeCache}
	if !r.Resolved() {
		return Resolution{}, false
	}
	return r, true
}

// Record writes a human correction — the "no, Tithe" path (4c's amend,
// the manual tier). A corrected card is still the resolution of that
// mumble, at full confidence: a person said what they meant.
func (s *Store) Record(ctx context.Context, gameID, spoken, card string) error {
	key := carddb.NormalizeName(spoken)
	if key == "" || strings.TrimSpace(card) == "" {
		return fmt.Errorf("%w: a correction needs both the spoken name and the card", ErrInvalid)
	}
	return s.write(ctx, gameID, key, strings.TrimSpace(card), MethodManual, ConfManual)
}

// write is the single INSERT the cache ever does. OR REPLACE because the
// cache miss and a concurrent correction can race, and the correction —
// being human — is the winner either way if it lands last.
func (s *Store) write(ctx context.Context, gameID, key, card string, method Method, confidence float64) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO mtg_name_resolutions (game_id, spoken, card_name, method, confidence, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		gameID, key, card, string(method), confidence, s.now().UnixMilli()); err != nil {
		return fmt.Errorf("cache name resolution: %w", err)
	}
	return nil
}
