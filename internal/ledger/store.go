package ledger

// The resource ledger's rows and orchestration (MAD-419). The pure grammar
// and derivation are ledger.go; this file is everything that touches a
// database:
//
//   - SyncEntity seeds one pc's sheet-derived pools (source 'sheet'),
//     re-deriving on every sheet write the way the projection cache does;
//     DM-registered pools (source 'dm') are never touched by a sync.
//   - Balances loads pools and the transaction log and folds — the log is
//     the only state, so every read re-derives and nothing can drift.
//   - Apply validates one transaction against the fold and appends it.
//   - Rest is the live DM button: the plan is computed, written, and — on
//     a long rest — the campaign clock advances a day, reason 'rest'.
//   - StageRest / FinalizeRestBatch are the machine path: a rest proposed
//     by a model goes through the review gate as one canon batch, and the
//     decided batch's finalizer applies the mechanics exactly once.
//
// Permission rules live in the server file; this store trusts its caller's
// scope decision and validates shape only.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

// Rest row statuses.
const (
	RestApplied   = "applied"
	RestStaged    = "staged"
	RestDiscarded = "discarded"
)

// Rest sources.
const (
	SourceDM    = "dm"
	SourceModel = "model"
)

// dbRunner is what a query runs over: the pool or a transaction.
type dbRunner interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RestRow is one rests row: a short or long rest, where it came from, and —
// for a long rest — the clock movement it caused.
type RestRow struct {
	ID         string
	CampaignID string
	Kind       string
	Source     string
	Status     string
	BatchID    string
	Actor      string
	SessionID  string
	AdvanceID  string
	ClockFrom  int64
	ClockTo    int64
	Note       string
	CreatedAt  time.Time
}

// TxnRow is one resource_transactions row as the reads return it.
type TxnRow struct {
	Transaction
	CreatedAt time.Time
}

// Store runs the ledger over the shared database handle.
type Store struct {
	db        *sql.DB
	campaigns *campaign.Store
	canon     *canon.Store
	now       func() time.Time
}

// New builds a ledger store on an open, migrated database handle. The canon
// store carries the review gate a model-proposed rest stages through; it
// may be the offline one.
func New(db *sql.DB, campaigns *campaign.Store, canonStore *canon.Store) (*Store, error) {
	if db == nil {
		return nil, errors.New("ledger: nil database handle")
	}
	if campaigns == nil || canonStore == nil {
		return nil, errors.New("ledger: the campaign and canon stores are both required")
	}
	return &Store{db: db, campaigns: campaigns, canon: canonStore, now: time.Now().UTC}, nil
}

/* ---------- the sheet sync ---------- */

// SyncEntity seeds one pc's sheet-derived pools from its payload. Rows whose
// definition the sheet still carries are updated in place (their ids — and
// therefore their transactions' references — survive); definitions the sheet
// dropped are deleted, their transactions kept by key with pool_id nulled;
// DM-registered pools are never touched. A pc without a typed sheet has no
// derived pools at all — no silent data invention.
func (s *Store) SyncEntity(ctx context.Context, campaignID, entityID string) error {
	var payloadJSON, status string
	err := s.db.QueryRowContext(ctx,
		`SELECT payload, status FROM entities WHERE id = ? AND campaign_id = ? AND kind = 'pc'`,
		entityID, campaignID).Scan(&payloadJSON, &status)
	if errors.Is(err, sql.ErrNoRows) {
		// Not a pc (any more): its pools have no definition to derive.
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM resource_pools WHERE entity_id = ? AND source = 'sheet'`, entityID); err != nil {
			return fmt.Errorf("ledger sync: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("ledger sync: %w", err)
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(payloadJSON), &payload)
	var derived []Pool
	if status != campaign.StatusDeleted {
		if sh, has, err := sheet.FromPayload(payload); err == nil && has {
			derived = PoolsOf(sh)
		}
	}
	return s.syncPools(ctx, campaignID, entityID, derived)
}

// SyncAll re-derives every pc's sheet pools — the boot-time rebuild that
// makes the pool table a cache, the same contract the sheet projection
// holds.
func (s *Store) SyncAll(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, campaign_id FROM entities WHERE kind = 'pc' AND status != 'deleted'`)
	if err != nil {
		return fmt.Errorf("ledger sync scan: %w", err)
	}
	type pc struct{ id, campaign string }
	var pcs []pc
	for rows.Next() {
		var p pc
		if err := rows.Scan(&p.id, &p.campaign); err != nil {
			rows.Close()
			return fmt.Errorf("ledger sync scan: %w", err)
		}
		pcs = append(pcs, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("ledger sync scan: %w", err)
	}
	rows.Close()
	for _, p := range pcs {
		if err := s.SyncEntity(ctx, p.campaign, p.id); err != nil {
			return err
		}
	}
	return nil
}

// syncPools upserts the derived pool set and drops the stale ones in one
// transaction. Only source 'sheet' rows are touched.
func (s *Store) syncPools(ctx context.Context, campaignID, entityID string, derived []Pool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger sync: %w", err)
	}
	defer tx.Rollback()
	now := s.now().UnixMilli()
	for _, p := range derived {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO resource_pools (id, campaign_id, entity_id, kind, name, label, size, recovery, granularity, source, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'sheet', ?, ?)
			ON CONFLICT(campaign_id, entity_id, kind, name) DO UPDATE SET
				label = excluded.label, size = excluded.size, recovery = excluded.recovery,
				granularity = excluded.granularity, updated_at = excluded.updated_at`,
			uuid.NewString(), campaignID, entityID, p.Kind, p.Name, p.Label, p.Size, p.Recovery,
			granularityOf(p), now, now); err != nil {
			return fmt.Errorf("ledger sync: %w", err)
		}
	}
	// The sheet no longer declares some pool: its definition goes, its
	// transactions stay (pool_id nulled, the key column keeps the history
	// readable). A DM's registrations are the DM's word, not the sheet's.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM resource_pools
		 WHERE entity_id = ? AND campaign_id = ? AND source = 'sheet'
		   AND (kind, name) NOT IN (
		       SELECT json_extract(v.value, '$[0]'), json_extract(v.value, '$[1]')
		       FROM json_each(?) v)`,
		entityID, campaignID, staleKeysJSON(derived)); err != nil {
		return fmt.Errorf("ledger sync: %w", err)
	}
	return tx.Commit()
}

