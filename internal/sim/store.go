package sim

// The simulation tick's rows and orchestration (MAD-367). The pure function
// is sim.go; this file is everything that touches a database:
//
//   - Preview loads the campaign's inputs (snapshot, calendar, plans,
//     schedule), runs the tick, writes one sim_ticks row, and returns the
//     deterministic outcome set — plus, when a model is configured, one
//     optional prose pass over the already-decided outcomes. Nothing the
//     preview computes touches the campaign graph.
//   - StageTick re-derives the outcomes (the tick is re-runnable: the row
//     carries the window, the seed and the snapshot digest, not a cached
//     result), checks the digest still matches, and stages the outcomes as
//     one proposal batch behind the same review gate everything else uses.
//   - FinalizeTickBatch is canon's TickFinalizer: when the batch is decided,
//     the campaign clock moves by exactly the window exactly once, and the
//     row's status lands on applied or discarded.
//
// The clock does not move until the batch is accepted. A preview is a
// question; accepting it is the answer, and it goes through the queue like
// every machine proposal (ADR 3 applied to time itself).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/clock"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
)

// Tick statuses.
const (
	TickPreview   = "preview"
	TickStaged    = "staged"
	TickApplied   = "applied"
	TickDiscarded = "discarded"
)

// Input bounds: a window is at least one day and at most a year.
const (
	MinDays = 1
	MaxDays = 365
)

