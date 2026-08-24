package canon

// The review queue (MAD-310): the human gate at the end of the canon engine.
//
// Nothing the AI proposes becomes true until a person says so. The extraction
// pass stages candidates, the adversarial pass verdicts them, the
// deterministic pass flags the graph — and all three feed this queue, which is
// the only surface that writes accepted material into the campaign graph.
//
// Queue semantics (ported from Arda's factcheck_reviews):
//
//   - a decided item keeps its decision forever;
//   - a decided item never resurrects;
//   - if the same finding reappears later it opens as a NEW item.
//
// The UNIQUE (campaign_id, dedup_key) constraint backs all three: BuildQueue
// only inserts rows whose dedup_key has never been seen, so a re-run is a
// no-op for anything already queued (open or decided), and a genuinely new
// occurrence of the same finding mints a new row.
//
// Contradictions are preserved, never smoothed: accepting a contradiction
// item registers the pair (campaign.RegisterContradiction), downgrades both
// sides to 'contested', and keeps per-source versions separable. The system
// never picks a winner on its own.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

/* ---------- vocabularies ---------- */

// Review item kinds. proposed_* items carry a candidate_id; engine_flag items
// carry a flag_id; contradiction items carry their sides in detail.
const (
	ReviewProposedFact         = "proposed_fact"
	ReviewProposedEvent        = "proposed_event"
	ReviewProposedDiscovery    = "proposed_discovery"
	ReviewProposedRelationship = "proposed_relationship"
	ReviewProposedEntity       = "proposed_entity"
	ReviewLowAgreement         = "low_agreement"
	ReviewContradiction        = "contradiction"
	ReviewEngineFlag           = "engine_flag"
)

// Review statuses. A decision is terminal: once accepted, modified or
// dismissed, the item never changes state again.
const (
	ReviewOpen      = "open"
	ReviewAccepted  = "accepted"
	ReviewModified  = "modified"
	ReviewDismissed = "dismissed"
)

// Review decisions a human may make on an open item.
const (
	DecisionAccept  = "accept"
	DecisionModify  = "modify"
	DecisionDismiss = "dismiss"
)

// candidateKindReview maps a staged candidate kind onto its proposed_* queue
// kind.
var candidateKindReview = map[string]string{
	KindFact:         ReviewProposedFact,
	KindEvent:        ReviewProposedEvent,
	KindDiscovery:    ReviewProposedDiscovery,
	KindRelationship: ReviewProposedRelationship,
	KindEntity:       ReviewProposedEntity,
}

// contextWindowBytes is how much source text rides along on each side of a
// quoted span for the review screen, so the DM reads the quote in context
// without opening the transcript.
const contextWindowBytes = 320

/* ---------- the stored shape ---------- */

// Review is one queue item. The first fields map the canon_reviews columns;
// the rest are rendering material loaded by Reviews (payload, quote, context,
// the adversarial verdict) so the review screen needs no follow-up queries.
type Review struct {
	ID           string
	CampaignID   string
	Kind         string
	Status       string
	DedupKey     string
	CandidateID  string
	FlagID       string
	Subject      string
	Summary      string
	Detail       string
	ResultRef    string
	DecisionNote string
	DecidedBy    string
	DecidedAt    time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time

	// Rendering material, loaded by Reviews from the candidate and verdict
	// tables. Payload is the candidate's (or modified) proposed change.
	Payload       map[string]any
	Quote         string
	SpanStart     int64
	SpanEnd       int64
	SourceKind    string
	SourceAuthor  string
	SourceTitle   string
	ContextBefore string
	ContextAfter  string
	Agreement     float64
	Rationale     string
	Verdict       string
	Confidence    float64
}

const reviewCols = `id, campaign_id, kind, status, dedup_key, candidate_id, flag_id, subject, summary,
                    detail, result_ref, decision_note, decided_by, decided_at, created_at, updated_at`

func scanReview(row interface{ Scan(...any) error }) (*Review, error) {
	var (
		r         Review
		candidate sql.NullString
		flagID    sql.NullString
		decidedAt sql.NullInt64
		created   int64
		updated   int64
	)
	if err := row.Scan(&r.ID, &r.CampaignID, &r.Kind, &r.Status, &r.DedupKey, &candidate,
		&flagID, &r.Subject, &r.Summary, &r.Detail, &r.ResultRef, &r.DecisionNote,
		&r.DecidedBy, &decidedAt, &created, &updated); err != nil {
		return nil, err
	}
	r.CandidateID = candidate.String
	r.FlagID = flagID.String
	if decidedAt.Valid {
		r.DecidedAt = time.UnixMilli(decidedAt.Int64).UTC()
	}
	r.CreatedAt = time.UnixMilli(created).UTC()
	r.UpdatedAt = time.UnixMilli(updated).UTC()
	r.Payload = map[string]any{}
	return &r, nil
}

/* ---------- construction ---------- */

