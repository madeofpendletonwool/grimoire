package leveling

// The leveling rows and orchestration (MAD-424). The pure half — the
// diff, the split, the mode config — is leveling.go and config.go; this
// file is everything that touches a database:
//
//   - Award prices an encounter with the builder's own oracle
//     (encounter.Evaluate's raw TotalXP — never the adjusted difficulty
//     number), splits it across the characters that fought, and writes
//     one xp_awards row per character plus the sheet's new XP total. A
//     DM pressing the award button is the confirmation; the rows are the
//     provenance. Milestone campaigns refuse: XP is not their currency.
//   - StageLevelUp / FinalizeLevelUpBatch are the machine path a
//     level-up always takes: computed from the 2014 tables, staged as a
//     canon batch, applied by the finalizer exactly once after the human
//     decision. There is no ungated level-up — a DM's shortcut IS the
//     acceptance, not a bypass.
//   - Reconcile / FinalizeReconcileBatch are the post-session pass: the
//     mechanical log and the sheet state checked against each other, a
//     discrepancy flagged, the correction proposed through the same gate.
//     Nothing auto-applies, ever.
//
// Permission rules live in the server file; this store trusts its
// caller's scope decision and validates shape only.

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
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/progression"
	"github.com/madeofpendletonwool/grimoire/internal/pubsub"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

// Level-up row statuses.
const (
	LevelUpStaged    = "staged"
	LevelUpApplied   = "applied"
	LevelUpDiscarded = "discarded"
)