// staleKeysJSON renders the derived set as the [kind,name] pairs the
// sync's NOT IN reads. An empty set still needs well-formed JSON.
func staleKeysJSON(derived []Pool) string {
	pairs := make([][2]string, 0, len(derived))
	for _, p := range derived {
		pairs = append(pairs, [2]string{p.Kind, p.Name})
	}
	blob, err := json.Marshal(pairs)
	if err != nil {
		return "[]"
	}
	return string(blob)
}

func granularityOf(p Pool) int {
	if p.Granularity > 1 {
		return p.Granularity
	}
	return 1
}

/* ---------- reads ---------- */

const poolCols = `id, campaign_id, entity_id, kind, name, label, size, recovery, granularity, source`

func scanPool(row interface{ Scan(...any) error }) (Pool, error) {
	var (
		p          Pool
		campaignID string
	)
	if err := row.Scan(&p.ID, &campaignID, new(string), &p.Kind, &p.Name, &p.Label,
		&p.Size, &p.Recovery, &p.Granularity, &p.Source); err != nil {
		return Pool{}, err
	}
	return p, nil
}

// Pools lists one character's pool definitions in canonical order.
func (s *Store) Pools(ctx context.Context, campaignID, entityID string) ([]Pool, error) {
	if err := s.characterExists(ctx, campaignID, entityID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+poolCols+` FROM resource_pools WHERE entity_id = ? AND campaign_id = ?`, entityID, campaignID)
	if err != nil {
		return nil, fmt.Errorf("ledger pools: %w", err)
	}
	defer rows.Close()
	var out []Pool
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out, nil
}

// Balances folds the log into one character's derived state.
func (s *Store) Balances(ctx context.Context, campaignID, entityID string) ([]Balance, error) {
	if err := s.characterExists(ctx, campaignID, entityID); err != nil {
		return nil, err
	}
	pools, err := s.Pools(ctx, campaignID, entityID)
	if err != nil {
		return nil, err
	}
	txns, err := s.transactions(ctx, entityID)
	if err != nil {
		return nil, err
	}
	return Derive(pools, txns), nil
}

