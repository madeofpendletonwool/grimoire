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

// Fact is one atomic statement: a subject entity, a predicate, and an object
// that is an entity or a literal — never both. The prose rendering lives
// alongside the triple so retrieval has something to put in a prompt.
type Fact struct {
	ID            string
	CampaignID    string
	SubjectEntity string
	Predicate     string
	ObjectEntity  string // set when the object is an entity; empty otherwise
	ObjectLiteral string // set when the object is a literal; empty otherwise
	Statement     string
	Confidence    string
	Visibility    string
	CreatedBy     string
	SupersededBy  string // the fact that replaced this one, once retconned
	CreatedAt     time.Time
}

// ProvenanceInput is one origin record for a fact being created. Method is
// required; the span quadruple (session, source, offsets, quote) is required
// for extracted facts — the span rule — and optional for the others.
type ProvenanceInput struct {
	SessionID string
	SourceID  string
	SpanStart int64
	SpanEnd   int64
	Quote     string
	Method    string
	// AcceptedBy / AcceptedAt record the human who accepted an extracted
	// fact as canon and when. They are set only by the canon engine's review
	// queue (MAD-310): a human decision is the one path that writes an
	// extracted fact at a non-proposed confidence.
	AcceptedBy string
	AcceptedAt time.Time
}

// Provenance is a stored origin record.
type Provenance struct {
	ID        string
	FactID    string
	SessionID string
	SourceID  string
	SpanStart int64 // 0 when unset
	SpanEnd   int64 // 0 when unset
	Quote     string
	Method    string
	// AcceptedBy / AcceptedAt record the human who accepted an extracted
	// fact as canon through the review queue, and when. Empty/zero for
	// human-authored and imported facts, which need no acceptance.
	AcceptedBy string
	AcceptedAt time.Time
	CreatedAt  time.Time
}

