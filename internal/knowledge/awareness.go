package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// Stance is what a knower holds relative to a fact.
const (
	// StanceKnows: the character has it right.
	StanceKnows = "knows"
	// StanceSuspects: believes it, not confirmed.
	StanceSuspects = "suspects"
	// StanceBelievesFalse: believes something that is not true — the
	// valuable one. The party is confidently wrong about the Duke.
	StanceBelievesFalse = "believes_false"
	// StanceUnaware: has not encountered it. Stored deliberately rather
	// than inferred from a missing row.
	StanceUnaware = "unaware"
)

// grantingStances are the stances that make a fact retrievable at a non-DM
// scope: the knower holds some version of it. Unaware is the explicit "no".
var grantingStances = map[string]bool{
	StanceKnows: true, StanceSuspects: true, StanceBelievesFalse: true,
}

var validStances = map[string]bool{
	StanceKnows: true, StanceSuspects: true, StanceBelievesFalse: true,
	StanceUnaware: true,
}

// stanceRank orders stances when a fact is granted to a scope's knower set at
// more than one stance (Thalia suspects, the party knows): the summary and
// the scoped read report the strongest held position, with the knower's own
// row preferred over the party's on a tie (Summarize only).
var stanceRank = map[string]int{
	StanceKnows: 0, StanceBelievesFalse: 1, StanceSuspects: 2,
}

// stanceTransitions is the legal movement between stances for one
// (knower, fact) pair. The empty string is "no row yet".
//
// Learning moves forward (unaware -> suspects -> knows, or any straight jump);
// correction moves sideways (believes_false -> knows is the "they had it
// wrong and now have it right" path); doubt re-opens a settled belief
// (knows -> suspects). Nothing moves back to unaware: a character does not
// un-learn a fact, and a fact that stops being true is retconned through
// SupersedeFact, not through the awareness rows.
var stanceTransitions = map[string]map[string]bool{
	"": {
		StanceKnows: true, StanceSuspects: true, StanceBelievesFalse: true, StanceUnaware: true,
	},
	StanceUnaware: {
		StanceKnows: true, StanceSuspects: true, StanceBelievesFalse: true, StanceUnaware: true,
	},
	StanceSuspects: {
		StanceKnows: true, StanceBelievesFalse: true, StanceSuspects: true,
	},
	StanceBelievesFalse: {
		StanceKnows: true, StanceSuspects: true, StanceBelievesFalse: true,
	},
	StanceKnows: {
		StanceSuspects: true, StanceBelievesFalse: true, StanceKnows: true,
	},
}

// CanTransition reports whether one stance may move to another. from is the
// current stance, or "" when no awareness row exists yet.
func CanTransition(from, to string) bool {
	if !validStances[to] {
		return false
	}
	allowed, ok := stanceTransitions[from]
	if !ok {
		return false
	}
	return allowed[to]
}

