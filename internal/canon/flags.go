// The canon_flags ledger (MAD-309): every finding the deterministic engine
// reports becomes a row here, keyed by (check_code, record_kind, record_id) —
// Arda's ledger semantics, ported exactly:
//
//   - a re-run refreshes severity and message but NEVER clobbers a human
//     decision: a flag the DM accepted or dismissed keeps that decision
//     forever;
//   - an open finding the engine stops reporting is marked 'cleared';
//   - a cleared finding that reappears reopens as 'open' — the engine's word
//     again, and the DM's to decide afresh.
//
// Decisions land on the row itself (decided_by / decided_at / decision_note);
// the review queue (MAD-310) layers canon_reviews items over this ledger for
// engine_flag entries rather than replacing it.

package canon

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// Flag statuses.
const (
	FlagOpen      = "open"
	FlagAccepted  = "accepted"
	FlagDismissed = "dismissed"
	FlagCleared   = "cleared"
)

// Flag decisions a human may make on an open flag.
const (
	DecisionAccepted  = "accepted"
	DecisionDismissed = "dismissed"
)

// Flag is one ledger row: a finding the engine reported, its current
// severity and message, and — once a human touches it — the decision.
type Flag struct {
	ID           string
	CampaignID   string
	CheckCode    string
	RecordKind   string
	RecordID     string
	Severity     string
	Message      string
	Status       string
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	ClearedAt    time.Time // zero while the flag is not cleared
	DecidedBy    string
	DecidedAt    time.Time // zero while undecided
	DecisionNote string
}

// key identifies a flag within a campaign: the triple the ledger is keyed by.
type key struct{ check, kind, record string }

func flagKey(f campaign.Finding) key {
	return key{f.Check, f.RecordKind, f.RecordID}
}

const flagCols = `id, campaign_id, check_code, record_kind, record_id, severity, message, status,
                  first_seen_at, last_seen_at, cleared_at, decided_by, decided_at, decision_note`

func scanFlag(row interface{ Scan(...any) error }) (*Flag, error) {
	var (
		f           Flag
		first, last int64
		cleared     sql.NullInt64
		decidedAt   sql.NullInt64
	)
	if err := row.Scan(&f.ID, &f.CampaignID, &f.CheckCode, &f.RecordKind, &f.RecordID,
		&f.Severity, &f.Message, &f.Status, &first, &last, &cleared, &f.DecidedBy, &decidedAt,
		&f.DecisionNote); err != nil {
		return nil, err
	}
	f.FirstSeenAt = time.UnixMilli(first).UTC()
	f.LastSeenAt = time.UnixMilli(last).UTC()
	if cleared.Valid {
		f.ClearedAt = time.UnixMilli(cleared.Int64).UTC()
	}
	if decidedAt.Valid {
		f.DecidedAt = time.UnixMilli(decidedAt.Int64).UTC()
	}
	return &f, nil
}

/* ---------- the engine entry point ---------- */

// CheckCampaign runs the deterministic engine over one campaign and refreshes
// its flag ledger in the same transaction: load, check, refresh, return. It
// needs no model and no key — this is the offline path the CLI subcommand and
// the API endpoint share. The returned flags are the campaign's whole ledger
// as it stands after the refresh, not just this run's findings, so callers
// see preserved decisions and cleared history alongside fresh findings.
func (s *Store) CheckCampaign(ctx context.Context, campaignID string, opts CheckOptions) ([]Flag, error) {
	snap, err := LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		return nil, err
	}
	findings := CheckSnapshot(snap, opts)
	if err := s.refreshFlags(ctx, campaignID, findings); err != nil {
		return nil, err
	}
	return s.Flags(ctx, campaignID, "")
}