// TickRow is one sim_ticks row: a preview's identity and where it went.
type TickRow struct {
	ID             string
	CampaignID     string
	FromDay        int64
	ToDay          int64
	Seed           int64
	SnapshotDigest string
	BatchID        string
	Status         string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Preview carries everything a preview response needs: the row, the
// deterministic outcome set, and the optional flavour pass over it (nil when
// no model is configured or the pass failed validation — the tick degrades
// to the deterministic summary, never fails).
type Preview struct {
	Tick    *TickRow
	Result  *Result
	Flavour map[string]string
	Offline bool
}

// Store runs the tick over the shared database handle.
type Store struct {
	db        *sql.DB
	campaigns *campaign.Store
	factions  *faction.Store
	canon     *canon.Store
	now       func() time.Time
}

// New builds a simulation store on an open, migrated database handle with
// the campaign, faction plan and canon stores it orchestrates. The canon
// store may be the offline one (no model): the tick works and reads plainly
// without a key.
func New(db *sql.DB, campaigns *campaign.Store, factions *faction.Store, canonStore *canon.Store) (*Store, error) {
	if db == nil {
		return nil, errors.New("sim: nil database handle")
	}
	if campaigns == nil || factions == nil || canonStore == nil {
		return nil, errors.New("sim: the campaign, faction and canon stores are all required")
	}
	return &Store{db: db, campaigns: campaigns, factions: factions, canon: canonStore, now: time.Now().UTC}, nil
}

/* ---------- loading the inputs ---------- */

// tickInputs is everything Tick consumes, loaded in one place so Preview and
// StageTick derive from exactly the same reads.
type tickInputs struct {
	snapshot *canon.Snapshot
	calendar *clock.Calendar
	plans    []faction.Plan
	entries  []campaign.ScheduledEvent
}

func (s *Store) loadInputs(ctx context.Context, campaignID string) (*tickInputs, error) {
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	cal, _, err := s.campaigns.GetCalendar(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	snap, err := canon.LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		return nil, err
	}
	snap.Clock = c.Clock
	plans, err := s.factions.ListPlans(ctx, campaign.ScopeDM, campaignID)
	if err != nil {
		return nil, err
	}
	entries, err := s.campaigns.ListScheduledEvents(ctx, campaignID, true)
	if err != nil {
		return nil, err
	}
	return &tickInputs{snapshot: snap, calendar: cal, plans: plans, entries: entries}, nil
}

/* ---------- preview ---------- */

// Preview computes one window and records its identity. Days is clamped to
// 1..365. A nil seed derives a deterministic default from the campaign, the
// day and the window, so re-running the same question re-derives the same
// answer. Nothing is written to the campaign graph — a preview is a
// question.
func (s *Store) Preview(ctx context.Context, campaignID string, days int, seed *int64, userID string) (*Preview, error) {
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	if days < MinDays || days > MaxDays {
		return nil, fmt.Errorf("%w: the window is %d days; it must be %d..%d",
			campaign.ErrInvalid, days, MinDays, MaxDays)
	}
	if seed == nil {
		derived := defaultSeed(c.ID, c.Clock, days)
		seed = &derived
	}
	in, err := s.loadInputs(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	res := Tick(in.snapshot, in.calendar, in.plans, in.entries, days, *seed)

	row := &TickRow{
		ID: uuid.NewString(), CampaignID: campaignID,
		FromDay: res.FromDay, ToDay: res.ToDay, Seed: *seed,
		SnapshotDigest: res.Digest, Status: TickPreview,
		CreatedBy: userID,
	}
	now := s.now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO sim_ticks (id, campaign_id, from_day, to_day, seed, snapshot_digest, status, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.CampaignID, row.FromDay, row.ToDay, row.Seed, row.SnapshotDigest,
		row.Status, row.CreatedBy, now, now); err != nil {
		return nil, fmt.Errorf("insert sim tick: %w", err)
	}
	row.CreatedAt, row.UpdatedAt = s.now(), s.now()

	pv := &Preview{Tick: row, Result: &res}
	pv.Flavour, pv.Offline = s.flavour(ctx, &res)
	return pv, nil
}

// defaultSeed derives the deterministic default: the campaign's own
// randomness identity (its calendar seed is its id by default) folded with
// the day and the window, so two DMs asking the same question at the same
// moment of the campaign get the same world.
// DefaultSeed derives the same deterministic default a caller-omitted seed
// falls back to.
func DefaultSeed(campaignID string, fromDay int64, days int) int64 {
	return defaultSeed(campaignID, fromDay, days)
}

func defaultSeed(campaignID string, fromDay int64, days int) int64 {
	h := fnv64(campaignID)
	h = mix(h, uint64(fromDay))
	h = mix(h, uint64(days))
	return int64(h >> 1) // non-negative
}

func fnv64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

func mix(h, v uint64) uint64 {
	h ^= v
	h *= 1099511628211
	return h
}

/* ---------- flavour ---------- */

// flavour is the optional second pass: the model is handed the
// already-decided outcomes and asked for one prose line each, keyed by
// outcome id. It may not add, remove or reorder an outcome — the harness
// validates exactly that — and any failure (no key configured, a garbage
// reply) degrades to nil rather than failing the tick.
func (s *Store) flavour(ctx context.Context, res *Result) (map[string]string, bool) {
	type outcome struct {
		id      string
		summary string
	}
	var outs []outcome
	for i := range res.Plans {
		if !res.Plans[i].Moved {
			continue
		}
		outs = append(outs, outcome{id: planItemID("", res.Plans[i].PlanID), summary: res.Plans[i].Progression.Summary()})
	}
	for _, d := range res.Due {
		outs = append(outs, outcome{id: dueItemID("", d.EntryID, d.Day), summary: fmt.Sprintf("%s happens as scheduled.", d.Name)})
	}
	for _, a := range res.Actions {
		outs = append(outs, outcome{id: actionItemID("", a.NPC), summary: a.Summary})
	}
	for _, c := range res.Consequences {
		outs = append(outs, outcome{id: reactItemID("", c.Reactor, c.PlanID), summary: c.Summary})
	}
	if len(outs) == 0 {
		return nil, true // nothing to narrate; deterministic summary is the whole answer
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].id < outs[j].id })
	fields := make([]canon.FieldSpec, 0, len(outs))
	structure := make([]map[string]string, 0, len(outs))
	for _, o := range outs {
		fields = append(fields, canon.FieldSpec{
			Key: o.id, Required: true, MaxLen: 280,
			Desc: fmt.Sprintf("One sentence of prose for this outcome: %s", o.summary),
		})
		structure = append(structure, map[string]string{"id": o.id, "summary": o.summary})
	}
	gen, err := s.canon.Generate(ctx, canon.GenerateInput{
		System: flavourSystemPrompt,
		Structure: map[string]any{
			"window":   map[string]any{"from_day": res.FromDay, "to_day": res.ToDay},
			"outcomes": structure,
		},
		Fields: fields,
		Note:   "Every key is one already-decided outcome. Write what it looked like in this campaign's own vocabulary; do not invent outcomes, drop outcomes, or change their order of importance.",
	})
	if err != nil {
		return nil, true // degrade: the deterministic summary is the answer
	}
	out := make(map[string]string, len(gen.Values))
	for _, o := range outs {
		if v, ok := gen.Values[o.id].(string); ok && strings.TrimSpace(v) != "" {
			out[o.id] = strings.TrimSpace(v)
		}
	}
	return out, false
}