// AwardRow is one xp_awards row as the reads return it.
type AwardRow struct {
	ID          string    `json:"id"`
	CampaignID  string    `json:"-"`
	EncounterID string    `json:"encounter_id"`
	EntityID    string    `json:"entity_id"`
	Amount      int       `json:"amount"`
	TotalXP     int       `json:"total_xp"`
	XPTotal     int       `json:"xp_total"`
	Actor       string    `json:"actor,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	Note        string    `json:"note,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// LevelUpRow is one level_ups row.
type LevelUpRow struct {
	ID         string    `json:"id"`
	CampaignID string    `json:"-"`
	EntityID   string    `json:"entity_id"`
	Class      string    `json:"class"`
	Subclass   string    `json:"subclass,omitempty"`
	FromLevel  int       `json:"from_level"`
	ToLevel    int       `json:"to_level"`
	Status     string    `json:"status"`
	BatchID    string    `json:"batch_id,omitempty"`
	Actor      string    `json:"actor,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Note       string    `json:"note,omitempty"`
	Diff       Diff      `json:"diff"`
	CreatedAt  time.Time `json:"created_at"`
}

// Store runs the leveling surface over the shared database handle.
type Store struct {
	db        *sql.DB
	campaigns *campaign.Store
	canon     *canon.Store
	ledgers   *ledger.Store
	now       func() time.Time
	broker    *pubsub.Broker
}

// New builds a leveling store on an open, migrated database handle. The
// canon store carries the review gate a level-up stages through; the
// ledger store is the reconciliation pass's read of the mechanical log.
func New(db *sql.DB, campaigns *campaign.Store, canonStore *canon.Store, ledgers *ledger.Store) (*Store, error) {
	if db == nil {
		return nil, errors.New("leveling: nil database handle")
	}
	if campaigns == nil || canonStore == nil || ledgers == nil {
		return nil, errors.New("leveling: the campaign, canon and ledger stores are all required")
	}
	return &Store{db: db, campaigns: campaigns, canon: canonStore, ledgers: ledgers,
		now: func() time.Time { return time.Now().UTC() }}, nil
}

// WithBroker moves the store onto the shared campaign broker: every
// committed award or applied level-up pings the campaign topic.
func (s *Store) WithBroker(b *pubsub.Broker) *Store {
	if b != nil {
		s.broker = b
	}
	return s
}

func (s *Store) notify(campaignID string) {
	if s.broker != nil {
		s.broker.Notify(campaignID)
	}
}

/* ---------- the XP award ---------- */

// AwardResult is what an award wrote: the encounter's raw total (the
// oracle's number), the per-character shares, and the level each
// character's XP now carries.
type AwardResult struct {
	EncounterID string              `json:"encounter_id"`
	TotalXP     int                 `json:"total_xp"`
	Shares      map[string]int      `json:"shares"`
	Awards      []AwardRow          `json:"awards"`
	Progress    []CharacterProgress `json:"progress"`
}

// Award prices one campaign encounter and writes the shares. The total is
// the builder's own raw XP — encounter.Evaluate's TotalXP with every
// monster's XP re-derived from its CR server-side — split across the
// characters named (every live pc when none are). The encounter moves to
// status 'run': the award is the writer that status was reserved for.
func (s *Store) Award(ctx context.Context, campaignID, encounterID string, entityIDs []string, sessionID, note, actor string) (*AwardResult, error) {
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	if ConfigOf(c.Settings[SettingsKey]).Mode == ModeMilestone {
		return nil, fmt.Errorf("%w: %s levels by milestone — there is no XP to award", campaign.ErrInvalid, c.Name)
	}

	enc, err := s.encounter(ctx, campaignID, encounterID)
	if err != nil {
		return nil, err
	}
	var already int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM xp_awards WHERE campaign_id = ? AND encounter_id = ?`,
		campaignID, encounterID).Scan(&already); err != nil {
		return nil, fmt.Errorf("check awards: %w", err)
	}
	if already > 0 {
		return nil, fmt.Errorf("%w: encounter %s has already been awarded", campaign.ErrAlreadyExists, enc.Name)
	}

	// The oracle: raw TotalXP, every monster's XP re-derived from its CR
	// — the same re-derivation the encounter surface performs on write,
	// so a tampered row cannot price an award.
	monsters, err := normalizeMonsters(enc.Monsters)
	if err != nil {
		return nil, err
	}
	total := encounter.Evaluate(enc.Party, monsters).TotalXP
	if total <= 0 {
		return nil, fmt.Errorf("%w: encounter %s carries no monsters to price", campaign.ErrInvalid, enc.Name)
	}

	if len(entityIDs) == 0 {
		ids, err := s.livePCs(ctx, campaignID)
		if err != nil {
			return nil, err
		}
		entityIDs = ids
	}
	entityIDs = dedupe(entityIDs)
	if len(entityIDs) == 0 {
		return nil, fmt.Errorf("%w: an award needs at least one character", campaign.ErrInvalid)
	}
	shares := Split(total, entityIDs)

	now := s.now()
	rows := make([]AwardRow, 0, len(entityIDs))
	for _, eid := range entityIDs {
		if shares[eid] <= 0 {
			continue
		}
		ent, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, eid)
		if err != nil {
			return nil, err
		}
		if ent.Kind != campaign.KindPC {
			return nil, fmt.Errorf("%w: %s is a %s, not a pc", campaign.ErrInvalid, ent.Name, ent.Kind)
		}
		sh, has, err := sheet.FromPayload(ent.Payload)
		if err != nil {
			return nil, fmt.Errorf("%w: %s's sheet does not decode: %v", campaign.ErrInvalid, ent.Name, err)
		}
		if !has {
			return nil, fmt.Errorf("%w: %s carries no typed sheet to hold XP", campaign.ErrInvalid, ent.Name)
		}
		newTotal := sh.XP + shares[eid]
		row := AwardRow{
			ID: uuid.NewString(), CampaignID: campaignID, EncounterID: encounterID,
			EntityID: eid, Amount: shares[eid], TotalXP: total, XPTotal: newTotal,
			Actor: actor, SessionID: strings.TrimSpace(sessionID),
			Note: strings.TrimSpace(note), CreatedAt: now,
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO xp_awards (id, campaign_id, encounter_id, entity_id, amount, total_xp, xp_total, actor, session_id, note, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.ID, row.CampaignID, row.EncounterID, row.EntityID, row.Amount, row.TotalXP,
			row.XPTotal, row.Actor, nullString(row.SessionID), row.Note, row.CreatedAt.UnixMilli()); err != nil {
			return nil, fmt.Errorf("write award: %w", err)
		}
		// The sheet's XP is the sheet's own field: bump it through the
		// payload, validated, and let the derivations follow.
		if err := s.bumpXP(ctx, campaignID, ent, sh, shares[eid]); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE encounters SET status = ?, updated_at = ? WHERE id = ? AND campaign_id = ?`,
		encounter.StatusRun, now.UnixMilli(), encounterID, campaignID); err != nil {
		return nil, fmt.Errorf("mark encounter run: %w", err)
	}

	progress, err := s.progressOf(ctx, campaignID, entityIDs)
	if err != nil {
		return nil, err
	}
	s.notify(campaignID)
	return &AwardResult{
		EncounterID: encounterID, TotalXP: total, Shares: shares,
		Awards: rows, Progress: progress,
	}, nil
}

// bumpXP adds amount to a pc's sheet XP through the entity payload and
// re-syncs the derivations — a DM-confirmed write, visible as the award
// row beside it. The sheet is the caller's read (the award priced it
// already); only the write happens here.
func (s *Store) bumpXP(ctx context.Context, campaignID string, ent *campaign.Entity, sh sheet.Sheet, amount int) error {
	sh.XP += amount
	if _, err := s.campaigns.UpdateEntity(ctx, campaignID, ent.ID, nil, nil, nil, campaign.WithSheet(ent.Payload, sh)); err != nil {
		return fmt.Errorf("write %s's xp: %w", ent.Name, err)
	}
	s.syncDerivations(ctx, campaignID, ent.ID)
	return nil
}

// syncDerivations refreshes the caches a sheet write feeds. Both rebuild
// at boot, so a failure here is reported to the log, never fatal to the
// write — the sheets.go contract.
func (s *Store) syncDerivations(ctx context.Context, campaignID, entityID string) {
	_ = sheet.SyncEntity(ctx, s.campaigns.DB(), campaignID, entityID)
	_ = s.ledgers.SyncEntity(ctx, campaignID, entityID)
}

/* ---------- the staged level-up ---------- */

// StageLevelUp is the level-up path, and the only one: the diff computed
// from the 2014 tables, stored as a staged level_ups row, handed to the
// review gate as one canon batch — one event item per character, the
// summary carrying exactly what the level grants. Nothing mechanical
// runs until the batch is decided. In xp mode the sheet must be carrying
// the XP for the level proposed; milestone mode asks nothing (the DM
// staging the level IS the milestone).
func (s *Store) StageLevelUp(ctx context.Context, campaignID, entityID, class, sessionID, note, userID string) (*LevelUpRow, *canon.Batch, error) {
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	mode := ConfigOf(c.Settings[SettingsKey]).Mode

	ent, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, entityID)
	if err != nil {
		return nil, nil, err
	}
	if ent.Kind != campaign.KindPC {
		return nil, nil, fmt.Errorf("%w: %s is a %s, not a pc", campaign.ErrInvalid, ent.Name, ent.Kind)
	}
	sh, has, err := sheet.FromPayload(ent.Payload)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s's sheet does not decode: %v", campaign.ErrInvalid, ent.Name, err)
	}
	if !has {
		return nil, nil, fmt.Errorf("%w: %s carries no typed sheet to level", campaign.ErrInvalid, ent.Name)
	}

	diff, err := ComputeDiff(DiffInput{Entity: entityID, Name: ent.Name, Sheet: sh, Class: class, Mode: mode})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", campaign.ErrInvalid, err)
	}
	if mode == ModeXP && !EligibleXP(sh, diff) {
		need, _ := progression.XPForLevel(diff.ToTotal)
		return nil, nil, fmt.Errorf("%w: %s has %d XP; level %d needs %d (milestone campaigns level by DM decree)",
			campaign.ErrInvalid, ent.Name, sh.XP, diff.ToTotal, need)
	}

	diffJSON, err := json.Marshal(diff)
	if err != nil {
		return nil, nil, fmt.Errorf("encode diff: %w", err)
	}
	row := &LevelUpRow{
		ID: uuid.NewString(), CampaignID: campaignID, EntityID: entityID,
		Class: diff.Class, Subclass: diff.Subclass,
		FromLevel: diff.FromClass, ToLevel: diff.ToClass,
		Status: LevelUpStaged, Actor: userID,
		SessionID: strings.TrimSpace(sessionID), Note: strings.TrimSpace(note),
		Diff: diff, CreatedAt: s.now(),
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO level_ups (id, campaign_id, entity_id, class, subclass, from_level, to_level, status, diff, batch_id, actor, session_id, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?)`,
		row.ID, row.CampaignID, row.EntityID, row.Class, row.Subclass,
		row.FromLevel, row.ToLevel, row.Status, string(diffJSON),
		row.Actor, nullString(row.SessionID), row.Note, row.CreatedAt.UnixMilli()); err != nil {
		return nil, nil, fmt.Errorf("stage level-up: %w", err)
	}

	summary := fmt.Sprintf("%s %s", ent.Name, Summary(diff))
	itemID := "levelup-" + row.ID
	batch, err := s.canon.StageBatch(ctx, canon.BatchInput{
		CampaignID: campaignID, Source: canon.BatchSourceLevelUp,
		Prompt:    fmt.Sprintf("proposed level-up: %s | %s", summary, strings.TrimSpace(note)),
		CreatedBy: userID,
		Items: []canon.BatchItemInput{{
			ID:      itemID,
			Kind:    "event",
			Subject: fmt.Sprintf("%s reaches level %d", ent.Name, diff.ToTotal),
			Summary: summary,
			Payload: map[string]any{
				"local_id":     itemID,
				"summary":      summary,
				"clock_at":     c.Clock,
				"participants": []map[string]any{{"entity": entityID, "role": "leveling"}},
				"level_up": map[string]any{
					"entity":   entityID,
					"class":    diff.Class,
					"to_level": diff.ToClass,
					"row":      row.ID,
				},
			},
		}},
	})
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE level_ups SET batch_id = ? WHERE id = ?`, batch.ID, row.ID); err != nil {
		return nil, nil, fmt.Errorf("link level-up batch: %w", err)
	}
	row.BatchID = batch.ID
	return row, batch, nil
}

/* ---------- completion: canon.LevelUpFinalizer ---------- */

// FinalizeLevelUpBatch completes a decided level-up batch: the mechanics
// the batch item cannot carry itself. For every accepted (or modified)
// item the diff is recomputed against the CURRENT sheet and applied —
// class level, features, saves, slot maxima, max HP — followed by the
// derivation syncs. Idempotent twice over: a row not staged is a no-op,
// and a sheet already at the proposed class level applies nothing.
func (s *Store) FinalizeLevelUpBatch(ctx context.Context, batch *canon.Batch) error {
	if batch == nil || batch.Source != canon.BatchSourceLevelUp {
		return nil
	}
	row, err := s.levelUpByBatch(ctx, batch.ID, batch.CampaignID)
	if err != nil || row == nil || row.Status != LevelUpStaged {
		return err
	}
	if batch.Status != canon.BatchAccepted && batch.Status != canon.BatchPartiallyAccepted {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE level_ups SET status = ? WHERE id = ? AND status = ?`,
			LevelUpDiscarded, row.ID, LevelUpStaged); err != nil {
			return fmt.Errorf("finish level-up: %w", err)
		}
		return nil
	}
	accepted, err := s.acceptedLevelUps(ctx, batch)
	if err != nil {
		return err
	}
	if len(accepted) == 0 {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE level_ups SET status = ? WHERE id = ? AND status = ?`,
			LevelUpDiscarded, row.ID, LevelUpStaged); err != nil {
			return fmt.Errorf("finish level-up: %w", err)
		}
		return nil
	}

	c, err := s.campaigns.GetCampaign(ctx, batch.CampaignID)
	if err != nil {
		return err
	}
	mode := ConfigOf(c.Settings[SettingsKey]).Mode
	for _, item := range accepted {
		// Recompute against the current sheet: the reviewed diff is
		// evidence, the 2014 tables are the truth, and whatever moved
		// since staging is respected.
		ent, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, batch.CampaignID, item.entity)
		if err != nil {
			return err
		}
		sh, has, err := sheet.FromPayload(ent.Payload)
		if err != nil || !has {
			return fmt.Errorf("finalize level-up: %s's sheet does not decode", ent.Name)
		}
		diff, err := ComputeDiff(DiffInput{Entity: ent.ID, Name: ent.Name, Sheet: sh, Class: item.class, Mode: mode})
		if err != nil {
			return fmt.Errorf("finalize level-up: %w", err)
		}
		next := ApplyDiff(sh, diff)
		if _, err := s.campaigns.UpdateEntity(ctx, batch.CampaignID, ent.ID, nil, nil, nil,
			campaign.WithSheet(ent.Payload, next)); err != nil {
			return fmt.Errorf("apply %s's level-up: %w", ent.Name, err)
		}
		s.syncDerivations(ctx, batch.CampaignID, ent.ID)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE level_ups SET status = ? WHERE id = ? AND status = ?`,
		LevelUpApplied, row.ID, LevelUpStaged); err != nil {
		return fmt.Errorf("finish level-up: %w", err)
	}
	s.notify(batch.CampaignID)
	return nil
}

