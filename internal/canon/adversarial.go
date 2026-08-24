package canon

// The adversarial second pass (MAD-308): the double-agent validation Collin
// called out, ported from Arda's validation stage. One work item is one staged
// candidate; an independent model client in an explicitly skeptical stance
// re-checks it against ONLY its own evidence — the quoted span, the
// surrounding context window, and the campaign entities it references. Not
// the rest of the campaign, not the model's own D&D knowledge. The prime
// directive, ported nearly verbatim:
//
//	Do NOT use your knowledge of tabletop tropes, or the rest of this
//	campaign, to rescue a candidate the transcript does not support. Outside
//	knowledge may deepen suspicion, never confirmation.
//
// The monotonicity rule (Arda policy §7, epistemics.md): a machine pass may
// only downgrade or flag — never upgrade, never delete. It is enforced in
// three places, each independently testable:
//
//   - the wire contract offers no upgrade verdict at all;
//   - applyVerdict rejects a downgrade whose proposed confidence is not
//     strictly below the candidate's current value — rejected and logged,
//     never applied;
//   - the confidence UPDATE that lands a downgrade carries a `confidence > ?`
//     guard, so if another pass already lowered the candidate further, this
//     one cannot raise it back. This pass and the deterministic engine
//     (MAD-309) therefore compose in either order.
//
// Verdicts land in canon_verdicts, keyed UNIQUE by candidate + prompt version
// + content checksum (the candidate's own checksum): idempotent, resumable,
// budget-bounded, and nothing is billed twice. The raw validator response is
// stored on the verdict row itself, with its model and tokens — a verdict's
// provenance bottoms out in exactly what the validator said.
//
// A discovery is the highest-risk candidate kind: "the party learned X" is
// easy to over-read from a transcript in which the DM said X out loud but no
// character was present to hear it. The prompt asks specifically: which
// character, in the fiction, perceived this, and where in the span do they
// perceive it?

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

/* ---------- vocabulary ---------- */

// VALIDATE_PROMPT_VERSION keys the verdict ledger, the way PROMPT_VERSION
// keys the extraction ledger. Changing the validator prompts in a way that
// affects verdicts means bumping this string — never editing in place —
// which re-validates the queue under the new contract.
const VALIDATE_PROMPT_VERSION = "canon-validate-001"

// Verdict values: what the adversarial pass concluded.
const (
	VerdictAgree      = "agree"       // evidence supports the candidate
	VerdictDowngrade  = "downgrade"   // support is weaker than the extractor claimed
	VerdictFlagReview = "flag_review" // the checker could not decide; a human must
)

// Verdict statuses: what the machine did with the model's proposal.
const (
	VerdictApplied     = "applied"     // honored: confirmed, downgraded, or flagged
	VerdictRejected    = "rejected"    // monotonicity refused it; logged, not applied
	VerdictUnparseable = "unparseable" // no usable verdict; flagged, never passed
)

// Rejection reasons — the vocabulary logged on a refused verdict proposal.
const (
	RejectUpgrade       = "upgrade_proposed" // not strictly below current confidence
	RejectNoConfidence  = "downgrade_without_confidence"
	RejectBadConfidence = "proposed_confidence_out_of_range"
)

// ValidateContextWindow is the bytes of surrounding source shown on each side
// of the quoted span. Enough to judge pronouns and scene presence; not enough
// to smuggle the rest of the campaign in, which the evidence boundary
// forbids anyway.
const ValidateContextWindow = 1200

// RunValidate is the canon_runs kind of a validation pass. (Extraction runs
// carry "extract"; the shared table is why migration 0007 widened the CHECK.)
const RunValidate = "validate"

/* ---------- the verdict wire ---------- */

// verdictWire is the JSON contract the validator must satisfy, as declared in
// validateSystemPrompt. Tolerant the same way the extraction wire is: numbers
// may arrive stringified, and only verdict and agreement are load-bearing on
// every response.
type verdictWire struct {
	Verdict   string     `json:"verdict"`
	Agreement *flexFloat `json:"agreement"`
	Rationale string     `json:"rationale"`
	Proposed  *flexFloat `json:"proposed_confidence"`
}