// History returns one character's transactions, most recent first up to
// limit (0 picks a sensible default).
func (s *Store) History(ctx context.Context, campaignID, entityID string, limit int) ([]TxnRow, error) {
	if err := s.characterExists(ctx, campaignID, entityID); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(pool_id, ''), pool, kind, amount, COALESCE(rest_id, ''),
		       COALESCE(session_event_id, ''), COALESCE(session_id, ''), actor, note, clock_day, created_at
		FROM resource_transactions WHERE entity_id = ? AND campaign_id = ?
		ORDER BY rowid DESC LIMIT ?`, entityID, campaignID, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger history: %w", err)
	}
	defer rows.Close()
	var out []TxnRow
	for rows.Next() {
		var (
			t       TxnRow
			created int64
		)
		if err := rows.Scan(&t.ID, &t.PoolID, &t.Pool, &t.Kind, &t.Amount, &t.RestID,
			&t.EventID, &t.SessionID, &t.Actor, &t.Note, &t.Day, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// transactions loads one character's log in fold order (rowid ascending).
func (s *Store) transactions(ctx context.Context, entityID string) ([]Transaction, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(pool_id, ''), pool, kind, amount, COALESCE(rest_id, ''),
		       COALESCE(session_event_id, ''), COALESCE(session_id, ''), actor, note, clock_day
		FROM resource_transactions WHERE entity_id = ?
		ORDER BY rowid`, entityID)
	if err != nil {
		return nil, fmt.Errorf("ledger log: %w", err)
	}
	defer rows.Close()
	var out []Transaction
	for rows.Next() {
		var t Transaction
		if err := rows.Scan(&t.ID, &t.PoolID, &t.Pool, &t.Kind, &t.Amount, &t.RestID,
			&t.EventID, &t.SessionID, &t.Actor, &t.Note, &t.Day); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

/* ---------- transactions ---------- */

// TxnInput is one transaction to append. Kind is spend | regain | set; the
// grammar-driven resets are written by the rest paths, never by hand.
type TxnInput struct {
	Kind      string `json:"kind"`
	Amount    int    `json:"amount"`
	Note      string `json:"note"`
	SessionID string `json:"session_id"`
	EventID   string `json:"session_event_id"`
}

// Apply validates and appends one transaction. The validation folds the log
// inside the transaction that inserts, so a spend lands against the ledger
// it validated, not a stale snapshot.
func (s *Store) Apply(ctx context.Context, campaignID, entityID, poolID string, in TxnInput, actor string) (*TxnRow, []Balance, error) {
	if err := s.characterExists(ctx, campaignID, entityID); err != nil {
		return nil, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger apply: %w", err)
	}
	defer tx.Rollback()

	p, err := poolOn(ctx, tx, campaignID, entityID, poolID)
	if err != nil {
		return nil, nil, err
	}
	balance, err := balanceOf(ctx, tx, p)
	if err != nil {
		return nil, nil, err
	}

	t := Transaction{PoolID: p.ID, Pool: p.Key(), Kind: in.Kind, Amount: in.Amount,
		SessionID: in.SessionID, EventID: in.EventID, Actor: actor, Note: strings.TrimSpace(in.Note)}
	if err := s.validateLinks(ctx, tx, campaignID, &t); err != nil {
		return nil, nil, err
	}
	switch in.Kind {
	case TxnSet:
		if err := ValidateSet(p, in.Amount); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
		}
	case TxnSpend, TxnRegain:
		if err := t.Validate(p, balance); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
		}
	default:
		return nil, nil, fmt.Errorf("%w: transaction kind %q (a reset is a rest's to write)", campaign.ErrInvalid, in.Kind)
	}

	day, err := currentDayOn(ctx, tx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	t.Day = day
	row := &TxnRow{Transaction: t, CreatedAt: s.now()}
	t.ID = uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO resource_transactions (id, campaign_id, entity_id, pool_id, pool, kind, amount,
		                                   session_event_id, session_id, actor, note, clock_day, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, campaignID, entityID, p.ID, t.Pool, t.Kind, t.Amount,
		nullString(t.EventID), nullString(t.SessionID), t.Actor, t.Note, t.Day,
		row.CreatedAt.UnixMilli()); err != nil {
		return nil, nil, fmt.Errorf("ledger apply: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("ledger apply: %w", err)
	}
	row.ID = t.ID
	balances, err := s.Balances(ctx, campaignID, entityID)
	if err != nil {
		return nil, nil, err
	}
	return row, balances, nil
}

// balanceOf folds one pool's log over a runner.
func balanceOf(ctx context.Context, q dbRunner, p Pool) (int, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT kind, amount FROM resource_transactions WHERE pool_id = ? ORDER BY rowid`, p.ID)
	if err != nil {
		return 0, fmt.Errorf("ledger fold: %w", err)
	}
	defer rows.Close()
	balance := p.Size
	for rows.Next() {
		var (
			kind   string
			amount int
		)
		if err := rows.Scan(&kind, &amount); err != nil {
			return 0, err
		}
		switch kind {
		case TxnSpend:
			balance -= amount
		case TxnRegain:
			balance += amount
		case TxnSet, TxnReset:
			balance = amount
		}
	}
	return balance, rows.Err()
}

// validateLinks checks the session and session-event references exist and
// belong to this campaign, filling the session in from the event when only
// the event was named.
func (s *Store) validateLinks(ctx context.Context, q dbRunner, campaignID string, t *Transaction) error {
	if t.SessionID != "" {
		var one int
		err := q.QueryRowContext(ctx,
			`SELECT 1 FROM game_sessions WHERE id = ? AND campaign_id = ?`, t.SessionID, campaignID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: session %s", campaign.ErrNotFound, t.SessionID)
		}
		if err != nil {
			return fmt.Errorf("check session: %w", err)
		}
	}
	if t.EventID != "" {
		var sid string
		err := q.QueryRowContext(ctx,
			`SELECT session_id FROM session_events WHERE id = ?`, t.EventID).Scan(&sid)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: session event %s", campaign.ErrNotFound, t.EventID)
		}
		if err != nil {
			return fmt.Errorf("check session event: %w", err)
		}
		if t.SessionID == "" {
			t.SessionID = sid
		}
	}
	return nil
}

/* ---------- DM pools ---------- */

// CreatePool registers a DM-defined pool — ki points, rage, ammunition, a
// homebrew currency — on the same grammar the sheet's pools use. Slot,
// hit-dice and currency pools are the sheet's to define; a registration
// must be a feature or an item.
func (s *Store) CreatePool(ctx context.Context, campaignID, entityID string, p Pool) (*Pool, error) {
	if err := s.characterExists(ctx, campaignID, entityID); err != nil {
		return nil, err
	}
	p.Source = SourceDM
	p.Kind = strings.TrimSpace(p.Kind)
	p.Name = strings.TrimSpace(p.Name)
	p.Label = strings.TrimSpace(p.Label)
	if p.Kind != KindFeature && p.Kind != KindItem {
		return nil, fmt.Errorf("%w: a registered pool is a feature or an item; %q is the sheet's to define",
			campaign.ErrInvalid, p.Kind)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO resource_pools (id, campaign_id, entity_id, kind, name, label, size, recovery, granularity, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'dm', ?, ?)`,
		uuid.NewString(), campaignID, entityID, p.Kind, p.Name, p.Label, p.Size, p.Recovery,
		granularityOf(p), s.now().UnixMilli(), s.now().UnixMilli()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, fmt.Errorf("%w: pool %s already exists", campaign.ErrAlreadyExists, p.Key())
		}
		return nil, fmt.Errorf("create pool: %w", err)
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+poolCols+` FROM resource_pools WHERE entity_id = ? AND campaign_id = ? AND kind = ? AND name = ?`,
		entityID, campaignID, p.Kind, p.Name)
	created, err := scanPool(row)
	if err != nil {
		return nil, fmt.Errorf("read back created pool: %w", err)
	}
	return &created, nil
}

