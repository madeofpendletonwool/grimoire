package knowledge

/*
Rumours with truth values, and who is repeating them (MAD-374).

A rumour is a belief in circulation — the epistemic layer's business, which
is why the store lives here and not in a package of its own. The schema is
owned by migration 0024; the shape is campaign.Rumor.

The one rule this file exists to enforce, same as every other read here
(ADR 2): truth is DM-only, always. A non-DM scope's SELECT does not merely
filter rows — it selects truth and fact_id as empty strings, so the columns
never cross the scope line at all, and the DM-only rumour (dm_only = 1) is
excluded from the row set entirely. The reflection leak test sweeps these
methods and asserts it; a retrieval added later that forgets fails there
rather than shipping.

Hearing a rumour (RumorHeard) is a stance write like any other, routed
through SetAwareness so the transition table governs every move: a true
rumour leaves suspects (hearsay is not knowledge; the DM upgrades it when
the party confirms it), a false or distorted one naming its fact leaves
believes_false — the case the feature exists for — and a fact-less rumour
carries on rumor_holders alone, because awareness's fact foreign key cannot
express "a belief with no fact under it" (documented in 0024, not
engineered around). A knower who already knows the fact is left alone:
gossip never downgrades knowledge.
*/

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

/* ---------- vocabulary ---------- */

// validRumorTruth / validRumorSpread / validRumorStatus validate the three
// controlled vocabularies migration 0024's CHECKs constrain. Duplicated
// here so a bad value surfaces as ErrInvalid before it surfaces as a
// constraint error.
var (
	validRumorTruth = map[string]bool{
		campaign.RumorTruthTrue: true, campaign.RumorTruthFalse: true, campaign.RumorTruthDistorted: true,
	}
	validRumorSpread = map[string]bool{
		campaign.RumorSpreadLocal: true, campaign.RumorSpreadRegional: true, campaign.RumorSpreadWidespread: true,
	}
	validRumorStatus = map[string]bool{
		campaign.RumorStatusCirculating: true, campaign.RumorStatusDebunked: true,
		campaign.RumorStatusConfirmed: true, campaign.RumorStatusDormant: true,
	}
)

// rumorConfidence is the confidence a heard rumour's stance row carries:
// hearsay, believed but not confirmed. It is the confidence of "they hold
// it", not of the fact.
const rumorConfidence = 0.6

/* ---------- the stored shape ---------- */

// rumorColsDM is the DM read: every column, truth included.
const rumorColsDM = `id, campaign_id, statement, truth, about_entity, fact_id,
                     origin, spread, status, dm_only, created_by, created_at, updated_at`

// rumorColsPlayer is the non-DM read: truth and fact_id are selected as
// empty strings — the columns never load, so no caller and no model
// downstream of a caller can be handed them by mistake.
const rumorColsPlayer = `id, campaign_id, statement, '' AS truth, about_entity, '' AS fact_id,
                         origin, spread, status, dm_only, created_by, created_at, updated_at`

func scanRumor(row interface{ Scan(...any) error }) (*campaign.Rumor, error) {
	var (
		r             campaign.Rumor
		about, factID sql.NullString
		created, upd  int64
	)
	if err := row.Scan(&r.ID, &r.CampaignID, &r.Statement, &r.Truth, &about, &factID,
		&r.Origin, &r.Spread, &r.Status, &r.DMOnly, &r.CreatedBy, &created, &upd); err != nil {
		return nil, err
	}
	r.AboutEntity = about.String
	r.FactID = factID.String
	r.CreatedAt = time.UnixMilli(created).UTC()
	r.UpdatedAt = time.UnixMilli(upd).UTC()
	return &r, nil
}

/* ---------- writes (the DM paths) ---------- */

// RumorInput is one hand-authored rumour. Truth is required — a rumour
// without a truth value is a fact. FactID is the canon fact the rumour
// attests (truth true) or distorts (distorted); it may be empty, in which
// case hearing the rumour grants no stance and the knower carries it as a
// holding on rumor_holders.
type RumorInput struct {
	Statement   string
	Truth       string
	AboutEntity string
	FactID      string
	Origin      string
	Spread      string
	Status      string
	DMOnly      bool
	CreatedBy   string
}

