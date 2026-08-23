package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Event is one thing that happened, on two timelines at once: clock_at is the
// in-world day it happened at (NULL when unknown), real_ordinal is the order
// the table actually played through it. They genuinely diverge — flashbacks,
// split parties, a session that covers three in-world weeks — and collapsing
// them loses a question people actually ask.
type Event struct {
	ID             string
	CampaignID     string
	SessionID      string // game_sessions rows land with the Stage 3 session layer (MAD-306)
	Summary        string
	ClockAt        *int64 // in-world day on the campaign clock; nil when unknown
	RealOrdinal    int64  // play order within the campaign, assigned on create
	LocationEntity string
	Participants   []EventParticipant
	Links          []EventLinkRef
	CreatedAt      time.Time
}

// EventParticipant is who was there and in what capacity.
type EventParticipant struct {
	ID       string
	EventID  string
	EntityID string
	Role     string
}

// EventLinkRef is a causal edge between events: caused, enabled or revealed.
type EventLinkRef struct {
	ID        string
	FromEvent string
	ToEvent   string
	Link      string
}

// CreateEvent records something that happened. real_ordinal is assigned as
// the next play-order slot in the campaign; clock_at is whatever in-world day
// the DM says it happened at, independently.
func (s *Store) CreateEvent(ctx context.Context, campaignID, sessionID, summary string, clockAt *int64, locationEntity string) (*Event, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil, fmt.Errorf("%w: event summary is required", ErrInvalid)
	}
	if locationEntity != "" {
		e, err := s.entityInCampaign(ctx, locationEntity, campaignID)
		if err != nil {
			return nil, err
		}
		if e.Status == StatusDeleted {
			return nil, fmt.Errorf("%w: location entity %s is deleted", ErrInvalid, locationEntity)
		}
	}
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	e := &Event{
		ID: uuid.NewString(), CampaignID: campaignID, SessionID: sessionID,
		Summary: summary, ClockAt: clockAt, LocationEntity: locationEntity,
		CreatedAt: now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("event tx: %w", err)
	}
	defer tx.Rollback()
	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(real_ordinal), 0) + 1 FROM events WHERE campaign_id = ?`, campaignID).Scan(&next); err != nil {
		return nil, fmt.Errorf("next ordinal: %w", err)
	}
	e.RealOrdinal = next
	var clock any
	if e.ClockAt != nil {
		clock = *e.ClockAt
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events (id, campaign_id, session_id, summary, clock_at, real_ordinal, location_entity, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.CampaignID, nullString(e.SessionID), e.Summary, clock, e.RealOrdinal,
		nullString(e.LocationEntity), now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("event commit: %w", err)
	}
	return e, nil
}

func (s *Store) eventInCampaign(ctx context.Context, id, campaignID string) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM events WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: event %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check event: %w", err)
	}
	return nil
}

const eventCols = `id, campaign_id, session_id, summary, clock_at, real_ordinal, location_entity, created_at`

func scanEvent(row interface{ Scan(...any) error }) (*Event, error) {
	var (
		e        Event
		session  sql.NullString
		clock    sql.NullInt64
		location sql.NullString
		created  int64
	)
	if err := row.Scan(&e.ID, &e.CampaignID, &session, &e.Summary, &clock, &e.RealOrdinal,
		&location, &created); err != nil {
		return nil, err
	}
	e.SessionID = session.String
	if clock.Valid {
		v := clock.Int64
		e.ClockAt = &v
	}
	e.LocationEntity = location.String
	e.CreatedAt = time.UnixMilli(created).UTC()
	return &e, nil
}

// GetEvent returns one event with its participants and links attached.
// DM-scope reads only: which events a perspective witnessed is awareness,
// enforced in internal/knowledge.
func (s *Store) GetEvent(ctx context.Context, scope Scope, campaignID, id string) (*Event, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE id = ? AND campaign_id = ?`, id, campaignID)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: event %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return s.attachEventDetail(ctx, e)
}

// attachEventDetail loads participants and links onto an event.
func (s *Store) attachEventDetail(ctx context.Context, e *Event) (*Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, event_id, entity_id, role FROM event_participants WHERE event_id = ? ORDER BY entity_id`, e.ID)
	if err != nil {
		return nil, fmt.Errorf("load participants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p EventParticipant
		if err := rows.Scan(&p.ID, &p.EventID, &p.EntityID, &p.Role); err != nil {
			return nil, err
		}
		e.Participants = append(e.Participants, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `
		SELECT id, from_event, to_event, link FROM event_links
		 WHERE from_event = ? OR to_event = ? ORDER BY from_event, to_event`, e.ID, e.ID)
	if err != nil {
		return nil, fmt.Errorf("load links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l EventLinkRef
		if err := rows.Scan(&l.ID, &l.FromEvent, &l.ToEvent, &l.Link); err != nil {
			return nil, err
		}
		e.Links = append(e.Links, l)
	}
	return e, rows.Err()
}

// ListEvents returns the campaign timeline in play order (real_ordinal),
// detail attached. DM-scope reads only; the scoped timeline a perspective
// witnessed lives in internal/knowledge.
func (s *Store) ListEvents(ctx context.Context, scope Scope, campaignID string) ([]Event, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventCols+` FROM events WHERE campaign_id = ? ORDER BY real_ordinal`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if _, err := s.attachEventDetail(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AddParticipant records that an entity was present at an event. A repeat
// add is ErrAlreadyExists.
func (s *Store) AddParticipant(ctx context.Context, campaignID, eventID, entityID, role string) error {
	if _, err := s.entityInCampaign(ctx, entityID, campaignID); err != nil {
		return err
	}
	if err := s.eventInCampaign(ctx, eventID, campaignID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO event_participants (id, event_id, entity_id, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		uuid.NewString(), eventID, entityID, role, time.Now().UTC().UnixMilli())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return fmt.Errorf("%w: entity %s already participates in event %s", ErrAlreadyExists, entityID, eventID)
		}
		return fmt.Errorf("insert participant: %w", err)
	}
	return nil
}

// LinkEvents records a causal edge from one event to another: caused, enabled
// or revealed. An event cannot cause itself, and the same link twice is
// ErrAlreadyExists.
func (s *Store) LinkEvents(ctx context.Context, campaignID, from, to, link string) error {
	if !validLinks[link] {
		return fmt.Errorf("%w: link %q", ErrInvalid, link)
	}
	if from == to {
		return fmt.Errorf("%w: an event cannot link to itself", ErrInvalid)
	}
	if err := s.eventInCampaign(ctx, from, campaignID); err != nil {
		return err
	}
	if err := s.eventInCampaign(ctx, to, campaignID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO event_links (id, from_event, to_event, link, created_at) VALUES (?, ?, ?, ?, ?)`,
		uuid.NewString(), from, to, link, time.Now().UTC().UnixMilli())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return fmt.Errorf("%w: %s link %s -> %s", ErrAlreadyExists, link, from, to)
		}
		return fmt.Errorf("insert link: %w", err)
	}
	return nil
}
