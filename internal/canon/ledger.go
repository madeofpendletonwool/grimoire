package canon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

/* ---------- run records ---------- */

// Run is one canon-engine run — extraction or validation — with its full
// stats payload. The stats are the run's accountability: every unit, chunk,
// request, token, staged candidate and drop reason is counted here, exactly
// as Arda's pipeline_runs stats do. For validation runs the fields map over
// (units are candidates; "staged" counts applied verdicts by outcome;
// "dropped" counts rejections, unparseable responses and low-agreement
// coercions by reason); see adversarial.go.
type Run struct {
	ID            string
	CampaignID    string
	SessionID     string
	Kind          string
	PromptVersion string
	Model         string
	Status        string
	StopReason    string
	Stats         *RunStats
	Error         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RunStats is the JSON stats blob carried on a run. Maps marshal with sorted
// keys, so the same counts render identically.
type RunStats struct {
	UnitsTotal    int            `json:"units_total"`
	UnitsDone     int            `json:"units_done"`
	UnitsSkipped  int            `json:"units_skipped"`
	Chunks        int            `json:"chunks"`
	Requests      int            `json:"requests"`
	InputTokens   int            `json:"input_tokens"`
	OutputTokens  int            `json:"output_tokens"`
	CostUSD       float64        `json:"cost_usd"`
	Staged        map[string]int `json:"staged"`
	Dropped       map[string]int `json:"dropped"`
	ParseProblems int            `json:"parse_problems"`
}

func newRunStats() *RunStats {
	return &RunStats{Staged: map[string]int{}, Dropped: map[string]int{}}
}

// StagedTotal sums the per-kind staged counts.
func (rs *RunStats) StagedTotal() int {
	total := 0
	for _, n := range rs.Staged {
		total += n
	}
	return total
}

/* ---------- staged candidates, drops, model outputs ---------- */

// Candidate is one staged candidate as stored: the validated wire payload
// (shape varies by kind), the span rule's full quadruple, and the checksum
// the adversarial pass keys verdicts against. Candidates are queue material:
// nothing reads them into the campaign graph until a human accepts them.
type Candidate struct {
	ID         string
	RunID      string
	CampaignID string
	SessionID  string
	SourceID   string
	ChunkIndex int
	Kind       string
	Payload    []byte
	Confidence float64
	SpanStart  int64
	SpanEnd    int64
	Quote      string
	Checksum   string
	CreatedAt  time.Time
}

// DropRow is one stored drop: what was rejected, where, and why.
type DropRow struct {
	ID         string
	RunID      string
	CampaignID string
	SourceID   string
	ChunkIndex int
	Kind       string
	Ref        string
	Reason     string
	Detail     string
	CreatedAt  time.Time
}

// ModelOutput is one raw model response, stored verbatim with its token
// accounting and input reference. The provenance floor: any candidate's
// origin bottoms out in exactly what the model said.
type ModelOutput struct {
	ID            string
	RunID         string
	Stage         string
	PromptVersion string
	SourceID      string
	ChunkIndex    int
	Model         string
	InputTokens   int
	OutputTokens  int
	Raw           string
	CreatedAt     time.Time
}

// modelOutputRow is the insert shape for ModelOutput.
type modelOutputRow = ModelOutput

// LedgerEntry is one row of the per-unit extraction ledger.
type LedgerEntry struct {
	ID            string
	CampaignID    string
	SourceID      string
	SessionID     string
	RunID         string
	PromptVersion string
	InputChecksum string
	Status        string
	Chunks        int
	Staged        int
	Dropped       int
	Error         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

/* ---------- writes ---------- */

func (s *Store) insertRun(ctx context.Context, run *Run) error {
	stats, err := json.Marshal(run.Stats)
	if err != nil {
		return fmt.Errorf("encode stats: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO canon_runs (id, campaign_id, session_id, kind, prompt_version, model, status, stop_reason, stats, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.CampaignID, nullString(run.SessionID), run.Kind, run.PromptVersion,
		run.Model, run.Status, run.StopReason, string(stats), run.Error,
		run.CreatedAt.UnixMilli(), run.UpdatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

// finishRun writes the terminal status, stop reason and final stats.
func (s *Store) finishRun(ctx context.Context, run *Run) error {
	stats, err := json.Marshal(run.Stats)
	if err != nil {
		return fmt.Errorf("encode stats: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE canon_runs SET status = ?, stop_reason = ?, stats = ?, error = ?, updated_at = ?
		 WHERE id = ?`,
		run.Status, run.StopReason, string(stats), run.Error, run.UpdatedAt.UnixMilli(), run.ID)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

// ledgerDone reports whether a source is already extracted under this prompt
// version and input checksum. The UNIQUE constraint on the ledger backs the
// answer: at most one row per key can exist.
func (s *Store) ledgerDone(ctx context.Context, sourceID, promptVersion, inputChecksum string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM canon_extract_ledger
		 WHERE source_id = ? AND prompt_version = ? AND input_checksum = ? AND status = ?`,
		sourceID, promptVersion, inputChecksum, LedgerDone).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check ledger: %w", err)
	}
	return true, nil
}

// commitUnit writes one unit's whole product in a single transaction —
// ledger row, model outputs, candidates, drops — and refreshes the run's
// stats row in the same commit, so an interrupted run never shows stats it
// did not earn. This is the transaction that makes extraction resumable.
func (s *Store) commitUnit(ctx context.Context, runID, campaignID string, u unit, inputCheck string, chunkCount int, staged []Staged, drops []Drop, outputs []modelOutputRow, stats *RunStats) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("unit tx: %w", err)
	}
	defer tx.Rollback()
	now := s.now().UnixMilli()

	for _, o := range outputs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_outputs (id, run_id, stage, prompt_version, source_id, chunk_index, model, input_tokens, output_tokens, raw, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			o.ID, o.RunID, o.Stage, o.PromptVersion, o.SourceID, o.ChunkIndex, o.Model,
			o.InputTokens, o.OutputTokens, o.Raw, now); err != nil {
			return fmt.Errorf("insert model output: %w", err)
		}
	}
	for _, c := range staged {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO canon_candidates (id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), runID, campaignID, u.SessionID, u.SourceID, c.ChunkIndex, c.Kind,
			string(c.Payload), c.Confidence, c.SpanStart, c.SpanEnd, c.Quote, c.Checksum, now); err != nil {
			return fmt.Errorf("insert candidate: %w", err)
		}
	}
	for _, d := range drops {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO canon_drops (id, run_id, campaign_id, source_id, chunk_index, kind, ref, reason, detail, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), runID, campaignID, u.SourceID, d.ChunkIndex, d.Kind, d.Ref, d.Reason, d.Detail, now); err != nil {
			return fmt.Errorf("insert drop: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO canon_extract_ledger (id, campaign_id, source_id, session_id, run_id, prompt_version, input_checksum, status, chunks, staged, dropped, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		uuid.NewString(), campaignID, u.SourceID, u.SessionID, runID, PROMPT_VERSION, inputCheck,
		LedgerDone, chunkCount, len(staged), len(drops), now, now); err != nil {
		return fmt.Errorf("insert ledger row: %w", err)
	}
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("encode stats: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE canon_runs SET stats = ?, updated_at = ? WHERE id = ?`, string(statsJSON), now, runID); err != nil {
		return fmt.Errorf("refresh run stats: %w", err)
	}
	return tx.Commit()
}

/* ---------- reads ---------- */

// GetRun returns one run of a campaign.
func (s *Store) GetRun(ctx context.Context, campaignID, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runCols+` FROM canon_runs WHERE id = ? AND campaign_id = ?`, id, campaignID)
	r, err := scanRun(row)
	if err == ErrNotFound {
		return nil, fmt.Errorf("%w: run %s", ErrNotFound, id)
	}
	return r, err
}

const runCols = `id, campaign_id, COALESCE(session_id, ''), kind, prompt_version, model, status, stop_reason, stats, error, created_at, updated_at`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var (
		r            Run
		statsJSON    string
		createdMilli int64
		updatedMilli int64
	)
	if err := row.Scan(&r.ID, &r.CampaignID, &r.SessionID, &r.Kind, &r.PromptVersion,
		&r.Model, &r.Status, &r.StopReason, &statsJSON, &r.Error, &createdMilli, &updatedMilli); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound // callers add the id; Scan wrote none of it
		}
		return nil, err
	}
	r.Stats = newRunStats()
	_ = json.Unmarshal([]byte(statsJSON), r.Stats)
	r.CreatedAt = time.UnixMilli(createdMilli).UTC()
	r.UpdatedAt = time.UnixMilli(updatedMilli).UTC()
	return &r, nil
}