// parseVerdictResponse parses one validator response. Field-level problems
// are returned as strings rather than raised, so one malformed field costs
// only itself; the decision rules then treat whatever is missing as
// unusable — and unusable is always the conservative flag, never a pass.
func parseVerdictResponse(text string) (verdictWire, []string) {
	var w verdictWire
	block, err := jsonBlock(text)
	if err != nil {
		return w, []string{err.Error()}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(block), &raw); err != nil {
		return w, []string{fmt.Sprintf("invalid JSON: %v", err)}
	}
	var problems []string
	if item, ok := raw["verdict"]; ok {
		if err := json.Unmarshal(item, &w.Verdict); err != nil {
			problems = append(problems, fmt.Sprintf("verdict: %v", err))
		}
	} else {
		problems = append(problems, "verdict: missing")
	}
	if item, ok := raw["agreement"]; ok {
		if err := json.Unmarshal(item, &w.Agreement); err != nil {
			problems = append(problems, fmt.Sprintf("agreement: %v", err))
		}
	} else {
		problems = append(problems, "agreement: missing")
	}
	if item, ok := raw["rationale"]; ok {
		if err := json.Unmarshal(item, &w.Rationale); err != nil {
			problems = append(problems, fmt.Sprintf("rationale: %v", err))
		}
	}
	if item, ok := raw["proposed_confidence"]; ok {
		if err := json.Unmarshal(item, &w.Proposed); err != nil {
			problems = append(problems, fmt.Sprintf("proposed_confidence: %v", err))
		}
	}
	return w, problems
}

/* ---------- the decision rules (pure) ---------- */

// verdictDecision is the outcome of applying the rules to one parsed response
// against one candidate's current confidence.
type verdictDecision struct {
	Verdict          string // effective: agree / downgrade / flag_review
	Status           string // applied / rejected / unparseable
	Agreement        float64
	Rationale        string
	Proposed         float64 // meaningful when a downgrade was applied
	HasProposed      bool
	ConfidenceBefore float64
	ConfidenceAfter  float64 // equal to before unless a downgrade landed
	RejectionReason  string
	LowAgreement     bool // model agreed/downgraded below threshold; coerced to a flag
}

// applyVerdict turns one parsed response into the machine's actual action.
// Pure: same wire, same current confidence, same threshold, same decision —
// the property that makes the monotonicity invariant honest to test.
//
// The order of the rules is the order of their authority:
//
//  1. an unusable response (unknown verdict, missing or out-of-range
//     agreement) becomes the machine's own conservatism — flag_review, never
//     a silent pass;
//  2. a downgrade that does not strictly lower confidence is rejected and
//     logged (the monotonicity rule; upgrades are never applied);
//  3. an agree or downgrade scoring below the threshold becomes flag_review
//     ("low_agreement — the checker could not decide");
//  4. everything else is applied as spoken.
func applyVerdict(current float64, w verdictWire, threshold float64) verdictDecision {
	d := verdictDecision{
		Verdict:          VerdictFlagReview,
		ConfidenceBefore: current,
		ConfidenceAfter:  current,
	}

	unusable := func(detail string) verdictDecision {
		return unparseableDecision(current, detail)
	}
	switch w.Verdict {
	case VerdictAgree, VerdictDowngrade, VerdictFlagReview:
	default:
		return unusable(fmt.Sprintf("verdict %q is not one of agree, downgrade or flag_review", w.Verdict))
	}
	if w.Agreement == nil {
		return unusable("agreement is missing")
	}
	agreement := float64(*w.Agreement)
	if agreement < 0 || agreement > 1 {
		return unusable(fmt.Sprintf("agreement %v is outside 0..1", agreement))
	}
	d.Agreement = agreement
	d.Rationale = strings.TrimSpace(w.Rationale)

	if w.Verdict == VerdictDowngrade {
		if w.Proposed == nil {
			d.Status = VerdictRejected
			d.RejectionReason = RejectNoConfidence
			return d
		}
		proposed := float64(*w.Proposed)
		// The proposal is recorded even on rejection — the log must carry
		// what the model wanted, not only what it got. An out-of-range
		// proposal is the exception: the column's CHECK would reject it,
		// so the value survives only in the raw response.
		d.Proposed = proposed
		d.HasProposed = proposed >= 0 && proposed <= 1
		if !d.HasProposed {
			d.Status = VerdictRejected
			d.RejectionReason = RejectBadConfidence
			return d
		}
		if proposed >= current {
			// The one the whole invariant hangs on: a downgrade that does
			// not strictly lower is an upgrade in disguise, and a machine
			// pass never upgrades. Logged, not applied.
			d.Status = VerdictRejected
			d.RejectionReason = RejectUpgrade
			return d
		}
	}

	if agreement < threshold && w.Verdict != VerdictFlagReview {
		// The checker could not decide; the candidate is flagged, not
		// confirmed and not downgraded.
		d.Verdict = VerdictFlagReview
		d.LowAgreement = true
		d.Status = VerdictApplied
		return d
	}

	d.Verdict = w.Verdict
	d.Status = VerdictApplied
	if w.Verdict == VerdictDowngrade {
		d.Proposed = float64(*w.Proposed)
		d.HasProposed = true
		d.ConfidenceAfter = d.Proposed
	}
	return d
}