// UpdatePool patches a DM-registered pool's definition. Sheet-derived pools
// refuse: their size and recovery are the sheet's numbers, and editing them
// here is exactly the two-truths drift the ledger exists to prevent.
func (s *Store) UpdatePool(ctx context.Context, campaignID, entityID, poolID string, label *string, size *int, recovery *string, granularity *int) (*Pool, error) {
	p, err := s.poolInCampaign(ctx, campaignID, entityID, poolID)
	if err != nil {
		return nil, err
	}
	if p.Source != SourceDM {
		return nil, fmt.Errorf("%w: %s is defined by the sheet; edit the sheet, not the pool", campaign.ErrInvalid, p.Key())
	}
	if label != nil {
		p.Label = strings.TrimSpace(*label)
	}
	if size != nil {
		p.Size = *size
	}
	if recovery != nil {
		p.Recovery = strings.TrimSpace(*recovery)
	}
	if granularity != nil {
		p.Granularity = *granularity
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE resource_pools SET label = ?, size = ?, recovery = ?, granularity = ?, updated_at = ?
		WHERE id = ? AND entity_id = ? AND campaign_id = ?`,
		p.Label, p.Size, p.Recovery, granularityOf(*p), s.now().UnixMilli(), p.ID, entityID, campaignID); err != nil {
		return nil, fmt.Errorf("update pool: %w", err)
	}
	return p, nil
}

// DeletePool removes a DM-registered pool; its transactions survive keyed
// history. Sheet-derived pools refuse for the same reason UpdatePool does.
func (s *Store) DeletePool(ctx context.Context, campaignID, entityID, poolID string) error {
	p, err := s.poolInCampaign(ctx, campaignID, entityID, poolID)
	if err != nil {
		return err
	}
	if p.Source != SourceDM {
		return fmt.Errorf("%w: %s is defined by the sheet", campaign.ErrInvalid, p.Key())
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM resource_pools WHERE id = ? AND entity_id = ? AND campaign_id = ?`,
		p.ID, entityID, campaignID); err != nil {
		return fmt.Errorf("delete pool: %w", err)
	}
	return nil
}