// WithGraphStores wires the campaign graph and knowledge stores the review
// queue writes into on accept. Without them the deterministic and queue-read
// surfaces still work, but accepting is refused with a clear error.
func (s *Store) WithGraphStores(c *campaign.Store, k *knowledge.Store) *Store {
	s.campaigns = c
	s.knowledge = k
	return s
}

func (s *Store) requireGraphStores() error {
	if s.campaigns == nil || s.knowledge == nil {
		return errors.New("canon: no graph stores wired (accepting needs campaign and knowledge stores)")
	}
	return nil
}

/* ---------- building the queue ---------- */

// BuildQueue creates queue items for every finding the three upstream passes
// produced that does not already have an item. It is idempotent and safe to
// call after every extraction, validation or engine run; a re-run only ever
// mints rows whose dedup_key has never been seen. Returns the whole queue as
// it stands afterwards.
func (s *Store) BuildQueue(ctx context.Context, campaignID string) ([]Review, error) {
	if strings.TrimSpace(campaignID) == "" {
		return nil, fmt.Errorf("%w: campaign id is required", ErrInvalid)
	}
	if err := s.buildCandidateItems(ctx, campaignID); err != nil {
		return nil, err
	}
	if err := s.buildFlagItems(ctx, campaignID); err != nil {
		return nil, err
	}
	return s.Reviews(ctx, campaignID, "")
}

