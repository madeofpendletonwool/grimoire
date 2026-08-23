package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Discovery is a first-class record of how somebody found something out:
// what fact, who found it, in which session, by what method ("read the
// mining ledger"), the source span when it came from recorded material, how
// confident the record is, and who accepted it as canon knowledge. This is
// the audit trail behind "why does Grimoire think Mira knows this?".
type Discovery struct {
	ID           string
	CampaignID   string
	FactID       string
	DiscoveredBy string // entity id or campaign.PartyKnower
	SessionID    string // game_sessions lands with the Stage 3 session layer (MAD-306)
	Method       string
	SourceID     string
	SpanStart    int64 // 0 when unset
	SpanEnd      int64 // 0 when unset
	Quote        string
	Confidence   float64
	AcceptedBy   string
	AcceptedAt   time.Time // zero when the discovery is still awaiting review
	CreatedAt    time.Time
}

// RecordDiscoveryInput is one discovery being recorded.
type RecordDiscoveryInput struct {
	CampaignID   string
	FactID       string
	DiscoveredBy string
	SessionID    string
	Method       string
	SourceID     string
	SpanStart    int64
	SpanEnd      int64
	Quote        string
	Confidence   float64
	AcceptedBy   string
	// Stance is what the discovery leaves the knower holding. A discovery
	// always grants: knows, suspects or believes_false. (A discovery that
	// leaves someone unaware is not a discovery.)
	Stance string
	// SinceEvent optionally ties the awareness row to the event the
	// discovery happened at.
	SinceEvent string
}

