package effects

// The duration engine's rows and orchestration (MAD-421). The pure
// grammar and both clocks are effects.go; this file is everything that
// touches a database:
//
//   - Apply writes one ongoing effect: the concentration rule enforced
//     first (a source concentrating on anything new ends their previous
//     link), the same effect re-applied supersedes its row (conditions do
//     not stack; the latest application wins, the old row kept as ended
//     history), and the new row anchors itself to the campaign clock.
//   - List derives every row's state at read time — the campaign clock
//     and the applied rests are the truth, remaining_seconds is the state
//     as of the row's anchor day, never a live total. The same
//     derivation-not-storage rule the ledger folds balances under.
//   - AdvanceRounds is the combat clock's persisting half: turn ticks
//     wear the stored seconds down and expire what reaches zero. Stage 5
//     wires the round counter to it.
//   - Conditions ground in the indexed SRD: applying or listing a
//     condition surfaces the real rules text, resolved through the index
//     at read time, never a paraphrase stored away from its source.
//
// Permission rules live in the server file; this store trusts its
// caller's scope decision and validates shape only.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/data"
	"github.com/madeofpendletonwool/grimoire/internal/index"
)

// dbRunner is what a query runs over: the pool or a transaction.
type dbRunner interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Store runs the effect engine over the shared database handle.
type Store struct {
	db        *sql.DB
	campaigns *campaign.Store
	idx       *index.Store // optional: SRD grounding for conditions
	now       func() time.Time
}

// New builds an effects store on an open, migrated database handle. The
// index store carries the SRD grounding conditions surface at apply and
// read time; nil disables grounding (the effect rows work unchanged).
func New(db *sql.DB, campaigns *campaign.Store, idx *index.Store) (*Store, error) {
	if db == nil {
		return nil, errors.New("effects: nil database handle")
	}
	if campaigns == nil {
		return nil, errors.New("effects: the campaign store is required")
	}
	return &Store{db: db, campaigns: campaigns, idx: idx, now: time.Now().UTC}, nil
}

/* ---------- the rows ---------- */

// Row is one ongoing_effects row. Running carries the derived state the
// read surfaces; the store fills it by running the pure engine against
// the campaign clock and the rest ledger.
type Row struct {
	Effect
	RemainingSeconds int64
	AppliedDay       int64
	Status           string
	EndReason        string
	EndedBy          string
	Actor            string
	SessionID        string
	EventID          string
	Note             string
	AppliedAt        time.Time
	EndedAt          time.Time
	UpdatedAt        time.Time
	Running          Running `json:"-"`
}

// ApplyInput is one effect to apply, already permission-checked by the
// handler. Duration is the declared grammar; Source and Session are
// optional links.
type ApplyInput struct {
	TargetID      string
	Kind          string
	Name          string
	Ref           string
	SourceID      string
	Concentration bool
	Duration      Duration
	Note          string
	SessionID     string
	EventID       string
}

// ApplyResult is what one application changed: the new row, the
// concentration link it broke (if any), and the row it superseded (if
// any) — the story the response tells.
type ApplyResult struct {
	Applied            Row
	BrokeConcentration []Row
	Superseded         []Row
}

/* ---------- apply ---------- */