// refreshFlags applies the ledger semantics to one run's findings, in one
// transaction: insert new findings as open, refresh still-reported rows,
// never clobber a decision, clear the ones that stopped coming.
//
// Info-severity findings (MAD-365's clock_never_advanced is the first) are
// nudges, not decisions waiting to happen — they ride the finding list, not
// the ledger.
func (s *Store) refreshFlags(ctx context.Context, campaignID string, findings []campaign.Finding) error {
	reportable := make([]campaign.Finding, 0, len(findings))
	for _, fd := range findings {
		if fd.Severity != campaign.SeverityInfo {
			reportable = append(reportable, fd)
		}
	}
	findings = reportable
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("flags tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT `+flagCols+` FROM canon_flags WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return fmt.Errorf("load flags: %w", err)
	}
	existing := map[key]*Flag{}
	for rows.Next() {
		f, err := scanFlag(rows)
		if err != nil {
			rows.Close()
			return err
		}
		existing[flagKey(campaign.Finding{Check: f.CheckCode, RecordKind: f.RecordKind, RecordID: f.RecordID})] = f
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	now := s.now().UnixMilli()
	reported := map[key]bool{}
	for _, fd := range findings {
		k := flagKey(fd)
		reported[k] = true
		prev, seen := existing[k]
		switch {
		case !seen:
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO canon_flags (id, campaign_id, check_code, record_kind, record_id, severity, message, status, first_seen_at, last_seen_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				uuid.NewString(), campaignID, fd.Check, fd.RecordKind, fd.RecordID,
				string(fd.Severity), fd.Message, FlagOpen, now, now); err != nil {
				return fmt.Errorf("insert flag: %w", err)
			}
		case prev.Status == FlagAccepted || prev.Status == FlagDismissed:
			// A human decided; refresh the report, keep the decision.
			if _, err := tx.ExecContext(ctx, `
				UPDATE canon_flags SET severity = ?, message = ?, last_seen_at = ? WHERE id = ?`,
				string(fd.Severity), fd.Message, now, prev.ID); err != nil {
				return fmt.Errorf("refresh decided flag: %w", err)
			}
		default:
			// Open or previously cleared: the engine's word again.
			if _, err := tx.ExecContext(ctx, `
				UPDATE canon_flags SET severity = ?, message = ?, status = ?, last_seen_at = ?, cleared_at = NULL WHERE id = ?`,
				string(fd.Severity), fd.Message, FlagOpen, now, prev.ID); err != nil {
				return fmt.Errorf("refresh flag: %w", err)
			}
		}
	}
	for k, prev := range existing {
		if reported[k] || prev.Status != FlagOpen {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE canon_flags SET status = ?, cleared_at = ? WHERE id = ?`,
			FlagCleared, now, prev.ID); err != nil {
			return fmt.Errorf("clear flag: %w", err)
		}
	}
	return tx.Commit()
}

/* ---------- reads and the human decision ---------- */

// Flags lists a campaign's ledger, oldest finding first. An empty status
// returns every flag; otherwise only those in that status.
func (s *Store) Flags(ctx context.Context, campaignID, status string) ([]Flag, error) {
	q := `SELECT ` + flagCols + ` FROM canon_flags WHERE campaign_id = ?`
	args := []any{campaignID}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY first_seen_at, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list flags: %w", err)
	}
	defer rows.Close()
	var out []Flag
	for rows.Next() {
		f, err := scanFlag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// DecideFlag records a human decision on one flag: accepted (the finding is
// real; the DM owns the fix) or dismissed (not a problem). Only an open flag
// can be decided — a decided flag keeps its decision forever, and a cleared
// one is not currently reported — so re-deciding is an error, never a
// silent overwrite.
func (s *Store) DecideFlag(ctx context.Context, campaignID, checkCode, recordKind, recordID, decision, note, by string) error {
	if decision != DecisionAccepted && decision != DecisionDismissed {
		return fmt.Errorf("%w: decision %q is not %s or %s", ErrInvalid, decision, DecisionAccepted, DecisionDismissed)
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("decide tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE canon_flags SET status = ?, decided_by = ?, decided_at = ?, decision_note = ?
		 WHERE campaign_id = ? AND check_code = ? AND record_kind = ? AND record_id = ? AND status = ?`,
		decision, by, now, note, campaignID, checkCode, recordKind, recordID, FlagOpen)
	if err != nil {
		return fmt.Errorf("decide flag: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		row := tx.QueryRowContext(ctx,
			`SELECT status FROM canon_flags WHERE campaign_id = ? AND check_code = ? AND record_kind = ? AND record_id = ?`,
			campaignID, checkCode, recordKind, recordID)
		var status string
		if err := row.Scan(&status); err == sql.ErrNoRows {
			return fmt.Errorf("%w: flag %s/%s/%s", ErrNotFound, checkCode, recordKind, recordID)
		}
		return fmt.Errorf("%w: flag is %s, not open; a decided flag keeps its decision", ErrInvalid, status)
	}
	return tx.Commit()
}