// buildCandidateItems turns validated staged candidates into queue items. A
// candidate whose latest verdict is agree/downgrade is a proposed_<kind>; a
// flag_review or unparseable verdict is a low_agreement item; a rejected
// verdict (the monotonicity rule refused an upgrade) has nothing to review.
// Fact candidates that contradict each other (same subject and predicate,
// different object) are claimed by a single contradiction item instead.
func (s *Store) buildCandidateItems(ctx context.Context, campaignID string) error {
	cands, err := s.ListCandidates(ctx, campaignID, CandidateFilter{})
	if err != nil {
		return err
	}
	// The effective verdict per candidate, oldest-wins-resolved to latest.
	type item struct {
		cand      Candidate
		verdict   string
		status    string
		agreement float64
		rationale string
	}
	var items []item
	for _, c := range cands {
		v, err := s.latestVerdict(ctx, c.ID)
		if err != nil {
			return err
		}
		if v == nil {
			continue // not validated yet; the adversarial pass owns it
		}
		items = append(items, item{cand: c, verdict: v.Verdict, status: v.Status,
			agreement: v.Agreement, rationale: v.Rationale})
	}

	// Contradictions: fact candidates that assert the same subject and
	// predicate with different objects, whose verdicts both agree/downgrade.
	// They are claimed by a single contradiction item; their ids are excluded
	// from the individual proposed_fact pass below.
	claimed := map[string]bool{}
	type side struct {
		candidateID string
		objectKey   string
	}
	byKey := map[string][]side{}
	for _, it := range items {
		if it.cand.Kind != KindFact || (it.verdict != "agree" && it.verdict != "downgrade") {
			continue
		}
		fs, ok := parseFactSide(it.cand.Payload)
		if !ok {
			continue
		}
		key := fs.subject + "\x00" + fs.predicate
		byKey[key] = append(byKey[key], side{
			candidateID: it.cand.ID,
			objectKey:   fs.objectEntity + "\x00" + fs.objectLiteral,
		})
	}

	type contradictionGroup struct {
		subject, predicate string
		ids                []string
	}
	var groups []contradictionGroup
	for key, sides := range byKey {
		distinct := map[string]bool{}
		for _, sd := range sides {
			distinct[sd.objectKey] = true
		}
		if len(distinct) < 2 {
			continue // same object is duplicate_fact's problem, not a contradiction
		}
		ids := make([]string, 0, len(sides))
		for _, sd := range sides {
			ids = append(ids, sd.candidateID)
		}
		sort.Strings(ids)
		subject, predicate, _ := strings.Cut(key, "\x00")
		groups = append(groups, contradictionGroup{subject: subject, predicate: predicate, ids: ids})
		for _, id := range ids {
			claimed[id] = true
		}
	}

	for _, g := range groups {
		dedup := "contradiction:" + strings.Join(g.ids, ",")
		detail, _ := json.Marshal(map[string]any{
			"subject":   g.subject,
			"predicate": g.predicate,
			"sides":     g.ids,
		})
		summary := fmt.Sprintf("%d sources disagree about %q on %q", len(g.ids), g.subject, g.predicate)
		if err := s.insertReview(ctx, campaignID, ReviewContradiction, dedup, "", "",
			"Contradiction", summary, string(detail)); err != nil {
			return err
		}
	}

	for _, it := range items {
		if claimed[it.cand.ID] {
			continue
		}
		// A rejected verdict (the monotonicity rule refused an upgrade) has
		// nothing to review; it is logged on the verdict row, not queued.
		switch {
		case it.status == "rejected":
			continue
		case it.status == "unparseable" || it.verdict == "flag_review":
			subject, summary := renderCandidate(it.cand)
			if err := s.insertReview(ctx, campaignID, ReviewLowAgreement, "candidate:"+it.cand.ID,
				it.cand.ID, "", subject, summary, it.rationale); err != nil {
				return err
			}
		case it.verdict == "agree" || it.verdict == "downgrade":
			kind, ok := candidateKindReview[it.cand.Kind]
			if !ok {
				continue
			}
			subject, summary := renderCandidate(it.cand)
			if err := s.insertReview(ctx, campaignID, kind, "candidate:"+it.cand.ID,
				it.cand.ID, "", subject, summary, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// buildFlagItems surfaces review-severity deterministic findings as
// engine_flag items, keyed to the flag row. The contradictory_facts check is
// deliberately excluded: a contradiction among already-canon facts is decided
// on the flag itself (DecideFlag), while a contradiction between staged
// candidates is handled by buildCandidateItems.
func (s *Store) buildFlagItems(ctx context.Context, campaignID string) error {
	flags, err := s.Flags(ctx, campaignID, "")
	if err != nil {
		return err
	}
	for _, f := range flags {
		if f.Status != FlagOpen {
			continue // decided or cleared flags are not re-queued
		}
		if string(f.Severity) != string(campaign.SeverityReview) {
			continue
		}
		if f.CheckCode == campaign.CheckContradictoryFacts {
			continue // decided on the flag, not re-invented as a queue item
		}
		if err := s.insertReview(ctx, campaignID, ReviewEngineFlag, "flag:"+f.ID,
			"", f.ID, "Engine flag — "+f.CheckCode, f.Message, ""); err != nil {
			return err
		}
	}
	return nil
}

// insertReview inserts one queue item, doing nothing when its dedup key has
// already been seen (open or decided) — the never-resurrect rule.
func (s *Store) insertReview(ctx context.Context, campaignID, kind, dedup, candidateID, flagID, subject, summary, detail string) error {
	now := s.now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO canon_reviews (id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
			subject, summary, detail, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (campaign_id, dedup_key) DO NOTHING`,
		uuid.NewString(), campaignID, kind, ReviewOpen, dedup, nullString(candidateID),
		nullString(flagID), subject, summary, detail, now, now)
	if err != nil {
		return fmt.Errorf("insert review: %w", err)
	}
	return nil
}

/* ---------- reads ---------- */

// Reviews lists a campaign's queue, oldest item first. An empty status
// returns every item; otherwise only those in that status. Rendering material
// (payload, span context, verdict) is attached per item.
func (s *Store) Reviews(ctx context.Context, campaignID, status string) ([]Review, error) {
	q := `SELECT ` + reviewCols + ` FROM canon_reviews WHERE campaign_id = ?`
	args := []any{campaignID}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	var out []Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Enrich after the rows are closed: enrich opens its own queries, and the
	// shared handle may run with a single connection.
	for i := range out {
		if err := s.enrich(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// enrich attaches the candidate payload, quoted span with context, and the
// adversarial verdict to a queue item, so the review screen renders without
// follow-up queries.
func (s *Store) enrich(ctx context.Context, r *Review) error {
	if r.CandidateID == "" {
		return nil
	}
	c, err := s.getCandidate(ctx, r.CampaignID, r.CandidateID)
	if err != nil {
		return err
	}
	r.Payload = map[string]any{}
	if len(c.Payload) > 0 {
		_ = json.Unmarshal(c.Payload, &r.Payload)
	}
	r.Quote = c.Quote
	r.SpanStart = c.SpanStart
	r.SpanEnd = c.SpanEnd
	r.Confidence = c.Confidence

	// Source identity for "who said it".
	if err := s.db.QueryRowContext(ctx, `
		SELECT kind, COALESCE(author, ''), COALESCE(title, '') FROM session_sources WHERE id = ?`,
		c.SourceID).Scan(&r.SourceKind, &r.SourceAuthor, &r.SourceTitle); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load source: %w", err)
	}
	before, after, err := s.spanContext(ctx, c.SourceID, c.SpanStart, c.SpanEnd)
	if err != nil {
		return err
	}
	r.ContextBefore = before
	r.ContextAfter = after

	if v, err := s.latestVerdict(ctx, c.ID); err != nil {
		return err
	} else if v != nil {
		r.Agreement = v.Agreement
		r.Rationale = v.Rationale
		r.Verdict = v.Verdict
	}
	return nil
}

// getCandidate loads one staged candidate, scoped to its campaign.
func (s *Store) getCandidate(ctx context.Context, campaignID, id string) (Candidate, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+candidateCols+` FROM canon_candidates WHERE id = ? AND campaign_id = ?`, id, campaignID)
	c, err := scanCandidate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Candidate{}, fmt.Errorf("%w: candidate %s", ErrNotFound, id)
	}
	return c, err
}

// latestVerdict returns the most recent verdict for a candidate, or nil when
// the candidate has not been validated.
func (s *Store) latestVerdict(ctx context.Context, candidateID string) (*Verdict, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, campaign_id, candidate_id, prompt_version, input_checksum,
		       verdict, status, agreement, rationale, proposed_confidence, confidence_before, confidence_after,
		       rejection_reason, model, input_tokens, output_tokens, raw, created_at
		  FROM canon_verdicts WHERE candidate_id = ? ORDER BY created_at DESC, id`, candidateID)
	if err != nil {
		return nil, fmt.Errorf("list verdicts: %w", err)
	}
	defer rows.Close()
	var out []Verdict
	for rows.Next() {
		var (
			v         Verdict
			proposed  sql.NullFloat64
			createdMi int64
		)
		if err := rows.Scan(&v.ID, &v.RunID, &v.CampaignID, &v.CandidateID, &v.PromptVersion,
			&v.InputChecksum, &v.Verdict, &v.Status, &v.Agreement, &v.Rationale,
			&proposed, &v.ConfidenceBefore, &v.ConfidenceAfter, &v.RejectionReason,
			&v.Model, &v.InputTokens, &v.OutputTokens, &v.Raw, &createdMi); err != nil {
			return nil, err
		}
		if proposed.Valid {
			v.ProposedConfidence = proposed.Float64
			v.HasProposed = true
		}
		v.CreatedAt = time.UnixMilli(createdMi).UTC()
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// spanContext returns the source text immediately before and after a quoted
// span, snapped to rune boundaries.
func (s *Store) spanContext(ctx context.Context, sourceID string, start, end int64) (before, after string, err error) {
	var content string
	if err := s.db.QueryRowContext(ctx, `SELECT content FROM session_sources WHERE id = ?`, sourceID).Scan(&content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("load source content: %w", err)
	}
	lo := start - contextWindowBytes
	if lo < 0 {
		lo = 0
	}
	hi := end + contextWindowBytes
	if hi > int64(len(content)) {
		hi = int64(len(content))
	}
	for lo > 0 && !utf8.RuneStart(content[lo]) {
		lo--
	}
	for hi < int64(len(content)) && !utf8.RuneStart(content[hi]) {
		hi--
	}
	if lo > start {
		lo = start
	}
	if hi < end {
		hi = end
	}
	return content[lo:start], content[end:hi], nil
}

/* ---------- decisions ---------- */

// DecideReview records a human decision on one open item: accept (write the
// proposed change into the campaign graph as canon), modify then accept (write
// the caller-supplied payload instead), or dismiss (record the decision,
// write nothing). decidedBy is recorded on the item and — for facts and
// discoveries — on the provenance/discovery rows. Only an open item can be
// decided; a decided item keeps its decision forever.
func (s *Store) DecideReview(ctx context.Context, campaignID, reviewID, decision, note, decidedBy string, modifiedPayload []byte) (*Review, error) {
	if decision != DecisionAccept && decision != DecisionModify && decision != DecisionDismiss {
		return nil, fmt.Errorf("%w: decision %q is not %s, %s or %s",
			ErrInvalid, decision, DecisionAccept, DecisionModify, DecisionDismiss)
	}
	rev, err := s.getReview(ctx, campaignID, reviewID)
	if err != nil {
		return nil, err
	}
	if rev.Status != ReviewOpen {
		return nil, fmt.Errorf("%w: review %s is %s; a decided item keeps its decision", ErrInvalid, reviewID, rev.Status)
	}
	if decision == DecisionModify && (rev.Kind == ReviewContradiction || rev.Kind == ReviewEngineFlag) {
		return nil, fmt.Errorf("%w: %s items cannot be modified", ErrInvalid, rev.Kind)
	}

	// Dismiss is the one decision that never touches the graph or the flag
	// ledger's decision fields beyond the note.
	if decision == DecisionDismiss {
		return s.finalizeReview(ctx, rev, ReviewDismissed, note, decidedBy, "")
	}

	if rev.Kind == ReviewEngineFlag {
		return s.decideEngineFlag(ctx, rev, decision, note, decidedBy)
	}

	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}

	var payload map[string]any
	if decision == DecisionModify && len(modifiedPayload) > 0 {
		if err := json.Unmarshal(modifiedPayload, &payload); err != nil {
			return nil, fmt.Errorf("%w: modified payload is not valid JSON: %v", ErrInvalid, err)
		}
	} else if rev.CandidateID != "" {
		c, err := s.getCandidate(ctx, campaignID, rev.CandidateID)
		if err != nil {
			return nil, err
		}
		payload = map[string]any{}
		if len(c.Payload) > 0 {
			_ = json.Unmarshal(c.Payload, &payload)
		}
	}

	resultRef, err := s.applyReview(ctx, rev, payload, decidedBy)
	if err != nil {
		return nil, err
	}
	status := ReviewAccepted
	if decision == DecisionModify {
		status = ReviewModified
	}
	return s.finalizeReview(ctx, rev, status, note, decidedBy, resultRef)
}

// getReview loads one item, scoped to its campaign.
func (s *Store) getReview(ctx context.Context, campaignID, id string) (*Review, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+reviewCols+` FROM canon_reviews WHERE id = ? AND campaign_id = ?`, id, campaignID)
	r, err := scanReview(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: review %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// finalizeReview flips one open item to its terminal state with a guarded
// UPDATE, so only the first decision on an item ever lands.
func (s *Store) finalizeReview(ctx context.Context, rev *Review, status, note, decidedBy, resultRef string) (*Review, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx, `
		UPDATE canon_reviews SET status = ?, decision_note = ?, decided_by = ?, decided_at = ?,
			result_ref = CASE WHEN ? <> '' THEN ? ELSE result_ref END, updated_at = ?
		 WHERE id = ? AND campaign_id = ? AND status = ?`,
		status, note, decidedBy, now.UnixMilli(), resultRef, resultRef, now.UnixMilli(),
		rev.ID, rev.CampaignID, ReviewOpen)
	if err != nil {
		return nil, fmt.Errorf("finalize review: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: review %s was already decided", ErrInvalid, rev.ID)
	}
	return s.getReview(ctx, rev.CampaignID, rev.ID)
}

// decideEngineFlag maps an engine_flag item's decision onto the underlying
// flag: accept the finding (the DM owns the fix) or dismiss it (not a
// problem). Modify is refused earlier.
func (s *Store) decideEngineFlag(ctx context.Context, rev *Review, decision, note, decidedBy string) (*Review, error) {
	flag, err := s.getFlag(ctx, rev.CampaignID, rev.FlagID)
	if err != nil {
		return nil, err
	}
	flagDecision := DecisionAccepted
	if decision == DecisionDismiss {
		flagDecision = DecisionDismissed
	}
	if err := s.DecideFlag(ctx, rev.CampaignID, flag.CheckCode, flag.RecordKind, flag.RecordID,
		flagDecision, note, decidedBy); err != nil {
		// A flag already decided some other way reads as invalid, but the
		// review item's own decision still stands; the queue is the DM's
		// surface, not the ledger's gatekeeper.
		if !strings.Contains(err.Error(), "not open") {
			return nil, err
		}
	}
	status := ReviewAccepted
	if decision == DecisionDismiss {
		status = ReviewDismissed
	}
	return s.finalizeReview(ctx, rev, status, note, decidedBy, flag.ID)
}

// getFlag loads one ledger row.
func (s *Store) getFlag(ctx context.Context, campaignID, id string) (*Flag, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+flagCols+` FROM canon_flags WHERE id = ? AND campaign_id = ?`, id, campaignID)
	f, err := scanFlag(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: flag %s", ErrNotFound, id)
	}
	return f, err
}

/* ---------- applying a decision into the graph ---------- */

// applyReview writes one accepted (or modified) proposal into the campaign
// graph and returns the graph object id it created. It is the only path that
// writes canon from the queue; everything else in this package stages.
func (s *Store) applyReview(ctx context.Context, rev *Review, payload map[string]any, decidedBy string) (string, error) {
	switch rev.Kind {
	case ReviewProposedFact:
		return s.applyFact(ctx, rev, payload, decidedBy)
	case ReviewProposedEvent:
		return s.applyEvent(ctx, rev, payload, decidedBy)
	case ReviewProposedDiscovery:
		return s.applyDiscovery(ctx, rev, payload, decidedBy)
	case ReviewProposedRelationship:
		return s.applyRelationship(ctx, rev, payload, decidedBy)
	case ReviewProposedEntity:
		return s.applyEntity(ctx, rev, payload, decidedBy)
	case ReviewLowAgreement:
		kind, err := s.lowAgreementCandidateKind(ctx, rev)
		if err != nil {
			return "", err
		}
		return s.applyCandidateByKind(ctx, rev, kind, payload, decidedBy)
	case ReviewContradiction:
		return s.applyContradiction(ctx, rev, decidedBy)
	default:
		return "", fmt.Errorf("%w: item kind %s cannot be accepted here", ErrInvalid, rev.Kind)
	}
}

// applyCandidateByKind dispatches a low_agreement candidate's payload to the
// apply function for its staged kind.
func (s *Store) applyCandidateByKind(ctx context.Context, rev *Review, kind string, payload map[string]any, decidedBy string) (string, error) {
	switch kind {
	case KindFact:
		return s.applyFact(ctx, rev, payload, decidedBy)
	case KindEvent:
		return s.applyEvent(ctx, rev, payload, decidedBy)
	case KindDiscovery:
		return s.applyDiscovery(ctx, rev, payload, decidedBy)
	case KindRelationship:
		return s.applyRelationship(ctx, rev, payload, decidedBy)
	case KindEntity:
		return s.applyEntity(ctx, rev, payload, decidedBy)
	default:
		return "", fmt.Errorf("%w: candidate kind %q cannot be accepted", ErrInvalid, kind)
	}
}

// low_agreement items point at a candidate, so their kind is low_agreement
// while the payload is whatever the candidate proposed. The candidate kind is
// recovered from the payload shape via the candidate row itself.
func (s *Store) lowAgreementCandidateKind(ctx context.Context, rev *Review) (string, error) {
	c, err := s.getCandidate(ctx, rev.CampaignID, rev.CandidateID)
	if err != nil {
		return "", err
	}
	return c.Kind, nil
}

// applyFact creates the accepted fact as canon with extracted provenance
// carrying the decision (who and when). The span quadruple comes from the
// staged candidate.
func (s *Store) applyFact(ctx context.Context, rev *Review, p map[string]any, decidedBy string) (string, error) {
	statement := str(p, "statement")
	subject := str(p, "subject")
	predicate := str(p, "predicate")
	objectEntity := str(p, "object_entity")
	objectLiteral := str(p, "object_literal")
	visibility := str(p, "visibility")
	if visibility == "" {
		visibility = campaign.VisibilityPublic
	}
	c, err := s.getCandidate(ctx, rev.CampaignID, rev.CandidateID)
	if err != nil {
		return "", err
	}
	fact, err := s.campaigns.CreateFact(ctx, rev.CampaignID, subject, predicate, objectEntity, objectLiteral,
		statement, campaign.ConfidenceCanon, visibility, decidedBy, []campaign.ProvenanceInput{{
			Method:     campaign.MethodExtracted,
			SessionID:  c.SessionID,
			SourceID:   c.SourceID,
			SpanStart:  c.SpanStart,
			SpanEnd:    c.SpanEnd,
			Quote:      c.Quote,
			AcceptedBy: decidedBy,
			AcceptedAt: s.now(),
		}})
	if err != nil {
		return "", fmt.Errorf("accept fact: %w", err)
	}
	return fact.ID, nil
}

// applyEvent creates the accepted event and its participants.
func (s *Store) applyEvent(ctx context.Context, rev *Review, p map[string]any, decidedBy string) (string, error) {
	summary := str(p, "summary")
	location := str(p, "location")
	c, err := s.getCandidate(ctx, rev.CampaignID, rev.CandidateID)
	if err != nil {
		return "", err
	}
	var clockAt *int64
	if v, ok := intField(p["clock_at"]); ok {
		clockAt = &v
	}
	ev, err := s.campaigns.CreateEvent(ctx, rev.CampaignID, c.SessionID, summary, clockAt, location)
	if err != nil {
		return "", fmt.Errorf("accept event: %w", err)
	}
	if parts, ok := p["participants"].([]any); ok {
		for _, part := range parts {
			if pm, ok := part.(map[string]any); ok {
				if entity := str(pm, "entity"); entity != "" {
					role := str(pm, "role")
					_ = s.campaigns.AddParticipant(ctx, rev.CampaignID, ev.ID, entity, role)
				}
			}
		}
	}
	return ev.ID, nil
}

// applyDiscovery records the accepted discovery with its awareness row,
// resolving the candidate's local fact reference to the fact its acceptance
// created (or a fact id the DM supplied in a modified payload).
func (s *Store) applyDiscovery(ctx context.Context, rev *Review, p map[string]any, decidedBy string) (string, error) {
	factRef := str(p, "fact")
	if factRef == "" {
		return "", fmt.Errorf("%w: discovery payload has no fact", ErrInvalid)
	}
	factID, err := s.resolveFactRef(ctx, rev.CampaignID, factRef)
	if err != nil {
		return "", err
	}
	discoveredBy := str(p, "discovered_by")
	stance := str(p, "stance")
	method := str(p, "method")
	c, err := s.getCandidate(ctx, rev.CampaignID, rev.CandidateID)
	if err != nil {
		return "", err
	}
	d, err := s.knowledge.RecordDiscovery(ctx, knowledge.RecordDiscoveryInput{
		CampaignID:   rev.CampaignID,
		FactID:       factID,
		DiscoveredBy: discoveredBy,
		SessionID:    c.SessionID,
		Method:       method,
		SourceID:     c.SourceID,
		SpanStart:    c.SpanStart,
		SpanEnd:      c.SpanEnd,
		Quote:        c.Quote,
		Confidence:   c.Confidence,
		AcceptedBy:   decidedBy,
		Stance:       stance,
	})
	if err != nil {
		return "", fmt.Errorf("accept discovery: %w", err)
	}
	return d.ID, nil
}

// applyRelationship creates the accepted edge.
func (s *Store) applyRelationship(ctx context.Context, rev *Review, p map[string]any, decidedBy string) (string, error) {
	from := str(p, "from_entity")
	relType := str(p, "rel_type")
	to := str(p, "to_entity")
	r, err := s.campaigns.CreateRelationship(ctx, rev.CampaignID, from, relType, to, 0, "", "")
	if err != nil {
		return "", fmt.Errorf("accept relationship: %w", err)
	}
	return r.ID, nil
}

// applyEntity creates the accepted entity node.
func (s *Store) applyEntity(ctx context.Context, rev *Review, p map[string]any, decidedBy string) (string, error) {
	kind := str(p, "kind")
	name := str(p, "name")
	summary := str(p, "summary")
	e, err := s.campaigns.CreateEntity(ctx, rev.CampaignID, kind, name, summary, nil)
	if err != nil {
		return "", fmt.Errorf("accept entity: %w", err)
	}
	return e.ID, nil
}

// applyContradiction accepts both sides of a staged contradiction as canon and
// registers the pair — never picking a winner. Both facts land 'contested'
// through RegisterContradiction's downgrade.
func (s *Store) applyContradiction(ctx context.Context, rev *Review, decidedBy string) (string, error) {
	var sides struct {
		Subject   string   `json:"subject"`
		Predicate string   `json:"predicate"`
		Sides     []string `json:"sides"`
	}
	if err := json.Unmarshal([]byte(rev.Detail), &sides); err != nil || len(sides.Sides) < 2 {
		return "", fmt.Errorf("%w: contradiction item has no sides", ErrInvalid)
	}
	type created struct{ id, label string }
	var createdSides []created
	for _, candID := range sides.Sides {
		c, err := s.getCandidate(ctx, rev.CampaignID, candID)
		if err != nil {
			return "", err
		}
		payload := map[string]any{}
		if len(c.Payload) > 0 {
			_ = json.Unmarshal(c.Payload, &payload)
		}
		label, err := s.candidateSourceLabel(ctx, c)
		if err != nil {
			return "", err
		}
		sub := &Review{ID: rev.ID, CampaignID: rev.CampaignID, Kind: ReviewProposedFact, CandidateID: candID}
		id, err := s.applyFact(ctx, sub, payload, decidedBy)
		if err != nil {
			return "", err
		}
		createdSides = append(createdSides, created{id: id, label: label})
	}
	factSides := make([]campaign.FactVersionSide, 0, len(createdSides))
	for _, c := range createdSides {
		factSides = append(factSides, campaign.FactVersionSide{FactID: c.id, Label: c.label})
	}
	con, err := s.campaigns.RegisterContradiction(ctx, rev.CampaignID, sides.Subject, sides.Predicate, factSides, "")
	if err != nil {
		return "", fmt.Errorf("register contradiction: %w", err)
	}
	return con.ID, nil
}

// resolveFactRef turns a discovery's fact reference into a real fact id: a
// campaign fact id passes through, a staged local_id resolves through the
// fact candidate's accepted review item.
func (s *Store) resolveFactRef(ctx context.Context, campaignID, factRef string) (string, error) {
	var one int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM facts WHERE id = ? AND campaign_id = ?`, factRef, campaignID).Scan(&one); err != nil {
		return "", fmt.Errorf("resolve fact: %w", err)
	}
	if one > 0 {
		return factRef, nil
	}
	var candID string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM canon_candidates
		 WHERE campaign_id = ? AND kind = 'fact' AND json_extract(payload, '$.local_id') = ?
		 ORDER BY created_at, id LIMIT 1`, campaignID, factRef).Scan(&candID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: fact reference %q resolves to no staged fact and no campaign fact", ErrInvalid, factRef)
	}
	if err != nil {
		return "", fmt.Errorf("resolve fact candidate: %w", err)
	}
	var ref string
	err = s.db.QueryRowContext(ctx, `
		SELECT result_ref FROM canon_reviews
		 WHERE candidate_id = ? AND status IN ('accepted','modified') AND result_ref <> ''
		 ORDER BY updated_at DESC LIMIT 1`, candID).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: fact %q has not been accepted yet — accept the fact before its discovery", ErrInvalid, factRef)
	}
	if err != nil {
		return "", fmt.Errorf("resolve accepted fact: %w", err)
	}
	return ref, nil
}

/* ---------- rendering helpers ---------- */

// renderCandidate produces the queue headline and body for a staged
// candidate.
func renderCandidate(c Candidate) (subject, summary string) {
	var m map[string]any
	if len(c.Payload) > 0 {
		_ = json.Unmarshal(c.Payload, &m)
	}
	strv := func(k string) string { return str(m, k) }
	switch c.Kind {
	case KindFact:
		return "Fact", strv("statement")
	case KindEvent:
		return "Event", strv("summary")
	case KindDiscovery:
		return "Discovery", fmt.Sprintf("%s learned %s", strv("discovered_by"), strv("fact"))
	case KindRelationship:
		return "Relationship", fmt.Sprintf("%s — %s — %s", strv("from_entity"), strv("rel_type"), strv("to_entity"))
	case KindEntity:
		return "Entity — " + strv("kind"), strv("name") + ": " + strv("summary")
	default:
		return "Proposal", string(c.Payload)
	}
}

// candidateSourceLabel names where a candidate came from, for contradiction
// side labels: "the transcript by DM" or just "player journal".
func (s *Store) candidateSourceLabel(ctx context.Context, c Candidate) (string, error) {
	var kind, author, title string
	if err := s.db.QueryRowContext(ctx, `
		SELECT kind, COALESCE(author, ''), COALESCE(title, '') FROM session_sources WHERE id = ?`,
		c.SourceID).Scan(&kind, &author, &title); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return c.SourceID, nil
		}
		return "", err
	}
	label := kind
	if author != "" {
		label += " by " + author
	}
	if title != "" {
		label += " (" + title + ")"
	}
	return label, nil
}