// Apply validates and writes one ongoing effect. Concentration is the
// first-class rule: when the new effect rides concentration from a named
// source, that source's previous concentration ends (end reason
// concentration_broken) — one link at a time, no orphans. Re-applying the
// same effect on the same target supersedes the standing row: the game's
// conditions do not stack, the latest application wins, and the replaced
// row stays in the history as ended.
func (s *Store) Apply(ctx context.Context, campaignID string, in ApplyInput, actor string) (*ApplyResult, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Kind == KindCondition {
		if canon := ConditionName(in.Name); canon != "" {
			in.Name = canon
		}
	}
	e := Effect{
		TargetID: strings.TrimSpace(in.TargetID), Kind: strings.TrimSpace(in.Kind),
		Name: in.Name, Ref: strings.TrimSpace(in.Ref), SourceID: strings.TrimSpace(in.SourceID),
		Concentration: in.Concentration, Duration: in.Duration,
	}
	if err := e.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	target, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, e.TargetID)
	if err != nil {
		return nil, err
	}
	if target.Status == campaign.StatusDeleted {
		return nil, fmt.Errorf("%w: %s is deleted", campaign.ErrInvalid, target.Name)
	}
	e.TargetName = target.Name
	if e.SourceID != "" {
		source, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, e.SourceID)
		if err != nil {
			return nil, err
		}
		if source.Status == campaign.StatusDeleted {
			return nil, fmt.Errorf("%w: source %s is deleted", campaign.ErrInvalid, source.Name)
		}
		e.SourceName = source.Name
	}
	if e.Concentration && e.SourceID == "" {
		return nil, fmt.Errorf("%w: a concentrating effect needs its source — who is concentrating", campaign.ErrInvalid)
	}
	sessionID, err := s.validateLinks(ctx, campaignID, in.SessionID, in.EventID)
	if err != nil {
		return nil, err
	}
	day, err := s.currentDay(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	remaining, _ := e.Duration.Seconds()
	now := s.now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("effects apply: %w", err)
	}
	defer tx.Rollback()

	out := &ApplyResult{}
	// The concentration rule: one link at a time per source.
	if e.Concentration {
		broken, err := endRowsOn(ctx, tx, campaignID, `
			SELECT `+rowCols+` FROM ongoing_effects
			 WHERE campaign_id = ? AND source_id = ? AND concentration = 1 AND status = 'active'`,
			[]any{campaignID, e.SourceID}, EndConcentrationBroken, now)
		if err != nil {
			return nil, err
		}
		out.BrokeConcentration = broken
	}
	// The same effect applied again: latest wins, history stays.
	superseded, err := endRowsOn(ctx, tx, campaignID, `
		SELECT `+rowCols+` FROM ongoing_effects
		 WHERE campaign_id = ? AND target_id = ? AND kind = ? AND lower(name) = lower(?) AND status = 'active'`,
		[]any{campaignID, e.TargetID, e.Kind, e.Name}, EndSuperseded, now)
	if err != nil {
		return nil, err
	}
	out.Superseded = superseded

	row := Row{
		Effect: e, RemainingSeconds: remaining, AppliedDay: day, Status: StatusActive,
		Actor: actor, SessionID: sessionID, EventID: strings.TrimSpace(in.EventID),
		Note: strings.TrimSpace(in.Note), AppliedAt: now, UpdatedAt: now,
	}
	row.ID = uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ongoing_effects (id, campaign_id, target_id, kind, name, ref, source_id, source_name,
		                             concentration, amount, unit, remaining_seconds, applied_day, status,
		                             actor, session_id, session_event_id, note, applied_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?)`,
		row.ID, campaignID, row.TargetID, row.Kind, row.Name, row.Ref, nullString(row.SourceID), row.SourceName,
		boolInt(row.Concentration), row.Amount, row.Unit, row.RemainingSeconds, row.AppliedDay,
		row.Actor, nullString(row.SessionID), nullString(row.EventID), row.Note,
		row.AppliedAt.UnixMilli(), row.UpdatedAt.UnixMilli()); err != nil {
		return nil, fmt.Errorf("effects apply: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("effects apply: %w", err)
	}
	out.Applied = row
	return out, nil
}

// endRowsOn flips every row the query selects to ended with the given
// reason, returning what it ended.
func endRowsOn(ctx context.Context, tx *sql.Tx, campaignID, query string, args []any, reason string, now time.Time) ([]Row, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("effects end scan: %w", err)
	}
	var ended []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ended = append(ended, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, r := range ended {
		if _, err := tx.ExecContext(ctx, `
			UPDATE ongoing_effects SET status = 'ended', end_reason = ?, ended_at = ?, updated_at = ?
			 WHERE id = ? AND campaign_id = ?`,
			reason, now.UnixMilli(), now.UnixMilli(), r.ID, campaignID); err != nil {
			return nil, fmt.Errorf("effects end: %w", err)
		}
	}
	return ended, nil
}

/* ---------- reads ---------- */

const rowCols = `id, target_id, kind, name, ref, COALESCE(source_id, ''), source_name, concentration,
                 amount, unit, remaining_seconds, applied_day, status, COALESCE(end_reason, ''),
                 COALESCE(ended_by, ''), actor, COALESCE(session_id, ''), COALESCE(session_event_id, ''), note,
                 applied_at, COALESCE(ended_at, 0), updated_at`

func scanRow(row interface{ Scan(...any) error }) (Row, error) {
	var (
		r                       Row
		concentration           int
		applied, ended, updated int64
	)
	if err := row.Scan(&r.ID, &r.TargetID, &r.Kind, &r.Name, &r.Ref, &r.SourceID, &r.SourceName,
		&concentration, &r.Amount, &r.Unit, &r.RemainingSeconds, &r.AppliedDay, &r.Status, &r.EndReason,
		&r.EndedBy, &r.Actor, &r.SessionID, &r.EventID, &r.Note, &applied, &ended, &updated); err != nil {
		return Row{}, err
	}
	r.Concentration = concentration == 1
	r.AppliedAt = time.UnixMilli(applied).UTC()
	if ended > 0 {
		r.EndedAt = time.UnixMilli(ended).UTC()
	}
	r.UpdatedAt = time.UnixMilli(updated).UTC()
	return r, nil
}

// List reads a campaign's (or one target's) effects and derives every
// row's running state against the world clock: the campaign clock spends
// 24 hours of every timed effect per day elapsed since its anchor, and an
// applied rest since the effect's application ends until-rest rows. Rows
// ended by a hand (dispel, a broken concentration, a superseding
// application, the rest) carry their persisted terminal state; rows time
// wore away derive as expired. includeEnded adds the history.
func (s *Store) List(ctx context.Context, campaignID, targetID string, includeEnded bool) ([]Row, error) {
	q := `SELECT ` + rowCols + ` FROM ongoing_effects WHERE campaign_id = ?`
	args := []any{campaignID}
	if targetID != "" {
		q += ` AND target_id = ?`
		args = append(args, targetID)
	}
	if !includeEnded {
		q += ` AND status = 'active'`
	}
	q += ` ORDER BY applied_at DESC, rowid DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("effects list: %w", err)
	}
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	day, err := s.currentDay(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	restMS, err := s.latestRestAt(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Running = out[i].derive(day, restMS)
	}
	return out, nil
}

