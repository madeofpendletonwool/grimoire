// Sources: the raw material a session is recorded from. Everything here is
// append-only — a source row is written once, verbatim, and never updated,
// because span offsets into it must stay valid forever. The immutability is
// structural: this file has no update function at all.

package gamesession

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Source is one raw document attached to a session: a transcript, DM notes,
// a player journal, a chat log, or a live mark typed at the table.
type Source struct {
	ID        string
	SessionID string
	Campaign  string // resolved through the session, for scope checks
	Kind      string
	Author    string
	Title     string
	// Content is stored verbatim. Span offsets everywhere in the campaign
	// layer are byte offsets into this exact string.
	Content  string
	Checksum string
	// Timing is the parsed cue list for .srt/.vtt sources, nil for plain
	// text. Kept on the row so a span can resolve back to a timestamp in
	// the recording (strictly more useful to a DM than one that cannot).
	Timing []Cue
	// ByteSize is len(Content); carried separately so list views can show
	// heft without loading the text.
	ByteSize int64
	// CreatedAt is when the source was ingested.
	CreatedAt time.Time
}

// AddSource ingests one source. The content must already be final: it is
// stored verbatim, checksummed, and never rewritten. Timing may be nil (plain
// text); the parser in ingest.go produces it for subtitle formats.
func (s *Store) AddSource(ctx context.Context, sessionID, kind, author, title, content string, timing []Cue) (*Source, error) {
	if !validSources[kind] {
		return nil, fmt.Errorf("%w: source kind %q", ErrInvalid, kind)
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("%w: source content is required", ErrInvalid)
	}
	campaignID, err := s.sessionCampaign(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(content))
	var timingJSON any
	if timing != nil {
		b, err := json.Marshal(timing)
		if err != nil {
			return nil, fmt.Errorf("encode timing: %w", err)
		}
		timingJSON = string(b)
	}
	src := &Source{
		ID: uuid.NewString(), SessionID: sessionID, Campaign: campaignID,
		Kind: kind, Author: strings.TrimSpace(author), Title: strings.TrimSpace(title),
		Content: content, Checksum: hex.EncodeToString(sum[:]), Timing: timing,
		ByteSize: int64(len(content)), CreatedAt: time.Now().UTC(),
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO session_sources (id, session_id, kind, author, title, content, checksum, timing, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		src.ID, src.SessionID, src.Kind, src.Author, src.Title, src.Content,
		src.Checksum, timingJSON, src.CreatedAt.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("insert source: %w", err)
	}
	return src, nil
}

const sourceCols = `src.id, src.session_id, gs.campaign_id, src.kind, src.author, src.title, src.checksum, src.timing, length(src.content), src.created_at`

// scanSourceMeta reads a source without its content — the list shape. Timing
// stays NULL in this projection; callers that need cues fetch the full row.
func scanSourceMeta(row interface{ Scan(...any) error }) (*Source, error) {
	var (
		src          Source
		timing       sql.NullString
		createdMilli int64
	)
	if err := row.Scan(&src.ID, &src.SessionID, &src.Campaign, &src.Kind, &src.Author,
		&src.Title, &src.Checksum, &timing, &src.ByteSize, &createdMilli); err != nil {
		return nil, err
	}
	src.CreatedAt = time.UnixMilli(createdMilli).UTC()
	return &src, nil
}

// ListSources returns a session's sources, oldest first. includeDMNotes is
// the role gate: DM-only kinds (dm_notes, live_mark) are filtered out in the
// SQL when it is false — a non-DM caller is never even handed the rows
// (ADR 2).
func (s *Store) ListSources(ctx context.Context, sessionID string, includeDMNotes bool) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+sourceCols+` FROM session_sources src
		JOIN game_sessions gs ON gs.id = src.session_id
		WHERE src.session_id = ? AND (? OR src.kind NOT IN ('dm_notes', 'live_mark'))
		ORDER BY src.created_at, src.id`,
		sessionID, includeDMNotes)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		src, err := scanSourceMeta(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *src)
	}
	return out, rows.Err()
}

// GetSource returns one source with its verbatim content and timing. A source
// in another campaign is indistinguishable from a missing one.
func (s *Store) GetSource(ctx context.Context, id string) (*Source, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+sourceCols+`, src.content FROM session_sources src
		JOIN game_sessions gs ON gs.id = src.session_id
		WHERE src.id = ?`, id)
	var (
		src          Source
		timing       sql.NullString
		createdMilli int64
	)
	if err := row.Scan(&src.ID, &src.SessionID, &src.Campaign, &src.Kind, &src.Author,
		&src.Title, &src.Checksum, &timing, &src.ByteSize, &createdMilli, &src.Content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: source %s", ErrNotFound, id)
		}
		return nil, err
	}
	src.CreatedAt = time.UnixMilli(createdMilli).UTC()
	if timing.Valid && timing.String != "" {
		_ = json.Unmarshal([]byte(timing.String), &src.Timing)
	}
	return &src, nil
}

/* ---------- addressable spans ---------- */

// Span is a verbatim quote resolved out of a source: the (source, offsets)
// quadruple everything downstream in the campaign layer cites, plus the text
// it resolves to. Start and End are byte offsets into the source content,
// half-open: content[start:end].
type Span struct {
	SourceID string `json:"source_id"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	Quote    string `json:"quote"`
}

// ResolveSpan resolves byte offsets back to the actual words — the function
// the whole "why does Grimoire think this?" path bottoms out in. The offsets
// must lie inside the source; anything else is ErrInvalid, never a truncated
// or empty quote.
func (s *Store) ResolveSpan(ctx context.Context, sourceID string, start, end int64) (*Span, error) {
	src, err := s.GetSource(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if start < 0 || end <= start || end > int64(len(src.Content)) {
		return nil, fmt.Errorf("%w: span [%d,%d) outside source of %d bytes",
			ErrInvalid, start, end, len(src.Content))
	}
	return &Span{
		SourceID: src.ID, Start: start, End: end,
		Quote: src.Content[start:end],
	}, nil
}

// LocateSpan is the reverse of ResolveSpan: given the quoted text, it finds
// the byte offsets of its first occurrence in a source. Extraction uses it to
// turn a model's quote into a real span; the round-trip test uses it to prove
// offsets and quote agree. A quote that is not verbatim in the source does
// not resolve — that is the span rule failing loudly, not a fallback.
func (s *Store) LocateSpan(ctx context.Context, sourceID, quote string) (*Span, error) {
	src, err := s.GetSource(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	span, ok := Locate(src.Content, quote)
	if !ok {
		return nil, fmt.Errorf("%w: quote not found verbatim in source %s", ErrNotFound, sourceID)
	}
	span.SourceID = sourceID
	return span, nil
}

// Locate finds quote's first occurrence in content and returns it as a span.
// Pure function: no DB, unit-testable.
func Locate(content, quote string) (*Span, bool) {
	if quote == "" {
		return nil, false
	}
	i := strings.Index(content, quote)
	if i < 0 {
		return nil, false
	}
	return &Span{Start: int64(i), End: int64(i + len(quote)), Quote: quote}, true
}