// unparseableDecision is the machine's own conservatism: a response that
// yielded no usable verdict flags the candidate for human review — agreement
// 0, nothing applied, never a silent pass. Outside knowledge may deepen
// suspicion, never confirmation, and that includes the suspicion that a
// broken response would otherwise have hidden.
func unparseableDecision(current float64, detail string) verdictDecision {
	return verdictDecision{
		Verdict:          VerdictFlagReview,
		Status:           VerdictUnparseable,
		ConfidenceBefore: current,
		ConfidenceAfter:  current,
		Rationale: fmt.Sprintf(
			"The validator's response yielded no usable verdict (%s); flagged for human review rather than passed unexamined.",
			detail),
	}
}

/* ---------- evidence assembly ---------- */

// candidateEvidence is everything the validator is shown for one candidate:
// the candidate itself, its source's header, the context window around its
// span with the span marked, the campaign entities it references, and — for
// discoveries — the statement of the fact the discovery claims was learned.
type candidateEvidence struct {
	candidate     Candidate
	sourceKind    string
	sourceAuthor  string
	sourceTitle   string
	windowStart   int64
	windowEnd     int64
	windowText    string
	entities      []promptEntity
	factStatement string
}

// buildWindow cuts the evidence window around a span: `window` bytes of
// context on each side, opened at a line break when one is nearby so the
// validator reads whole lines, never a half-sentence. The span itself is
// always fully inside the window by construction.
func buildWindow(content string, spanStart, spanEnd, window int64) (start, end int64) {
	n := int64(len(content))
	if spanStart < 0 {
		spanStart = 0
	}
	if spanEnd > n {
		spanEnd = n
	}
	if spanStart >= n || spanEnd <= spanStart {
		return spanStart, spanEnd
	}
	start = spanStart - window
	if start < 0 {
		start = 0
	} else if i := strings.IndexAny(content[start:spanStart], "\n"); i >= 0 {
		start += int64(i) + 1 // first line break after the cut: open on a fresh line
	}
	for start > 0 && !utf8.RuneStart(content[start]) {
		start--
	}
	end = spanEnd + window
	if end >= n {
		end = n
	} else if cut := lastBreak(content, spanEnd, end); cut > spanEnd {
		end = cut
	} else {
		for end > spanEnd && !utf8.RuneStart(content[end]) {
			end--
		}
	}
	return start, end
}

// markWindow renders the context window with the quoted span wrapped in ⟦ ⟧,
// so the validator can see at a glance exactly which words are load-bearing.
func markWindow(content string, windowStart, spanStart, spanEnd, windowEnd int64) string {
	var b strings.Builder
	b.WriteString(content[windowStart:spanStart])
	b.WriteString("⟦")
	b.WriteString(content[spanStart:spanEnd])
	b.WriteString("⟧")
	b.WriteString(content[spanEnd:windowEnd])
	return b.String()
}

// payloadRefs lists the campaign entity references in a candidate's payload,
// per kind. Local ids from the extraction payload resolve nowhere in the
// campaign and are skipped — an entity candidate carries its own name and
// summary, and a discovery's fact ref is handled by factStatement instead.
func payloadRefs(kind string, payload []byte) []string {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return nil
	}
	var refs []string
	seen := map[string]bool{}
	add := func(ref any) {
		s, _ := ref.(string)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		refs = append(refs, s)
	}
	switch kind {
	case KindFact:
		add(m["subject"])
		add(m["object_entity"])
	case KindEvent:
		add(m["location"])
		if participants, ok := m["participants"].([]any); ok {
			for _, p := range participants {
				if pm, ok := p.(map[string]any); ok {
					add(pm["entity"])
				}
			}
		}
	case KindRelationship:
		add(m["from_entity"])
		add(m["to_entity"])
	case KindDiscovery:
		if by, _ := m["discovered_by"].(string); by != "" && by != "party" {
			add(by)
		}
	}
	return refs
}

