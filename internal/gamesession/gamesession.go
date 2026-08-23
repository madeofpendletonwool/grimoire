// Package gamesession is the canon engine's input side: game sessions, the
// raw sources attached to them, and the chronological event log.
//
// MAD-286's table/session mode is absorbed here (ADR 7): the ruling log is
// one kind of session event rather than a parallel system, and the
// prior-ruling FTS surfacing ships verbatim — see MatchPriorRulings.
//
// The load-bearing constraint this package owns is the addressable span:
// sources are stored verbatim and immutable, and anything downstream refers
// to (source_id, span_start, span_end) byte offsets plus the quoted text.
// That quadruple is what makes "why does Grimoire think this?" resolve to
// actual words someone actually said; ResolveSpan is the function that
// resolves it back.
//
// The schema is owned by migration 0004; this package creates no tables,
// following the pattern internal/campaign set.
package gamesession

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// Errors. The session layer reuses the campaign vocabulary, the same way
// internal/knowledge does: callers branch on one set of sentinels across
// every campaign package.
var (
	ErrNotFound = campaign.ErrNotFound
	ErrInvalid  = campaign.ErrInvalid
)

/* ---------- vocabularies ---------- */

// Source kinds. Attribution matters: a player journal and a DM note carry
// different weight and can contradict each other on purpose.
const (
	SourceTranscript    = "transcript"
	SourceDMNotes       = "dm_notes"
	SourcePlayerJournal = "player_journal"
	SourceChatLog       = "chat_log"
	SourceLiveMark      = "live_mark"
)

// validSources is the set the CHECK constraint enforces, mirrored here so
// callers get a clean error instead of a constraint traceback.
var validSources = map[string]bool{
	SourceTranscript: true, SourceDMNotes: true, SourcePlayerJournal: true,
	SourceChatLog: true, SourceLiveMark: true,
}

// DMOnlySources are the kinds a non-DM member must not see. The filter is in
// the SQL of ListSources, not applied after loading (ADR 2: authorization
// happens in the query).
var DMOnlySources = map[string]bool{
	SourceDMNotes:  true,
	SourceLiveMark: true,
}

// Session status.
const (
	StatusPlanned = "planned"
	StatusLive    = "live"
	StatusDone    = "done"
)

var validStatus = map[string]bool{
	StatusPlanned: true, StatusLive: true, StatusDone: true,
}

// Event kinds. The ruling log from MAD-286 is one kind here.
const (
	EventQA        = "qa"
	EventRuling    = "ruling"
	EventNote      = "note"
	EventDiscovery = "discovery"
	EventEncounter = "encounter"
)

var validEvents = map[string]bool{
	EventQA: true, EventRuling: true, EventNote: true,
	EventDiscovery: true, EventEncounter: true,
}

/* ---------- the store ---------- */

// Store reads and writes the session layer on the shared database handle.
// The schema must already be applied (migrate.Up runs before anything
// serves).
type Store struct {
	db *sql.DB
}