/* ---------- the live rest ---------- */

// Rest executes the DM's live rest button: one rest row, the planned
// transactions written under it, and — for a long rest — the campaign clock
// advanced by one day under reason 'rest'. No gate: a DM pressing the button
// live is the confirmation every machine proposal waits for.
func (s *Store) Rest(ctx context.Context, campaignID string, entityIDs []string, restKind, sessionID, note, actor string) (*RestRow, map[string][]PlannedTxn, error) {
	entityIDs = dedupe(entityIDs)
	if len(entityIDs) == 0 {
		return nil, nil, fmt.Errorf("%w: a rest needs at least one character", campaign.ErrInvalid)
	}
	if restKind != RestShort && restKind != RestLong {
		return nil, nil, fmt.Errorf("%w: rest kind %q", campaign.ErrInvalid, restKind)
	}
	plans := make(map[string][]PlannedTxn, len(entityIDs))
	var clockFrom int64
	for _, eid := range entityIDs {
		plan, err := s.planFor(ctx, campaignID, eid, restKind)
		if err != nil {
			return nil, nil, err
		}
		plans[eid] = plan
	}
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	clockFrom = c.Clock

	rest := &RestRow{
		ID: uuid.NewString(), CampaignID: campaignID, Kind: restKind, Source: SourceDM,
		Status: RestApplied, Actor: actor, SessionID: strings.TrimSpace(sessionID),
		Note: strings.TrimSpace(note), ClockFrom: clockFrom, ClockTo: clockFrom,
		CreatedAt: s.now(),
	}
	if err := s.writeRest(ctx, rest, plans, true); err != nil {
		return nil, nil, err
	}
	if restKind == RestLong {
		adv, _, err := s.campaigns.AdvanceClockBy(ctx, campaignID, 1, campaign.AdvanceRest,
			fmt.Sprintf("%s rest %s", restKind, shortID(rest.ID)), sessionID, actor)
		if err != nil {
			return nil, nil, err
		}
		rest.AdvanceID, rest.ClockTo = adv.ID, adv.ToDay
		if _, err := s.db.ExecContext(ctx,
			`UPDATE rests SET advance_id = ?, clock_to = ? WHERE id = ?`,
			rest.AdvanceID, rest.ClockTo, rest.ID); err != nil {
			return nil, nil, fmt.Errorf("record rest advance: %w", err)
		}
	}
	return rest, plans, nil
}

// planFor computes one character's rest plan against the current ledger.
func (s *Store) planFor(ctx context.Context, campaignID, entityID, restKind string) ([]PlannedTxn, error) {
	pools, err := s.Pools(ctx, campaignID, entityID)
	if err != nil {
		return nil, err
	}
	txns, err := s.transactions(ctx, entityID)
	if err != nil {
		return nil, err
	}
	plan := RestPlan(restKind, pools, Derive(pools, txns))
	for i := range plan {
		plan[i].Entity = entityID
		plan[i].PoolID = poolIDOf(pools, plan[i].Pool)
	}
	return plan, nil
}