// loadEvidence assembles one candidate's evidence: source header + window +
// referenced entities (resolved through the campaign's entity list, aliases
// included) and, for discoveries, the sibling fact candidate's statement —
// the validator cannot judge "the party learned X" without X.
func (s *Store) loadEvidence(ctx context.Context, tctx *campaignContext, sourceCache map[string]unit, factCache map[string]map[string]string, cand Candidate) (*candidateEvidence, error) {
	src, ok := sourceCache[cand.SourceID]
	if !ok {
		var err error
		src, err = s.loadSource(ctx, cand.SourceID)
		if err != nil {
			return nil, err
		}
		sourceCache[cand.SourceID] = src
	}
	ev := &candidateEvidence{
		candidate:    cand,
		sourceKind:   src.Kind,
		sourceAuthor: src.Author,
		sourceTitle:  src.Title,
	}
	wStart, wEnd := buildWindow(src.Content, cand.SpanStart, cand.SpanEnd, ValidateContextWindow)
	ev.windowStart, ev.windowEnd = wStart, wEnd
	ev.windowText = markWindow(src.Content, wStart, cand.SpanStart, cand.SpanEnd, wEnd)

	for _, ref := range payloadRefs(cand.Kind, cand.Payload) {
		if e, ok := tctx.byID(ref); ok {
			ev.entities = append(ev.entities, e)
		}
	}
	if cand.Kind == KindDiscovery {
		var fact string
		if m, ok := factCache[cand.RunID]; ok {
			fact = m[discoveryFactRef(cand.Payload)]
		} else {
			statements, err := s.factStatements(ctx, cand.RunID)
			if err != nil {
				return nil, err
			}
			factCache[cand.RunID] = statements
			fact = statements[discoveryFactRef(cand.Payload)]
		}
		ev.factStatement = fact
	}
	return ev, nil
}

// discoveryFactRef pulls a discovery payload's fact local_id.
func discoveryFactRef(payload []byte) string {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	s, _ := m["fact"].(string)
	return s
}

// loadSource fetches one session source for evidence assembly.
func (s *Store) loadSource(ctx context.Context, id string) (unit, error) {
	var u unit
	err := s.db.QueryRowContext(ctx,
		`SELECT id, session_id, kind, author, title, content, checksum FROM session_sources WHERE id = ?`, id).
		Scan(&u.SourceID, &u.SessionID, &u.Kind, &u.Author, &u.Title, &u.Content, &u.Checksum)
	if err == sql.ErrNoRows {
		return u, fmt.Errorf("%w: source %s", ErrNotFound, id)
	}
	return u, err
}