// CreateRumor writes one rumour. DM-gated at the handler; like every other
// write here the store validates shape, not perspective.
func (s *Store) CreateRumor(ctx context.Context, campaignID string, in RumorInput) (*campaign.Rumor, error) {
	in.Statement = strings.TrimSpace(in.Statement)
	if in.Statement == "" {
		return nil, fmt.Errorf("%w: a rumour needs a statement", ErrInvalid)
	}
	if !validRumorTruth[in.Truth] {
		return nil, fmt.Errorf("%w: truth %q", ErrInvalid, in.Truth)
	}
	if in.Spread == "" {
		in.Spread = campaign.RumorSpreadLocal
	}
	if !validRumorSpread[in.Spread] {
		return nil, fmt.Errorf("%w: spread %q", ErrInvalid, in.Spread)
	}
	if in.Status == "" {
		in.Status = campaign.RumorStatusCirculating
	}
	if !validRumorStatus[in.Status] {
		return nil, fmt.Errorf("%w: status %q", ErrInvalid, in.Status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("rumor tx: %w", err)
	}
	defer tx.Rollback()
	if in.AboutEntity != "" {
		if err := entityInCampaignTx(ctx, tx, in.AboutEntity, campaignID); err != nil {
			return nil, err
		}
	}
	if in.FactID != "" {
		if err := factInCampaignTx(ctx, tx, in.FactID, campaignID); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	r := &campaign.Rumor{
		ID: uuid.NewString(), CampaignID: campaignID, Statement: in.Statement, Truth: in.Truth,
		AboutEntity: in.AboutEntity, FactID: in.FactID, Origin: in.Origin, Spread: in.Spread,
		Status: in.Status, DMOnly: in.DMOnly, CreatedBy: in.CreatedBy, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rumors (id, campaign_id, statement, truth, about_entity, fact_id,
		                    origin, spread, status, dm_only, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.CampaignID, r.Statement, r.Truth, nullString(r.AboutEntity), nullString(r.FactID),
		r.Origin, r.Spread, r.Status, r.DMOnly, r.CreatedBy, now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert rumor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("rumor commit: %w", err)
	}
	return r, nil
}

// RumorUpdate patches one rumour. nil leaves a field alone; a pointer to ""
// clears AboutEntity / FactID. Truth may be corrected — the DM settles
// what the rumour actually was.
type RumorUpdate struct {
	Statement   *string
	Truth       *string
	AboutEntity *string
	FactID      *string
	Origin      *string
	Spread      *string
	Status      *string
	DMOnly      *bool
}

// UpdateRumor applies a patch and returns the rumour as it stands.
func (s *Store) UpdateRumor(ctx context.Context, campaignID, rumorID string, up RumorUpdate) (*campaign.Rumor, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("rumor tx: %w", err)
	}
	defer tx.Rollback()
	current, err := rumorInCampaignTx(ctx, tx, rumorID, campaignID)
	if err != nil {
		return nil, err
	}
	if up.Statement != nil {
		v := strings.TrimSpace(*up.Statement)
		if v == "" {
			return nil, fmt.Errorf("%w: a rumour needs a statement", ErrInvalid)
		}
		current.Statement = v
	}
	if up.Truth != nil {
		if !validRumorTruth[*up.Truth] {
			return nil, fmt.Errorf("%w: truth %q", ErrInvalid, *up.Truth)
		}
		current.Truth = *up.Truth
	}
	if up.AboutEntity != nil {
		v := strings.TrimSpace(*up.AboutEntity)
		if v != "" {
			if err := entityInCampaignTx(ctx, tx, v, campaignID); err != nil {
				return nil, err
			}
		}
		current.AboutEntity = v
	}
	if up.FactID != nil {
		v := strings.TrimSpace(*up.FactID)
		if v != "" {
			if err := factInCampaignTx(ctx, tx, v, campaignID); err != nil {
				return nil, err
			}
		}
		current.FactID = v
	}
	if up.Origin != nil {
		current.Origin = *up.Origin
	}
	if up.Spread != nil {
		if !validRumorSpread[*up.Spread] {
			return nil, fmt.Errorf("%w: spread %q", ErrInvalid, *up.Spread)
		}
		current.Spread = *up.Spread
	}
	if up.Status != nil {
		if !validRumorStatus[*up.Status] {
			return nil, fmt.Errorf("%w: status %q", ErrInvalid, *up.Status)
		}
		current.Status = *up.Status
	}
	if up.DMOnly != nil {
		current.DMOnly = *up.DMOnly
	}
	now := time.Now().UTC()
	current.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `
		UPDATE rumors SET statement = ?, truth = ?, about_entity = ?, fact_id = ?,
		                  origin = ?, spread = ?, status = ?, dm_only = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ?`,
		current.Statement, current.Truth, nullString(current.AboutEntity), nullString(current.FactID),
		current.Origin, current.Spread, current.Status, current.DMOnly, now.UnixMilli(),
		rumorID, campaignID); err != nil {
		return nil, fmt.Errorf("update rumor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("rumor commit: %w", err)
	}
	return current, nil
}

// DeleteRumor removes a rumour and its holders. Rumours are not canon —
// nothing is built on them and the retcon ledger does not apply — so a
// delete is a delete.
func (s *Store) DeleteRumor(ctx context.Context, campaignID, rumorID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM rumors WHERE id = ? AND campaign_id = ?`, rumorID, campaignID)
	if err != nil {
		return fmt.Errorf("delete rumor: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: rumor %s", ErrNotFound, rumorID)
	}
	return nil
}

/* ---------- holders ---------- */

// SetRumorHolder records who repeats a rumour, in their own words. The
// upsert is the holder's wording changing as the story drifts, not a new
// row. EntityID is an entity id or campaign.PartyKnower (a fact-less
// rumour a knower carries is a holding — see RumorHeard).
func (s *Store) SetRumorHolder(ctx context.Context, campaignID, rumorID, entityID, variant, sinceEvent string) (*campaign.RumorHolder, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("holder tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := rumorInCampaignTx(ctx, tx, rumorID, campaignID); err != nil {
		return nil, err
	}
	if err := validateKnowerTx(ctx, tx, entityID, campaignID); err != nil {
		return nil, err
	}
	if sinceEvent != "" {
		if err := eventInCampaignTx(ctx, tx, sinceEvent, campaignID); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	h := &campaign.RumorHolder{
		RumorID: rumorID, EntityID: entityID, Variant: variant, SinceEvent: sinceEvent, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rumor_holders (rumor_id, entity_id, variant, since_event, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (rumor_id, entity_id) DO UPDATE SET
			variant = excluded.variant, since_event = excluded.since_event`,
		h.RumorID, h.EntityID, h.Variant, nullString(h.SinceEvent), now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("upsert holder: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("holder commit: %w", err)
	}
	return h, nil
}

// RemoveRumorHolder takes a holder out of the mill.
func (s *Store) RemoveRumorHolder(ctx context.Context, campaignID, rumorID, entityID string) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM rumor_holders WHERE rumor_id = ? AND entity_id = ?
		  AND rumor_id IN (SELECT id FROM rumors WHERE campaign_id = ?)`,
		rumorID, entityID, campaignID)
	if err != nil {
		return fmt.Errorf("delete holder: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: holder %s on %s", ErrNotFound, entityID, rumorID)
	}
	return nil
}

// Holders lists who repeats one rumour. The rumour must be readable at the
// scope — a DM-only rumour's holders are as invisible as its truth — and
// the holders themselves are public: whoever spreads a rumour is part of
// the rumour the hearer received.
func (s *Store) Holders(ctx context.Context, scope Scope, campaignID, rumorID string) ([]campaign.RumorHolder, error) {
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	q := `
		SELECT h.rumor_id, h.entity_id, h.variant, COALESCE(h.since_event, ''), h.created_at
		  FROM rumor_holders h JOIN rumors r ON r.id = h.rumor_id
		 WHERE r.campaign_id = ? AND h.rumor_id = ?`
	args := []any{campaignID, rumorID}
	if !scope.IsDM() {
		q += ` AND r.dm_only = 0`
	}
	q += ` ORDER BY h.created_at, h.entity_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list holders: %w", err)
	}
	defer rows.Close()
	var out []campaign.RumorHolder
	for rows.Next() {
		var (
			h       campaign.RumorHolder
			created int64
		)
		if err := rows.Scan(&h.RumorID, &h.EntityID, &h.Variant, &h.SinceEvent, &created); err != nil {
			return nil, err
		}
		h.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

/* ---------- scoped reads ---------- */

// RumorFilter narrows Rumors. Zero values mean "no restriction". Truth is
// a DM-only filter and is refused at non-DM scopes — a player asking for
// "only the false ones" is a question the scope line exists to refuse.
type RumorFilter struct {
	About  string
	Status string
	Truth  string
}

// Rumors lists the rumours a scope may read, newest first. The DM reads
// every column; every other scope reads the statement, the spread, the
// status and who holds it — never truth, never fact_id, never a DM-only
// rumour.
func (s *Store) Rumors(ctx context.Context, scope Scope, campaignID string, filter RumorFilter) ([]campaign.Rumor, error) {
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	if filter.Truth != "" {
		if !validRumorTruth[filter.Truth] {
			return nil, fmt.Errorf("%w: truth filter %q", ErrInvalid, filter.Truth)
		}
		if !scope.IsDM() {
			return nil, fmt.Errorf("%w: truth filtering is the dm's question; a player scope cannot ask it", ErrScope)
		}
	}
	if filter.Status != "" && !validRumorStatus[filter.Status] {
		return nil, fmt.Errorf("%w: status filter %q", ErrInvalid, filter.Status)
	}
	cols := rumorColsDM
	q := ` FROM rumors r WHERE r.campaign_id = ?`
	args := []any{campaignID}
	if !scope.IsDM() {
		cols = rumorColsPlayer
		q += ` AND r.dm_only = 0`
	}
	if filter.About != "" {
		q += ` AND r.about_entity = ?`
		args = append(args, filter.About)
	}
	if filter.Status != "" {
		q += ` AND r.status = ?`
		args = append(args, filter.Status)
	}
	if filter.Truth != "" && scope.IsDM() {
		q += ` AND r.truth = ?`
		args = append(args, filter.Truth)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+q+` ORDER BY r.created_at DESC, r.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("scoped rumors: %w", err)
	}
	defer rows.Close()
	var out []campaign.Rumor
	for rows.Next() {
		r, err := scanRumor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// Rumor returns one rumour if the scope may read it. A DM-only rumour at a
// player scope, like a truth value anywhere below the DM, is
// indistinguishable from a missing one.
func (s *Store) Rumor(ctx context.Context, scope Scope, campaignID, rumorID string) (*campaign.Rumor, error) {
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	cols := rumorColsDM
	q := ` FROM rumors r WHERE r.campaign_id = ? AND r.id = ?`
	args := []any{campaignID, rumorID}
	if !scope.IsDM() {
		cols = rumorColsPlayer
		q += ` AND r.dm_only = 0`
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+cols+q, args...)
	r, err := scanRumor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: rumor %s", ErrNotFound, rumorID)
	}
	return r, err
}

/* ---------- hearing a rumour ---------- */

// The outcomes of one hearing, in one breath each.
const (
	// RumorHeardGranted: a stance was written — suspects on a true
	// rumour's fact, believes_false on the fact a false or distorted one
	// contradicts.
	RumorHeardGranted = "granted"
	// RumorHeardCarried: the rumour names no fact, so the holding lives
	// on rumor_holders alone and renders as a rumour the character
	// carries.
	RumorHeardCarried = "carried"
	// RumorHeardKnowsAlready: the knower already knows the fact and is
	// left alone — gossip never downgrades knowledge.
	RumorHeardKnowsAlready = "knows_already"
	// RumorHeardUnchanged: the knower already holds exactly this stance;
	// a repeat grant is not a change.
	RumorHeardUnchanged = "unchanged"
)

// RumorHeardResult is what one hearing wrote: the outcome, and the stance
// row when one was written or already held.
type RumorHeardResult struct {
	RumorID string `json:"rumor_id"`
	Knower  string `json:"knower"`
	Outcome string `json:"outcome"`
	Stance  string `json:"stance"`
}

// RumorHeard records that a knower heard a rumour, and writes the stance
// the rumour earns:
//
//   - truth true, with a fact: suspects on that fact. Hearsay is not
//     knowledge; the DM upgrades it when the party confirms it.
//   - truth false or distorted, naming the fact it contradicts:
//     believes_false on that fact — the case the feature exists for. The
//     fact column carries the true statement the belief contradicts; the
//     wrong content is the rumour (epistemics.md, "What believes_false
//     means, exactly").
//   - no fact named: the holding lives on rumor_holders alone.
//
// Every stance write goes through SetAwareness, so CanTransition governs
// every move; a knower who already knows the fact is left alone rather
// than downgraded by gossip.
func (s *Store) RumorHeard(ctx context.Context, campaignID, rumorID, knower, sinceEvent string) (*RumorHeardResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("heard tx: %w", err)
	}
	defer tx.Rollback()
	r, err := rumorInCampaignTx(ctx, tx, rumorID, campaignID)
	if err != nil {
		return nil, err
	}
	if err := validateKnowerTx(ctx, tx, knower, campaignID); err != nil {
		return nil, err
	}
	if sinceEvent != "" {
		if err := eventInCampaignTx(ctx, tx, sinceEvent, campaignID); err != nil {
			return nil, err
		}
	}

	res := &RumorHeardResult{RumorID: r.ID, Knower: knower}
	if r.FactID == "" {
		// No fact to grant and none to contradict: the holding lives on
		// the mill itself.
		h := &campaign.RumorHolder{
			RumorID: r.ID, EntityID: knower, SinceEvent: sinceEvent, CreatedAt: time.Now().UTC(),
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rumor_holders (rumor_id, entity_id, variant, since_event, created_at)
			VALUES (?, ?, '', ?, ?)
			ON CONFLICT (rumor_id, entity_id) DO UPDATE SET since_event = excluded.since_event`,
			h.RumorID, h.EntityID, nullString(h.SinceEvent), h.CreatedAt.UnixMilli()); err != nil {
			return nil, fmt.Errorf("carry rumor: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("heard commit: %w", err)
		}
		res.Outcome = RumorHeardCarried
		return res, nil
	}

	// The stance this rumour earns on its fact.
	target := StanceSuspects
	if r.Truth != campaign.RumorTruthTrue {
		target = StanceBelievesFalse
	}
	current, err := s.getAwareness(ctx, tx, campaignID, knower, r.FactID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if current != nil && current.Stance == StanceKnows {
		// Left alone: knowledge is never downgraded by gossip. Commit the
		// read-only tx so Rollback's defer stays honest.
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("heard commit: %w", err)
		}
		res.Outcome = RumorHeardKnowsAlready
		res.Stance = StanceKnows
		return res, nil
	}
	if current != nil && current.Stance == target {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("heard commit: %w", err)
		}
		res.Outcome = RumorHeardUnchanged
		res.Stance = target
		return res, nil
	}
	// The write — through SetAwareness's own transaction, after this read
	// tx closes, because the shared database runs one connection at a
	// time. The transition table still governs: an illegal move is
	// refused here, never written.
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("heard commit: %w", err)
	}
	a, err := s.SetAwareness(ctx, campaignID, knower, r.FactID, target, rumorConfidence, sinceEvent, "")
	if err != nil {
		return nil, err
	}
	res.Outcome = RumorHeardGranted
	res.Stance = a.Stance
	return res, nil
}

/* ---------- tx helpers ---------- */

// rumorInCampaignTx loads one rumour inside a tx; ErrNotFound when it is
// missing or belongs to another campaign.
func rumorInCampaignTx(ctx context.Context, tx *sql.Tx, id, campaignID string) (*campaign.Rumor, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+rumorColsDM+` FROM rumors WHERE id = ? AND campaign_id = ?`,
		id, campaignID)
	r, err := scanRumor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: rumor %s", ErrNotFound, id)
	}
	return r, err
}

// entityInCampaignTx is factInCampaignTx for an entity id.
func entityInCampaignTx(ctx context.Context, tx *sql.Tx, id, campaignID string) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM entities WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: entity %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check entity: %w", err)
	}
	return nil
}