// New builds a session store on an open, migrated database handle.
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("gamesession: nil database handle")
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying handle for later stages (the canon engine reads
// sources through it).
func (s *Store) DB() *sql.DB { return s.db }

/* ---------- sessions ---------- */

// Session is one played (or planned) sitting of a campaign.
type Session struct {
	ID        string
	Campaign  string
	Ordinal   int64
	Name      string
	Status    string
	StartedAt time.Time // zero when the session has not started
	EndedAt   time.Time // zero when the session has not ended
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateSession appends a session to a campaign. The ordinal is max+1 within
// the campaign, assigned inside the INSERT itself so two tables recording at
// once cannot take the same number. An empty name defaults to "Session
// <ordinal>".
func (s *Store) CreateSession(ctx context.Context, campaignID, name string) (*Session, error) {
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	ses := &Session{
		ID: uuid.NewString(), Campaign: campaignID,
		Status: StatusPlanned, CreatedAt: now, UpdatedAt: now,
	}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO game_sessions (id, campaign_id, ordinal, name, status, created_at, updated_at)
		VALUES (?, ?, (SELECT COALESCE(MAX(ordinal), 0) + 1 FROM game_sessions WHERE campaign_id = ?), '', ?, ?, ?)
		RETURNING ordinal`,
		ses.ID, ses.Campaign, ses.Campaign, ses.Status,
		ses.CreatedAt.UnixMilli(), ses.UpdatedAt.UnixMilli()).Scan(&ses.Ordinal)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	ses.Name = strings.TrimSpace(name)
	if ses.Name == "" {
		ses.Name = fmt.Sprintf("Session %d", ses.Ordinal)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE game_sessions SET name = ? WHERE id = ?`, ses.Name, ses.ID); err != nil {
		return nil, fmt.Errorf("name session: %w", err)
	}
	return ses, nil
}

const sessionCols = `s.id, s.campaign_id, s.ordinal, s.name, s.status, s.started_at, s.ended_at, s.created_at, s.updated_at`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var (
		ses              Session
		started, ended   sql.NullInt64
		created, updated int64
	)
	if err := row.Scan(&ses.ID, &ses.Campaign, &ses.Ordinal, &ses.Name, &ses.Status,
		&started, &ended, &created, &updated); err != nil {
		return nil, err
	}
	if started.Valid {
		ses.StartedAt = time.UnixMilli(started.Int64).UTC()
	}
	if ended.Valid {
		ses.EndedAt = time.UnixMilli(ended.Int64).UTC()
	}
	// Defensive default for a row whose naming UPDATE never landed (see
	// CreateSession): an unnamed session still reports its number.
	if strings.TrimSpace(ses.Name) == "" {
		ses.Name = fmt.Sprintf("Session %d", ses.Ordinal)
	}
	ses.CreatedAt = time.UnixMilli(created).UTC()
	ses.UpdatedAt = time.UnixMilli(updated).UTC()
	return &ses, nil
}

// GetSession returns one session by id. A session in another campaign is
// indistinguishable from a missing one: a 404 either way.
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM game_sessions s WHERE s.id = ?`, id)
	ses, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return ses, err
}

// ListSessions returns a campaign's sessions in play order.
func (s *Store) ListSessions(ctx context.Context, campaignID string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM game_sessions s WHERE s.campaign_id = ? ORDER BY s.ordinal`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		ses, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ses)
	}
	return out, rows.Err()
}

// UpdateSession replaces the mutable fields. nil pointers leave the
// corresponding field alone. Flipping status to live records started_at when
// it is unset; flipping to done records ended_at the same way — the DM should
// not have to type a timestamp to close a session.
func (s *Store) UpdateSession(ctx context.Context, id string, name, status *string) (*Session, error) {
	ses, err := s.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if name != nil {
		ses.Name = strings.TrimSpace(*name)
		if ses.Name == "" {
			return nil, fmt.Errorf("%w: session name is required", ErrInvalid)
		}
	}
	now := time.Now().UTC()
	if status != nil {
		st := strings.TrimSpace(*status)
		if !validStatus[st] {
			return nil, fmt.Errorf("%w: status %q", ErrInvalid, st)
		}
		if ses.Status == StatusDone && st != StatusDone {
			return nil, fmt.Errorf("%w: a done session cannot reopen", ErrInvalid)
		}
		ses.Status = st
		if st == StatusLive && ses.StartedAt.IsZero() {
			ses.StartedAt = now
		}
		if st == StatusDone && ses.EndedAt.IsZero() {
			ses.EndedAt = now
		}
	}
	ses.UpdatedAt = now
	var startedAt, endedAt any
	if !ses.StartedAt.IsZero() {
		startedAt = ses.StartedAt.UnixMilli()
	}
	if !ses.EndedAt.IsZero() {
		endedAt = ses.EndedAt.UnixMilli()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE game_sessions SET name = ?, status = ?, started_at = ?, ended_at = ?, updated_at = ?
		 WHERE id = ?`,
		ses.Name, ses.Status, startedAt, endedAt, ses.UpdatedAt.UnixMilli(), id)
	if err != nil {
		return nil, fmt.Errorf("update session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return ses, nil
}

// campaignExists reports ErrNotFound when the campaign row is missing, so a
// session for a foreign id is a 404 rather than a constraint traceback.
func (s *Store) campaignExists(ctx context.Context, id string) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM campaigns WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: campaign %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check campaign: %w", err)
	}
	return nil
}

// sessionCampaign resolves the campaign a session belongs to, for handlers
// that scope by campaign before touching the session.
func (s *Store) sessionCampaign(ctx context.Context, sessionID string) (string, error) {
	var cid string
	err := s.db.QueryRowContext(ctx,
		`SELECT campaign_id FROM game_sessions WHERE id = ?`, sessionID).Scan(&cid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
	}
	return cid, err
}
