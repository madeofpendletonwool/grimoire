// Session events: the chronological log of what happened at the table —
// Q&A, rulings, notes, discoveries, encounters. The in-play "+ DISCOVERY"
// button writes one of these; MAD-286's ruling log is kind 'ruling' here.

package gamesession

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Event is one entry in a session's log.
type Event struct {
	ID        string
	SessionID string
	Campaign  string // resolved through the session, for scope checks
	Seq       int64
	Kind      string
	Summary   string // the question or one-line label
	Detail    string // the ruling / answer / note body
	Payload   map[string]any
	CreatedAt time.Time
}

// AddEvent appends one event, assigning the next per-session seq inside the
// transaction so the log order is insertion order even under concurrent
// writes. summary is required for qa and ruling — the prior-ruling matcher
// matches against it, and a ruling nobody can scan is not a log.
func (s *Store) AddEvent(ctx context.Context, sessionID, kind, summary, detail string, payload map[string]any) (*Event, error) {
	if !validEvents[kind] {
		return nil, fmt.Errorf("%w: event kind %q", ErrInvalid, kind)
	}
	summary = strings.TrimSpace(summary)
	if (kind == EventQA || kind == EventRuling) && summary == "" {
		return nil, fmt.Errorf("%w: %s events need a question or summary", ErrInvalid, kind)
	}
	campaignID, err := s.sessionCampaign(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}
	ev := &Event{
		ID: uuid.NewString(), SessionID: sessionID, Campaign: campaignID,
		Kind: kind, Summary: summary, Detail: detail, Payload: payload,
		CreatedAt: time.Now().UTC(),
	}
	// The seq is assigned inside the INSERT itself: a single statement is
	// atomic, so two events logged at the same moment cannot take the same
	// position in the log.
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO session_events (id, session_id, seq, kind, summary, detail, payload, created_at)
		VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM session_events WHERE session_id = ?), ?, ?, ?, ?, ?)
		RETURNING seq`,
		ev.ID, ev.SessionID, ev.SessionID, ev.Kind, ev.Summary, ev.Detail,
		string(payloadJSON), ev.CreatedAt.UnixMilli()).Scan(&ev.Seq)
	if err != nil {
		return nil, fmt.Errorf("insert event: %w", err)
	}
	return ev, nil
}

// ListEvents returns a session's log in play order.
func (s *Store) ListEvents(ctx context.Context, sessionID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.session_id, gs.campaign_id, e.seq, e.kind, e.summary, e.detail, e.payload, e.created_at
		FROM session_events e
		JOIN game_sessions gs ON gs.id = e.session_id
		WHERE e.session_id = ?
		ORDER BY e.seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	return out, rows.Err()
}

// GetEvent returns one event by id.
func (s *Store) GetEvent(ctx context.Context, id string) (*Event, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT e.id, e.session_id, gs.campaign_id, e.seq, e.kind, e.summary, e.detail, e.payload, e.created_at
		FROM session_events e
		JOIN game_sessions gs ON gs.id = e.session_id
		WHERE e.id = ?`, id)
	ev, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: event %s", ErrNotFound, id)
	}
	return ev, err
}

func scanEvent(row interface{ Scan(...any) error }) (*Event, error) {
	var (
		ev           Event
		payloadJSON  string
		createdMilli int64
	)
	if err := row.Scan(&ev.ID, &ev.SessionID, &ev.Campaign, &ev.Seq, &ev.Kind,
		&ev.Summary, &ev.Detail, &payloadJSON, &createdMilli); err != nil {
		return nil, err
	}
	ev.Payload = map[string]any{}
	if payloadJSON != "" && payloadJSON != "{}" {
		_ = json.Unmarshal([]byte(payloadJSON), &ev.Payload)
	}
	ev.CreatedAt = time.UnixMilli(createdMilli).UTC()
	return &ev, nil
}

/* ---------- prior-ruling surfacing (retained from MAD-286) ---------- */

// PriorRuling is one past ruling the FTS matcher surfaced for a question.
type PriorRuling struct {
	EventID   string  `json:"event_id"`
	SessionID string  `json:"session_id"`
	Seq       int64   `json:"seq"`
	Ordinal   int64   `json:"session_ordinal"` // which sitting, by play order
	Session   string  `json:"session_name"`
	Summary   string  `json:"summary"`
	Detail    string  `json:"detail"`
	At        string  `json:"at"`
	Rank      float64 `json:"-"` // bm25 score, lower is better; ordering only
}

// MatchPriorRulings is the feature MAD-286 specified, shipped verbatim in
// spirit: FTS-match a question against the campaign's past ruling events and
// return the top matches — "you ruled the other way on this three sessions
// ago." No LLM involved.
//
// Only kind 'ruling' is matched back: a ruling is the DM's word, while a past
// Q&A is a hint, not precedent. excludeEventID drops the event currently
// being recorded (the caller just inserted it, and a question trivially
// matches itself).
func (s *Store) MatchPriorRulings(ctx context.Context, campaignID, question, excludeEventID string, limit int) ([]PriorRuling, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	q := ftsQuery(question)
	if q == "" {
		return nil, nil
	}
	exclude := excludeEventID
	if exclude == "" {
		exclude = "\x00" // never matches a uuid; keeps the SQL shape fixed
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.session_id, e.seq, gs.ordinal, gs.name, e.summary, e.detail, e.created_at,
		       bm25(ruling_fts) AS rank
		FROM ruling_fts f
		JOIN session_events e ON e.id = f.event_id
		JOIN game_sessions gs ON gs.id = e.session_id
		WHERE ruling_fts MATCH ?
		  AND f.campaign_id = ?
		  AND e.kind = 'ruling'
		  AND e.id <> ?
		ORDER BY rank, gs.ordinal DESC
		LIMIT ?`, q, campaignID, exclude, limit)
	if err != nil {
		return nil, fmt.Errorf("match prior rulings: %w", err)
	}
	defer rows.Close()
	var out []PriorRuling
	for rows.Next() {
		var (
			p     PriorRuling
			atMS  int64
			rankF float64
		)
		if err := rows.Scan(&p.EventID, &p.SessionID, &p.Seq, &p.Ordinal, &p.Session,
			&p.Summary, &p.Detail, &atMS, &rankF); err != nil {
			return nil, err
		}
		p.At = time.UnixMilli(atMS).UTC().Format(time.RFC3339)
		p.Rank = rankF
		out = append(out, p)
	}
	return out, rows.Err()
}

// ftsQuery turns free text into a safe FTS5 MATCH expression: every
// alphanumeric word is quoted (doubling embedded quotes) and the words are
// joined with OR, so recall wins and bm25 does the ranking. A user question
// is never parsed as an FTS query language expression.
func ftsQuery(text string) string {
	var terms []string
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicodeLetterDigit(r)
	}) {
		if w == "" {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " OR ")
}

func unicodeLetterDigit(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127
}