// RecordDiscovery writes a discovery and the awareness row that carries it in
// one transaction: a discovery without awareness would be an audit trail to
// a fact nobody knows, and awareness without a discovery is the
// awareness_without_source integrity finding.
//
// acceptedBy records the human who says this is real knowledge — at this
// stage recording a discovery IS accepting it (the DM typed it); the Stage 3
// canon engine will write candidate discoveries with AcceptedBy empty and
// leave accepting to the review queue.
func (s *Store) RecordDiscovery(ctx context.Context, in RecordDiscoveryInput) (*Discovery, error) {
	if !grantingStances[in.Stance] {
		return nil, fmt.Errorf("%w: a discovery leaves the knower %s, suspects or believes_false, not %q",
			ErrInvalid, StanceKnows, in.Stance)
	}
	if in.Confidence < 0 || in.Confidence > 1 {
		return nil, fmt.Errorf("%w: confidence %f outside 0..1", ErrInvalid, in.Confidence)
	}
	if in.SpanStart != 0 || in.SpanEnd != 0 {
		if in.SpanStart < 0 || in.SpanEnd <= in.SpanStart {
			return nil, fmt.Errorf("%w: span %d..%d is not a valid byte range", ErrInvalid, in.SpanStart, in.SpanEnd)
		}
	}
	in.Method = strings.TrimSpace(in.Method)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("discovery tx: %w", err)
	}
	defer tx.Rollback()

	if err := validateKnowerTx(ctx, tx, in.DiscoveredBy, in.CampaignID); err != nil {
		return nil, err
	}
	if err := factInCampaignTx(ctx, tx, in.FactID, in.CampaignID); err != nil {
		return nil, err
	}
	if in.SinceEvent != "" {
		if err := eventInCampaignTx(ctx, tx, in.SinceEvent, in.CampaignID); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	d := &Discovery{
		ID: uuid.NewString(), CampaignID: in.CampaignID, FactID: in.FactID,
		DiscoveredBy: in.DiscoveredBy, SessionID: in.SessionID, Method: in.Method,
		SourceID: in.SourceID, SpanStart: in.SpanStart, SpanEnd: in.SpanEnd,
		Quote: in.Quote, Confidence: in.Confidence,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO discoveries (id, campaign_id, fact_id, discovered_by, session_id, method,
		                         source_id, span_start, span_end, quote, confidence, accepted_by, accepted_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.CampaignID, d.FactID, d.DiscoveredBy, nullString(d.SessionID), d.Method,
		nullString(d.SourceID), nullInt64(d.SpanStart), nullInt64(d.SpanEnd), d.Quote,
		d.Confidence, in.AcceptedBy, now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert discovery: %w", err)
	}
	if in.AcceptedBy != "" {
		d.AcceptedBy = in.AcceptedBy
		d.AcceptedAt = now
	}

	// The awareness row the discovery grants. A knower who already holds a
	// stronger stance keeps it: RecordDiscovery transitions through the same
	// stance table as SetAwareness, and a repeat grant at the same stance is
	// a no-op.
	if _, err := s.setAwareness(ctx, tx, in.CampaignID, in.DiscoveredBy, in.FactID,
		in.Stance, in.Confidence, in.SinceEvent, d.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("discovery commit: %w", err)
	}
	d.CreatedAt = now
	return d, nil
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

const discoveryCols = `id, campaign_id, fact_id, discovered_by, session_id, method,
                       source_id, span_start, span_end, quote, confidence, accepted_by, accepted_at, created_at`

func scanDiscovery(row interface{ Scan(...any) error }) (*Discovery, error) {
	var (
		d          Discovery
		session    sql.NullString
		source     sql.NullString
		spanStart  sql.NullInt64
		spanEnd    sql.NullInt64
		acceptedBy sql.NullString
		acceptedAt sql.NullInt64
		createdMS  int64
	)
	if err := row.Scan(&d.ID, &d.CampaignID, &d.FactID, &d.DiscoveredBy, &session, &d.Method,
		&source, &spanStart, &spanEnd, &d.Quote, &d.Confidence, &acceptedBy, &acceptedAt, &createdMS); err != nil {
		return nil, err
	}
	d.SessionID = session.String
	d.SourceID = source.String
	d.SpanStart = spanStart.Int64
	d.SpanEnd = spanEnd.Int64
	d.AcceptedBy = acceptedBy.String
	if acceptedAt.Valid {
		d.AcceptedAt = time.UnixMilli(acceptedAt.Int64).UTC()
	}
	d.CreatedAt = time.UnixMilli(createdMS).UTC()
	return &d, nil
}

// GetDiscovery returns one discovery. The DM reads any; a non-DM scope reads
// only its own knowers' — another character's learning history is not theirs.
func (s *Store) GetDiscovery(ctx context.Context, scope Scope, campaignID, id string) (*Discovery, error) {
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	d, err := s.getDiscovery(ctx, scope, campaignID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: discovery %s", ErrNotFound, id)
	}
	return d, err
}

func (s *Store) getDiscovery(ctx context.Context, scope Scope, campaignID, id string) (*Discovery, error) {
	q := `SELECT ` + discoveryCols + ` FROM discoveries WHERE id = ? AND campaign_id = ?`
	args := []any{id, campaignID}
	if !scope.IsDM() {
		ks := knowers(scope)
		q += ` AND discovered_by IN ` + grantPlaceholders(len(ks))
		for _, k := range ks {
			args = append(args, k)
		}
	}
	row := s.db.QueryRowContext(ctx, q, args...)
	return scanDiscovery(row)
}

// Discoveries lists a campaign's discoveries, oldest first, optionally for
// one fact. Scoped like GetDiscovery. At the PlayerView the list is also
// narrowed to discoveries of non-secret facts (see playerview.go).
func (s *Store) Discoveries(ctx context.Context, scope Scope, campaignID, factID string) ([]Discovery, error) {
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	q := `SELECT ` + discoveryCols + ` FROM discoveries WHERE campaign_id = ?`
	args := []any{campaignID}
	if factID != "" {
		q += ` AND fact_id = ?`
		args = append(args, factID)
	}
	if !scope.IsDM() {
		ks := knowers(scope)
		q += ` AND discovered_by IN ` + grantPlaceholders(len(ks))
		for _, k := range ks {
			args = append(args, k)
		}
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list discoveries: %w", err)
	}
	defer rows.Close()
	var out []Discovery
	for rows.Next() {
		d, err := scanDiscovery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}
