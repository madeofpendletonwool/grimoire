package table

// The screen tokens (MAD-425): the share page's access model applied to
// a live surface. A token is 128 bits from crypto/rand in base64url —
// unguessable, carrying no sequential ids — minted and revoked by the
// DM, and it is the whole access model: the public endpoints ask for
// nothing but it, and a revoked token is a 410, not a pretense that the
// link never existed.
//
// The screens reference the campaign rather than snapshotting it: the
// rows are tiny, the state is derived on every read, and revocation is
// one UPDATE. last_seen_at is the projector's heartbeat, stamped where
// the token is resolved — best-effort, never a read's failure.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/pubsub"
)

// Errors callers branch on: Resolve returns ErrNotFound for an unknown
// token and ErrRevoked for a closed one, so the public surface can
// answer 404 and 410 the way the share page does.
var (
	ErrNotFound = errors.New("table screen not found")
	ErrRevoked  = errors.New("table screen has been revoked")
)

// Screen is one minted projector link.
type Screen struct {
	Token     string
	CreatedBy string
	CreatedAt time.Time
	RevokedAt time.Time // zero while the screen stands
	LastSeen  time.Time // zero until the screen first loads
}

// Store mints and resolves screen tokens and derives the screen's
// public reads. Construct with New and wire it once.
type Store struct {
	db        *sql.DB
	campaigns Campaigns
	boards    Boards
	combats   Combats
	rolls     Rolls
	broker    *pubsub.Broker
	now       func() time.Time
}

// New builds a table screen store. boards and campaigns are required —
// a screen that cannot read the board is no screen; combats and rolls
// wire optionally, the board's own partial-table convention (nil tests
// wire partial stacks and the screen renders what it has).
func New(db *sql.DB, campaigns Campaigns, boards Boards, combats Combats, rolls Rolls) (*Store, error) {
	if db == nil {
		return nil, errors.New("table: nil database handle")
	}
	if campaigns == nil {
		return nil, errors.New("table: the campaign store is required")
	}
	if boards == nil {
		return nil, errors.New("table: the board store is required")
	}
	return &Store{
		db: db, campaigns: campaigns, boards: boards, combats: combats, rolls: rolls,
		broker: pubsub.New(), now: time.Now().UTC,
	}, nil
}

// WithBroker moves the store onto the shared campaign broker — the
// same one every mechanical store pings, so the screen's streams wake
// with everyone else's.
func (s *Store) WithBroker(b *pubsub.Broker) *Store {
	if b != nil {
		s.broker = b
	}
	return s
}

// Subscribe is the screen's wake channel: anonymous, like the dice
// feed's — a projector is not a presence at the table, it is the table.
func (s *Store) Subscribe(campaignID string) (<-chan struct{}, func()) {
	return s.broker.Subscribe(campaignID)
}

/* ---------- minting and revoking ---------- */

// Mint creates a fresh screen link for the campaign. The DM may keep a
// few — the TV and the projector are different screens — so nothing
// dedupes; each token is its own revocable artifact, the share page's
// rule verbatim.
func (s *Store) Mint(ctx context.Context, campaignID, userID string) (string, error) {
	now := s.now().UnixMilli()
	var token string
	for attempt := 0; ; attempt++ {
		t, err := newToken()
		if err != nil {
			return "", err
		}
		token = t
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO table_screens (token, campaign_id, created_by, created_at) VALUES (?, ?, ?, ?)`,
			token, campaignID, userID, now)
		if err == nil {
			break
		}
		if attempt < 2 && strings.Contains(strings.ToLower(err.Error()), "unique") {
			continue
		}
		return "", fmt.Errorf("mint table screen: %w", err)
	}
	return token, nil
}

// List reads the campaign's screens, newest first, closed ones included
// so the DM sees what they ended.
func (s *Store) List(ctx context.Context, campaignID string) ([]Screen, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT token, created_by, created_at, COALESCE(revoked_at, 0), COALESCE(last_seen_at, 0)
		   FROM table_screens WHERE campaign_id = ?
		  ORDER BY created_at DESC`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list table screens: %w", err)
	}
	defer rows.Close()
	var out []Screen
	for rows.Next() {
		var (
			sc                Screen
			created           int64
			revoked, lastSeen int64
		)
		if err := rows.Scan(&sc.Token, &sc.CreatedBy, &created, &revoked, &lastSeen); err != nil {
			return nil, err
		}
		sc.CreatedAt = time.UnixMilli(created).UTC()
		sc.RevokedAt = time.UnixMilli(revoked).UTC()
		sc.LastSeen = time.UnixMilli(lastSeen).UTC()
		out = append(out, sc)
	}
	return out, rows.Err()
}

// Revoke closes a screen. Ownership rides on the campaign check the
// handler already made; an unknown token and an already-closed one are
// the same success-or-404 pair the share store keeps.
func (s *Store) Revoke(ctx context.Context, campaignID, token string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE table_screens SET revoked_at = ? WHERE token = ? AND campaign_id = ? AND revoked_at IS NULL`,
		s.now().UnixMilli(), token, campaignID)
	if err != nil {
		return fmt.Errorf("revoke table screen: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM table_screens WHERE token = ? AND campaign_id = ?`, token, campaignID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// Already revoked in this campaign is a success — the caller
		// wanted it closed, and it is.
	}
	return nil
}

// Resolve maps a token to its campaign, stamping the screen's heartbeat
// best-effort. A revoked screen is ErrRevoked so the public endpoints
// can say "the keeper closed this screen" instead of a bare 404.
func (s *Store) Resolve(ctx context.Context, token string) (string, error) {
	campaignID, err := s.state(ctx, token)
	if err != nil {
		return "", err
	}
	_, _ = s.db.ExecContext(ctx,
		`UPDATE table_screens SET last_seen_at = ? WHERE token = ?`, s.now().UnixMilli(), token)
	return campaignID, nil
}

// Check maps a token to its campaign without stamping the heartbeat —
// the stream's every-iteration read, so a closed screen is noticed on
// the next wake rather than the next ping.
func (s *Store) Check(ctx context.Context, token string) (string, error) {
	return s.state(ctx, token)
}

// state is the token's row, checked.
func (s *Store) state(ctx context.Context, token string) (string, error) {
	var (
		campaignID string
		revoked    sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT campaign_id, revoked_at FROM table_screens WHERE token = ?`, token).
		Scan(&campaignID, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve table screen: %w", err)
	}
	if revoked.Valid {
		return "", ErrRevoked
	}
	return campaignID, nil
}

// Notify pings the campaign's streams — the revoke path, so an open
// projector learns it is closed on the next wake instead of hanging on
// a dead token.
func (s *Store) Notify(campaignID string) {
	s.broker.Notify(campaignID)
}

// newToken mints a screen token: 16 bytes from crypto/rand, base64url
// without padding — 22 characters, URL-safe, no sequential ids for
// anyone to walk. The share store's own spelling.
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