// factStatements maps local_id -> statement for one extraction run's fact
// candidates, so a discovery can be shown the fact it claims was learned.
func (s *Store) factStatements(ctx context.Context, runID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT payload FROM canon_candidates WHERE run_id = ? AND kind = ?`, runID, KindFact)
	if err != nil {
		return nil, fmt.Errorf("load fact candidates: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var m map[string]any
		if json.Unmarshal([]byte(payload), &m) != nil {
			continue
		}
		id, _ := m["local_id"].(string)
		statement, _ := m["statement"].(string)
		if id != "" {
			out[id] = statement
		}
	}
	return out, rows.Err()
}

// byID resolves a campaign entity (with aliases) from the run's loaded list.
func (c *campaignContext) byID(id string) (promptEntity, bool) {
	e, ok := c.entityByID[id]
	return e, ok
}

/* ---------- the validation run ---------- */

// ValidateInput selects what one validation run covers. Zero optional values
// mean the campaign's every staged candidate, bounded by the batch size.
type ValidateInput struct {
	CampaignID string
	// SessionID narrows the run to one session's candidates.
	SessionID string
	// CandidateIDs selects candidates explicitly (their campaign must match).
	CandidateIDs []string
	// Limit caps the candidates validated this run; the remainder is
	// deferred and a later run picks them up. Zero uses the batch size.
	Limit int
}

// Validate runs the adversarial pass over the selected staged candidates.
//
// Per-item semantics mirror extraction's per-unit semantics: each candidate
// commits in its own transaction — verdict row plus (when a downgrade lands)
// the guarded confidence update together — so an interrupted run resumes
// exactly where it stopped, and a candidate already verdicted under this
// prompt version and content checksum is skipped without a model call. The
// guards stop the run between items: the USD budget (when prices are
// configured) and the batch limit; the remainder stays deferred.
//
// RunStats mapping for validation runs: units are candidates; "staged" counts
// applied verdicts by outcome (agree / downgrade / flag_review); "dropped"
// counts rejections, unparseable responses and low-agreement coercions by
// reason; chunks is extraction-only and stays zero.
func (s *Store) Validate(ctx context.Context, in ValidateInput) (*Run, error) {
	if s.validator == nil {
		return nil, errOffline
	}
	if strings.TrimSpace(in.CampaignID) == "" {
		return nil, fmt.Errorf("%w: campaign id is required", ErrInvalid)
	}
	camp, err := s.loadCampaign(ctx, in.CampaignID)
	if err != nil {
		return nil, err
	}
	tctx, err := s.loadTaskContext(ctx, in.CampaignID)
	if err != nil {
		return nil, err
	}
	cands, err := s.loadValidationItems(ctx, in)
	if err != nil {
		return nil, err
	}

	stats := newRunStats()
	run := &Run{
		ID: uuid.NewString(), CampaignID: in.CampaignID, SessionID: in.SessionID,
		Kind: RunValidate, PromptVersion: VALIDATE_PROMPT_VERSION, Model: s.validator.ModelName(),
		Status: RunRunning, Stats: stats, CreatedAt: s.now(), UpdatedAt: s.now(),
	}
	if err := s.insertRun(ctx, run); err != nil {
		return nil, err
	}

	limit := in.Limit
	if limit <= 0 {
		limit = s.cfg.BatchSize
	}
	budgetKnown := s.cfg.BudgetUSD > 0 && s.cfg.PriceInMTok > 0 && s.cfg.PriceOutMTok > 0
	sourceCache := map[string]unit{}
	factCache := map[string]map[string]string{}
	var lastCall time.Time
	var fatal error
	stopReason := ""
	remaining := limit

	for _, cand := range cands {
		// The verdict ledger key: candidate + prompt version + content
		// checksum. Same key means already verdicted and skipped; a changed
		// checksum (the candidate's content changed) re-validates.
		done, err := s.verdictDone(ctx, cand.ID, VALIDATE_PROMPT_VERSION, cand.Checksum)
		if err != nil {
			fatal = err
			break
		}
		if done {
			stats.UnitsSkipped++
			continue
		}
		// The limit bounds model work: a ledger skip is not work and does
		// not consume the batch.
		if remaining <= 0 {
			stopReason = StopUnits
			break
		}
		if budgetKnown && stats.CostUSD >= s.cfg.BudgetUSD {
			stopReason = StopBudget
			break
		}
		stats.UnitsTotal++

		ev, err := s.loadEvidence(ctx, tctx, sourceCache, factCache, cand)
		if err != nil {
			fatal = fmt.Errorf("evidence for candidate %s: %w", cand.ID, err)
			break
		}
		if err := s.waitInterval(ctx, &lastCall); err != nil {
			fatal = err
			break
		}
		compl, err := s.validator.Complete(ctx, validateSystemPrompt(), validateUserPrompt(s.cfg.AgreementThreshold, camp, ev))
		if err != nil {
			fatal = fmt.Errorf("model call failed on candidate %s: %w", cand.ID, err)
			break
		}
		lastCall = s.now()
		stats.Requests++
		stats.InputTokens += compl.InputTokens
		stats.OutputTokens += compl.OutputTokens
		stats.CostUSD += s.cfg.costUSD(compl.InputTokens, compl.OutputTokens)

		wire, problems := parseVerdictResponse(compl.Text)
		decision := applyVerdict(cand.Confidence, wire, s.cfg.AgreementThreshold)
		if len(problems) > 0 {
			stats.ParseProblems += len(problems)
			decision = unparseableDecision(cand.Confidence, strings.Join(problems, "; "))
		}
		switch decision.Status {
		case VerdictApplied:
			stats.Staged[decision.Verdict]++
			if decision.LowAgreement {
				stats.Dropped["low_agreement"]++
			}
		case VerdictRejected:
			stats.Dropped[decision.RejectionReason]++
		case VerdictUnparseable:
			stats.Dropped["unparseable_response"]++
		}

		rec := verdictRecord{
			ID: uuid.NewString(), RunID: run.ID, CampaignID: in.CampaignID,
			CandidateID: cand.ID, PromptVersion: VALIDATE_PROMPT_VERSION,
			InputChecksum: cand.Checksum, Decision: decision,
			Model: s.validator.ModelName(), InputTokens: compl.InputTokens,
			OutputTokens: compl.OutputTokens, Raw: compl.Text, CreatedAt: s.now(),
		}
		// The item's own transaction: verdict row and confidence update
		// commit together or not at all. A failed model call left no
		// verdict row, so a re-run retries the candidate.
		if err := s.commitVerdict(ctx, run.ID, rec, stats); err != nil {
			fatal = err
			break
		}
		stats.UnitsDone++
		remaining--
	}

	if fatal != nil {
		run.Status = RunFailed
		run.StopReason = StopError
		run.Error = fatal.Error()
	} else if stopReason != "" {
		run.Status = RunStopped
		run.StopReason = stopReason
	} else {
		run.Status = RunCompleted
	}
	run.UpdatedAt = s.now()
	if err := s.finishRun(ctx, run); err != nil {
		if fatal == nil {
			return run, err
		}
	}
	return run, fatal
}

// verdictRecord is the insert shape for one verdict row.
type verdictRecord struct {
	ID            string
	RunID         string
	CampaignID    string
	CandidateID   string
	PromptVersion string
	InputChecksum string
	Decision      verdictDecision
	Model         string
	InputTokens   int
	OutputTokens  int
	Raw           string
	CreatedAt     time.Time
}

// verdictDone reports whether a candidate already carries a verdict under
// this prompt version and content checksum. The UNIQUE constraint on the
// verdict ledger backs the answer: at most one row per key can exist.
func (s *Store) verdictDone(ctx context.Context, candidateID, promptVersion, inputChecksum string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM canon_verdicts
		 WHERE candidate_id = ? AND prompt_version = ? AND input_checksum = ?`,
		candidateID, promptVersion, inputChecksum).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check verdict ledger: %w", err)
	}
	return true, nil
}