// acceptedLevelUps reads the decided batch back and returns the level-up
// targets whose items were accepted or modified.
func (s *Store) acceptedLevelUps(ctx context.Context, batch *canon.Batch) ([]struct{ entity, class string }, error) {
	full, err := s.canon.GetBatch(ctx, batch.CampaignID, batch.ID)
	if err != nil {
		return nil, err
	}
	var out []struct{ entity, class string }
	for _, it := range full.Items {
		if it.Status != canon.ReviewAccepted && it.Status != canon.ReviewModified {
			continue
		}
		var payload map[string]any
		if len(it.Detail) > 0 {
			_ = json.Unmarshal([]byte(it.Detail), &payload)
		}
		if lu, ok := payload["level_up"].(map[string]any); ok {
			entity, _ := lu["entity"].(string)
			class, _ := lu["class"].(string)
			if entity != "" && class != "" {
				out = append(out, struct{ entity, class string }{entity, class})
			}
		}
	}
	return out, nil
}

/* ---------- the reconciliation pass ---------- */

// Finding is one discrepancy the pass caught: what drifted, by how much,
// and the correction it proposes. The proposal is data — nothing is
// applied until a human decides the batch.
type Finding struct {
	Entity   string `json:"entity"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // pool_bounds | sheet_xp
	Pool     string `json:"pool,omitempty"`
	Have     int    `json:"have"`
	Proposed int    `json:"proposed"`
	Summary  string `json:"summary"`
}

// ReconcileResult is what one pass produced: the findings in deterministic
// order, and the batch they staged behind — nil when the books agree.
type ReconcileResult struct {
	Findings []Finding    `json:"findings"`
	Batch    *canon.Batch `json:"batch,omitempty"`
}

// Reconcile is the post-session truth pass: the mechanical log and the
// sheet state checked against each other, deterministically, read-only
// until the gate says otherwise. It catches:
//
//   - pool_bounds — a bounded pool whose derived balance sits outside
//     0..size (a spend the fold cannot justify, or a sheet whose maxima
//     shrank below what was already spent); proposes the clamping set.
//   - sheet_xp — a sheet whose XP total is below what the award log says
//     it was given; proposes the awarded total.
//
// Every finding becomes one proposal item. The DM accepts, amends, or
// discards; nothing edits a sheet or writes a transaction until then.
func (s *Store) Reconcile(ctx context.Context, campaignID, userID, note string) (*ReconcileResult, error) {
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	pcIDs, err := s.livePCs(ctx, campaignID)
	if err != nil {
		return nil, err
	}

	var findings []Finding
	for _, eid := range pcIDs {
		ent, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, eid)
		if err != nil {
			return nil, err
		}

		// Pool bounds: the fold, against every pool's grammar.
		balances, err := s.ledgers.Balances(ctx, campaignID, eid)
		if err != nil {
			return nil, err
		}
		for _, b := range balances {
			if !b.Pool.Bounded() {
				continue
			}
			var proposed int
			switch {
			case b.Current < 0:
				proposed = 0
			case b.Current > b.Pool.Size:
				proposed = b.Pool.Size
			default:
				continue
			}
			findings = append(findings, Finding{
				Entity: eid, Name: ent.Name, Kind: "pool_bounds",
				Pool: b.Pool.Key(), Have: b.Current, Proposed: proposed,
				Summary: fmt.Sprintf("%s's %s holds %d outside 0..%d — propose set to %d",
					ent.Name, b.Pool.Key(), b.Current, b.Pool.Size, proposed),
			})
		}

		// XP arithmetic: the sheet against the newest award's recorded
		// total. A sheet above it is a DM's bonus XP — fine; below it,
		// the sheet lost XP the award log says it was given. A character
		// with no awards has no invariant to check.
		var lastTotal sql.NullInt64
		err = s.db.QueryRowContext(ctx,
			`SELECT xp_total FROM xp_awards WHERE campaign_id = ? AND entity_id = ?
			 ORDER BY created_at DESC, rowid DESC LIMIT 1`,
			campaignID, eid).Scan(&lastTotal)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		if err != nil {
			return nil, fmt.Errorf("read last award: %w", err)
		}
		if lastTotal.Valid {
			if sh, has, err := sheet.FromPayload(ent.Payload); err == nil && has && sh.XP < int(lastTotal.Int64) {
				findings = append(findings, Finding{
					Entity: eid, Name: ent.Name, Kind: "sheet_xp",
					Have: sh.XP, Proposed: int(lastTotal.Int64),
					Summary: fmt.Sprintf("%s's sheet carries %d XP; the award log last recorded %d — propose %d",
						ent.Name, sh.XP, lastTotal.Int64, lastTotal.Int64),
				})
			}
		}
	}
	if len(findings) == 0 {
		return &ReconcileResult{Findings: findings}, nil
	}

	// Deterministic order: entity, kind, pool.
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Entity != findings[j].Entity {
			return findings[i].Entity < findings[j].Entity
		}
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Pool < findings[j].Pool
	})

	items := make([]canon.BatchItemInput, 0, len(findings))
	for i, f := range findings {
		id := fmt.Sprintf("reconcile-%d", i)
		payload := map[string]any{
			"local_id":     id,
			"summary":      f.Summary,
			"clock_at":     c.Clock,
			"participants": []map[string]any{{"entity": f.Entity, "role": "corrected"}},
			"reconcile": map[string]any{
				"kind": f.Kind, "entity": f.Entity, "pool": f.Pool, "value": f.Proposed,
			},
		}
		subject := fmt.Sprintf("Reconcile: %s", f.Summary)
		items = append(items, canon.BatchItemInput{
			ID: id, Kind: "event",
			Subject: subject, Summary: f.Summary,
			Payload: payload,
		})
	}
	batch, err := s.canon.StageBatch(ctx, canon.BatchInput{
		CampaignID: campaignID, Source: canon.BatchSourceReconcile,
		Prompt: fmt.Sprintf("post-session reconciliation: %d finding(s) | %s",
			len(findings), strings.TrimSpace(note)),
		CreatedBy: userID, Items: items,
	})
	if err != nil {
		return nil, err
	}
	return &ReconcileResult{Findings: findings, Batch: batch}, nil
}

// FinalizeReconcileBatch completes a decided reconciliation batch: every
// accepted item's correction, applied as the visible, attributed write it
// is — a ledger 'set' for a pool, a sheet XP correction for the XP
// total. Idempotent by construction: both corrections set absolute
// values, so a re-run writes the same number the fold already carries.
func (s *Store) FinalizeReconcileBatch(ctx context.Context, batch *canon.Batch) error {
	if batch == nil || batch.Source != canon.BatchSourceReconcile {
		return nil
	}
	if batch.Status != canon.BatchAccepted && batch.Status != canon.BatchPartiallyAccepted {
		return nil
	}
	full, err := s.canon.GetBatch(ctx, batch.CampaignID, batch.ID)
	if err != nil {
		return err
	}
	for _, it := range full.Items {
		if it.Status != canon.ReviewAccepted && it.Status != canon.ReviewModified {
			continue
		}
		var payload map[string]any
		if len(it.Detail) > 0 {
			_ = json.Unmarshal([]byte(it.Detail), &payload)
		}
		fix, ok := payload["reconcile"].(map[string]any)
		if !ok {
			continue
		}
		entity, _ := fix["entity"].(string)
		kind, _ := fix["kind"].(string)
		value := int(numField(fix["value"]))
		if entity == "" {
			continue
		}
		switch kind {
		case "pool_bounds":
			poolKey, _ := fix["pool"].(string)
			pools, err := s.ledgers.Pools(ctx, batch.CampaignID, entity)
			if err != nil {
				return err
			}
			for _, p := range pools {
				if p.Key() != poolKey {
					continue
				}
				if _, _, err := s.ledgers.Apply(ctx, batch.CampaignID, entity, p.ID,
					ledger.TxnInput{Kind: ledger.TxnSet, Amount: value,
						Note: fmt.Sprintf("reconciliation (reviewed): %s set to %d", poolKey, value)},
					batch.CreatedBy); err != nil {
					return fmt.Errorf("reconcile %s: %w", poolKey, err)
				}
				break
			}
		case "sheet_xp":
			ent, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, batch.CampaignID, entity)
			if err != nil {
				return err
			}
			sh, has, err := sheet.FromPayload(ent.Payload)
			if err != nil || !has {
				return fmt.Errorf("reconcile xp: %s carries no typed sheet", ent.Name)
			}
			if sh.XP != value {
				sh.XP = value
				if _, err := s.campaigns.UpdateEntity(ctx, batch.CampaignID, ent.ID, nil, nil, nil,
					campaign.WithSheet(ent.Payload, sh)); err != nil {
					return fmt.Errorf("reconcile %s's xp: %w", ent.Name, err)
				}
				s.syncDerivations(ctx, batch.CampaignID, ent.ID)
			}
		}
	}
	s.notify(batch.CampaignID)
	return nil
}

/* ---------- reads ---------- */

// CharacterProgress is one pc's place on the advancement ladder.
type CharacterProgress struct {
	EntityID  string `json:"entity_id"`
	Name      string `json:"name"`
	Classes   string `json:"classes,omitempty"`
	Level     int    `json:"level"`
	XP        int    `json:"xp"`
	NextLevel int    `json:"next_level,omitempty"`
	NextAt    int    `json:"next_at,omitempty"`
	Eligible  bool   `json:"eligible"`
}

// PartyProgress lists every live pc's level, XP and next threshold.
func (s *Store) PartyProgress(ctx context.Context, campaignID string) ([]CharacterProgress, error) {
	ids, err := s.livePCs(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	return s.progressOf(ctx, campaignID, ids)
}

func (s *Store) progressOf(ctx context.Context, campaignID string, ids []string) ([]CharacterProgress, error) {
	out := make([]CharacterProgress, 0, len(ids))
	for _, eid := range ids {
		ent, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, campaignID, eid)
		if err != nil {
			return nil, err
		}
		p := CharacterProgress{EntityID: eid, Name: ent.Name}
		if sh, has, err := sheet.FromPayload(ent.Payload); err == nil && has {
			p.Classes = sh.ClassesLabel()
			p.Level = sh.TotalLevel()
			p.XP = sh.XP
			if p.Level < progression.MaxLevel {
				p.NextLevel = p.Level + 1
				if next, err := progression.XPForLevel(p.NextLevel); err == nil {
					p.NextAt = next
					p.Eligible = sh.XP >= next
				}
			} else {
				p.Eligible = false
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Awards lists a campaign's XP awards, newest first.
func (s *Store) Awards(ctx context.Context, campaignID string) ([]AwardRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, campaign_id, encounter_id, entity_id, amount, total_xp, xp_total, actor,
		       COALESCE(session_id, ''), note, created_at
		FROM xp_awards WHERE campaign_id = ? ORDER BY created_at DESC, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list awards: %w", err)
	}
	defer rows.Close()
	var out []AwardRow
	for rows.Next() {
		var (
			r       AwardRow
			created int64
		)
		if err := rows.Scan(&r.ID, &r.CampaignID, &r.EncounterID, &r.EntityID, &r.Amount,
			&r.TotalXP, &r.XPTotal, &r.Actor, &r.SessionID, &r.Note, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// LevelUps lists a campaign's level-up rows, newest first.
func (s *Store) LevelUps(ctx context.Context, campaignID string) ([]LevelUpRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, campaign_id, entity_id, class, subclass, from_level, to_level, status,
		       COALESCE(batch_id, ''), actor, COALESCE(session_id, ''), note, diff, created_at
		FROM level_ups WHERE campaign_id = ? ORDER BY created_at DESC, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list level-ups: %w", err)
	}
	defer rows.Close()
	var out []LevelUpRow
	for rows.Next() {
		var (
			r        LevelUpRow
			diffJSON string
			created  int64
		)
		if err := rows.Scan(&r.ID, &r.CampaignID, &r.EntityID, &r.Class, &r.Subclass,
			&r.FromLevel, &r.ToLevel, &r.Status, &r.BatchID, &r.Actor, &r.SessionID,
			&r.Note, &diffJSON, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(diffJSON), &r.Diff)
		r.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

/* ---------- helpers ---------- */

// encounter loads one campaign-scoped encounter row: party and monsters
// for the oracle.
type encounterRow struct {
	Name     string
	Party    []int
	Monsters []encounter.Monster
}

func (s *Store) encounter(ctx context.Context, campaignID, encounterID string) (*encounterRow, error) {
	var name, partyJSON, monstersJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT name, COALESCE(party, '[]'), COALESCE(monsters, '[]') FROM encounters
		 WHERE id = ? AND campaign_id = ?`, encounterID, campaignID).
		Scan(&name, &partyJSON, &monstersJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: encounter %s", campaign.ErrNotFound, encounterID)
	}
	if err != nil {
		return nil, fmt.Errorf("load encounter: %w", err)
	}
	row := &encounterRow{Name: name}
	_ = json.Unmarshal([]byte(partyJSON), &row.Party)
	if err := json.Unmarshal([]byte(monstersJSON), &row.Monsters); err != nil {
		return nil, fmt.Errorf("%w: encounter %s's monsters do not decode: %v", campaign.ErrInvalid, name, err)
	}
	return row, nil
}

