package canon

// Proposal batches (MAD-359): multi-object AI proposals through the one
// review gate.
//
// Every stage-5 generator produces many linked graph objects at once — a
// campaign skeleton is factions, a central secret, hooks and the edges
// between them — and ADR 3 says none of it becomes canon without a human
// decision. A batch is that decision's unit: one generator run stages N
// proposed_* items that reference each other by name, and one DecideBatch
// call is the human's accept-an-act / reject-it / override-two-names.
//
// The items are ordinary canon_reviews rows (same proposed_* kinds as
// extraction, so they render on the same screen), carrying two extra
// columns: batch_id, and depends_on — the JSON array of sibling review ids
// that makes applying a dependency-ordered operation. Applying resolves
// entity references against the entities this same batch already created,
// falling back to the campaign graph and staged candidates, in topological
// order with entities first among independent items.
//
// Provenance is the argument reveal.go makes for NPC reveals, settled once
// here for every generator: the span rule binds extraction candidates to
// transcript offsets, while a generated proposal's evidence is the premise
// and campaign state it was generated from — recorded in the batch's prompt
// and the item's detail. So every fact a batch writes carries a
// fact_provenance row with method 'ai_proposed', no span, and the human who
// accepted it.
//
// Atomicity: the shared database runs on a single connection, so a batch
// decision cannot open one transaction that also spans the graph writes —
// the campaign and knowledge stores own their transactions. DecideBatch
// follows DecideReview's claim-then-apply pattern instead: everything
// static is validated before the first claim, each item's terminal status
// is a guarded flip that only the first decision can land, and an apply
// failure rolls the failed item's claim back and stops the batch, leaving
// no decided-with-nothing-written items behind.

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

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

/* ---------- vocabularies ---------- */

// Batch statuses. Like a review decision, a batch's status is terminal: once
// accepted, partially accepted or dismissed, deciding it again is a no-op.
const (
	BatchOpen              = "open"
	BatchAccepted          = "accepted"
	BatchPartiallyAccepted = "partially_accepted"
	BatchDismissed         = "dismissed"
)

// Batch sources — the generators that stage batches. A plain string column,
// not a widened CHECK: a new generator names itself without a migration.
// Store-side this list is validated so typos surface as errors, not as
// silent rows no surface recognizes.
const (
	BatchSourceSkeleton    = "skeleton"
	BatchSourceStoryPlan   = "story_plan"
	BatchSourceScene       = "scene"
	BatchSourceNLCommand   = "nl_command"
	BatchSourceSessionPrep = "session_prep"
	BatchSourceTick        = "tick"
	BatchSourceDowntime    = "downtime"
	BatchSourceQuest       = "quest"
	BatchSourceLocation    = "location"
	BatchSourceDungeon     = "dungeon"
	BatchSourceRumor       = "rumor"
	BatchSourceJourney     = "journey"
	BatchSourceMonster     = "monster"
	BatchSourceItem        = "item"
	BatchSourceLoot        = "loot"
)

// batchSources is the validated source vocabulary.
var batchSources = map[string]bool{
	BatchSourceSkeleton: true, BatchSourceStoryPlan: true, BatchSourceScene: true,
	BatchSourceNLCommand: true, BatchSourceSessionPrep: true, BatchSourceTick: true,
	BatchSourceDowntime: true, BatchSourceQuest: true, BatchSourceLocation: true,
	BatchSourceDungeon: true, BatchSourceRumor: true, BatchSourceJourney: true,
	BatchSourceMonster: true, BatchSourceItem: true, BatchSourceLoot: true,
}

/* ---------- the stored shape ---------- */

// Batch is one generator run's proposal: N queue items behind one decision.
// Items is populated by the reads (GetBatch, StageBatch's return); the list
// read carries counts instead.
type Batch struct {
	ID         string
	CampaignID string
	Source     string
	Prompt     string
	Status     string
	CreatedBy  string
	CreatedAt  time.Time
	DecidedAt  time.Time

	Items     []Review
	ItemCount int
	OpenCount int
	// Skipped carries the input items StageBatch did not stage because an
	// item with the same dedup key already exists (open or decided) — the
	// never-resurrect rule. Not persisted; a read-back batch never has it.
	Skipped []BatchSkipped
}

// BatchSkipped is one StageBatch input that resolved to an existing queue
// item instead of a new one.
type BatchSkipped struct {
	InputID  string `json:"input_id"`
	ReviewID string `json:"review_id"`
	Status   string `json:"status"`
}

const batchCols = `id, campaign_id, source, prompt, status, created_by, created_at, decided_at`

func scanBatch(row interface{ Scan(...any) error }) (*Batch, error) {
	var (
		b         Batch
		decidedAt sql.NullInt64
		created   int64
	)
	if err := row.Scan(&b.ID, &b.CampaignID, &b.Source, &b.Prompt, &b.Status, &b.CreatedBy,
		&created, &decidedAt); err != nil {
		return nil, err
	}
	b.CreatedAt = time.UnixMilli(created).UTC()
	if decidedAt.Valid {
		b.DecidedAt = time.UnixMilli(decidedAt.Int64).UTC()
	}
	return &b, nil
}

/* ---------- staging ---------- */

// BatchItemInput is one proposed object in a batch. ID is the item's
// caller-chosen identity inside the batch (unique among siblings);
// DependsOn references sibling IDs, and becomes the stored depends_on (the
// minted review ids) once staged. Kind is a proposed_* review kind or its
// bare candidate kind ("entity", "fact", ...).
type BatchItemInput struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Subject   string         `json:"subject"`
	Summary   string         `json:"summary"`
	Payload   map[string]any `json:"payload"`
	DependsOn []string       `json:"depends_on"`
}

// BatchInput is one generator run to stage: Source names the generator,
// Prompt is the premise and campaign state it ran from (the batch's
// evidence), Items are the proposed objects.
type BatchInput struct {
	CampaignID string
	Source     string
	Prompt     string
	CreatedBy  string
	Items      []BatchItemInput
}

// reviewKindForBatchItem maps an input kind onto its proposed_* queue kind.
func reviewKindForBatchItem(kind string) (string, bool) {
	if k, ok := candidateKindReview[kind]; ok {
		return k, true
	}
	switch kind {
	case ReviewProposedFact, ReviewProposedEvent, ReviewProposedDiscovery,
		ReviewProposedRelationship, ReviewProposedEntity, ReviewProposedQuest:
		return kind, true
	case "quest":
		return ReviewProposedQuest, true
	case KindPlanTransition, ReviewProposedPlanTransition:
		return ReviewProposedPlanTransition, true
	case "rumor", ReviewProposedRumor:
		return ReviewProposedRumor, true
	}
	return "", false
}