/* ---------- payload helpers ---------- */

func parseFactSide(payload []byte) (factSide, bool) {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return factSide{}, false
	}
	subject := str(m, "subject")
	predicate := str(m, "predicate")
	if subject == "" || predicate == "" {
		return factSide{}, false
	}
	return factSide{
		subject: subject, predicate: predicate,
		objectEntity: str(m, "object_entity"), objectLiteral: str(m, "object_literal"),
		statement: str(m, "statement"),
	}, true
}

type factSide struct {
	subject, predicate, objectEntity, objectLiteral string
	statement                                       string
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	v, _ := m[k].(string)
	return strings.TrimSpace(v)
}

func intField(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

/* ---------- export ---------- */

// AppliedReviews returns the applied (accepted or modified) decisions of a
// session, or of the whole campaign when sessionID is empty. Dismissed items
// never appear.
func (s *Store) AppliedReviews(ctx context.Context, campaignID, sessionID string) ([]Review, error) {
	reviews, err := s.Reviews(ctx, campaignID, "")
	if err != nil {
		return nil, err
	}
	var out []Review
	for _, r := range reviews {
		if r.Status != ReviewAccepted && r.Status != ReviewModified {
			continue
		}
		if sessionID != "" && !s.reviewBelongsToSession(ctx, r, sessionID) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// ExportApplied renders a session's applied review decisions as Markdown: the
// DM's record of what the machine changed. Sessionless applied items (engine
// flags, contradictions among candidates from several sessions) are grouped
// at the end. Uses the review queue itself, so dismissed items never appear.
func (s *Store) ExportApplied(ctx context.Context, campaignID, sessionID string) (string, error) {
	reviews, err := s.AppliedReviews(ctx, campaignID, sessionID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Applied canon changes\n\n")
	if len(reviews) == 0 {
		b.WriteString("_No applied changes._\n")
		return b.String(), nil
	}
	for _, r := range reviews {
		fmt.Fprintf(&b, "## %s — %s\n\n", kindLabel(r.Kind), r.Subject)
		if r.Summary != "" {
			fmt.Fprintf(&b, "%s\n\n", r.Summary)
		}
		if r.Quote != "" {
			fmt.Fprintf(&b, "> %s\n\n", r.Quote)
		}
		if r.DecidedBy != "" || !r.DecidedAt.IsZero() {
			by := r.DecidedBy
			if by == "" {
				by = "the DM"
			}
			fmt.Fprintf(&b, "Accepted by %s", by)
			if !r.DecidedAt.IsZero() {
				fmt.Fprintf(&b, " at %s", r.DecidedAt.Format("2006-01-02 15:04 UTC"))
			}
			b.WriteString("\n\n")
		}
		if r.DecisionNote != "" {
			fmt.Fprintf(&b, "Note: %s\n\n", r.DecisionNote)
		}
	}
	return b.String(), nil
}

// reviewBelongsToSession reports whether a review item's candidate cites the
// given session. Items without a session citation belong to no session.
func (s *Store) reviewBelongsToSession(ctx context.Context, r Review, sessionID string) bool {
	if r.CandidateID == "" {
		return false
	}
	var sid string
	if err := s.db.QueryRowContext(ctx,
		`SELECT session_id FROM canon_candidates WHERE id = ?`, r.CandidateID).Scan(&sid); err != nil {
		return false
	}
	return sid == sessionID
}

// kindLabel renders a queue kind for the export and the UI.
func kindLabel(kind string) string {
	switch kind {
	case ReviewProposedFact:
		return "Fact"
	case ReviewProposedEvent:
		return "Event"
	case ReviewProposedDiscovery:
		return "Discovery"
	case ReviewProposedRelationship:
		return "Relationship"
	case ReviewProposedEntity:
		return "Entity"
	case ReviewLowAgreement:
		return "Low agreement"
	case ReviewContradiction:
		return "Contradiction"
	case ReviewEngineFlag:
		return "Engine flag"
	default:
		return kind
	}
}