// commitVerdict writes one verdict in a single transaction and refreshes the
// run's stats row in the same commit. A downgrade lands through a guarded
// UPDATE — `confidence > proposed` — so the write can only ever lower the
// value; if another pass already lowered it further, the verdict row records
// the actual resulting confidence instead of the proposal.
func (s *Store) commitVerdict(ctx context.Context, runID string, rec verdictRecord, stats *RunStats) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("verdict tx: %w", err)
	}
	defer tx.Rollback()

	if rec.Decision.Status == VerdictApplied && rec.Decision.Verdict == VerdictDowngrade {
		res, err := tx.ExecContext(ctx,
			`UPDATE canon_candidates SET confidence = ? WHERE id = ? AND confidence > ?`,
			rec.Decision.ConfidenceAfter, rec.CandidateID, rec.Decision.ConfidenceAfter)
		if err != nil {
			return fmt.Errorf("apply downgrade: %w", err)
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			if err := tx.QueryRowContext(ctx,
				`SELECT confidence FROM canon_candidates WHERE id = ?`, rec.CandidateID).
				Scan(&rec.Decision.ConfidenceAfter); err != nil {
				return fmt.Errorf("read back confidence: %w", err)
			}
		}
	}

	var proposed any
	if rec.Decision.HasProposed {
		proposed = rec.Decision.Proposed
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO canon_verdicts (id, run_id, campaign_id, candidate_id, prompt_version, input_checksum,
			verdict, status, agreement, rationale, proposed_confidence, confidence_before, confidence_after,
			rejection_reason, model, input_tokens, output_tokens, raw, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.RunID, rec.CampaignID, rec.CandidateID, rec.PromptVersion, rec.InputChecksum,
		rec.Decision.Verdict, rec.Decision.Status, rec.Decision.Agreement, rec.Decision.Rationale,
		proposed, rec.Decision.ConfidenceBefore, rec.Decision.ConfidenceAfter,
		rec.Decision.RejectionReason, rec.Model, rec.InputTokens, rec.OutputTokens, rec.Raw,
		rec.CreatedAt.UnixMilli()); err != nil {
		return fmt.Errorf("insert verdict: %w", err)
	}

	statsJSON, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("encode stats: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE canon_runs SET stats = ?, updated_at = ? WHERE id = ?`,
		string(statsJSON), s.now().UnixMilli(), runID); err != nil {
		return fmt.Errorf("refresh run stats: %w", err)
	}
	return tx.Commit()
}