// normalizeMonsters re-derives every monster's XP from its CR — the
// builder's own rule, applied wherever monsters enter a computation.
func normalizeMonsters(in []encounter.Monster) ([]encounter.Monster, error) {
	out := make([]encounter.Monster, 0, len(in))
	for _, m := range in {
		xp, err := encounter.XPForCR(m.CR)
		if err != nil {
			return nil, fmt.Errorf("%w: %s's challenge rating %q: %v", campaign.ErrInvalid, m.Name, m.CR, err)
		}
		m.XP = xp
		out = append(out, m)
	}
	return out, nil
}

// livePCs lists a campaign's pc entity ids, name order.
func (s *Store) livePCs(ctx context.Context, campaignID string) ([]string, error) {
	entities, err := s.campaigns.ListEntities(ctx, campaign.ScopeDM, campaignID, campaign.KindPC)
	if err != nil {
		return nil, err
	}
	type named struct{ id, name string }
	var pcs []named
	for _, e := range entities {
		if e.Status == campaign.StatusDeleted {
			continue
		}
		pcs = append(pcs, named{e.ID, e.Name})
	}
	sort.Slice(pcs, func(i, j int) bool { return pcs[i].name < pcs[j].name })
	out := make([]string, 0, len(pcs))
	for _, p := range pcs {
		out = append(out, p.id)
	}
	return out, nil
}

func (s *Store) levelUpByBatch(ctx context.Context, batchID, campaignID string) (*LevelUpRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, campaign_id, entity_id, class, subclass, from_level, to_level, status,
		       COALESCE(batch_id, ''), actor, COALESCE(session_id, ''), note, diff, created_at
		FROM level_ups WHERE batch_id = ? AND campaign_id = ?`, batchID, campaignID)
	var (
		r        LevelUpRow
		diffJSON string
		created  int64
	)
	if err := row.Scan(&r.ID, &r.CampaignID, &r.EntityID, &r.Class, &r.Subclass,
		&r.FromLevel, &r.ToLevel, &r.Status, &r.BatchID, &r.Actor, &r.SessionID,
		&r.Note, &diffJSON, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // a foreign level-up-source batch; nothing of ours
		}
		return nil, fmt.Errorf("load level-up: %w", err)
	}
	_ = json.Unmarshal([]byte(diffJSON), &r.Diff)
	r.CreatedAt = time.UnixMilli(created).UTC()
	return &r, nil
}

func numField(v any) float64 {
	if n, ok := v.(float64); ok {
		return n
	}
	return 0
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

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