// Get loads one effect row with its derived state. includeEnded=false
// refuses a row time or a hand already ended.
func (s *Store) Get(ctx context.Context, campaignID, effectID string, includeEnded bool) (*Row, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+rowCols+` FROM ongoing_effects WHERE id = ? AND campaign_id = ?`, effectID, campaignID)
	r, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: effect %s", campaign.ErrNotFound, effectID)
	}
	if err != nil {
		return nil, fmt.Errorf("effects get: %w", err)
	}
	if r.Status == StatusEnded && !includeEnded {
		return nil, fmt.Errorf("%w: effect %s has ended", campaign.ErrNotFound, effectID)
	}
	day, err := s.currentDay(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	restMS, err := s.latestRestAt(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	r.Running = r.derive(day, restMS)
	return &r, nil
}

// derive runs the pure engine over one row: the world the campaign clock
// and the rest ledger say it sits in. The row's remaining_seconds is the
// engine's persisted state as of its anchor day (combat ticks wear it);
// the world clock's wear derives on top of that, never from the declared
// amount — what the spell said is provenance, not live state.
func (r Row) derive(clockDay, latestRestMS int64) Running {
	w := World{DaysSinceAnchor: clockDay - r.AppliedDay}
	if latestRestMS > 0 && latestRestMS > r.AppliedAt.UnixMilli() {
		w.RestSinceApply = true
	}
	if r.Status == StatusEnded {
		running := Running{Effect: r.Effect, Remaining: r.RemainingSeconds, Ended: true, EndReason: r.EndReason}
		if IsTimed(r.Unit) && r.EndReason == EndExpired {
			running.Remaining = 0
		}
		return running
	}
	if !IsTimed(r.Unit) {
		return Run(r.Effect, w)
	}
	running := Running{Effect: r.Effect, Remaining: r.RemainingSeconds - w.DaysSinceAnchor*DaySeconds}
	if running.Remaining <= 0 {
		running.Remaining, running.Ended, running.EndReason = 0, true, EndExpired
	}
	return running
}

/* ---------- ending ---------- */

// EndReasons a hand may write.
const (
	endDispelled = "dispelled"
	endManual    = "manual"
)

// End ends an active effect by hand — a dispel or a DM decision. Ending
// is a state change with provenance, not a delete: the row stays, its
// reason and the hand that wrote it tell the story.
func (s *Store) End(ctx context.Context, campaignID, effectID, reason, actor string) (*Row, error) {
	switch reason {
	case "", endDispelled, endManual:
	default:
		return nil, fmt.Errorf("%w: end reason %q (dispel or manual)", campaign.ErrInvalid, reason)
	}
	if reason == "" {
		reason = endManual
	}
	r, err := s.Get(ctx, campaignID, effectID, false)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE ongoing_effects SET status = 'ended', end_reason = ?, ended_by = ?, ended_at = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ? AND status = 'active'`,
		reason, actor, now.UnixMilli(), now.UnixMilli(), r.ID, campaignID); err != nil {
		return nil, fmt.Errorf("effects end: %w", err)
	}
	return s.Get(ctx, campaignID, effectID, true)
}

/* ---------- the combat clock ---------- */

// AdvanceRounds is the combat clock: every active timed effect in the
// campaign wears rounds * 6 seconds, and what reaches zero ends (reason
// expired). Turn advance is Stage 5's to wire; the engine and this
// persisting pass are its model. Until-units are untouched — a turn
// dispels nothing. The returned rows are the derived active state after
// the tick, then the rows the tick expired.
func (s *Store) AdvanceRounds(ctx context.Context, campaignID string, rounds int64) ([]Row, []Row, error) {
	if rounds < 1 || rounds > 1000 {
		return nil, nil, fmt.Errorf("%w: rounds %d (1..1000)", campaign.ErrInvalid, rounds)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("effects advance: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT `+rowCols+` FROM ongoing_effects
		 WHERE campaign_id = ? AND status = 'active' AND unit IN ('round','minute','hour','day')
		 ORDER BY applied_at, id`, campaignID)
	if err != nil {
		return nil, nil, fmt.Errorf("effects advance scan: %w", err)
	}
	var active []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		active = append(active, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()

	now := s.now()
	var expired []Row
	for _, r := range active {
		spent := r.RemainingSeconds - rounds*RoundSeconds
		r.RemainingSeconds = spent
		r.UpdatedAt = now
		if spent <= 0 {
			r.Status, r.EndReason, r.EndedAt = StatusEnded, EndExpired, now
			r.RemainingSeconds = 0
			expired = append(expired, r)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE ongoing_effects SET remaining_seconds = ?, status = ?, end_reason = ?, ended_at = ?, updated_at = ?
			 WHERE id = ? AND campaign_id = ?`,
			r.RemainingSeconds, r.Status, nullString(r.EndReason), nullTime(r.EndedAt),
			now.UnixMilli(), r.ID, campaignID); err != nil {
			return nil, nil, fmt.Errorf("effects advance: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("effects advance: %w", err)
	}

	// The response reads back the full derived state — the wear and the
	// expiries, exactly as the next read would show them.
	after, err := s.List(ctx, campaignID, "", false)
	if err != nil {
		return nil, nil, err
	}
	return after, expired, nil
}

/* ---------- concentration ---------- */

// Concentrations lists the campaign's active concentration links — who is
// concentrating on what. One row per concentrator by construction: Apply
// breaks the old link the moment a new one lands.
func (s *Store) Concentrations(ctx context.Context, campaignID string) ([]Row, error) {
	day, err := s.currentDay(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	restMS, err := s.latestRestAt(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+rowCols+` FROM ongoing_effects
		 WHERE campaign_id = ? AND concentration = 1 AND status = 'active'
		 ORDER BY source_name COLLATE NOCASE, applied_at, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("effects concentrations: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		r.Running = r.derive(day, restMS)
		out = append(out, r)
	}
	return out, rows.Err()
}

/* ---------- SRD grounding ---------- */

// SRDText is the indexed rules text behind a condition: the reference key
// and the body, resolved through the index the same way the study deck's
// conditions deck resolves its entries — the real text, not a paraphrase.
type SRDText struct {
	Ref  string `json:"ref,omitempty"`
	Body string `json:"body,omitempty"`
}

// GroundCondition resolves a condition's SRD entry: a focused retrieval
// filtered to the entry whose trailing title segment is the condition's
// name. Empty strings when the index holds nothing (grounding is
// best-effort; the effect row never waits on it).
func (s *Store) GroundCondition(ctx context.Context, name string) SRDText {
	if s.idx == nil {
		return SRDText{}
	}
	hits, err := s.idx.Retrieve(ctx, data.CorpusDND, name+" condition", 8)
	if err != nil {
		return SRDText{}
	}
	for _, h := range hits {
		if strings.EqualFold(conditionTitleName(h.Title), name) {
			body := strings.TrimSpace(h.Body)
			if body == "" {
				continue
			}
			return SRDText{Ref: h.Number, Body: body}
		}
	}
	return SRDText{}
}

// conditionTitleName pulls the trailing segment of an indexed title: the
// SRD's ancestor-joined titles read "Conditions — Blinded"; the condition
// is the last segment. A bare "Blinded" is returned unchanged.
func conditionTitleName(title string) string {
	title = strings.TrimSpace(title)
	if i := strings.LastIndex(title, "—"); i >= 0 {
		return strings.TrimSpace(title[i+len("—"):])
	}
	return title
}

/* ---------- helpers ---------- */

// validateLinks checks the session and session-event references exist and
// belong to this campaign, filling the session in from the event when
// only the event was named.
func (s *Store) validateLinks(ctx context.Context, campaignID, sessionID, eventID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	eventID = strings.TrimSpace(eventID)
	if sessionID != "" {
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM game_sessions WHERE id = ? AND campaign_id = ?`, sessionID, campaignID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: session %s", campaign.ErrNotFound, sessionID)
		}
		if err != nil {
			return "", fmt.Errorf("check session: %w", err)
		}
	}
	if eventID != "" {
		var sid string
		err := s.db.QueryRowContext(ctx,
			`SELECT session_id FROM session_events WHERE id = ?`, eventID).Scan(&sid)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: session event %s", campaign.ErrNotFound, eventID)
		}
		if err != nil {
			return "", fmt.Errorf("check session event: %w", err)
		}
		if sessionID == "" {
			sessionID = sid
		}
	}
	return sessionID, nil
}

// currentDay reads the campaign clock.
func (s *Store) currentDay(ctx context.Context, campaignID string) (int64, error) {
	var day int64
	err := s.db.QueryRowContext(ctx,
		`SELECT clock FROM campaigns WHERE id = ?`, campaignID).Scan(&day)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: campaign %s", campaign.ErrNotFound, campaignID)
	}
	if err != nil {
		return 0, fmt.Errorf("read clock: %w", err)
	}
	return day, nil
}

// latestRestAt is the created_at of the newest applied rest in the
// campaign, 0 when none — the timestamp until-rest rows are judged
// against. Staged (undecided) and discarded rests end nothing.
func (s *Store) latestRestAt(ctx context.Context, campaignID string) (int64, error) {
	var ms int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(created_at), 0) FROM rests
		 WHERE campaign_id = ? AND status = 'applied'`, campaignID).Scan(&ms)
	if err != nil {
		return 0, fmt.Errorf("read rests: %w", err)
	}
	return ms, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}