// loadValidationItems gathers the run's work items in staging order.
func (s *Store) loadValidationItems(ctx context.Context, in ValidateInput) ([]Candidate, error) {
	q := `SELECT ` + candidateCols + ` FROM canon_candidates WHERE campaign_id = ?`
	args := []any{in.CampaignID}
	if in.SessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, in.SessionID)
	}
	if len(in.CandidateIDs) > 0 {
		q += ` AND id IN (` + placeholders(len(in.CandidateIDs)) + `)`
		for _, id := range in.CandidateIDs {
			args = append(args, id)
		}
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("load candidates: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

/* ---------- reads ---------- */

// Verdict is one stored verdict: the effective outcome, the machine's status
// for it, the before/after confidence, and the raw validator response with
// its model and tokens. Queue material for the review screen (MAD-310).
type Verdict struct {
	ID                 string
	RunID              string
	CampaignID         string
	CandidateID        string
	PromptVersion      string
	InputChecksum      string
	Verdict            string
	Status             string
	Agreement          float64
	Rationale          string
	ProposedConfidence float64
	HasProposed        bool
	ConfidenceBefore   float64
	ConfidenceAfter    float64
	RejectionReason    string
	Model              string
	InputTokens        int
	OutputTokens       int
	Raw                string
	CreatedAt          time.Time
}

// VerdictFilter narrows Verdicts. Zero values mean "no restriction".
type VerdictFilter struct {
	RunID       string
	CandidateID string
	Status      string
}

// Verdicts returns a campaign's verdicts, oldest first.
func (s *Store) Verdicts(ctx context.Context, campaignID string, filter VerdictFilter) ([]Verdict, error) {
	q := `SELECT id, run_id, campaign_id, candidate_id, prompt_version, input_checksum,
			verdict, status, agreement, rationale, proposed_confidence, confidence_before, confidence_after,
			rejection_reason, model, input_tokens, output_tokens, raw, created_at
		FROM canon_verdicts WHERE campaign_id = ?`
	args := []any{campaignID}
	if filter.RunID != "" {
		q += ` AND run_id = ?`
		args = append(args, filter.RunID)
	}
	if filter.CandidateID != "" {
		q += ` AND candidate_id = ?`
		args = append(args, filter.CandidateID)
	}
	if filter.Status != "" {
		q += ` AND status = ?`
		args = append(args, filter.Status)
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
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
	return out, rows.Err()
}

/* ---------- the validator prompts ---------- */

// validateSystemPrompt is the validator's standing instruction: an explicitly
// skeptical stance over an evidence boundary that excludes everything except
// the candidate's own citation. The prime directive is Arda's, nearly
// verbatim.
func validateSystemPrompt() string {
	return `You are the adversarial reviewer for a D&D campaign canon engine. A separate extraction pass has proposed a candidate canon item, citing a verbatim span of a session source. Your job is to try to BREAK it. Assume extraction over-reads; your value is in what you catch, not in what you let through.

PRIME DIRECTIVE — NO RESCUE: Do NOT use your knowledge of tabletop tropes, or the rest of this campaign, to rescue a candidate the transcript does not support. Outside knowledge may deepen suspicion, never confirmation.

EVIDENCE BOUNDARY (hard): judge the candidate ONLY against the quoted span, its surrounding context window, and the referenced campaign entities (which resolve who the span's words refer to — nothing more). You were NOT given the rest of the campaign. Your own D&D knowledge is not evidence. When the span and your knowledge disagree, the span wins or the candidate is flagged.

MONOTONICITY (hard): a machine pass may only downgrade or flag — never upgrade, never delete. "agree" confirms the candidate as it stands. "downgrade" must propose a confidence STRICTLY LOWER than the candidate's current one. "flag_review" hands the decision to a human. A downgrade that does not strictly lower is discarded and logged; there is no verdict that raises confidence.

DISCOVERIES are the highest-risk kind. "The party learned X" is easy to over-read from a transcript in which the DM said X out loud but no character was present to hear it. For any discovery, ask: which character, in the fiction, perceived this, and where in the span do they perceive it? If the span shows the information being spoken with no character present to perceive it, that is a flag or a downgrade, never an agree.

AGREEMENT (0-1) is how well the evidence supports the candidate: 1.0 stated outright, lower for inferred, and low when the span supports only part of the claim or none of it.

RATIONALE: one or two sentences quoting what the span does and does not show.

OUTPUT FORMAT:
Return ONE JSON object and nothing else — no prose, no markdown fences. Shape:
{"verdict": "agree|downgrade|flag_review", "agreement": 0.0, "rationale": "...", "proposed_confidence": null}
proposed_confidence carries a number ONLY for a downgrade, strictly below the candidate's stated confidence; null otherwise. The task message states the agreement threshold: at or above it "agree" may stand; below it the honest verdict is flag_review — the checker could not decide.`
}

// validateUserPrompt renders one candidate's whole evidence for the task
// message: the campaign header, the source header, the candidate with its
// payload and current confidence, the context window with the span marked,
// the referenced entities, and — for discoveries — the fact it claims was
// learned plus the perception question.
func validateUserPrompt(threshold float64, camp campaignHeader, ev *candidateEvidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TASK: Adversarially re-check ONE proposed canon candidate against its own evidence. Agreement threshold: %.2f.\n\n", threshold)

	fmt.Fprintf(&b, "Campaign: %s (in-world day %d)\n", camp.Name, camp.Clock)
	fmt.Fprintf(&b, "Source: %s", ev.sourceKind)
	if ev.sourceAuthor != "" {
		fmt.Fprintf(&b, " by %s", ev.sourceAuthor)
	}
	if ev.sourceTitle != "" {
		fmt.Fprintf(&b, " — %s", ev.sourceTitle)
	}
	b.WriteString("\n\n")

	cand := ev.candidate
	fmt.Fprintf(&b, "CANDIDATE (kind: %s, extractor confidence %.2f):\n", cand.Kind, cand.Confidence)
	b.WriteString(renderPayload(cand.Kind, cand.Payload))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "QUOTED SPAN (source bytes %d..%d):\n%s\n\n", cand.SpanStart, cand.SpanEnd, cand.Quote)
	fmt.Fprintf(&b, "CONTEXT WINDOW (source bytes %d..%d, the quoted span marked ⟦thus⟧ — judge only from this text):\n%s\n\n",
		ev.windowStart, ev.windowEnd, ev.windowText)

	if len(ev.entities) > 0 {
		b.WriteString("REFERENCED CAMPAIGN ENTITIES (identity only — for resolving who the span's words refer to):\n")
		for _, e := range ev.entities {
			line := fmt.Sprintf("- %s (%s, %s", e.ID, e.Kind, e.Name)
			if len(e.Aliases) > 0 {
				line += "; aka " + strings.Join(e.Aliases, ", ")
			}
			line += ")\n"
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	if cand.Kind == KindDiscovery {
		if ev.factStatement != "" {
			fmt.Fprintf(&b, "THE FACT THIS DISCOVERY CLAIMS WAS LEARNED: %s\n", ev.factStatement)
		}
		b.WriteString(`DISCOVERY CHECK — the highest-risk candidate kind: which character, in the fiction, perceived this, and where in the span do they perceive it?
`)
		b.WriteString("\n")
	}

	b.WriteString("Return only the JSON verdict object.")
	return b.String()
}

// renderPayload renders one candidate's payload human-readably for the
// prompt, per kind — the statement or summary first, then the references.
func renderPayload(kind string, payload []byte) string {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return string(payload)
	}
	str := func(k string) string {
		s, _ := m[k].(string)
		return strings.TrimSpace(s)
	}
	var b strings.Builder
	switch kind {
	case KindEntity:
		fmt.Fprintf(&b, "NEW ENTITY: %s (%s)\n", str("name"), str("kind"))
		if s := str("summary"); s != "" {
			fmt.Fprintf(&b, "SUMMARY: %s\n", s)
		}
	case KindFact:
		fmt.Fprintf(&b, "STATEMENT: %s\n", str("statement"))
		fmt.Fprintf(&b, "TRIPLE: %s — %s — %s\n", str("subject"), str("predicate"), firstNonEmpty(str("object_entity"), str("object_literal")))
		if v := str("visibility"); v != "" {
			fmt.Fprintf(&b, "VISIBILITY: %s\n", v)
		}
	case KindEvent:
		fmt.Fprintf(&b, "EVENT: %s\n", str("summary"))
		if participants, ok := m["participants"].([]any); ok && len(participants) > 0 {
			b.WriteString("PARTICIPANTS:")
			for _, p := range participants {
				if pm, ok := p.(map[string]any); ok {
					entity, _ := pm["entity"].(string)
					role, _ := pm["role"].(string)
					fmt.Fprintf(&b, " %s (%s)", entity, role)
				}
			}
			b.WriteString("\n")
		}
		if loc := str("location"); loc != "" {
			fmt.Fprintf(&b, "LOCATION: %s\n", loc)
		}
	case KindDiscovery:
		fmt.Fprintf(&b, "DISCOVERY: %s learned fact %q\n", firstNonEmpty(str("discovered_by"), "unknown"), str("fact"))
		fmt.Fprintf(&b, "STANCE: %s\n", str("stance"))
		if method := str("method"); method != "" {
			fmt.Fprintf(&b, "METHOD: %s\n", method)
		}
	case KindRelationship:
		fmt.Fprintf(&b, "RELATIONSHIP: %s — %s — %s\n", str("from_entity"), str("rel_type"), str("to_entity"))
	default:
		return string(payload)
	}
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
