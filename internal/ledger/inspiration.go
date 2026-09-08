package ledger

// Inspiration (MAD-428, stage 11 of MAD-417): the one pool the 2014
// rules name for everyone. A character either holds inspiration or does
// not — size 1, never stacked — the DM awards it for good play, and
// spending it grants advantage on one attack roll, saving throw or
// ability check (the roll flow spends it; internal/server/dice.go).
//
// It registers through the pool grammar unchanged: kind feature, name
// inspiration, size 1, recovery manual — a rest does not refill it
// (2014: it lasts until spent or given away), the board reads it like
// any balance, and the DM's generic pool tools still correct it. The
// registration happens at the first award, because a pool a character
// has never earned would read "full" the day it was minted: the
// registration writes an explicit set-to-zero transaction first, so the
// derived balance starts at "does not hold it" — the log is the state.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// InspirationKey is the inspiration pool's canonical identity.
const InspirationKey = KindFeature + ":" + "inspiration"

// InspirationPool is the inspiration pool's definition: one slot,
// manual recovery — no rest refills it.
func InspirationPool() Pool {
	return Pool{
		Kind: KindFeature, Name: "inspiration", Label: "Inspiration",
		Size: 1, Recovery: RecoveryManual, Source: SourceDM,
	}
}

// InspirationBalance reports whether a character holds inspiration:
// the derived balance of the feature:inspiration pool. ok is false
// when no such pool exists — the character has never been awarded
// inspiration — or the read fails.
func (s *Store) InspirationBalance(ctx context.Context, campaignID, entityID string) (int, bool) {
	pools, err := s.Pools(ctx, campaignID, entityID)
	if err != nil {
		return 0, false
	}
	var poolID string
	for _, p := range pools {
		if p.Key() == InspirationKey {
			poolID = p.ID
			break
		}
	}
	if poolID == "" {
		return 0, false
	}
	for _, b := range s.balancesOf(ctx, campaignID, entityID, pools) {
		if b.Pool.Key() == InspirationKey {
			return b.Current, true
		}
	}
	return 0, false
}

// AwardInspiration grants inspiration: the DM's word, written as one
// visible transaction. The pool is registered on first award — seeded
// with an explicit set-to-none so the fold reads "does not hold it"
// until this award says otherwise — and the 2014 rule that it does not
// stack is the refusal when the character already holds it.
func (s *Store) AwardInspiration(ctx context.Context, campaignID, entityID, note, actor string) (*TxnRow, []Balance, error) {
	if err := s.characterExists(ctx, campaignID, entityID); err != nil {
		return nil, nil, err
	}
	if held, ok := s.InspirationBalance(ctx, campaignID, entityID); ok && held > 0 {
		return nil, nil, fmt.Errorf("%w: %s already holds inspiration — 2014: it does not stack",
			campaign.ErrInvalid, entityID)
	}
	if _, err := s.ensureInspirationPool(ctx, campaignID, entityID); err != nil {
		return nil, nil, err
	}
	pools, err := s.Pools(ctx, campaignID, entityID)
	if err != nil {
		return nil, nil, err
	}
	award := "inspiration awarded"
	if note = strings.TrimSpace(note); note != "" {
		award += " — " + note
	}
	for _, p := range pools {
		if p.Key() == InspirationKey {
			return s.Apply(ctx, campaignID, entityID, p.ID,
				TxnInput{Kind: TxnSet, Amount: 1, Note: award}, actor)
		}
	}
	return nil, nil, fmt.Errorf("%w: the inspiration pool did not survive its own registration", campaign.ErrInvalid)
}