// createFact validates and inserts one fact plus its provenance rows inside
// tx. Exported entry points wrap it in a transaction; nothing writes a fact
// without provenance.
func (s *Store) createFact(ctx context.Context, tx *sql.Tx, campaignID, subject, predicate, objectEntity, objectLiteral, statement, confidence, visibility, createdBy string, provenance []ProvenanceInput) (*Fact, error) {
	if !validConfidence[confidence] {
		return nil, fmt.Errorf("%w: confidence %q", ErrInvalid, confidence)
	}
	if !validVisibility[visibility] {
		return nil, fmt.Errorf("%w: visibility %q", ErrInvalid, visibility)
	}
	predicate = strings.TrimSpace(predicate)
	if predicate == "" {
		return nil, fmt.Errorf("%w: predicate is required", ErrInvalid)
	}
	statement = strings.TrimSpace(statement)
	if statement == "" {
		return nil, fmt.Errorf("%w: statement is required", ErrInvalid)
	}
	if (objectEntity == "") == (objectLiteral == "") {
		return nil, fmt.Errorf("%w: a fact's object is an entity or a literal, never both and never neither", ErrInvalid)
	}
	if len(provenance) == 0 {
		return nil, fmt.Errorf("%w: every fact carries at least one provenance row", ErrInvalid)
	}
	for _, p := range provenance {
		if !validMethods[p.Method] {
			return nil, fmt.Errorf("%w: provenance method %q", ErrInvalid, p.Method)
		}
		if p.Method == MethodExtracted {
			if p.SessionID == "" || p.SourceID == "" || p.SpanStart < 0 || p.SpanEnd <= p.SpanStart || p.Quote == "" {
				return nil, fmt.Errorf("%w: an extracted fact must cite session, source, a byte-offset span and a verbatim quote", ErrInvalid)
			}
		}
	}
	// Staging rule (ADR 3): only a human decision writes canon. Machine
	// methods may only stage proposals; human methods may assert canon or
	// derived but never stage. The exceptions carry AcceptedBy, the human
	// who made the decision through the review queue: the queue's
	// acceptance of an extracted fact, and its acceptance of an
	// AI-proposed suggestion (the NPC simulation's reveals). That human
	// marker is what makes a canon-confidence machine fact legal.
	for _, p := range provenance {
		switch p.Method {
		case MethodAIProposed:
			if confidence != ConfidenceProposed && strings.TrimSpace(p.AcceptedBy) == "" {
				return nil, fmt.Errorf("%w: a %s fact lands as proposed unless a human accepts it through the review queue", ErrInvalid, p.Method)
			}
		case MethodExtracted:
			if confidence != ConfidenceProposed && strings.TrimSpace(p.AcceptedBy) == "" {
				return nil, fmt.Errorf("%w: an extracted fact lands as proposed unless a human accepts it through the review queue", ErrInvalid)
			}
		case MethodDMAuthored, MethodImported:
			if confidence != ConfidenceCanon && confidence != ConfidenceDerived {
				return nil, fmt.Errorf("%w: a %s fact is human-written and lands as canon or derived, not %q", ErrInvalid, p.Method, confidence)
			}
		}
	}
	subjectEntity, err := s.entityInCampaignTx(ctx, tx, subject, campaignID)
	if err != nil {
		return nil, err
	}
	if subjectEntity.Status == StatusDeleted {
		return nil, fmt.Errorf("%w: subject entity %s is deleted", ErrInvalid, subject)
	}
	if objectEntity != "" {
		objectEnt, err := s.entityInCampaignTx(ctx, tx, objectEntity, campaignID)
		if err != nil {
			return nil, err
		}
		if objectEnt.Status == StatusDeleted {
			return nil, fmt.Errorf("%w: object entity %s is deleted", ErrInvalid, objectEntity)
		}
	}
	now := time.Now().UTC()
	f := &Fact{
		ID: uuid.NewString(), CampaignID: campaignID, SubjectEntity: subject,
		Predicate: predicate, ObjectEntity: objectEntity, ObjectLiteral: objectLiteral,
		Statement: statement, Confidence: confidence, Visibility: visibility,
		CreatedBy: createdBy, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO facts (id, campaign_id, subject_entity, predicate, object_entity, object_literal,
		                   statement, confidence, visibility, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.CampaignID, f.SubjectEntity, f.Predicate, nullString(f.ObjectEntity),
		nullString(f.ObjectLiteral), f.Statement, f.Confidence, f.Visibility,
		f.CreatedBy, now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert fact: %w", err)
	}
	for _, p := range provenance {
		var acceptedAt any
		if !p.AcceptedAt.IsZero() {
			acceptedAt = p.AcceptedAt.UnixMilli()
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO fact_provenance (id, fact_id, session_id, source_id, span_start, span_end, quote, method, accepted_by, accepted_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), f.ID, nullString(p.SessionID), nullString(p.SourceID),
			nullInt64(p.SpanStart), nullInt64(p.SpanEnd), p.Quote, p.Method,
			p.AcceptedBy, acceptedAt, now.UnixMilli())
		if err != nil {
			return nil, fmt.Errorf("insert provenance: %w", err)
		}
	}
	return f, nil
}

// CreateFact records a fact and its provenance rows in one transaction, so a
// fact can never exist without at least one provenance row.
func (s *Store) CreateFact(ctx context.Context, campaignID, subject, predicate, objectEntity, objectLiteral, statement, confidence, visibility, createdBy string, provenance []ProvenanceInput) (*Fact, error) {
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("fact tx: %w", err)
	}
	defer tx.Rollback()
	f, err := s.createFact(ctx, tx, campaignID, subject, predicate, objectEntity, objectLiteral,
		statement, confidence, visibility, createdBy, provenance)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("fact commit: %w", err)
	}
	return f, nil
}

// entityInCampaignTx is entityInCampaign over an open transaction.
func (s *Store) entityInCampaignTx(ctx context.Context, tx *sql.Tx, id, campaignID string) (*Entity, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+entityCols+` FROM entities WHERE id = ? AND campaign_id = ?`, id, campaignID)
	e, err := scanEntity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: entity %s", ErrNotFound, id)
	}
	return e, err
}

const factCols = `id, campaign_id, subject_entity, predicate, object_entity, object_literal,
                   statement, confidence, visibility, created_by, superseded_by, created_at`