// Awareness is one (knower, fact) row: what the knower holds, how confident
// the record itself is (0–1 — this is the confidence of "they know it", not
// of the fact), the event that first put them in front of it, and the
// discovery that explains why Grimoire thinks so.
type Awareness struct {
	CampaignID  string
	Knower      string // entity id or campaign.PartyKnower
	FactID      string
	Stance      string
	Confidence  float64
	SinceEvent  string
	DiscoveryID string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const awarenessCols = `campaign_id, knower, fact_id, stance, confidence, since_event, discovery_id, created_at, updated_at`

func scanAwareness(row interface{ Scan(...any) error }) (*Awareness, error) {
	var (
		a         Awareness
		since     sql.NullString
		discovery sql.NullString
		createdMS int64
		updatedMS int64
	)
	if err := row.Scan(&a.CampaignID, &a.Knower, &a.FactID, &a.Stance, &a.Confidence,
		&since, &discovery, &createdMS, &updatedMS); err != nil {
		return nil, err
	}
	a.SinceEvent = since.String
	a.DiscoveryID = discovery.String
	a.CreatedAt = time.UnixMilli(createdMS).UTC()
	a.UpdatedAt = time.UnixMilli(updatedMS).UTC()
	return &a, nil
}

// getAwareness loads one awareness row; sql.ErrNoRows when there is none.
func (s *Store) getAwareness(ctx context.Context, tx *sql.Tx, campaignID, knower, factID string) (*Awareness, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+awarenessCols+` FROM awareness WHERE campaign_id = ? AND knower = ? AND fact_id = ?`,
		campaignID, knower, factID)
	a, err := scanAwareness(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return a, err
}

// setAwareness validates and writes one awareness row inside tx, enforcing
// the stance transition table. from "" (no row) any stance is legal; existing
// rows may only move along stanceTransitions.
func (s *Store) setAwareness(ctx context.Context, tx *sql.Tx, campaignID, knower, factID, stance string, confidence float64, sinceEvent, discoveryID string) (*Awareness, error) {
	if !validStances[stance] {
		return nil, fmt.Errorf("%w: stance %q", ErrInvalid, stance)
	}
	if confidence < 0 || confidence > 1 {
		return nil, fmt.Errorf("%w: confidence %f outside 0..1", ErrInvalid, confidence)
	}
	if err := validateKnowerTx(ctx, tx, knower, campaignID); err != nil {
		return nil, err
	}
	if err := factInCampaignTx(ctx, tx, factID, campaignID); err != nil {
		return nil, err
	}
	if sinceEvent != "" {
		if err := eventInCampaignTx(ctx, tx, sinceEvent, campaignID); err != nil {
			return nil, err
		}
	}
	if discoveryID != "" {
		if err := discoveryInCampaignTx(ctx, tx, discoveryID, campaignID); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	current, err := s.getAwareness(ctx, tx, campaignID, knower, factID)
	if errors.Is(err, sql.ErrNoRows) {
		a := &Awareness{
			CampaignID: campaignID, Knower: knower, FactID: factID, Stance: stance,
			Confidence: confidence, SinceEvent: sinceEvent, DiscoveryID: discoveryID,
			CreatedAt: now, UpdatedAt: now,
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO awareness (campaign_id, knower, fact_id, stance, confidence, since_event, discovery_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.CampaignID, a.Knower, a.FactID, a.Stance, a.Confidence,
			nullString(a.SinceEvent), nullString(a.DiscoveryID), now.UnixMilli(), now.UnixMilli()); err != nil {
			return nil, fmt.Errorf("insert awareness: %w", err)
		}
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if current.Stance == stance && current.Confidence == confidence &&
		(discoveryID == "" || discoveryID == current.DiscoveryID) {
		return current, nil // no-op; a repeat grant is not an error
	}
	if !CanTransition(current.Stance, stance) {
		return nil, fmt.Errorf("%w: %s cannot move %s -> %s on %s",
			ErrInvalid, knower, current.Stance, stance, factID)
	}
	if discoveryID != "" {
		current.DiscoveryID = discoveryID
	}
	if sinceEvent != "" {
		current.SinceEvent = sinceEvent
	}
	current.Stance = stance
	current.Confidence = confidence
	current.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `
		UPDATE awareness SET stance = ?, confidence = ?, since_event = ?, discovery_id = ?, updated_at = ?
		 WHERE campaign_id = ? AND knower = ? AND fact_id = ?`,
		current.Stance, current.Confidence, nullString(current.SinceEvent),
		nullString(current.DiscoveryID), now.UnixMilli(),
		campaignID, knower, factID); err != nil {
		return nil, fmt.Errorf("update awareness: %w", err)
	}
	return current, nil
}

// SetAwareness records what a knower holds for one fact. Transitions are
// validated against the stance table (see CanTransition); a repeat of the
// current stance and confidence is a no-op, not an error.
func (s *Store) SetAwareness(ctx context.Context, campaignID, knower, factID, stance string, confidence float64, sinceEvent, discoveryID string) (*Awareness, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("awareness tx: %w", err)
	}
	defer tx.Rollback()
	a, err := s.setAwareness(ctx, tx, campaignID, knower, factID, stance, confidence, sinceEvent, discoveryID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("awareness commit: %w", err)
	}
	return a, nil
}

// Awareness lists awareness rows. The DM scope reads any knower's rows (both
// filters optional); a non-DM scope reads only its own knowers' rows and is
// refused with ErrScope when asked for another knower's.
func (s *Store) Awareness(ctx context.Context, scope Scope, campaignID, knower, factID string) ([]Awareness, error) {
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	q := `SELECT ` + awarenessCols + ` FROM awareness WHERE campaign_id = ?`
	args := []any{campaignID}
	if !scope.IsDM() {
		ks := knowers(scope)
		q += ` AND knower IN ` + grantPlaceholders(len(ks))
		for _, k := range ks {
			args = append(args, k)
		}
		if knower != "" {
			own := false
			for _, k := range ks {
				if knower == k {
					own = true
					break
				}
			}
			if !own {
				return nil, fmt.Errorf("%w: %s may not read %s's awareness", ErrScope, scope, knower)
			}
		}
	}
	if knower != "" {
		q += ` AND knower = ?`
		args = append(args, knower)
	}
	if factID != "" {
		q += ` AND fact_id = ?`
		args = append(args, factID)
	}
	q += ` ORDER BY knower, fact_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list awareness: %w", err)
	}
	defer rows.Close()
	var out []Awareness
	for rows.Next() {
		a, err := scanAwareness(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

/* ---------- tx helpers over the graph tables ---------- */

// validateKnowerTx is validateKnower inside a transaction.
func validateKnowerTx(ctx context.Context, tx *sql.Tx, knower, campaignID string) error {
	if knower == campaign.PartyKnower {
		return nil
	}
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM entities WHERE id = ? AND campaign_id = ?`, knower, campaignID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: knower %s", ErrNotFound, knower)
	}
	if err != nil {
		return fmt.Errorf("check knower: %w", err)
	}
	if status == campaign.StatusDeleted {
		return fmt.Errorf("%w: knower %s is deleted", ErrInvalid, knower)
	}
	return nil
}

func factInCampaignTx(ctx context.Context, tx *sql.Tx, id, campaignID string) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM facts WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: fact %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check fact: %w", err)
	}
	return nil
}

func eventInCampaignTx(ctx context.Context, tx *sql.Tx, id, campaignID string) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM events WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: event %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check event: %w", err)
	}
	return nil
}

func discoveryInCampaignTx(ctx context.Context, tx *sql.Tx, id, campaignID string) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM discoveries WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: discovery %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check discovery: %w", err)
	}
	return nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// newID is the one uuid use in this file's inserts, kept local so the import
// list stays honest.
func newID() string { return uuid.NewString() }