const flavourSystemPrompt = `You are Grimoire's simulation narrator. You are handed the decided outcomes of a deterministic world simulation — plans that advanced, scheduled events that came due, NPCs acting on their goals, factions reacting — and asked for prose only. You may not add, remove or reorder outcomes, and you may not change what happened: every number, name and state in the input is already law. Write one plain sentence per outcome, concrete and in the campaign's own vocabulary, present tense, no filler, no commentary.`

/* ---------- staging ---------- */

// StageTick turns one preview into a proposal batch: re-derives the outcomes
// from the stored window and seed, refuses if the campaign has changed since
// the preview (the digest no longer matches) or the clock has moved off the
// window's start, and stages the outcomes — the plan transitions, the events
// (dated on the clock), and the facts that publicize publicly visible plan
// moves — as one batch behind the review gate.
func (s *Store) StageTick(ctx context.Context, campaignID, tickID, userID string) (*canon.Batch, *Result, error) {
	row, err := s.tickInCampaign(ctx, tickID, campaignID)
	if err != nil {
		return nil, nil, err
	}
	if row.Status != TickPreview {
		return nil, nil, fmt.Errorf("%w: tick %s is %s; only a preview can be staged",
			campaign.ErrInvalid, tickID, row.Status)
	}
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	if c.Clock != row.FromDay {
		return nil, nil, fmt.Errorf("%w: the clock sits at day %d, not %d where this window began; run a new simulation",
			campaign.ErrInvalid, c.Clock, row.FromDay)
	}
	in, err := s.loadInputs(ctx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	days := int(row.ToDay - row.FromDay)
	res := Tick(in.snapshot, in.calendar, in.plans, in.entries, days, row.Seed)
	if res.Digest != row.SnapshotDigest {
		return nil, nil, fmt.Errorf("%w: the campaign has changed since this preview; run a new simulation",
			campaign.ErrInvalid)
	}
	items := batchItems(&res, "")
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("%w: the window produced no outcomes; there is nothing to stage", campaign.ErrInvalid)
	}
	batch, err := s.canon.StageBatch(ctx, canon.BatchInput{
		CampaignID: campaignID, Source: canon.BatchSourceTick,
		Prompt: fmt.Sprintf("simulation tick %s: day %d -> %d (%dd), seed %d, digest %s | %d entities, %d facts, %d plans, %d schedule entries",
			shortID(row.ID), row.FromDay, row.ToDay, days, row.Seed, row.SnapshotDigest,
			len(in.snapshot.Entities), len(in.snapshot.Facts), len(in.plans), len(in.entries)),
		CreatedBy: userID, Items: items,
	})
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE sim_ticks SET batch_id = ?, status = ?, updated_at = ? WHERE id = ? AND campaign_id = ?`,
		batch.ID, TickStaged, s.now().UnixMilli(), tickID, campaignID); err != nil {
		return nil, nil, fmt.Errorf("stage sim tick: %w", err)
	}
	return batch, &res, nil
}

/* ---------- completion: canon.TickFinalizer ---------- */

// FinalizeTickBatch completes a decided tick batch: the one write the batch
// cannot carry itself. An accepted (or partially accepted) batch moves the
// campaign clock by exactly the window, once — reason 'tick' — and the row
// lands on applied; a dismissed batch lands on discarded and time does not
// pass. Idempotent: a row not in staged status is a no-op, so a failed
// completion heals on the retry a decided batch allows.
func (s *Store) FinalizeTickBatch(ctx context.Context, batch *canon.Batch) error {
	if batch == nil || batch.Source != canon.BatchSourceTick {
		return nil
	}
	var (
		id     string
		from   int64
		to     int64
		status string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, from_day, to_day, status FROM sim_ticks WHERE batch_id = ? AND campaign_id = ?`,
		batch.ID, batch.CampaignID).Scan(&id, &from, &to, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // a foreign tick-source batch; nothing of ours to finish
	}
	if err != nil {
		return fmt.Errorf("load sim tick: %w", err)
	}
	if status != TickStaged {
		return nil
	}
	next := TickDiscarded
	if batch.Status == canon.BatchAccepted || batch.Status == canon.BatchPartiallyAccepted {
		delta := to - from
		note := fmt.Sprintf("simulation tick %s (day %d -> %d)", shortID(id), from, to)
		if _, _, err := s.campaigns.AdvanceClockBy(ctx, batch.CampaignID, delta,
			campaign.AdvanceTick, note, "", batch.CreatedBy); err != nil {
			return fmt.Errorf("advance clock: %w", err)
		}
		next = TickApplied
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sim_ticks SET status = ?, updated_at = ? WHERE id = ? AND campaign_id = ? AND status = ?`,
		next, s.now().UnixMilli(), id, batch.CampaignID, TickStaged); err != nil {
		return fmt.Errorf("finish sim tick: %w", err)
	}
	return nil
}

/* ---------- reads ---------- */

const tickCols = `id, campaign_id, from_day, to_day, seed, snapshot_digest,
                  COALESCE(batch_id, ''), status, created_by, created_at, updated_at`

func scanTick(row interface{ Scan(...any) error }) (*TickRow, error) {
	var (
		t       TickRow
		created int64
		updated int64
	)
	if err := row.Scan(&t.ID, &t.CampaignID, &t.FromDay, &t.ToDay, &t.Seed, &t.SnapshotDigest,
		&t.BatchID, &t.Status, &t.CreatedBy, &created, &updated); err != nil {
		return nil, err
	}
	t.CreatedAt = time.UnixMilli(created).UTC()
	t.UpdatedAt = time.UnixMilli(updated).UTC()
	return &t, nil
}

func (s *Store) tickInCampaign(ctx context.Context, id, campaignID string) (*TickRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+tickCols+` FROM sim_ticks WHERE id = ? AND campaign_id = ?`, id, campaignID)
	t, err := scanTick(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: tick %s", campaign.ErrNotFound, id)
	}
	return t, err
}

// Ticks lists a campaign's ticks, newest first.
func (s *Store) Ticks(ctx context.Context, campaignID string) ([]TickRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+tickCols+` FROM sim_ticks WHERE campaign_id = ? ORDER BY created_at DESC, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list sim ticks: %w", err)
	}
	defer rows.Close()
	var out []TickRow
	for rows.Next() {
		t, err := scanTick(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

/* ---------- the batch items ---------- */

func planItemID(prefix, planID string) string     { return prefix + "plan-" + planID }
func planMoveItemID(prefix, planID string) string { return prefix + "planmove-" + planID }
func planFactItemID(prefix, planID string) string { return prefix + "planfact-" + planID }
func dueItemID(prefix, entryID string, day int64) string {
	return fmt.Sprintf("%sdue-%s-%d", prefix, entryID, day)
}
func actionItemID(prefix, npc string) string { return prefix + "npcact-" + npc }
func reactItemID(prefix, reactor, planID string) string {
	return prefix + "react-" + reactor + "-" + planID
}

// BatchItems turns the tick's outcomes into one proposal batch's items,
// under a caller-chosen item-id prefix ("" for the tick's own staging;
// internal/downtime prefixes its copy so the two never share dedup keys).
// See the unexported batchItems for the item graph's shape.
func BatchItems(res *Result, prefix string) []canon.BatchItemInput {
	return batchItems(res, prefix)
}

// batchItems turns the tick's outcomes into one proposal batch's items:
//
//   - every moved plan gets an event (dated on the clock), the plan
//     transition itself (the whole advance — state, carried progress, the
//     moves and the arithmetic — atomic per plan), and, when the plan is
//     public, the fact that makes the move publicly visible;
//   - every due occurrence becomes an event dated on its day;
//   - every NPC action becomes an event on its seeded day, dependent on the
//     plan move that triggered it;
//   - every consequence becomes an event on its seeded day, dependent on the
//     public fact it reacts to.
//
// depends_on is the causality: dismissing a plan move refuses its NPC
// actions, dismissing the public fact refuses the reactions — MAD-359's
// cascade, consumed as designed.
func batchItems(res *Result, prefix string) []canon.BatchItemInput {
	var items []canon.BatchItemInput
	for i := range res.Plans {
		pa := &res.Plans[i]
		if !pa.Moved {
			continue
		}
		pr := pa.Progression
		summary := fmt.Sprintf("%s: %s", pa.Name, pr.Summary())
		items = append(items, canon.BatchItemInput{
			ID: planItemID(prefix, pa.PlanID), Kind: "event",
			Subject: fmt.Sprintf("%s: %s advances", pa.FactionName, pa.Name),
			Summary: summary,
			Payload: map[string]any{
				"local_id": planItemID(prefix, pa.PlanID),
				"summary":  summary,
				"clock_at": res.ToDay,
				"participants": []map[string]any{
					{"entity": pa.FactionEntity, "role": "faction"},
				},
			},
		})
		payload := map[string]any{
			"plan_id":       pa.PlanID,
			"from_state":    pr.FromState,
			"to_state":      pr.ToState,
			"from_progress": pr.FromProgress,
			"to_progress":   pr.ToProgress,
			"days":          pr.Days,
			"gain":          pr.Gain,
			"clock_day":     res.ToDay,
			"event":         planItemID(prefix, pa.PlanID),
		}
		if pr.Halted != "" {
			payload["halted"] = pr.Halted
		}
		if len(pr.Moves) > 0 {
			moves := make([]map[string]any, 0, len(pr.Moves))
			for _, m := range pr.Moves {
				moves = append(moves, map[string]any{"to": m.To, "carry": m.Carry})
			}
			payload["moves"] = moves
		}
		if len(pr.Terms) > 0 {
			terms := make([]map[string]any, 0, len(pr.Terms))
			for _, t := range pr.Terms {
				terms = append(terms, map[string]any{"label": t.Label, "delta": t.Delta, "reason": t.Reason})
			}
			payload["terms"] = terms
		}
		items = append(items, canon.BatchItemInput{
			ID: planMoveItemID(prefix, pa.PlanID), Kind: "plan_transition",
			Subject:   fmt.Sprintf("%s: %s — %s -> %s", pa.FactionName, pa.Name, pr.FromState, pr.ToState),
			Summary:   summary,
			Payload:   payload,
			DependsOn: []string{planItemID(prefix, pa.PlanID)},
		})
		if pa.Advanced.Visibility == campaign.VisibilityPublic {
			statement := fmt.Sprintf("%s advances its plan to %s.", pa.FactionName, pr.ToState)
			items = append(items, canon.BatchItemInput{
				ID: planFactItemID(prefix, pa.PlanID), Kind: "fact",
				Subject: fmt.Sprintf("%s's plan reaches %s", pa.FactionName, pr.ToState),
				Summary: statement,
				Payload: map[string]any{
					"local_id":       planFactItemID(prefix, pa.PlanID),
					"statement":      statement,
					"subject":        pa.FactionEntity,
					"predicate":      "plan_reached",
					"object_literal": pr.ToState,
					"visibility":     campaign.VisibilityPublic,
				},
				DependsOn: []string{planMoveItemID(prefix, pa.PlanID)},
			})
		}
	}
	for _, d := range res.Due {
		summary := fmt.Sprintf("%s happens as scheduled.", d.Name)
		payload := map[string]any{
			"local_id": dueItemID(prefix, d.EntryID, d.Day),
			"summary":  summary,
			"clock_at": d.Day,
		}
		if d.Entity != "" {
			payload["participants"] = []map[string]any{{"entity": d.Entity, "role": "whose routine"}}
		}
		if d.Location != "" {
			payload["location"] = d.Location
		}
		subject := d.Name
		if d.Secret {
			subject += " (secret)"
		}
		items = append(items, canon.BatchItemInput{
			ID: dueItemID(prefix, d.EntryID, d.Day), Kind: "event",
			Subject: subject, Summary: summary, Payload: payload,
		})
	}
	for _, a := range res.Actions {
		items = append(items, canon.BatchItemInput{
			ID: actionItemID(prefix, a.NPC), Kind: "event",
			Subject: fmt.Sprintf("%s acts", a.NPCName),
			Summary: a.Summary,
			Payload: map[string]any{
				"local_id": actionItemID(prefix, a.NPC),
				"summary":  a.Summary,
				"clock_at": a.Day,
				"participants": []map[string]any{
					{"entity": a.NPC, "role": "actor"},
				},
			},
			DependsOn: []string{planMoveItemID(prefix, a.TriggerPlanID)},
		})
	}
	for _, c := range res.Consequences {
		items = append(items, canon.BatchItemInput{
			ID: reactItemID(prefix, c.Reactor, c.PlanID), Kind: "event",
			Subject: fmt.Sprintf("%s reacts", c.ReactorName),
			Summary: c.Summary,
			Payload: map[string]any{
				"local_id": reactItemID(prefix, c.Reactor, c.PlanID),
				"summary":  c.Summary,
				"clock_at": c.Day,
				"participants": []map[string]any{
					{"entity": c.Reactor, "role": "faction"},
				},
			},
			DependsOn: []string{planFactItemID(prefix, c.PlanID)},
		})
	}
	return items
}

// shortID renders a tick id for human-readable notes without leaking the
// whole uuid into the ledger prose.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