func scanFact(row interface{ Scan(...any) error }) (*Fact, error) {
	var (
		f             Fact
		objectEntity  sql.NullString
		objectLiteral sql.NullString
		supersededBy  sql.NullString
		createdMilli  int64
	)
	if err := row.Scan(&f.ID, &f.CampaignID, &f.SubjectEntity, &f.Predicate, &objectEntity,
		&objectLiteral, &f.Statement, &f.Confidence, &f.Visibility, &f.CreatedBy,
		&supersededBy, &createdMilli); err != nil {
		return nil, err
	}
	f.ObjectEntity = objectEntity.String
	f.ObjectLiteral = objectLiteral.String
	f.SupersededBy = supersededBy.String
	f.CreatedAt = time.UnixMilli(createdMilli).UTC()
	return &f, nil
}

func (s *Store) factInCampaign(ctx context.Context, id, campaignID string) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM facts WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: fact %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("check fact: %w", err)
	}
	return nil
}

// GetFact returns one fact of a campaign. DM-scope reads only; scoped fact
// retrieval (what this knower may see) lives in internal/knowledge.
func (s *Store) GetFact(ctx context.Context, scope Scope, campaignID, id string) (*Fact, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+factCols+` FROM facts WHERE id = ? AND campaign_id = ?`, id, campaignID)
	f, err := scanFact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: fact %s", ErrNotFound, id)
	}
	return f, err
}

// FactFilter narrows ListFacts. Zero values mean "no restriction".
type FactFilter struct {
	SubjectEntity string
	Confidence    string // exact confidence, e.g. ConfidenceCanon
	Visibility    string
	Predicate     string
	NotSuperseded bool // exclude retconned facts that a replacement exists for
}

// ListFacts returns a campaign's facts, newest first, through the filter.
// DM-scope reads only; scoped fact retrieval lives in internal/knowledge.
// Note that the DM scope is not a dump: filter.Confidence can select
// 'proposed', but callers serving the DM chat must not — a proposed fact is
// invisible to every retrieval path until a human accepts it (ADR 3).
func (s *Store) ListFacts(ctx context.Context, scope Scope, campaignID string, filter FactFilter) ([]Fact, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	q := `SELECT ` + factCols + ` FROM facts WHERE campaign_id = ?`
	args := []any{campaignID}
	if filter.SubjectEntity != "" {
		q += ` AND subject_entity = ?`
		args = append(args, filter.SubjectEntity)
	}
	if filter.Confidence != "" {
		if !validConfidence[filter.Confidence] {
			return nil, fmt.Errorf("%w: confidence %q", ErrInvalid, filter.Confidence)
		}
		q += ` AND confidence = ?`
		args = append(args, filter.Confidence)
	}
	if filter.Visibility != "" {
		if !validVisibility[filter.Visibility] {
			return nil, fmt.Errorf("%w: visibility %q", ErrInvalid, filter.Visibility)
		}
		q += ` AND visibility = ?`
		args = append(args, filter.Visibility)
	}
	if filter.Predicate != "" {
		q += ` AND predicate = ?`
		args = append(args, filter.Predicate)
	}
	if filter.NotSuperseded {
		q += ` AND superseded_by IS NULL`
	}
	q += ` ORDER BY created_at DESC, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list facts: %w", err)
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// SupersedeFact retcons oldID: its confidence becomes 'retconned' and it
// points at newID, which must exist in the same campaign. The old row stays —
// a campaign's history of its own retcons is part of the campaign.
func (s *Store) SupersedeFact(ctx context.Context, campaignID, oldID, newID string) error {
	if err := s.factInCampaign(ctx, newID, campaignID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE facts SET confidence = ?, superseded_by = ?
		 WHERE id = ? AND campaign_id = ? AND superseded_by IS NULL`,
		ConfidenceRetconned, newID, oldID, campaignID)
	if err != nil {
		return fmt.Errorf("supersede fact: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either it does not exist or it was already superseded; both read
		// the same to the caller.
		return fmt.Errorf("%w: fact %s", ErrNotFound, oldID)
	}
	return nil
}

