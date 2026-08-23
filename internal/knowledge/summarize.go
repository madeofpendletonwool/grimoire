package knowledge

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// Summary answers "what does this perspective know about X?" as four
// deterministic buckets — no model call anywhere. The DM question the parent
// brainstorm calls worth more than most AI features: confirmed, suspected,
// what they are confidently wrong about, and what they have not met.
//
// Unknown at a non-DM scope lists only public facts the knower has no
// awareness of: a perspective cannot be shown the secrets it does not know
// (that is the whole architecture). The DM's complement — including the
// secrets the party has not found — is Unknown, which requires the DM scope.
type Summary struct {
	Subject string
	// Confirmed: the knower holds stance knows.
	Confirmed []campaign.Fact
	// Suspected: stance suspects — believed, not confirmed.
	Suspected []campaign.Fact
	// Incorrect: stance believes_false — the knower believes something that
	// is not true. The fact rendered is the true statement the belief
	// contradicts; the wrong content lives in the discovery trail.
	Incorrect []campaign.Fact
	// Unknown: facts about the subject the knower has no granting awareness
	// of. Public facts only at non-DM scopes.
	Unknown []campaign.Fact
}

// Summarize builds the four-bucket summary of what the scope's knower holds
// about one subject entity, deterministically. The scope must be a knower
// scope — party, character:<id> or npc:<id>; the DM does not have beliefs
// about the campaign, the DM has the campaign.
//
// A fact granted to the knower set at more than one stance resolves to the
// strongest one (knows > believes_false > suspects), with the knower's own
// row preferred over the party's shared row — the presentation picks one
// stance per fact; the authorization has already been settled in SQL.
func (s *Store) Summarize(ctx context.Context, scope Scope, campaignID, subject string) (*Summary, error) {
	if scope.IsDM() {
		return nil, fmt.Errorf("%w: summarize describes a knower's view; pass party, character or npc", ErrInvalid)
	}
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	if err := s.validateSubject(ctx, subject, campaignID); err != nil {
		return nil, err
	}
	ks := knowers(scope)
	out := &Summary{Subject: subject}

	granted, err := s.grantedFactsAbout(ctx, campaignID, ks, subject)
	if err != nil {
		return nil, err
	}
	for _, g := range granted {
		switch g.BestStance {
		case StanceKnows:
			out.Confirmed = append(out.Confirmed, g.Fact)
		case StanceSuspects:
			out.Suspected = append(out.Suspected, g.Fact)
		case StanceBelievesFalse:
			out.Incorrect = append(out.Incorrect, g.Fact)
		}
	}

	// Unknown: live, public facts about the subject with no granting row in
	// the knower set. Secrets never appear here — a perspective cannot be
	// told what it does not know.
	q := `SELECT ` + factCols + ` FROM facts f
		 WHERE f.campaign_id = ? AND (f.subject_entity = ? OR f.object_entity = ?)
		   AND f.confidence <> 'proposed' AND f.superseded_by IS NULL
		   AND f.visibility <> 'secret'
		   AND NOT EXISTS (SELECT 1 FROM awareness a
		                    WHERE a.campaign_id = f.campaign_id AND a.fact_id = f.id
		                      AND a.knower IN ` + grantPlaceholders(len(ks)) + `
		                      AND a.stance IN ('knows','suspects','believes_false'))
		 ORDER BY f.predicate, f.statement`
	args := []any{campaignID, subject, subject}
	for _, k := range ks {
		args = append(args, k)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("summarize unknown: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out.Unknown = append(out.Unknown, *f)
	}
	return out, rows.Err()
}

// Unknown is the DM's complement view: the live facts about a subject that a
// given knower ('party' or an entity id) holds no granting awareness of —
// secrets included, because the caller is the DM. This is what makes the
// four-bucket panel complete for the DM: Summarize(party, duke) plus
// Unknown(dm, party, duke) is exactly the parent brainstorm's answer,
// including "he is a vampire".
//
// Non-DM scopes are refused with ErrScope; the party's own unknowns, public
// only, are the Unknown bucket Summarize already returns.
func (s *Store) Unknown(ctx context.Context, scope Scope, campaignID, knower, subject string) ([]campaign.Fact, error) {
	if !scope.IsDM() {
		return nil, fmt.Errorf("%w: %s; the party's own unknowns are Summarize's Unknown bucket", ErrScope, scope)
	}
	if err := s.validateKnower(ctx, knower, campaignID); err != nil {
		return nil, err
	}
	if err := s.validateSubject(ctx, subject, campaignID); err != nil {
		return nil, err
	}
	q := `SELECT ` + factCols + ` FROM facts f
		 WHERE f.campaign_id = ? AND (f.subject_entity = ? OR f.object_entity = ?)
		   AND f.confidence <> 'proposed' AND f.superseded_by IS NULL
		   AND NOT EXISTS (SELECT 1 FROM awareness a
		                    WHERE a.campaign_id = f.campaign_id AND a.fact_id = f.id
		                      AND a.knower = ?
		                      AND a.stance IN ('knows','suspects','believes_false'))
		 ORDER BY f.predicate, f.statement`
	rows, err := s.db.QueryContext(ctx, q, campaignID, subject, subject, knower)
	if err != nil {
		return nil, fmt.Errorf("unknown facts: %w", err)
	}
	defer rows.Close()
	var out []campaign.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// grantedFact pairs a fact with the stance it resolves to for a knower set.
type grantedFact struct {
	Fact       campaign.Fact
	BestStance string
	BestKnower string
}

// grantedFactsAbout loads the facts a knower set holds about a subject,
// resolving multi-stance grants to one row per fact: the knower's own row
// beats the party's shared row, and knows beats believes_false beats
// suspects. The authorization ran in SQL; this is presentation only.
func (s *Store) grantedFactsAbout(ctx context.Context, campaignID string, ks []string, subject string) ([]grantedFact, error) {
	q := `SELECT f.id, f.campaign_id, f.subject_entity, f.predicate, f.object_entity, f.object_literal,
	              f.statement, f.confidence, f.visibility, f.created_by, f.superseded_by, f.created_at,
	              a.knower, a.stance
		 FROM facts f
		 JOIN awareness a ON a.fact_id = f.id AND a.campaign_id = f.campaign_id
		 WHERE f.campaign_id = ? AND (f.subject_entity = ? OR f.object_entity = ?)
		   AND f.confidence <> 'proposed' AND f.superseded_by IS NULL
		   AND a.knower IN ` + grantPlaceholders(len(ks)) + `
		   AND a.stance IN ('knows','suspects','believes_false')`
	args := []any{campaignID, subject, subject}
	for _, k := range ks {
		args = append(args, k)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("summarize granted: %w", err)
	}
	defer rows.Close()
	best := map[string]*grantedFact{}
	var order []string
	for rows.Next() {
		var (
			f             campaign.Fact
			objectEntity  sql.NullString
			objectLiteral sql.NullString
			supersededBy  sql.NullString
			createdMS     int64
			knower, stanc string
		)
		if err := rows.Scan(&f.ID, &f.CampaignID, &f.SubjectEntity, &f.Predicate, &objectEntity,
			&objectLiteral, &f.Statement, &f.Confidence, &f.Visibility, &f.CreatedBy,
			&supersededBy, &createdMS, &knower, &stanc); err != nil {
			return nil, err
		}
		f.ObjectEntity = objectEntity.String
		f.ObjectLiteral = objectLiteral.String
		f.SupersededBy = supersededBy.String
		if existing, ok := best[f.ID]; ok {
			if betterGrant(stanc, knower, existing.BestStance, existing.BestKnower, ks[0]) {
				existing.BestStance = stanc
				existing.BestKnower = knower
			}
			continue
		}
		best[f.ID] = &grantedFact{Fact: f, BestStance: stanc, BestKnower: knower}
		order = append(order, f.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]grantedFact, 0, len(order))
	for _, id := range order {
		out = append(out, *best[id])
	}
	return out, nil
}

// betterGrant reports whether the (stance, knower) pair beats the current
// best: the knower's own row outranks the party's shared one, then the
// stronger stance wins (knows > believes_false > suspects).
func betterGrant(stance, knower, curStance, curKnower, ownKnower string) bool {
	newOwn, curOwn := knower == ownKnower, curKnower == ownKnower
	if newOwn != curOwn {
		return newOwn
	}
	return stanceRank[stance] < stanceRank[curStance]
}

// validateSubject checks the subject entity exists in the campaign.
func (s *Store) validateSubject(ctx context.Context, subject, campaignID string) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM entities WHERE id = ? AND campaign_id = ?`, subject, campaignID).Scan(&one)
	if err != nil {
		return fmt.Errorf("subject %s: %w", subject, ErrNotFound)
	}
	return nil
}