// batchItemDedup is an item's dedup key: the kind plus the canonical JSON
// of its payload (json.Marshal orders map keys, so equal payloads hash
// equal). An identical re-proposal is skipped whatever state the earlier
// item is in — the queue's never-resurrect rule, kept per item.
func batchItemDedup(kind string, payload []byte) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + string(payload)))
	return "batch-item:" + hex.EncodeToString(sum[:])
}

// StageBatch stages one generator run: a proposal_batches row plus one
// canon_reviews row per item, in a single transaction, so a batch never
// exists half-staged. An input whose dedup key has already been seen
// (anywhere in the campaign, open or decided) is skipped and reported; its
// dependents' depends_on points at the existing review.
//
// The depends_on graph is validated BEFORE anything is written: every
// reference must name a sibling item, nothing depends on itself, and the
// graph must be acyclic.
func (s *Store) StageBatch(ctx context.Context, in BatchInput) (*Batch, error) {
	in.Source = strings.TrimSpace(in.Source)
	if strings.TrimSpace(in.CampaignID) == "" {
		return nil, fmt.Errorf("%w: campaign id is required", ErrInvalid)
	}
	if !batchSources[in.Source] {
		return nil, fmt.Errorf("%w: source %q is not a known generator", ErrInvalid, in.Source)
	}
	if _, err := s.loadCampaign(ctx, in.CampaignID); err != nil {
		return nil, err
	}
	if len(in.Items) == 0 {
		return nil, fmt.Errorf("%w: a batch needs at least one item", ErrInvalid)
	}

	// Validate every item, then the dependency graph, before any write.
	byInputID := map[string]int{}
	for i := range in.Items {
		it := &in.Items[i]
		it.ID = strings.TrimSpace(it.ID)
		if it.ID == "" {
			return nil, fmt.Errorf("%w: batch item %d has no id", ErrInvalid, i)
		}
		if _, dup := byInputID[it.ID]; dup {
			return nil, fmt.Errorf("%w: batch item id %q declared twice", ErrInvalid, it.ID)
		}
		if _, ok := reviewKindForBatchItem(it.Kind); !ok {
			return nil, fmt.Errorf("%w: batch item %q kind %q is not a proposed_* kind", ErrInvalid, it.ID, it.Kind)
		}
		if len(it.Payload) == 0 {
			return nil, fmt.Errorf("%w: batch item %q has no payload", ErrInvalid, it.ID)
		}
		byInputID[it.ID] = i
	}
	var path []int
	visiting := map[int]bool{}
	done := map[int]bool{}
	var visit func(int) error
	visit = func(i int) error {
		if done[i] {
			return nil
		}
		visiting[i] = true
		path = append(path, i)
		for _, dep := range in.Items[i].DependsOn {
			j, ok := byInputID[dep]
			if !ok {
				return fmt.Errorf("%w: batch item %q depends on %q, which is not in this batch",
					ErrInvalid, in.Items[i].ID, dep)
			}
			if j == i {
				return fmt.Errorf("%w: batch item %q depends on itself", ErrInvalid, in.Items[i].ID)
			}
			if visiting[j] {
				return fmt.Errorf("%w: batch item %q depends on %q, which depends on it",
					ErrInvalid, in.Items[i].ID, dep)
			}
			if err := visit(j); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		visiting[i] = false
		done[i] = true
		return nil
	}
	for i := range in.Items {
		if err := visit(i); err != nil {
			return nil, err
		}
	}

	// Pre-render the rows: payload JSON (the item's detail and dedup key)
	// and the subject/summary headline.
	type stagedItem struct {
		input  BatchItemInput
		kind   string
		dedup  string
		detail string
	}
	staged := make([]stagedItem, 0, len(in.Items))
	for _, it := range in.Items {
		kind, _ := reviewKindForBatchItem(it.Kind)
		payload, err := json.Marshal(it.Payload)
		if err != nil {
			return nil, fmt.Errorf("%w: batch item %q payload: %v", ErrInvalid, it.ID, err)
		}
		staged = append(staged, stagedItem{
			input: it, kind: kind,
			dedup:  batchItemDedup(kind, payload),
			detail: string(payload),
		})
	}

	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("batch tx: %w", err)
	}
	defer tx.Rollback()

	batchID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO proposal_batches (id, campaign_id, source, prompt, status, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		batchID, in.CampaignID, in.Source, in.Prompt, BatchOpen, in.CreatedBy, now); err != nil {
		return nil, fmt.Errorf("insert batch: %w", err)
	}

	// Two phases: resolve every item's review id first — a dedup hit
	// resolves to the existing review, keeping its state — then insert the
	// new rows with depends_on pointing at those ids.
	reviewID := make([]string, len(staged))
	isNew := make([]bool, len(staged))
	for i, st := range staged {
		var existing, status string
		err := tx.QueryRowContext(ctx,
			`SELECT id, status FROM canon_reviews WHERE campaign_id = ? AND dedup_key = ?`,
			in.CampaignID, st.dedup).Scan(&existing, &status)
		if err == nil {
			reviewID[i] = existing
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("lookup dedup: %w", err)
		}
		reviewID[i] = uuid.NewString()
		isNew[i] = true
	}
	for i, st := range staged {
		if !isNew[i] {
			continue // never-resurrect: the existing item keeps its state
		}
		deps := make([]string, 0, len(st.input.DependsOn))
		for _, dep := range st.input.DependsOn {
			deps = append(deps, reviewID[byInputID[dep]])
		}
		subject, summary := st.input.Subject, st.input.Summary
		if subject == "" || summary == "" {
			ds, dsum := renderCandidate(Candidate{Kind: strings.TrimPrefix(st.kind, "proposed_"), Payload: []byte(st.detail)})
			if subject == "" {
				subject = ds
			}
			if summary == "" {
				summary = dsum
			}
		}
		if err := insertReviewOn(ctx, tx, reviewID[i], in.CampaignID, st.kind, st.dedup, "", "", batchID, deps,
			subject, summary, st.detail, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("batch commit: %w", err)
	}

	out, err := s.GetBatch(ctx, in.CampaignID, batchID)
	if err != nil {
		return nil, err
	}
	for i, st := range staged {
		if !isNew[i] {
			var status string
			_ = s.db.QueryRowContext(ctx,
				`SELECT status FROM canon_reviews WHERE id = ?`, reviewID[i]).Scan(&status)
			out.Skipped = append(out.Skipped, BatchSkipped{InputID: st.input.ID, ReviewID: reviewID[i], Status: status})
		}
	}
	return out, nil
}

/* ---------- reads ---------- */

// Batches lists a campaign's batches, newest first, with item and open
// counts. An empty status returns every batch.
func (s *Store) Batches(ctx context.Context, campaignID, status string) ([]Batch, error) {
	q := `SELECT ` + batchCols + `,
			(SELECT COUNT(*) FROM canon_reviews r WHERE r.batch_id = proposal_batches.id),
			(SELECT COUNT(*) FROM canon_reviews r WHERE r.batch_id = proposal_batches.id AND r.status = 'open')
		  FROM proposal_batches WHERE campaign_id = ?`
	args := []any{campaignID}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list batches: %w", err)
	}
	defer rows.Close()
	var out []Batch
	for rows.Next() {
		var (
			b         Batch
			decidedAt sql.NullInt64
			created   int64
		)
		if err := rows.Scan(&b.ID, &b.CampaignID, &b.Source, &b.Prompt, &b.Status, &b.CreatedBy,
			&created, &decidedAt, &b.ItemCount, &b.OpenCount); err != nil {
			return nil, err
		}
		b.CreatedAt = time.UnixMilli(created).UTC()
		if decidedAt.Valid {
			b.DecidedAt = time.UnixMilli(decidedAt.Int64).UTC()
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBatch loads one batch with its items, oldest first, enriched the way
// the queue enriches them.
func (s *Store) GetBatch(ctx context.Context, campaignID, batchID string) (*Batch, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+batchCols+` FROM proposal_batches WHERE id = ? AND campaign_id = ?`, batchID, campaignID)
	b, err := scanBatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: batch %s", ErrNotFound, batchID)
	}
	if err != nil {
		return nil, fmt.Errorf("load batch: %w", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+reviewCols+` FROM canon_reviews WHERE batch_id = ? AND campaign_id = ? ORDER BY created_at, id`,
		batchID, campaignID)
	if err != nil {
		return nil, fmt.Errorf("load batch items: %w", err)
	}
	var items []Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, *r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	// Enrich after the rows are closed: enrich opens its own queries, and
	// the shared handle runs with a single connection.
	b.Items = items
	b.ItemCount = len(items)
	for i := range b.Items {
		if b.Items[i].Status == ReviewOpen {
			b.OpenCount++
		}
		if err := s.enrich(ctx, &b.Items[i]); err != nil {
			return nil, err
		}
	}
	return b, nil
}

/* ---------- deciding ---------- */

// ItemDecision is one per-item override inside a batch decision: modify
// (apply this payload instead) or dismiss (drop this item and everything
// that depends on it). ItemID is the item's review id.
type ItemDecision struct {
	ItemID   string          `json:"item_id"`
	Decision string          `json:"decision"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Note     string          `json:"note,omitempty"`
}

// BatchItemOutcome reports one item's fate in a batch decision: its final
// status, the graph object an acceptance wrote, and — when it was dropped,
// refused or left open — why.
type BatchItemOutcome struct {
	ReviewID  string `json:"review_id"`
	Kind      string `json:"kind"`
	Subject   string `json:"subject"`
	Status    string `json:"status"`
	ResultRef string `json:"result_ref,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// BatchDecision is the whole DecideBatch result.
type BatchDecision struct {
	Batch *Batch             `json:"batch"`
	Items []BatchItemOutcome `json:"items"`
}

// batchPlan is one open item's effective decision inside a DecideBatch
// run: the item, whether a per-item modify override replaced its payload,
// and the override's note.
type batchPlan struct {
	review  *Review
	modify  bool
	payload map[string]any
	note    string
}

// DecideBatch records the DM's decision on one open batch. Decision is
// accept (apply every open item) or dismiss (drop them all); items carries
// per-item overrides — modify with a replacement payload, or dismiss, which
// drops that item and transitively everything depending on it, each refusal
// reported rather than cascading silently.
//
// Items apply in topological order (dependencies first; among independent
// items, entities before the facts, relationships and events about them),
// through the same graph-write path an accepted extraction takes. A batch
// already decided is a no-op: it returns the current state unchanged — the
// never-resurrect rule.
func (s *Store) DecideBatch(ctx context.Context, campaignID, batchID, decision string, items []ItemDecision, decidedBy string) (*BatchDecision, error) {
	if decision != DecisionAccept && decision != DecisionDismiss {
		return nil, fmt.Errorf("%w: decision %q is not %s or %s", ErrInvalid, decision, DecisionAccept, DecisionDismiss)
	}
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	overrides := map[string]ItemDecision{}
	for _, it := range items {
		if it.Decision != DecisionModify && it.Decision != DecisionDismiss {
			return nil, fmt.Errorf("%w: item decision %q is not %s or %s", ErrInvalid, it.Decision, DecisionModify, DecisionDismiss)
		}
		if it.Decision == DecisionModify && len(it.Payload) == 0 {
			return nil, fmt.Errorf("%w: item %s modify needs the replacement payload", ErrInvalid, it.ItemID)
		}
		overrides[it.ItemID] = it
	}

	batch, err := s.GetBatch(ctx, campaignID, batchID)
	if err != nil {
		return nil, err
	}
	res := &BatchDecision{Batch: batch}
	for i := range batch.Items {
		r := &batch.Items[i]
		res.Items = append(res.Items, BatchItemOutcome{
			ReviewID: r.ID, Kind: r.Kind, Subject: r.Subject, Status: r.Status, ResultRef: r.ResultRef,
		})
	}

	// A decided batch keeps its decision forever: return the current state.
	if batch.Status != BatchOpen {
		return res, nil
	}

	// Split the items: already-decided ones keep their decisions; open ones
	// take the batch decision or a per-item override.
	outcome := map[string]*BatchItemOutcome{}
	for i := range res.Items {
		outcome[res.Items[i].ReviewID] = &res.Items[i]
	}
	var toApply []*batchPlan
	dismissedNow := map[string]bool{}
	for i := range batch.Items {
		r := &batch.Items[i]
		if r.Status != ReviewOpen {
			if outcome[r.ID].Reason == "" {
				outcome[r.ID].Reason = "already decided individually; kept its decision"
			}
			continue
		}
		if ov, ok := overrides[r.ID]; ok {
			if ov.Decision == DecisionDismiss {
				dismissedNow[r.ID] = true
				if err := s.claimReview(ctx, r, ReviewDismissed, ov.Note, decidedBy); err != nil {
					return nil, err
				}
				outcome[r.ID].Status = ReviewDismissed
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(ov.Payload, &payload); err != nil {
				return nil, fmt.Errorf("%w: item %s modified payload is not valid JSON: %v", ErrInvalid, r.ID, err)
			}
			toApply = append(toApply, &batchPlan{review: r, modify: true, payload: payload, note: ov.Note})
			continue
		}
		if decision == DecisionDismiss {
			dismissedNow[r.ID] = true
			if err := s.claimReview(ctx, r, ReviewDismissed, "batch dismissed", decidedBy); err != nil {
				return nil, err
			}
			outcome[r.ID].Status = ReviewDismissed
			outcome[r.ID].Reason = "batch dismissed"
			continue
		}
		toApply = append(toApply, &batchPlan{review: r})
	}
	if len(toApply) == 0 {
		return s.finishDecide(ctx, campaignID, batch, res, decision)
	}

	// The refusal pass: an item whose dependency is (or becomes) dismissed,
	// or whose dependency will neither be applied by this decision nor was
	// accepted before it, is refused — dropped with the reason reported,
	// nothing written for it. Refusal cascades transitively through the
	// batch's own dependency graph.
	byID := map[string]*Review{}
	for i := range batch.Items {
		byID[batch.Items[i].ID] = &batch.Items[i]
	}
	inApply := map[string]bool{} // this decision applies them, topologically first
	for _, p := range toApply {
		inApply[p.review.ID] = true
	}
	depStatus := func(id string) string { // status of a dependency outside this batch
		if r, ok := byID[id]; ok {
			return r.Status
		}
		var st string
		_ = s.db.QueryRowContext(ctx,
			`SELECT status FROM canon_reviews WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&st)
		return st
	}
	refused := map[string]string{}
	for changed := true; changed; {
		changed = false
		for i := range batch.Items {
			r := &batch.Items[i]
			if r.Status != ReviewOpen || refused[r.ID] != "" || dismissedNow[r.ID] {
				continue
			}
			for _, dep := range r.DependsOn {
				depRefused := false
				reason := ""
				switch {
				case dismissedNow[dep]:
					depRefused, reason = true, fmt.Sprintf("depends on %s, which was dismissed", depLabel(byID, dep))
				case refused[dep] != "":
					depRefused, reason = true, fmt.Sprintf("depends on %s, which was refused", depLabel(byID, dep))
				case inApply[dep]:
					// satisfied — this decision applies it first
				default:
					switch depStatus(dep) {
					case ReviewAccepted, ReviewModified:
						// satisfied by an earlier decision
					case ReviewDismissed:
						depRefused, reason = true, fmt.Sprintf("depends on %s, which was dismissed", depLabel(byID, dep))
					default: // open outside this decision, or a foreign id that resolves nowhere
						depRefused, reason = true, fmt.Sprintf("depends on %s, which has not been accepted", depLabel(byID, dep))
					}
				}
				if depRefused {
					refused[r.ID] = reason
					changed = true
					break
				}
			}
		}
	}

	// Topological order over the survivors: dependencies first; among
	// independent items, entities before facts before the edges and events
	// about them, then stage order. Names an item references resolve only
	// after the item that declares them applies.
	var order []*batchPlan
	satisfied := map[string]bool{}
	for _, r := range batch.Items {
		if r.Status == ReviewAccepted || r.Status == ReviewModified {
			satisfied[r.ID] = true
		}
	}
	remaining := make([]*batchPlan, 0, len(toApply))
	for _, p := range toApply {
		if refused[p.review.ID] == "" {
			remaining = append(remaining, p)
		}
	}
	for len(remaining) > 0 {
		pick := -1
		for i, p := range remaining {
			ready := true
			for _, dep := range p.review.DependsOn {
				if !satisfied[dep] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			if pick == -1 || batchItemBefore(p, remaining[pick]) {
				pick = i
			}
		}
		if pick == -1 {
			// Cannot happen for a graph StageBatch validated, but a
			// hand-edited depends_on must fail closed, not loop.
			for _, p := range remaining {
				refused[p.review.ID] = "dependency order is unresolvable"
			}
			break
		}
		order = append(order, remaining[pick])
		satisfied[remaining[pick].review.ID] = true
		remaining = append(remaining[:pick], remaining[pick+1:]...)
	}

	// Refuse the dependents: dropped with the reason, nothing written.
	for _, p := range toApply {
		if reason := refused[p.review.ID]; reason != "" {
			r := p.review
			if err := s.claimReview(ctx, r, ReviewDismissed, reason, decidedBy); err != nil {
				return nil, err
			}
			outcome[r.ID].Status = ReviewDismissed
			outcome[r.ID].Reason = reason
		}
	}

	// Apply in order. The first failure rolls its own claim back, stops the
	// batch and leaves the rest open for an individual look — the same
	// contract DecideReview makes for one item.
	resolution := newBatchResolution()
	for i, p := range order {
		r := p.review
		payload := p.payload
		if payload == nil {
			payload = map[string]any{}
			if len(r.Detail) > 0 {
				_ = json.Unmarshal([]byte(r.Detail), &payload)
			}
		}
		status := ReviewAccepted
		note := p.note
		if note == "" {
			note = "batch accepted"
		}
		if p.modify {
			status = ReviewModified
			if p.note == "" {
				note = "batch accepted with modifications"
			}
		}
		if err := s.claimReview(ctx, r, status, note, decidedBy); err != nil {
			return nil, err
		}
		resultRef, err := s.applyBatchItem(ctx, r, payload, decidedBy, resolution)
		if err != nil {
			s.unclaimReview(ctx, r)
			outcome[r.ID].Reason = "apply failed: " + err.Error()
			for _, q := range order[i+1:] {
				if outcome[q.review.ID].Reason == "" {
					outcome[q.review.ID].Reason = "not applied: the batch stopped after an item failed"
				}
			}
			break
		}
		if err := s.setBatchResultRef(ctx, r, resultRef); err != nil {
			return nil, err
		}
		outcome[r.ID].Status = status
		outcome[r.ID].ResultRef = resultRef
	}

	return s.finishDecide(ctx, campaignID, batch, res, decision)
}

// finishDecide wraps finishBatchDecision with the source-specific
// completions: a tick batch that leaves open hands off to the tick finalizer
// — the campaign clock's move by exactly the window and the sim_ticks row's
// status flip — and a downtime batch to the downtime finalizer, same
// contract under reason 'downtime'. Whatever path decided it. The finalizers
// are idempotent (guarded on their own rows' statuses), so a failed
// completion heals on the decision retry a decided batch allows.
func (s *Store) finishDecide(ctx context.Context, campaignID string, batch *Batch, res *BatchDecision, decision string) (*BatchDecision, error) {
	out, err := s.finishBatchDecision(ctx, campaignID, batch, res, decision)
	if err != nil {
		return nil, err
	}
	if out.Batch != nil && out.Batch.Status != BatchOpen {
		switch out.Batch.Source {
		case BatchSourceTick:
			if s.tickFinalizer != nil {
				if err := s.tickFinalizer.FinalizeTickBatch(ctx, out.Batch); err != nil {
					return nil, fmt.Errorf("finish tick batch %s: %w", out.Batch.ID, err)
				}
			}
		case BatchSourceDowntime:
			if s.downtimeFinalizer != nil {
				if err := s.downtimeFinalizer.FinalizeDowntimeBatch(ctx, out.Batch); err != nil {
					return nil, fmt.Errorf("finish downtime batch %s: %w", out.Batch.ID, err)
				}
			}
		case BatchSourceDungeon:
			// The dungeon finalizer is internal: it needs only the
			// campaign store every canon store already carries.
			if err := s.finalizeDungeonBatch(ctx, out.Batch); err != nil {
				return nil, fmt.Errorf("finish dungeon batch %s: %w", out.Batch.ID, err)
			}
		case BatchSourceJourney:
			if s.journeyFinalizer != nil {
				if err := s.journeyFinalizer.FinalizeJourneyBatch(ctx, out.Batch); err != nil {
					return nil, fmt.Errorf("finish journey batch %s: %w", out.Batch.ID, err)
				}
			}
		}
	}
	return out, nil
}

// batchItemBefore orders two independent items: entities first, then facts,
// then the relationships and events about them, then discoveries — the
// acceptPriority the queue's batch affordance already uses — then stage
// order.
func batchItemBefore(a, b *batchPlan) bool {
	pa, pb := acceptPriority[a.review.Kind], acceptPriority[b.review.Kind]
	if pa != pb {
		return pa < pb
	}
	if !a.review.CreatedAt.Equal(b.review.CreatedAt) {
		return a.review.CreatedAt.Before(b.review.CreatedAt)
	}
	return a.review.ID < b.review.ID
}

func depLabel(byID map[string]*Review, id string) string {
	if r, ok := byID[id]; ok {
		if r.Subject != "" {
			return r.Subject
		}
		return r.ID
	}
	return id
}

// setBatchResultRef records the graph object an accepted batch item wrote.
func (s *Store) setBatchResultRef(ctx context.Context, rev *Review, resultRef string) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE canon_reviews SET result_ref = ?, updated_at = ? WHERE id = ? AND campaign_id = ?`,
		resultRef, s.now().UnixMilli(), rev.ID, rev.CampaignID); err != nil {
		return fmt.Errorf("record result: %w", err)
	}
	return nil
}

// finishBatchDecision computes the batch's status from its items' final
// statuses — accepted only when everything landed, dismissed only when
// nothing did, partially_accepted for the mix — stamps decided_at when it
// leaves open, and re-reads the batch for the return value.
func (s *Store) finishBatchDecision(ctx context.Context, campaignID string, batch *Batch, res *BatchDecision, decision string) (*BatchDecision, error) {
	var accepted, dismissed, open int
	for _, it := range res.Items {
		switch it.Status {
		case ReviewAccepted, ReviewModified:
			accepted++
		case ReviewDismissed:
			dismissed++
		default:
			open++
		}
	}
	status := BatchOpen
	switch {
	case len(res.Items) == 0:
		// Nothing to decide (every input dedup-skipped at staging): close
		// it by the decision rather than leaving an immortal empty batch.
		if decision == DecisionAccept {
			status = BatchAccepted
		} else {
			status = BatchDismissed
		}
	case accepted == 0 && dismissed == 0:
		// Nothing landed (every apply failed): the batch stays decidable.
		status = BatchOpen
	case accepted == 0 && open == 0:
		status = BatchDismissed
	case dismissed == 0 && open == 0:
		status = BatchAccepted
	default:
		status = BatchPartiallyAccepted
	}
	if status != BatchOpen {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE proposal_batches SET status = ?, decided_at = ? WHERE id = ? AND campaign_id = ?`,
			status, s.now().UnixMilli(), batch.ID, campaignID); err != nil {
			return nil, fmt.Errorf("finish batch: %w", err)
		}
	}
	out, err := s.GetBatch(ctx, campaignID, batch.ID)
	if err != nil {
		return nil, err
	}
	res.Batch = out // keep the per-item outcomes: they carry reasons a re-read lacks
	return res, nil
}

/* ---------- batch-aware application into the graph ---------- */

// batchResolution remembers what this batch's applied items created, so a
// later item's references resolve against them: entity names and local ids,
// fact statements and local ids, event local ids. Lookup is
// case-insensitive on names (the "Duke Aldric" case), exact on local ids.
type batchResolution struct {
	entities map[string]string
	facts    map[string]string
	events   map[string]string
}

func newBatchResolution() *batchResolution {
	return &batchResolution{entities: map[string]string{}, facts: map[string]string{}, events: map[string]string{}}
}

func (b *batchResolution) noteEvent(localID, id string) {
	if localID != "" {
		b.events[localID] = id
	}
}

func (b *batchResolution) noteEntity(localID, name, id string) {
	if name != "" {
		b.entities[strings.ToLower(strings.TrimSpace(name))] = id
	}
	if localID != "" {
		b.entities[localID] = id
	}
}

func (b *batchResolution) noteFact(localID, statement, id string) {
	if statement != "" {
		b.facts[strings.TrimSpace(statement)] = id
	}
	if localID != "" {
		b.facts[localID] = id
	}
}

// resolveBatchEntityRef resolves an entity reference for a batch item: the
// entities this batch already created (by name or local id) come first,
// then the campaign graph and staged candidates — resolveEntityRef.
func (s *Store) resolveBatchEntityRef(ctx context.Context, campaignID, ref string, res *batchResolution) (string, error) {
	if res != nil {
		if id, ok := res.entities[strings.ToLower(strings.TrimSpace(ref))]; ok {
			return id, nil
		}
	}
	return s.resolveEntityRef(ctx, campaignID, ref)
}

// resolveBatchFactRef is resolveBatchEntityRef for a discovery's fact: a
// fact this batch created (by statement or local id), then resolveFactRef.
func (s *Store) resolveBatchFactRef(ctx context.Context, campaignID, ref string, res *batchResolution) (string, error) {
	if res != nil {
		if id, ok := res.facts[strings.TrimSpace(ref)]; ok {
			return id, nil
		}
	}
	return s.resolveFactRef(ctx, campaignID, ref)
}

// applyBatchItem writes one accepted (or modified) batch item into the
// campaign graph through the same stores an accepted extraction uses, and
// returns the graph object id it created. It is the only apply path for
// batch items; provenance says what happened — the model proposed it, the
// human accepted it through this queue.
func (s *Store) applyBatchItem(ctx context.Context, rev *Review, p map[string]any, decidedBy string, res *batchResolution) (string, error) {
	switch rev.Kind {
	case ReviewProposedEntity:
		return s.applyGeneratedEntity(ctx, rev, p, decidedBy, res)
	case ReviewProposedFact:
		return s.applyGeneratedFact(ctx, rev, p, decidedBy, res)
	case ReviewProposedRelationship:
		return s.applyGeneratedRelationship(ctx, rev, p, decidedBy, res)
	case ReviewProposedEvent:
		return s.applyGeneratedEvent(ctx, rev, p, decidedBy, res)
	case ReviewProposedDiscovery:
		return s.applyGeneratedDiscovery(ctx, rev, p, decidedBy, res)
	case ReviewProposedPlanTransition:
		return s.applyGeneratedPlanTransition(ctx, rev, p, decidedBy, res)
	case ReviewProposedQuest:
		return s.applyGeneratedQuest(ctx, rev, p, decidedBy, res)
	case ReviewProposedRumor:
		return s.applyGeneratedRumor(ctx, rev, p, decidedBy)
	default:
		return "", fmt.Errorf("%w: batch item kind %s cannot be accepted here", ErrInvalid, rev.Kind)
	}
}

// applyGeneratedEntity writes an accepted entity item into the graph. Two
// payloads share one kind: a whole new entity (the default), and the
// flesh-out update (MAD-372) — entity_update names an existing entity
// whose payload the item's payload block replaces and whose summary is
// only set when it was empty, because a flesh-out proposes around what is
// already there, never replacing it. The structured payload blocks (a
// location's place and travel blocks, an npc's agent block) are written
// verbatim; the generator computed the merge at staging time.
func (s *Store) applyGeneratedEntity(ctx context.Context, rev *Review, p map[string]any, decidedBy string, res *batchResolution) (string, error) {
	kind := str(p, "kind")
	name := str(p, "name")
	summary := str(p, "summary")
	payload, _ := p["payload"].(map[string]any)
	if updateID := str(p, "entity_update"); updateID != "" {
		existing, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, rev.CampaignID, updateID)
		if err != nil {
			return "", fmt.Errorf("accept entity update: %w", err)
		}
		var summaryPtr *string
		if summary != "" && existing.Summary == "" {
			summaryPtr = &summary
		}
		updated, err := s.campaigns.UpdateEntity(ctx, rev.CampaignID, updateID, nil, summaryPtr, nil, payload)
		if err != nil {
			return "", fmt.Errorf("accept entity update: %w", err)
		}
		res.noteEntity(str(p, "local_id"), updated.Name, updated.ID)
		return updated.ID, nil
	}
	e, err := s.campaigns.CreateEntity(ctx, rev.CampaignID, kind, name, summary, payload)
	if err != nil {
		return "", fmt.Errorf("accept entity: %w", err)
	}
	res.noteEntity(str(p, "local_id"), name, e.ID)
	if len(rev.Detail) > 0 {
		var staged map[string]any
		if json.Unmarshal([]byte(rev.Detail), &staged) == nil {
			if stagedName := str(staged, "name"); stagedName != "" && stagedName != name {
				res.noteEntity("", stagedName, e.ID)
			}
		}
	}
	return e.ID, nil
}

// applyGeneratedFact creates the accepted fact with ai_proposed
// provenance: no span, the human's acceptance recorded. The payload may be
// the staged one (accept) or a DM-corrected one (modify).
func (s *Store) applyGeneratedFact(ctx context.Context, rev *Review, p map[string]any, decidedBy string, res *batchResolution) (string, error) {
	statement := str(p, "statement")
	if statement == "" {
		return "", fmt.Errorf("%w: fact payload has no statement", ErrInvalid)
	}
	predicate := str(p, "predicate")
	if predicate == "" {
		return "", fmt.Errorf("%w: fact payload has no predicate", ErrInvalid)
	}
	visibility := str(p, "visibility")
	if visibility == "" {
		visibility = campaign.VisibilityPublic
	}
	subject, err := s.resolveBatchEntityRef(ctx, rev.CampaignID, str(p, "subject"), res)
	if err != nil {
		return "", err
	}
	objectLiteral := str(p, "object_literal")
	var objectEntity string
	if ref := str(p, "object_entity"); ref != "" {
		objectEntity, err = s.resolveBatchEntityRef(ctx, rev.CampaignID, ref, res)
		if err != nil {
			return "", err
		}
		// A fact's object is an entity or a literal, never both.
		objectLiteral = ""
	}
	fact, err := s.campaigns.CreateFact(ctx, rev.CampaignID, subject, predicate, objectEntity, objectLiteral,
		statement, campaign.ConfidenceCanon, visibility, decidedBy, []campaign.ProvenanceInput{{
			Quote:      statement,
			Method:     campaign.MethodAIProposed,
			AcceptedBy: decidedBy,
			AcceptedAt: s.now(),
		}})
	if err != nil {
		return "", fmt.Errorf("accept fact: %w", err)
	}
	res.noteFact(str(p, "local_id"), statement, fact.ID)
	return fact.ID, nil
}

// applyGeneratedRelationship creates the accepted edge. Both ends may be
// names or local ids of entities this same batch created.
func (s *Store) applyGeneratedRelationship(ctx context.Context, rev *Review, p map[string]any, decidedBy string, res *batchResolution) (string, error) {
	relType := str(p, "rel_type")
	from, err := s.resolveBatchEntityRef(ctx, rev.CampaignID, str(p, "from_entity"), res)
	if err != nil {
		return "", err
	}
	to, err := s.resolveBatchEntityRef(ctx, rev.CampaignID, str(p, "to_entity"), res)
	if err != nil {
		return "", err
	}
	r, err := s.campaigns.CreateRelationship(ctx, rev.CampaignID, from, relType, to, 0, "", "")
	if err != nil {
		return "", fmt.Errorf("accept relationship: %w", err)
	}
	return r.ID, nil
}

// applyGeneratedEvent creates the accepted event and its participants.
// Every entity reference (location, participants) is resolved BEFORE the
// event exists, so a bad reference fails the accept with nothing
// half-written; a duplicate participant is dropped rather than failing.
func (s *Store) applyGeneratedEvent(ctx context.Context, rev *Review, p map[string]any, decidedBy string, res *batchResolution) (string, error) {
	summary := str(p, "summary")
	if summary == "" {
		return "", fmt.Errorf("%w: event payload has no summary", ErrInvalid)
	}
	location := str(p, "location")
	if location != "" {
		var err error
		location, err = s.resolveBatchEntityRef(ctx, rev.CampaignID, location, res)
		if err != nil {
			return "", err
		}
	}
	type participant struct{ entity, role string }
	var parts []participant
	seen := map[string]bool{}
	if arr, ok := p["participants"].([]any); ok {
		for _, part := range arr {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			entity := str(pm, "entity")
			if entity == "" {
				continue
			}
			id, err := s.resolveBatchEntityRef(ctx, rev.CampaignID, entity, res)
			if err != nil {
				return "", err
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			parts = append(parts, participant{id, str(pm, "role")})
		}
	}
	var clockAt *int64
	if v, ok := intField(p["clock_at"]); ok {
		clockAt = &v
	}
	// A generated event belongs to no session: it is a design proposal,
	// not something that happened at the table.
	ev, err := s.campaigns.CreateEvent(ctx, rev.CampaignID, "", summary, clockAt, location)
	if err != nil {
		return "", fmt.Errorf("accept event: %w", err)
	}
	res.noteEvent(str(p, "local_id"), ev.ID)
	for _, part := range parts {
		if err := s.campaigns.AddParticipant(ctx, rev.CampaignID, ev.ID, part.entity, part.role); err != nil {
			return "", fmt.Errorf("accept event participant: %w", err)
		}
	}
	return ev.ID, nil
}

// applyGeneratedPlanTransition applies an accepted tick advance to its plan
// through the faction store — the same persistence AdvancePlan writes, minus
// re-deriving the arithmetic, which the tick already did deterministically.
// The payload is the whole advance: from/to state, the carried progress, the
// moves with their carries, and the event that caused it (a sibling local
// id this batch already applied). The plan must still sit where the advance
// was computed from; if the world moved it since, the accept fails loudly
// and nothing is written.
func (s *Store) applyGeneratedPlanTransition(ctx context.Context, rev *Review, p map[string]any, decidedBy string, res *batchResolution) (string, error) {
	if err := s.requireFactions(); err != nil {
		return "", err
	}
	planID := str(p, "plan_id")
	if planID == "" {
		return "", fmt.Errorf("%w: plan transition payload has no plan_id", ErrInvalid)
	}
	pr := faction.Progression{
		FromState:    str(p, "from_state"),
		ToState:      str(p, "to_state"),
		FromProgress: numField(p["from_progress"]),
		ToProgress:   numField(p["to_progress"]),
		Days:         int(numField(p["days"])),
		Gain:         numField(p["gain"]),
		Halted:       str(p, "halted"),
	}
	if arr, ok := p["moves"].([]any); ok {
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			pr.Moves = append(pr.Moves, faction.StepMove{
				To:    str(m, "to"),
				Carry: numField(m["carry"]),
			})
		}
	}
	if arr, ok := p["terms"].([]any); ok {
		for _, item := range arr {
			t, ok := item.(map[string]any)
			if !ok {
				continue
			}
			pr.Terms = append(pr.Terms, faction.Term{
				Label: str(t, "label"), Delta: numField(t["delta"]), Reason: str(t, "reason"),
			})
		}
	}
	eventID := ""
	if ref := str(p, "event"); ref != "" && res != nil {
		eventID = res.events[ref]
	}
	plan, err := s.factions.ApplyAdvance(ctx, rev.CampaignID, planID, pr, eventID)
	if err != nil {
		return "", fmt.Errorf("accept plan transition: %w", err)
	}
	return plan.ID, nil
}

// applyGeneratedQuest writes an accepted quest proposal into the campaign
// graph. Two payloads share one kind: a whole new quest (the designer's
// output — machine, cast links, state facts) and a machine edit of an
// existing quest (the "branch this quest" operation, which carries quest_id).
// Every entity and fact reference is resolved BEFORE anything is written, so
// a bad reference fails the accept with nothing half-written.
func (s *Store) applyGeneratedQuest(ctx context.Context, rev *Review, p map[string]any, decidedBy string, res *batchResolution) (string, error) {
	if err := s.requireGraphStores(); err != nil {
		return "", err
	}
	machineRaw, err := json.Marshal(p["machine"])
	if err != nil {
		return "", fmt.Errorf("%w: quest payload has no machine: %v", ErrInvalid, err)
	}
	machine, err := campaign.ParseStateMachine(string(machineRaw))
	if err != nil {
		return "", err
	}

	// The branch operation: an edit of a quest the campaign already holds.
	// UpdateQuest is the same path a hand edit takes — it validates the
	// machine and refuses to orphan any recorded transition.
	if questID := str(p, "quest_id"); questID != "" {
		if _, err := s.campaigns.UpdateQuest(ctx, rev.CampaignID, questID, campaign.QuestUpdate{Machine: &machine}); err != nil {
			return "", fmt.Errorf("accept quest edit: %w", err)
		}
		return questID, nil
	}

	name := str(p, "name")
	if name == "" {
		return "", fmt.Errorf("%w: quest payload has no name", ErrInvalid)
	}
	type linkSlot struct{ entity, role string }
	type factSlot struct{ state, fact, disposition string }
	var links []linkSlot
	seenLink := map[string]bool{}
	if arr, ok := p["entities"].([]any); ok {
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			entity := str(m, "entity")
			role := str(m, "role")
			if entity == "" || role == "" {
				continue
			}
			id, err := s.resolveBatchEntityRef(ctx, rev.CampaignID, entity, res)
			if err != nil {
				return "", err
			}
			k := id + "\x00" + role
			if seenLink[k] {
				continue
			}
			seenLink[k] = true
			links = append(links, linkSlot{id, role})
		}
	}
	var facts []factSlot
	seenFact := map[string]bool{}
	if arr, ok := p["state_facts"].([]any); ok {
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			state := str(m, "state")
			factRef := str(m, "fact")
			disposition := str(m, "disposition")
			if state == "" || factRef == "" {
				continue
			}
			if disposition == "" {
				disposition = campaign.QuestFactReveals
			}
			id, err := s.resolveBatchFactRef(ctx, rev.CampaignID, factRef, res)
			if err != nil {
				return "", err
			}
			k := state + "\x00" + id
			if seenFact[k] {
				continue
			}
			seenFact[k] = true
			facts = append(facts, factSlot{state, id, disposition})
		}
	}

	q, err := s.campaigns.CreateQuest(ctx, rev.CampaignID, campaign.QuestInput{
		Name: name, Summary: str(p, "summary"), Machine: machine,
	})
	if err != nil {
		return "", fmt.Errorf("accept quest: %w", err)
	}
	for _, l := range links {
		if _, err := s.campaigns.AddQuestEntity(ctx, rev.CampaignID, q.ID, l.entity, l.role); err != nil {
			return "", fmt.Errorf("accept quest cast (%s): %w", l.role, err)
		}
	}
	for _, f := range facts {
		if _, err := s.campaigns.SetQuestStateFact(ctx, rev.CampaignID, q.ID, f.state, f.fact, f.disposition); err != nil {
			return "", fmt.Errorf("accept quest state fact (%s): %w", f.state, err)
		}
	}
	return q.ID, nil
}

// numField reads a JSON number field, 0 when absent or not a number.
func numField(v any) float64 {
	if n, ok := v.(float64); ok {
		return n
	}
	return 0
}

// applyGeneratedDiscovery records the accepted discovery with its awareness
// row. The fact may be one this same batch created; the event the discovery
// happened at may be a sibling this same batch applied (since_event carries
// its local id).
func (s *Store) applyGeneratedDiscovery(ctx context.Context, rev *Review, p map[string]any, decidedBy string, res *batchResolution) (string, error) {
	factRef := str(p, "fact")
	if factRef == "" {
		return "", fmt.Errorf("%w: discovery payload has no fact", ErrInvalid)
	}
	factID, err := s.resolveBatchFactRef(ctx, rev.CampaignID, factRef, res)
	if err != nil {
		return "", err
	}
	discoveredBy := str(p, "discovered_by")
	if discoveredBy != "" && discoveredBy != campaign.PartyKnower {
		discoveredBy, err = s.resolveBatchEntityRef(ctx, rev.CampaignID, discoveredBy, res)
		if err != nil {
			return "", err
		}
	}
	var sinceEvent string
	if ref := str(p, "since_event"); ref != "" && res != nil {
		sinceEvent = res.events[ref]
	}
	confidence := 1.0
	if c, ok := p["confidence"].(float64); ok && c >= 0 && c <= 1 {
		confidence = c
	}
	d, err := s.knowledge.RecordDiscovery(ctx, knowledge.RecordDiscoveryInput{
		CampaignID:   rev.CampaignID,
		FactID:       factID,
		DiscoveredBy: discoveredBy,
		Method:       str(p, "method"),
		Confidence:   confidence,
		AcceptedBy:   decidedBy,
		Stance:       str(p, "stance"),
		SinceEvent:   sinceEvent,
	})
	if err != nil {
		return "", fmt.Errorf("accept discovery: %w", err)
	}
	return d.ID, nil
}

// applyGeneratedRumor writes the accepted rumour into the mill: the row
// itself plus its holders, through the same knowledge store the DM-facing
// API uses. The truth value rode in the review queue's DM-only surface and
// lands in the DM-only column; a player scope never sees either. Holder
// variants are part of the payload — the drifted wording is the charm, and
// the generator computed the distribution as a join before any prompt.
func (s *Store) applyGeneratedRumor(ctx context.Context, rev *Review, p map[string]any, decidedBy string) (string, error) {
	statement := str(p, "statement")
	if statement == "" {
		return "", fmt.Errorf("%w: rumor payload has no statement", ErrInvalid)
	}
	truth := str(p, "truth")
	if truth != campaign.RumorTruthTrue && truth != campaign.RumorTruthFalse && truth != campaign.RumorTruthDistorted {
		return "", fmt.Errorf("%w: rumor payload truth %q", ErrInvalid, truth)
	}
	in := knowledge.RumorInput{
		Statement: statement, Truth: truth,
		AboutEntity: str(p, "about_entity"), FactID: str(p, "fact_id"),
		Origin: str(p, "origin"), Spread: str(p, "spread"),
		Status: campaign.RumorStatusCirculating, CreatedBy: decidedBy,
	}
	if in.Spread == "" {
		in.Spread = campaign.RumorSpreadLocal
	}
	r, err := s.knowledge.CreateRumor(ctx, rev.CampaignID, in)
	if err != nil {
		return "", fmt.Errorf("accept rumor: %w", err)
	}
	if holders, ok := p["holders"].([]any); ok {
		for _, h := range holders {
			m, ok := h.(map[string]any)
			if !ok {
				continue
			}
			entity := str(m, "entity")
			if entity == "" {
				continue
			}
			if _, err := s.knowledge.SetRumorHolder(ctx, rev.CampaignID, r.ID, entity, str(m, "variant"), ""); err != nil {
				return "", fmt.Errorf("accept rumor holder: %w", err)
			}
		}
	}
	return r.ID, nil
}