// writeRest writes a rest row (insert=true) or reuses the staged one, and
// its transactions, in one transaction.
func (s *Store) writeRest(ctx context.Context, rest *RestRow, plans map[string][]PlannedTxn, insert bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rest tx: %w", err)
	}
	defer tx.Rollback()
	if insert {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rests (id, campaign_id, kind, source, status, plan, batch_id, actor, session_id, advance_id, clock_from, clock_to, note, created_at)
			VALUES (?, ?, ?, ?, ?, '[]', NULL, ?, ?, NULL, ?, ?, ?, ?)`,
			rest.ID, rest.CampaignID, rest.Kind, rest.Source, rest.Status,
			rest.Actor, nullString(rest.SessionID), rest.ClockFrom, rest.ClockTo,
			rest.Note, rest.CreatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("insert rest: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx,
		`UPDATE rests SET status = ?, clock_from = ?, clock_to = ? WHERE id = ?`,
		rest.Status, rest.ClockFrom, rest.ClockTo, rest.ID); err != nil {
		return fmt.Errorf("finish rest: %w", err)
	}
	for _, eid := range sortedKeys(plans) {
		for _, t := range plans[eid] {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO resource_transactions (id, campaign_id, entity_id, pool_id, pool, kind, amount,
				                                   rest_id, session_event_id, session_id, actor, note, clock_day, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
				uuid.NewString(), rest.CampaignID, eid, nullString(t.PoolID), t.Pool, t.Kind, t.Amount,
				rest.ID, nullString(rest.SessionID), rest.Actor, t.Reason, rest.ClockFrom,
				s.now().UnixMilli()); err != nil {
				return fmt.Errorf("insert rest transaction: %w", err)
			}
		}
	}
	return tx.Commit()
}

/* ---------- the proposed rest ---------- */

// StageRest is the machine path: a rest proposed by a model (a transcribed
// "we take a long rest") computed now, stored as a staged rests row, and
// handed to the review gate as one canon batch — one event item per
// character, the summary carrying exactly what the rest would do. Nothing
// mechanical runs until the batch is decided.
func (s *Store) StageRest(ctx context.Context, campaignID string, entityIDs []string, restKind, sessionID, note, userID string) (*RestRow, *canon.Batch, map[string][]PlannedTxn, error) {
	entityIDs = dedupe(entityIDs)
	if len(entityIDs) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: a rest needs at least one character", campaign.ErrInvalid)
	}
	if restKind != RestShort && restKind != RestLong {
		return nil, nil, nil, fmt.Errorf("%w: rest kind %q", campaign.ErrInvalid, restKind)
	}
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, nil, nil, err
	}
	plans := make(map[string][]PlannedTxn, len(entityIDs))
	items := make([]canon.BatchItemInput, 0, len(entityIDs))
	for _, eid := range entityIDs {
		ent, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, eid)
		if err != nil {
			return nil, nil, nil, err
		}
		if ent.Kind != campaign.KindPC {
			return nil, nil, nil, fmt.Errorf("%w: %s is a %s, not a pc", campaign.ErrInvalid, ent.Name, ent.Kind)
		}
		plan, err := s.planFor(ctx, campaignID, eid, restKind)
		if err != nil {
			return nil, nil, nil, err
		}
		for i := range plan {
			plan[i].ItemRef = restItemID(eid)
		}
		plans[eid] = plan
		summary := fmt.Sprintf("%s %s", ent.Name, RestSummary(restKind, plan))
		items = append(items, canon.BatchItemInput{
			ID:      restItemID(eid),
			Kind:    "event",
			Subject: fmt.Sprintf("%s takes a %s rest", ent.Name, restKind),
			Summary: summary,
			Payload: map[string]any{
				"local_id":     restItemID(eid),
				"summary":      summary,
				"clock_at":     restClockAt(c.Clock, restKind),
				"participants": []map[string]any{{"entity": eid, "role": "resting"}},
				"rest":         map[string]any{"kind": restKind, "character": eid},
			},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

	planJSON, err := json.Marshal(plans)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode rest plan: %w", err)
	}
	rest := &RestRow{
		ID: uuid.NewString(), CampaignID: campaignID, Kind: restKind, Source: SourceModel,
		Status: RestStaged, Actor: userID, SessionID: strings.TrimSpace(sessionID),
		Note: strings.TrimSpace(note), ClockFrom: c.Clock, ClockTo: c.Clock,
		CreatedAt: s.now(),
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO rests (id, campaign_id, kind, source, status, plan, batch_id, actor, session_id, advance_id, clock_from, clock_to, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, NULL, ?, ?, ?, ?)`,
		rest.ID, rest.CampaignID, rest.Kind, rest.Source, rest.Status, string(planJSON),
		rest.Actor, nullString(rest.SessionID), rest.ClockFrom, rest.ClockTo,
		rest.Note, rest.CreatedAt.UnixMilli()); err != nil {
		return nil, nil, nil, fmt.Errorf("stage rest: %w", err)
	}

	verb := "short"
	if restKind == RestLong {
		verb = "long"
	}
	batch, err := s.canon.StageBatch(ctx, canon.BatchInput{
		CampaignID: campaignID, Source: canon.BatchSourceRest,
		Prompt: fmt.Sprintf("proposed %s rest: %d character(s), day %d | %s",
			verb, len(entityIDs), c.Clock, strings.TrimSpace(note)),
		CreatedBy: userID, Items: items,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE rests SET batch_id = ? WHERE id = ?`, batch.ID, rest.ID); err != nil {
		return nil, nil, nil, fmt.Errorf("link rest batch: %w", err)
	}
	rest.BatchID = batch.ID
	return rest, batch, plans, nil
}

// restItemID is the batch item id one character's rest rides on.
func restItemID(entityID string) string { return "rest-" + entityID }

// restClockAt is the day a rest's event lands on: a long rest ends on the
// next day, a short rest on the same one.
func restClockAt(day int64, restKind string) int64 {
	if restKind == RestLong {
		return day + 1
	}
	return day
}

/* ---------- completion: canon.RestFinalizer ---------- */

// FinalizeRestBatch completes a decided rest batch: the mechanics the batch
// cannot carry itself. For every character whose item was accepted (or
// modified), the rest plan is recomputed against the current ledger and
// written under the rest row; a long rest with at least one resting
// character advances the campaign clock by exactly one day, once, reason
// 'rest'. A dismissed batch discards the rest and nothing moves.
// Idempotent: a rest not in staged status is a no-op, so a failed
// completion heals on the retry a decided batch allows.
func (s *Store) FinalizeRestBatch(ctx context.Context, batch *canon.Batch) error {
	if batch == nil || batch.Source != canon.BatchSourceRest {
		return nil
	}
	rest, err := s.restByBatch(ctx, batch.ID, batch.CampaignID)
	if err != nil {
		return err
	}
	if rest == nil || rest.Status != RestStaged {
		return nil
	}
	if batch.Status != canon.BatchAccepted && batch.Status != canon.BatchPartiallyAccepted {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE rests SET status = ? WHERE id = ? AND status = ?`,
			RestDiscarded, rest.ID, RestStaged); err != nil {
			return fmt.Errorf("finish rest: %w", err)
		}
		return nil
	}
	accepted, err := s.acceptedRestCharacters(ctx, batch)
	if err != nil {
		return err
	}
	if len(accepted) == 0 {
		// Every character's item was refused: nobody rested, the clock
		// does not move, the rest is discarded all the same.
		if _, err := s.db.ExecContext(ctx,
			`UPDATE rests SET status = ? WHERE id = ? AND status = ?`,
			RestDiscarded, rest.ID, RestStaged); err != nil {
			return fmt.Errorf("finish rest: %w", err)
		}
		return nil
	}
	// Recompute against the current ledger: the reviewed plan is evidence,
	// resets are idempotent, and the PHB is the PHB however long the queue
	// took.
	plans := make(map[string][]PlannedTxn, len(accepted))
	for _, eid := range accepted {
		plan, err := s.planFor(ctx, batch.CampaignID, eid, rest.Kind)
		if err != nil {
			return err
		}
		if len(plan) > 0 {
			plans[eid] = plan
		}
	}
	applied := &RestRow{
		ID: rest.ID, CampaignID: rest.CampaignID, Kind: rest.Kind, Source: rest.Source,
		Status: RestApplied, BatchID: rest.BatchID, Actor: rest.Actor, SessionID: rest.SessionID,
		ClockFrom: rest.ClockFrom, ClockTo: rest.ClockFrom, Note: rest.Note, CreatedAt: rest.CreatedAt,
	}
	if err := s.writeRest(ctx, applied, plans, false); err != nil {
		return err
	}
	if rest.Kind == RestLong {
		adv, _, err := s.campaigns.AdvanceClockBy(ctx, batch.CampaignID, 1, campaign.AdvanceRest,
			fmt.Sprintf("%s rest (reviewed) %s", rest.Kind, shortID(rest.ID)), rest.SessionID, batch.CreatedBy)
		if err != nil {
			return fmt.Errorf("advance clock: %w", err)
		}
		applied.AdvanceID, applied.ClockTo = adv.ID, adv.ToDay
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE rests SET status = ?, advance_id = COALESCE(?, advance_id), clock_to = ? WHERE id = ?`,
		RestApplied, nullString(applied.AdvanceID), applied.ClockTo, rest.ID); err != nil {
		return fmt.Errorf("finish rest: %w", err)
	}
	return nil
}

// acceptedRestCharacters reads the decided batch back and returns the
// entity ids whose rest items were accepted or modified.
func (s *Store) acceptedRestCharacters(ctx context.Context, batch *canon.Batch) ([]string, error) {
	full, err := s.canon.GetBatch(ctx, batch.CampaignID, batch.ID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, it := range full.Items {
		if it.Status != canon.ReviewAccepted && it.Status != canon.ReviewModified {
			continue
		}
		var payload map[string]any
		if len(it.Detail) > 0 {
			_ = json.Unmarshal([]byte(it.Detail), &payload)
		}
		if rest, ok := payload["rest"].(map[string]any); ok {
			if eid, _ := rest["character"].(string); eid != "" {
				out = append(out, eid)
			}
		}
	}
	return dedupe(out), nil
}

/* ---------- reads and helpers ---------- */

// Rests lists a campaign's rests, newest first.
func (s *Store) Rests(ctx context.Context, campaignID string) ([]RestRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, campaign_id, kind, source, status, COALESCE(batch_id, ''), actor,
		       COALESCE(session_id, ''), COALESCE(advance_id, ''),
		       COALESCE(clock_from, 0), COALESCE(clock_to, 0), note, created_at
		FROM rests WHERE campaign_id = ? ORDER BY created_at DESC, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list rests: %w", err)
	}
	defer rows.Close()
	var out []RestRow
	for rows.Next() {
		var (
			r       RestRow
			created int64
		)
		if err := rows.Scan(&r.ID, &r.CampaignID, &r.Kind, &r.Source, &r.Status, &r.BatchID, &r.Actor,
			&r.SessionID, &r.AdvanceID, &r.ClockFrom, &r.ClockTo, &r.Note, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) characterExists(ctx context.Context, campaignID, entityID string) error {
	var kind string
	err := s.db.QueryRowContext(ctx,
		`SELECT kind FROM entities WHERE id = ? AND campaign_id = ? AND status != 'deleted'`,
		entityID, campaignID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: character %s", campaign.ErrNotFound, entityID)
	}
	if err != nil {
		return fmt.Errorf("check character: %w", err)
	}
	if kind != campaign.KindPC {
		return fmt.Errorf("%w: %s is a %s, not a pc", campaign.ErrInvalid, entityID, kind)
	}
	return nil
}

func (s *Store) poolInCampaign(ctx context.Context, campaignID, entityID, poolID string) (*Pool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+poolCols+` FROM resource_pools WHERE id = ? AND entity_id = ? AND campaign_id = ?`,
		poolID, entityID, campaignID)
	p, err := scanPool(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: pool %s", campaign.ErrNotFound, poolID)
	}
	if err != nil {
		return nil, fmt.Errorf("load pool: %w", err)
	}
	return &p, nil
}

// poolOn loads a pool over a runner.
func poolOn(ctx context.Context, q dbRunner, campaignID, entityID, poolID string) (Pool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+poolCols+` FROM resource_pools WHERE id = ? AND entity_id = ? AND campaign_id = ?`,
		poolID, entityID, campaignID)
	p, err := scanPool(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Pool{}, fmt.Errorf("%w: pool %s", campaign.ErrNotFound, poolID)
	}
	if err != nil {
		return Pool{}, fmt.Errorf("load pool: %w", err)
	}
	return p, nil
}

func (s *Store) restByBatch(ctx context.Context, batchID, campaignID string) (*RestRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, campaign_id, kind, source, status, COALESCE(batch_id, ''), actor,
		       COALESCE(session_id, ''), COALESCE(advance_id, ''),
		       COALESCE(clock_from, 0), COALESCE(clock_to, 0), note, created_at
		FROM rests WHERE batch_id = ? AND campaign_id = ?`, batchID, campaignID)
	var (
		r       RestRow
		created int64
	)
	if err := row.Scan(&r.ID, &r.CampaignID, &r.Kind, &r.Source, &r.Status, &r.BatchID, &r.Actor,
		&r.SessionID, &r.AdvanceID, &r.ClockFrom, &r.ClockTo, &r.Note, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // a foreign rest-source batch; nothing of ours to finish
		}
		return nil, fmt.Errorf("load rest: %w", err)
	}
	r.CreatedAt = time.UnixMilli(created).UTC()
	return &r, nil
}

// currentDayOn reads the campaign clock over a runner.
func currentDayOn(ctx context.Context, q dbRunner, campaignID string) (int64, error) {
	var day int64
	err := q.QueryRowContext(ctx,
		`SELECT clock FROM campaigns WHERE id = ?`, campaignID).Scan(&day)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: campaign %s", campaign.ErrNotFound, campaignID)
	}
	if err != nil {
		return 0, fmt.Errorf("read clock: %w", err)
	}
	return day, nil
}

// poolIDOf resolves a pool key to its row id in the loaded set.
func poolIDOf(pools []Pool, key string) string {
	for _, p := range pools {
		if p.Key() == key {
			return p.ID
		}
	}
	return ""
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// shortID renders a rest id for human-readable notes without leaking the
// whole uuid into the ledger prose.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