// FactProvenance lists where a fact came from, oldest record first.
// DM-scope reads only: quotes and spans are the raw material of secrets, and
// the "why does Grimoire think this?" button is a DM surface.
func (s *Store) FactProvenance(ctx context.Context, scope Scope, campaignID, factID string) ([]Provenance, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	if err := s.factInCampaign(ctx, factID, campaignID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, fact_id, session_id, source_id, span_start, span_end, quote, method,
		       accepted_by, accepted_at, created_at
		  FROM fact_provenance WHERE fact_id = ? ORDER BY rowid`, factID)
	if err != nil {
		return nil, fmt.Errorf("list provenance: %w", err)
	}
	defer rows.Close()
	var out []Provenance
	for rows.Next() {
		var (
			p          Provenance
			sessionID  sql.NullString
			sourceID   sql.NullString
			spanStart  sql.NullInt64
			spanEnd    sql.NullInt64
			acceptedAt sql.NullInt64
			created    int64
		)
		if err := rows.Scan(&p.ID, &p.FactID, &sessionID, &sourceID, &spanStart, &spanEnd,
			&p.Quote, &p.Method, &p.AcceptedBy, &acceptedAt, &created); err != nil {
			return nil, err
		}
		p.SessionID = sessionID.String
		p.SourceID = sourceID.String
		p.SpanStart = spanStart.Int64
		p.SpanEnd = spanEnd.Int64
		if acceptedAt.Valid {
			p.AcceptedAt = time.UnixMilli(acceptedAt.Int64).UTC()
		}
		p.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

/* ---------- the contradiction register ---------- */

// Contradiction is the register entry linking two or more credibly-sourced
// facts that conflict. Nothing picks a winner: both sides are kept, downgraded
// to 'contested', and their per-source versions stay separable.
type Contradiction struct {
	ID             string
	CampaignID     string
	SubjectEntity  string
	Predicate      string
	Status         string
	ResolutionNote string
	CreatedAt      time.Time
}

// FactVersion is one side of a contradiction: which fact, under what label
// ("the dm's notes", "the player journal"), and what it says.
type FactVersion struct {
	ID              string
	ContradictionID string
	FactID          string
	Label           string
	Statement       string
	CreatedAt       time.Time
}

// downgradeContested moves a fact to 'contested' only when that is strictly
// downward on the confidence ladder (canon or derived). Proposed facts are
// already lower and invisible; retconned facts are terminal. This is the
// monotonicity rule applied at the row level.
func downgradeContested(confidence string) (string, bool) {
	switch confidence {
	case ConfidenceCanon, ConfidenceDerived:
		return ConfidenceContested, true
	default:
		return confidence, false
	}
}

// RegisterContradiction links conflicting facts in the register, records one
// FactVersion per fact (labeled by where each account came from), and
// downgrades every side to 'contested' — never deleting or picking a winner.
// The subject and predicate must match the facts given. Registering the same
// pair twice is ErrAlreadyExists.
func (s *Store) RegisterContradiction(ctx context.Context, campaignID, subject, predicate string, sides []FactVersionSide, note string) (*Contradiction, error) {
	if len(sides) < 2 {
		return nil, fmt.Errorf("%w: a contradiction needs at least two sides", ErrInvalid)
	}
	predicate = strings.TrimSpace(predicate)
	if predicate == "" {
		return nil, fmt.Errorf("%w: predicate is required", ErrInvalid)
	}
	for _, side := range sides {
		if strings.TrimSpace(side.Label) == "" {
			return nil, fmt.Errorf("%w: every side needs a label", ErrInvalid)
		}
	}
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("contradiction tx: %w", err)
	}
	defer tx.Rollback()

	facts := make([]*Fact, 0, len(sides))
	seen := map[string]bool{}
	for _, side := range sides {
		row := tx.QueryRowContext(ctx,
			`SELECT `+factCols+` FROM facts WHERE id = ? AND campaign_id = ?`, side.FactID, campaignID)
		f, err := scanFact(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: fact %s", ErrNotFound, side.FactID)
		}
		if err != nil {
			return nil, err
		}
		if f.SubjectEntity != subject || f.Predicate != predicate {
			return nil, fmt.Errorf("%w: fact %s is not %q on the subject given", ErrInvalid, side.FactID, predicate)
		}
		if seen[f.ID] {
			return nil, fmt.Errorf("%w: fact %s given twice", ErrInvalid, f.ID)
		}
		seen[f.ID] = true
		facts = append(facts, f)
	}

	var already int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM fact_versions v JOIN contradictions c ON c.id = v.contradiction_id
		 WHERE c.campaign_id = ? AND c.subject_entity = ? AND c.predicate = ? AND c.status = ?
		   AND v.fact_id IN (`+placeholders(len(sides))+`)`,
		append([]any{campaignID, subject, predicate, ContradictionOpen}, factIDs(facts)...)...).Scan(&already); err != nil {
		return nil, fmt.Errorf("check existing contradiction: %w", err)
	}
	if already > 0 {
		return nil, fmt.Errorf("%w: an open contradiction already covers these facts", ErrAlreadyExists)
	}

	con := &Contradiction{
		ID: uuid.NewString(), CampaignID: campaignID, SubjectEntity: subject,
		Predicate: predicate, Status: ContradictionOpen, ResolutionNote: note,
		CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contradictions (id, campaign_id, subject_entity, predicate, status, resolution_note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		con.ID, con.CampaignID, con.SubjectEntity, con.Predicate, con.Status,
		con.ResolutionNote, now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert contradiction: %w", err)
	}
	for i, side := range sides {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO fact_versions (id, contradiction_id, fact_id, label, statement, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), con.ID, facts[i].ID, side.Label, facts[i].Statement, now.UnixMilli()); err != nil {
			return nil, fmt.Errorf("insert fact version: %w", err)
		}
		if next, ok := downgradeContested(facts[i].Confidence); ok {
			if _, err := tx.ExecContext(ctx,
				`UPDATE facts SET confidence = ? WHERE id = ?`, next, facts[i].ID); err != nil {
				return nil, fmt.Errorf("downgrade fact: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("contradiction commit: %w", err)
	}
	return con, nil
}

// FactVersionSide is one account of a contradiction: the fact carrying it and
// a label naming where the account came from.
type FactVersionSide struct {
	FactID string
	Label  string
}

func factIDs(facts []*Fact) []any {
	out := make([]any, len(facts))
	for i, f := range facts {
		out[i] = f.ID
	}
	return out
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Contradictions lists the register, open entries first. DM-scope reads
// only: both sides of a contradiction are spoiler-shaped until resolved.
func (s *Store) Contradictions(ctx context.Context, scope Scope, campaignID string) ([]Contradiction, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, campaign_id, subject_entity, predicate, status, resolution_note, created_at
		  FROM contradictions WHERE campaign_id = ?
		 ORDER BY CASE status WHEN 'open' THEN 0 ELSE 1 END, created_at`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list contradictions: %w", err)
	}
	defer rows.Close()
	var out []Contradiction
	for rows.Next() {
		var (
			c       Contradiction
			created int64
		)
		if err := rows.Scan(&c.ID, &c.CampaignID, &c.SubjectEntity, &c.Predicate, &c.Status,
			&c.ResolutionNote, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// FactVersions lists the sides of one contradiction. DM-scope reads only.
func (s *Store) FactVersions(ctx context.Context, scope Scope, campaignID, contradictionID string) ([]FactVersion, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.id, v.contradiction_id, v.fact_id, v.label, v.statement, v.created_at
		  FROM fact_versions v JOIN contradictions c ON c.id = v.contradiction_id
		 WHERE c.id = ? AND c.campaign_id = ? ORDER BY v.rowid`,
		contradictionID, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list fact versions: %w", err)
	}
	defer rows.Close()
	var out []FactVersion
	for rows.Next() {
		var (
			v       FactVersion
			created int64
		)
		if err := rows.Scan(&v.ID, &v.ContradictionID, &v.FactID, &v.Label, &v.Statement, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

// ResolveContradiction closes a register entry with a note. It records the
// human decision on the register only — the facts stay 'contested' until the
// DM supersedes whichever side lost, which is a separate, deliberate act.
func (s *Store) ResolveContradiction(ctx context.Context, campaignID, id, note string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE contradictions SET status = ?, resolution_note = ? WHERE id = ? AND campaign_id = ?`,
		ContradictionResolvedByReview, note, id, campaignID)
	if err != nil {
		return fmt.Errorf("resolve contradiction: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: contradiction %s", ErrNotFound, id)
	}
	return nil
}

// nullInt64 maps 0 to SQL NULL. Span offsets of 0 are treated as unset; a
// span that starts at byte 0 carries start 0 / end n with n > 0, and a
// mandated span always has end > start >= 0, so the mapping cannot erase a
// real span.
func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