// TrySpendInspiration spends inspiration atomically: one transaction
// that loads the pool, folds its log, and inserts the spend — so two
// tabs spending the same inspiration cannot both succeed. A character
// with no inspiration pool (never awarded) or an empty one is refused
// before anything is written.
func (s *Store) TrySpendInspiration(ctx context.Context, campaignID, entityID, note, actor string) (*TxnRow, error) {
	if err := s.characterExists(ctx, campaignID, entityID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("spend inspiration: %w", err)
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx,
		`SELECT `+poolCols+` FROM resource_pools
		  WHERE entity_id = ? AND campaign_id = ? AND kind = ? AND name = ?`,
		entityID, campaignID, KindFeature, "inspiration")
	p, err := scanPool(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s has never been awarded inspiration", campaign.ErrInvalid, entityID)
	}
	if err != nil {
		return nil, fmt.Errorf("spend inspiration: %w", err)
	}
	balance, err := balanceOf(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	t := Transaction{
		PoolID: p.ID, Pool: p.Key(), Kind: TxnSpend, Amount: 1,
		Actor: actor, Note: "inspiration spent",
	}
	if note = strings.TrimSpace(note); note != "" {
		t.Note += " — " + note
	}
	if err := t.Validate(p, balance); err != nil {
		return nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	day, err := currentDayOn(ctx, tx, campaignID)
	if err != nil {
		return nil, err
	}
	t.Day = day
	t.ID = uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO resource_transactions (id, campaign_id, entity_id, pool_id, pool, kind, amount,
		                                   session_event_id, session_id, actor, note, clock_day, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?)`,
		t.ID, campaignID, entityID, p.ID, t.Pool, t.Kind, t.Amount,
		t.Actor, t.Note, t.Day, s.now().UnixMilli()); err != nil {
		return nil, fmt.Errorf("spend inspiration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("spend inspiration: %w", err)
	}
	s.notify(campaignID)
	out := &TxnRow{Transaction: t, CreatedAt: s.now()}
	return out, nil
}

// LinkTxnEvent points one transaction at the session event its change
// rode on — the roll's mirror, written after the roll commits, so the
// ledger's provenance reads the same story the log does. Best-effort by
// design: a failed link never unwinds the spend; the note carries the
// roll id as well.
func (s *Store) LinkTxnEvent(ctx context.Context, txnID, eventID, sessionID string) error {
	txnID = strings.TrimSpace(txnID)
	eventID = strings.TrimSpace(eventID)
	if txnID == "" || eventID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE resource_transactions SET session_event_id = ?, session_id = COALESCE(?, session_id) WHERE id = ?`,
		eventID, nullString(strings.TrimSpace(sessionID)), txnID)
	if err != nil {
		return fmt.Errorf("link transaction event: %w", err)
	}
	return nil
}

// ensureInspirationPool registers the inspiration pool when it does not
// exist, seeding it with an explicit set-to-none transaction so the
// derived balance is zero — a pool is born full, and inspiration is
// held, not owned. Returns the pool's row id.
func (s *Store) ensureInspirationPool(ctx context.Context, campaignID, entityID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM resource_pools
		  WHERE entity_id = ? AND campaign_id = ? AND kind = ? AND name = ?`,
		entityID, campaignID, KindFeature, "inspiration").Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("find inspiration pool: %w", err)
	}
	p := InspirationPool()
	id = uuid.NewString()
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("register inspiration pool: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO resource_pools (id, campaign_id, entity_id, kind, name, label, size, recovery, granularity, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 'dm', ?, ?)`,
		id, campaignID, entityID, p.Kind, p.Name, p.Label, p.Size, p.Recovery, now, now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			// Another award registered it first; spend the row that won.
			var existing string
			if err2 := tx.QueryRowContext(ctx,
				`SELECT id FROM resource_pools
				  WHERE entity_id = ? AND campaign_id = ? AND kind = ? AND name = ?`,
				entityID, campaignID, KindFeature, "inspiration").Scan(&existing); err2 == nil {
				return existing, nil
			}
		}
		return "", fmt.Errorf("register inspiration pool: %w", err)
	}
	day, err := currentDayOn(ctx, tx, campaignID)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO resource_transactions (id, campaign_id, entity_id, pool_id, pool, kind, amount,
		                                   session_event_id, session_id, actor, note, clock_day, created_at)
		VALUES (?, ?, ?, ?, ?, 'set', 0, NULL, NULL, 'system', 'inspiration registered at none — held, not owned', ?, ?)`,
		uuid.NewString(), campaignID, entityID, id, p.Key(), day, now); err != nil {
		return "", fmt.Errorf("seed inspiration pool: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("register inspiration pool: %w", err)
	}
	s.notify(campaignID)
	return id, nil
}