// ListRuns returns a campaign's runs, newest first.
func (s *Store) ListRuns(ctx context.Context, campaignID string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+` FROM canon_runs WHERE campaign_id = ? ORDER BY created_at DESC, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// CandidateFilter narrows ListCandidates. Zero values mean "no restriction".
type CandidateFilter struct {
	Kind     string
	SourceID string
	RunID    string
}

// candidateCols and scanCandidate are the one scan path for canon_candidates
// rows, shared by the queue read and the validation pass's item loader.
const candidateCols = `id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at`

func scanCandidate(row interface{ Scan(...any) error }) (Candidate, error) {
	var (
		c            Candidate
		createdMilli int64
	)
	if err := row.Scan(&c.ID, &c.RunID, &c.CampaignID, &c.SessionID, &c.SourceID, &c.ChunkIndex,
		&c.Kind, &c.Payload, &c.Confidence, &c.SpanStart, &c.SpanEnd, &c.Quote, &c.Checksum, &createdMilli); err != nil {
		return c, err
	}
	c.CreatedAt = time.UnixMilli(createdMilli).UTC()
	return c, nil
}

// ListCandidates returns staged candidates for a campaign, oldest first.
// Queue material for the review screen (MAD-310); no graph read path ever
// touches this table.
func (s *Store) ListCandidates(ctx context.Context, campaignID string, filter CandidateFilter) ([]Candidate, error) {
	q := `SELECT ` + candidateCols + ` FROM canon_candidates WHERE campaign_id = ?`
	args := []any{campaignID}
	if filter.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, filter.Kind)
	}
	if filter.SourceID != "" {
		q += ` AND source_id = ?`
		args = append(args, filter.SourceID)
	}
	if filter.RunID != "" {
		q += ` AND run_id = ?`
		args = append(args, filter.RunID)
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
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

// ListDrops returns a run's drop log, oldest first — the "why was this not
// extracted" surface.
func (s *Store) ListDrops(ctx context.Context, campaignID, runID string) ([]DropRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, campaign_id, source_id, chunk_index, kind, ref, reason, detail, created_at
		  FROM canon_drops WHERE campaign_id = ? AND run_id = ? ORDER BY created_at, id`,
		campaignID, runID)
	if err != nil {
		return nil, fmt.Errorf("list drops: %w", err)
	}
	defer rows.Close()
	var out []DropRow
	for rows.Next() {
		var d DropRow
		var createdMilli int64
		if err := rows.Scan(&d.ID, &d.RunID, &d.CampaignID, &d.SourceID, &d.ChunkIndex, &d.Kind,
			&d.Ref, &d.Reason, &d.Detail, &createdMilli); err != nil {
			return nil, err
		}
		d.CreatedAt = time.UnixMilli(createdMilli).UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

// LedgerForSource returns a source's ledger history, oldest first: every
// prompt version and input checksum it has been extracted under.
func (s *Store) LedgerForSource(ctx context.Context, campaignID, sourceID string) ([]LedgerEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, campaign_id, source_id, session_id, run_id, prompt_version, input_checksum, status, chunks, staged, dropped, error, created_at, updated_at
		  FROM canon_extract_ledger WHERE campaign_id = ? AND source_id = ? ORDER BY created_at, id`,
		campaignID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list ledger: %w", err)
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		var createdMilli, updatedMilli int64
		if err := rows.Scan(&e.ID, &e.CampaignID, &e.SourceID, &e.SessionID, &e.RunID, &e.PromptVersion,
			&e.InputChecksum, &e.Status, &e.Chunks, &e.Staged, &e.Dropped, &e.Error, &createdMilli, &updatedMilli); err != nil {
			return nil, err
		}
		e.CreatedAt = time.UnixMilli(createdMilli).UTC()
		e.UpdatedAt = time.UnixMilli(updatedMilli).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// ModelOutputs returns the raw responses a run recorded, in chunk order —
// the verbatim floor under every candidate and drop.
func (s *Store) ModelOutputs(ctx context.Context, campaignID, runID string) ([]ModelOutput, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id, o.run_id, o.stage, o.prompt_version, o.source_id, o.chunk_index, o.model, o.input_tokens, o.output_tokens, o.raw, o.created_at
		  FROM model_outputs o JOIN canon_runs r ON r.id = o.run_id
		 WHERE r.campaign_id = ? AND o.run_id = ? ORDER BY o.source_id, o.chunk_index`,
		campaignID, runID)
	if err != nil {
		return nil, fmt.Errorf("list model outputs: %w", err)
	}
	defer rows.Close()
	var out []ModelOutput
	for rows.Next() {
		var o ModelOutput
		var createdMilli int64
		if err := rows.Scan(&o.ID, &o.RunID, &o.Stage, &o.PromptVersion, &o.SourceID, &o.ChunkIndex,
			&o.Model, &o.InputTokens, &o.OutputTokens, &o.Raw, &createdMilli); err != nil {
			return nil, err
		}
		o.CreatedAt = time.UnixMilli(createdMilli).UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}
