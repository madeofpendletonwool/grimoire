package journey

// The journeys rows and orchestration (MAD-375). The pure function is
// journey.go; this file is everything that touches a database:
//
//   - Plan computes the day table once (the seeded roll is a pure function
//     of the snapshot, the route, the density and the seed), optionally
//     runs one prose pass over the already-chosen days (never for density
//     none — the hand-wave costs zero model calls by construction), and
//     writes the journeys row plus its day rows. Nothing else: a planned
//     journey writes no graph rows and does not move the clock.
//   - ResolveDay is the DM's "this day happened at the table" mark, with
//     an optional detail override and the encounter that was actually
//     run.
//   - Resolve stages the journey's outcomes as one proposal batch behind
//     the review gate: the travel event, one event per non-uneventful
//     day, a discovery per rumour day whose rumour carries a fact, and a
//     fact per discovery day. Accepting is what makes it true.
//   - FinalizeJourneyBatch is canon's JourneyFinalizer: when the batch is
//     decided, the campaign clock moves by exactly the journey's days,
//     once — reason 'travel', through the clock_advances ledger — the
//     fact-less rumour days write the same holding a rumour heard in a
//     tavern writes, and the row lands on done.
//
// The clock does not move until the batch is accepted — the same rule the
// tick and downtime follow, ADR 3 applied to a week on the road.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

/* ---------- the rows ---------- */

// JourneyRow is one journeys row: the journey's identity, the route it
// walked, and where its proposal went.
type JourneyRow struct {
	ID         string
	CampaignID string
	FromEntity string
	ToEntity   string
	Route      []Leg
	StartDay   int64
	Days       int64
	Density    string
	Pace       string
	Seed       int64
	Status     string
	SessionID  string
	BatchID    string
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Store plans journeys over the shared database handle.
type Store struct {
	db        *sql.DB
	campaigns *campaign.Store
	canon     *canon.Store
	knowledge *knowledge.Store
	now       func() time.Time
}

// New builds a journey store on an open, migrated database handle with
// the campaign, canon and knowledge stores it orchestrates. The canon
// store may be the offline one: the day table is deterministic, and only
// the optional prose pass needs a model.
func New(db *sql.DB, campaigns *campaign.Store, canonStore *canon.Store, knowledgeStore *knowledge.Store) (*Store, error) {
	if db == nil {
		return nil, errors.New("journey: nil database handle")
	}
	if campaigns == nil || canonStore == nil || knowledgeStore == nil {
		return nil, errors.New("journey: the campaign, canon and knowledge stores are all required")
	}
	return &Store{db: db, campaigns: campaigns, canon: canonStore, knowledge: knowledgeStore, now: time.Now().UTC}, nil
}

/* ---------- plan ---------- */

// Plan records one journey and computes its day table. daysOverride, when
// set, wins over any computed route — the DM saying "the long way round
// takes twelve" — and is the required answer when the map holds no road
// at all. A nil seed derives a deterministic default from the campaign,
// the day, the endpoints, the density and the pace, so re-asking the same
// question re-derives the same table. Nothing is written to the campaign
// graph and the clock does not move: a plan is a question.
func (s *Store) Plan(ctx context.Context, campaignID, from, to string, daysOverride *int64, density, pace string, seed *int64, sessionID, userID string) (*JourneyRow, *Result, error) {
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	if density == "" {
		density = DensityStandard
	}
	if !knownDensity(density) {
		return nil, nil, fmt.Errorf("%w: density %q", campaign.ErrInvalid, density)
	}
	if pace == "" {
		pace = PaceNormal
	}
	if !knownPace(pace) {
		return nil, nil, fmt.Errorf("%w: pace %q", campaign.ErrInvalid, pace)
	}
	if sessionID != "" {
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM game_sessions WHERE id = ? AND campaign_id = ?`, sessionID, campaignID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("%w: session %s", campaign.ErrNotFound, sessionID)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("check session: %w", err)
		}
	}
	in, err := s.loadInputs(ctx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	in.From, in.To, in.DaysOverride = from, to, daysOverride
	in.Density, in.Pace = density, pace
	if seed == nil {
		derived := DefaultSeed(campaignID, c.Clock, from, to, density, pace)
		seed = &derived
	}
	in.Seed = *seed
	res, err := Plan(*in)
	if err != nil {
		return nil, nil, err
	}

	// The prose pass: one line per day the roll already chose, never for
	// density none (that path never reaches here — the day table is empty
	// and the loop below is zero iterations, so no model is called).
	if len(res.DayTable) > 0 {
		s.prose(ctx, res)
	}

	route, err := json.Marshal(res.Route)
	if err != nil {
		return nil, nil, fmt.Errorf("encode route: %w", err)
	}
	row := &JourneyRow{
		ID: uuid.NewString(), CampaignID: campaignID,
		FromEntity: from, ToEntity: to, Route: res.Route,
		StartDay: res.FromDay, Days: res.Days, Density: density, Pace: pace, Seed: *seed,
		Status: StatusPlanned, SessionID: sessionID, CreatedBy: userID,
	}
	now := s.now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO journeys (id, campaign_id, from_entity, to_entity, route, start_day, days,
		                      density, pace, seed, status, session_id, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.CampaignID, row.FromEntity, row.ToEntity, string(route), row.StartDay, row.Days,
		row.Density, row.Pace, row.Seed, row.Status, nullString(row.SessionID), row.CreatedBy, now, now); err != nil {
		return nil, nil, fmt.Errorf("insert journey: %w", err)
	}
	for i := range res.DayTable {
		d := &res.DayTable[i]
		weather, err := json.Marshal(d.Weather)
		if err != nil {
			return nil, nil, fmt.Errorf("encode weather: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO journey_days (journey_id, day_index, clock_day, leg, weather, event_kind,
			                          detail, entity_id, encounter_id, resolved)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
			row.ID, d.Index, d.ClockDay, d.Leg, string(weather), d.EventKind,
			d.Detail, d.EntityID, d.Encounter); err != nil {
			return nil, nil, fmt.Errorf("insert journey day %d: %w", d.Index, err)
		}
	}
	row.CreatedAt, row.UpdatedAt = s.now(), s.now()
	return row, res, nil
}

// DefaultSeed derives the deterministic default a caller-omitted seed
// falls back to: the campaign, the day and the endpoints — the same fold
// the tick uses — mixed with the density and the pace, so two questions
// that differ in the knob differ in their honest dice.
func DefaultSeed(campaignID string, day int64, from, to, density, pace string) int64 {
	h := fnv64(campaignID)
	h = mix(h, uint64(day))
	h = mix(h, fnv64(from))
	h = mix(h, fnv64(to))
	h = mix(h, fnv64(density))
	h = mix(h, fnv64(pace))
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

// loadInputs is everything the pure planner consumes, loaded in one place
// so every journey derives from the same reads the clock's own surfaces
// use.
func (s *Store) loadInputs(ctx context.Context, campaignID string) (*Inputs, error) {
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	cal, weatherSeed, err := s.campaigns.GetCalendar(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	snap, err := canon.LoadSnapshot(ctx, s.db, campaignID)
	if err != nil {
		return nil, err
	}
	snap.Clock = c.Clock
	locations, err := s.campaigns.ListEntities(ctx, campaign.ScopeDM, campaignID, campaign.KindLocation)
	if err != nil {
		return nil, err
	}
	return &Inputs{Snapshot: snap, Calendar: cal, WeatherSeed: weatherSeed, Locations: locations}, nil
}

/* ---------- the prose pass ---------- */

// prose is the optional second pass: the model is handed the days the
// roll already chose and asked for one line each, keyed by day index. It
// may not add, remove or move a day — the harness validates exactly that,
// the keys are the chosen days and nothing else — and any failure (no key
// configured, a garbage reply) degrades to the deterministic detail
// rather than failing the journey.
func (s *Store) prose(ctx context.Context, res *Result) {
	type day struct {
		index   int64
		summary string
	}
	var days []day
	for i := range res.DayTable {
		d := &res.DayTable[i]
		if d.EventKind == EventUneventful {
			continue
		}
		days = append(days, day{index: d.Index, summary: d.Detail})
	}
	if len(days) == 0 {
		return
	}
	fields := make([]canon.FieldSpec, 0, len(days))
	structure := make([]map[string]any, 0, len(days))
	for _, d := range days {
		fields = append(fields, canon.FieldSpec{
			Key: fmt.Sprintf("day-%d", d.index), Required: true, MaxLen: 280,
			Desc: fmt.Sprintf("One sentence of prose for day %d of the journey: %s", d.index+1, d.summary),
		})
		structure = append(structure, map[string]any{"day": d.index, "summary": d.summary})
	}
	gen, err := s.canon.Generate(ctx, canon.GenerateInput{
		System: proseSystemPrompt,
		Structure: map[string]any{
			"journey": map[string]any{
				"from": res.FromName, "to": res.ToName,
				"days": res.Days, "density": res.Density,
			},
			"days_table": structure,
		},
		Fields: fields,
		Note:   "Every key is one already-rolled day of the day table. Write what it looked like in this campaign's own vocabulary; do not invent days, drop days, or move what happens to another day.",
	})
	if err != nil {
		return // degrade: the deterministic detail is the answer
	}
	for i := range res.DayTable {
		d := &res.DayTable[i]
		if v, ok := gen.Values[fmt.Sprintf("day-%d", d.Index)].(string); ok && strings.TrimSpace(v) != "" {
			d.Detail = strings.TrimSpace(v)
		}
	}
}

const proseSystemPrompt = `You are Grimoire's travel narrator. You are handed the decided day table of a deterministic journey planner — which days carry an incident, what each incident is, the weather each day — and asked for prose only. You may not add, remove or reorder days, and you may not change what happens: every name, place and weather in the input is already law. Write one plain sentence per day, concrete and in the campaign's own vocabulary, present tense, no filler, no commentary.`

/* ---------- reads ---------- */

const journeyCols = `id, campaign_id, from_entity, to_entity, route, start_day, days,
                     density, pace, seed, status, COALESCE(session_id, ''), COALESCE(batch_id, ''),
                     created_by, created_at, updated_at`

func scanJourney(row interface{ Scan(...any) error }) (*JourneyRow, error) {
	var (
		j       JourneyRow
		route   string
		created int64
		updated int64
	)
	if err := row.Scan(&j.ID, &j.CampaignID, &j.FromEntity, &j.ToEntity, &route, &j.StartDay, &j.Days,
		&j.Density, &j.Pace, &j.Seed, &j.Status, &j.SessionID, &j.BatchID,
		&j.CreatedBy, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(route), &j.Route); err != nil {
		j.Route = nil
	}
	j.CreatedAt = time.UnixMilli(created).UTC()
	j.UpdatedAt = time.UnixMilli(updated).UTC()
	return &j, nil
}

func (s *Store) journeyInCampaign(ctx context.Context, id, campaignID string) (*JourneyRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+journeyCols+` FROM journeys WHERE id = ? AND campaign_id = ?`, id, campaignID)
	j, err := scanJourney(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: journey %s", campaign.ErrNotFound, id)
	}
	return j, err
}

// Get returns the journey and its day table, days in index order.
func (s *Store) Get(ctx context.Context, campaignID, id string) (*JourneyRow, []DayPlan, error) {
	j, err := s.journeyInCampaign(ctx, id, campaignID)
	if err != nil {
		return nil, nil, err
	}
	days, err := s.days(ctx, id)
	return j, days, err
}

func (s *Store) days(ctx context.Context, journeyID string) ([]DayPlan, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT day_index, clock_day, leg, weather, event_kind, detail, entity_id, encounter_id, resolved
		  FROM journey_days WHERE journey_id = ? ORDER BY day_index`, journeyID)
	if err != nil {
		return nil, fmt.Errorf("read journey days: %w", err)
	}
	defer rows.Close()
	var out []DayPlan
	for rows.Next() {
		var (
			d         DayPlan
			weather   string
			encounter string
			resolved  int
		)
		if err := rows.Scan(&d.Index, &d.ClockDay, &d.Leg, &weather, &d.EventKind,
			&d.Detail, &d.EntityID, &encounter, &resolved); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(weather), &d.Weather)
		d.Encounter = encounter
		d.Resolved = resolved != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// List returns the campaign's journeys, newest first.
func (s *Store) List(ctx context.Context, campaignID string) ([]JourneyRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+journeyCols+` FROM journeys WHERE campaign_id = ? ORDER BY created_at DESC, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list journeys: %w", err)
	}
	defer rows.Close()
	var out []JourneyRow
	for rows.Next() {
		j, err := scanJourney(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

/* ---------- patch ---------- */

// Patch edits a journey's bookkeeping. Status may move to 'abandoned'
// from planned or underway — done is the finalizer's verdict alone — and
// the session binding may be set or cleared. Everything else about a
// journey is the record of what was planned and does not edit.
func (s *Store) Patch(ctx context.Context, campaignID, id string, status, sessionID *string) (*JourneyRow, error) {
	j, err := s.journeyInCampaign(ctx, id, campaignID)
	if err != nil {
		return nil, err
	}
	if status != nil {
		if *status != StatusAbandoned {
			return nil, fmt.Errorf("%w: a journey's status moves to abandoned by the DM, or to done by accepting its proposal — not to %q",
				campaign.ErrInvalid, *status)
		}
		if j.Status == StatusDone || j.Status == StatusAbandoned {
			return nil, fmt.Errorf("%w: journey %s is %s", campaign.ErrInvalid, id, j.Status)
		}
		j.Status = *status
	}
	if sessionID != nil {
		if *sessionID != "" {
			var one int
			err := s.db.QueryRowContext(ctx,
				`SELECT 1 FROM game_sessions WHERE id = ? AND campaign_id = ?`, *sessionID, campaignID).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w: session %s", campaign.ErrNotFound, *sessionID)
			}
			if err != nil {
				return nil, fmt.Errorf("check session: %w", err)
			}
		}
		j.SessionID = *sessionID
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE journeys SET status = ?, session_id = ?, updated_at = ? WHERE id = ? AND campaign_id = ?`,
		j.Status, nullString(j.SessionID), s.now().UnixMilli(), id, campaignID); err != nil {
		return nil, fmt.Errorf("patch journey: %w", err)
	}
	return j, nil
}

/* ---------- day resolve ---------- */

// ResolveDay marks one day as happened at the table: an optional detail
// override (the DM's own account of what the day became) and the id of
// the encounter that was actually run. The first resolved day moves the
// journey to 'underway' — the party is on the road.
func (s *Store) ResolveDay(ctx context.Context, campaignID, id string, dayIndex int64, detail, encounterID *string) (*JourneyRow, *DayPlan, error) {
	j, err := s.journeyInCampaign(ctx, id, campaignID)
	if err != nil {
		return nil, nil, err
	}
	if j.Status == StatusDone || j.Status == StatusAbandoned {
		return nil, nil, fmt.Errorf("%w: journey %s is %s", campaign.ErrInvalid, id, j.Status)
	}
	var (
		kind      string
		day       DayPlan
		weather   string
		encounter string
		resolved  int
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT day_index, clock_day, leg, weather, event_kind, detail, entity_id, encounter_id, resolved
		  FROM journey_days WHERE journey_id = ? AND day_index = ?`, id, dayIndex).
		Scan(&day.Index, &day.ClockDay, &day.Leg, &weather, &kind, &day.Detail, &day.EntityID, &encounter, &resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: journey %s has no day %d", campaign.ErrNotFound, id, dayIndex)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read journey day: %w", err)
	}
	_ = json.Unmarshal([]byte(weather), &day.Weather)
	day.EventKind, day.Encounter, day.Resolved = kind, encounter, resolved != 0
	if day.Resolved {
		return j, &day, nil // already happened; an idempotent re-read
	}
	if detail != nil {
		day.Detail = strings.TrimSpace(*detail)
	}
	if encounterID != nil {
		id := strings.TrimSpace(*encounterID)
		if id != "" {
			var one int
			err := s.db.QueryRowContext(ctx, `SELECT 1 FROM encounters WHERE id = ?`, id).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, fmt.Errorf("%w: encounter %s", campaign.ErrNotFound, id)
			}
			if err != nil {
				return nil, nil, fmt.Errorf("check encounter: %w", err)
			}
		}
		day.Encounter = id
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE journey_days SET detail = ?, encounter_id = ?, resolved = 1 WHERE journey_id = ? AND day_index = ?`,
		day.Detail, day.Encounter, id, dayIndex); err != nil {
		return nil, nil, fmt.Errorf("resolve journey day: %w", err)
	}
	day.Resolved = true
	if j.Status == StatusPlanned {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE journeys SET status = ?, updated_at = ? WHERE id = ? AND campaign_id = ? AND status = ?`,
			StatusUnderway, s.now().UnixMilli(), id, campaignID, StatusPlanned); err != nil {
			return nil, nil, fmt.Errorf("mark journey underway: %w", err)
		}
		j.Status = StatusUnderway
	}
	return j, &day, nil
}

/* ---------- resolve: the batch ---------- */

// Resolve turns a planned journey into one proposal batch: every
// still-unresolved day resolves as rolled, and the outcomes — the travel
// event, one event per non-uneventful day, a discovery per rumour day
// whose rumour carries a fact, a fact per discovery day — go through the
// review gate as one decision, source "journey". Nothing is written to
// the graph and the clock does not move until the batch is accepted.
//
// Refused, with the reason: a journey already done or abandoned, one
// whose batch is still open on the queue, and one whose window the clock
// has left (something else moved time — record a new journey).
func (s *Store) Resolve(ctx context.Context, campaignID, id, userID string) (*canon.Batch, error) {
	j, err := s.journeyInCampaign(ctx, id, campaignID)
	if err != nil {
		return nil, err
	}
	if j.Status == StatusDone || j.Status == StatusAbandoned {
		return nil, fmt.Errorf("%w: journey %s is %s", campaign.ErrInvalid, id, j.Status)
	}
	if j.BatchID != "" {
		var status string
		err := s.db.QueryRowContext(ctx,
			`SELECT status FROM proposal_batches WHERE id = ? AND campaign_id = ?`, j.BatchID, campaignID).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read batch: %w", err)
		}
		if status == canon.BatchOpen {
			return nil, fmt.Errorf("%w: journey %s is already staged; decide its batch first", campaign.ErrInvalid, id)
		}
	}
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	if c.Clock != j.StartDay {
		return nil, fmt.Errorf("%w: the clock sits at day %d, not %d where this journey began; record a new journey",
			campaign.ErrInvalid, c.Clock, j.StartDay)
	}
	in, err := s.loadInputs(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	days, err := s.days(ctx, id)
	if err != nil {
		return nil, err
	}
	// The days still unresolved resolve as rolled — the per-day resolve
	// was the DM's chance to edit; the journey's own resolve is the
	// whole-road decision.
	for _, d := range days {
		if !d.Resolved {
			if _, err := s.db.ExecContext(ctx, `
				UPDATE journey_days SET resolved = 1 WHERE journey_id = ? AND day_index = ?`, id, d.Index); err != nil {
				return nil, fmt.Errorf("resolve journey day %d: %w", d.Index, err)
			}
		}
	}

	batch, err := s.canon.StageBatch(ctx, canon.BatchInput{
		CampaignID: campaignID, Source: canon.BatchSourceJourney,
		Prompt: fmt.Sprintf("journey %s: %s -> %s, %d days at %s density (day %d -> %d, seed %d) | %d event days",
			shortID(j.ID), nameIn(in.Snapshot, j.FromEntity), nameIn(in.Snapshot, j.ToEntity),
			j.Days, j.Density, j.StartDay, j.StartDay+j.Days, j.Seed, countEventDays(days)),
		CreatedBy: userID, Items: batchItems(j, days, in.Snapshot),
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE journeys SET status = ?, batch_id = ?, updated_at = ? WHERE id = ? AND campaign_id = ?`,
		StatusUnderway, batch.ID, s.now().UnixMilli(), id, campaignID); err != nil {
		return nil, fmt.Errorf("stage journey: %w", err)
	}
	return batch, nil
}

func countEventDays(days []DayPlan) int {
	n := 0
	for _, d := range days {
		if d.EventKind != EventUneventful {
			n++
		}
	}
	return n
}

// nameIn renders an entity id as its name from the snapshot, falling
// back to the id.
func nameIn(snap *canon.Snapshot, id string) string {
	for i := range snap.Entities {
		if snap.Entities[i].ID == id {
			return snap.Entities[i].Name
		}
	}
	return id
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

/* ---------- completion: canon.JourneyFinalizer ---------- */

// FinalizeJourneyBatch completes a decided journey batch: the one write
// the batch cannot carry itself. An accepted (or partially accepted)
// batch moves the campaign clock by exactly the journey's days, once —
// reason 'travel', through the clock_advances ledger — writes the
// fact-less rumour holdings the road handed out (the same write a rumour
// heard in a tavern makes), and the row lands on done. A dismissed batch
// puts the row back to planned: the DM records a new journey for a
// different take. Idempotent: a row not underway is a no-op, so a failed
// completion heals on the retry a decided batch allows.
func (s *Store) FinalizeJourneyBatch(ctx context.Context, batch *canon.Batch) error {
	if batch == nil || batch.Source != canon.BatchSourceJourney {
		return nil
	}
	var (
		id      string
		from    string
		to      string
		days    int64
		session sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, from_entity, to_entity, days, session_id FROM journeys WHERE batch_id = ? AND campaign_id = ?`,
		batch.ID, batch.CampaignID).Scan(&id, &from, &to, &days, &session)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // a foreign journey-source batch; nothing of ours to finish
	}
	if err != nil {
		return fmt.Errorf("load journey: %w", err)
	}
	var status string
	if err := s.db.QueryRowContext(ctx,
		`SELECT status FROM journeys WHERE id = ?`, id).Scan(&status); err != nil {
		return fmt.Errorf("read journey status: %w", err)
	}
	if status != StatusUnderway {
		return nil
	}
	next := StatusPlanned
	if batch.Status == canon.BatchAccepted || batch.Status == canon.BatchPartiallyAccepted {
		note := fmt.Sprintf("journey %s: %s to %s (day %d -> %d)",
			shortID(id), nameByID(ctx, s.db, batch.CampaignID, from), nameByID(ctx, s.db, batch.CampaignID, to),
			batchStartDay(ctx, s.db, id), batchStartDay(ctx, s.db, id)+days)
		if _, _, err := s.campaigns.AdvanceClockBy(ctx, batch.CampaignID, days,
			campaign.AdvanceTravel, note, session.String, batch.CreatedBy); err != nil {
			return fmt.Errorf("advance clock: %w", err)
		}
		if err := s.carryRumors(ctx, batch.CampaignID, id); err != nil {
			return fmt.Errorf("carry road rumours: %w", err)
		}
		next = StatusDone
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE journeys SET status = ?, batch_id = ?, updated_at = ? WHERE id = ? AND campaign_id = ? AND status = ?`,
		next, nullStringFor(next, batch.ID), s.now().UnixMilli(), id, batch.CampaignID, StatusUnderway); err != nil {
		return fmt.Errorf("finish journey: %w", err)
	}
	return nil
}

// nullStringFor keeps the batch link on a done journey (the record of
// where its outcomes went) and clears it on a dismissed one (the row goes
// back to planned and may stage again).
func nullStringFor(status, batchID string) any {
	if status == StatusDone {
		return batchID
	}
	return nil
}

// batchStartDay reads the journey's start day for the ledger note.
func batchStartDay(ctx context.Context, db *sql.DB, journeyID string) int64 {
	var start int64
	_ = db.QueryRowContext(ctx, `SELECT start_day FROM journeys WHERE id = ?`, journeyID).Scan(&start)
	return start
}

// nameByID renders an entity id as its name for the ledger note.
func nameByID(ctx context.Context, db *sql.DB, campaignID, id string) string {
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM entities WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&name)
	if err != nil {
		return id
	}
	return name
}

// carryRumors writes the stances the road handed out for the rumour days
// whose rumours carry no fact: the same holding a rumour heard in a
// tavern writes (RumorHeard with the party as knower), because awareness
// cannot express a fact-less belief — the documented limit migration 0024
// set. Rumours with facts wrote their discoveries as batch items; only
// the fact-less arrive here.
func (s *Store) carryRumors(ctx context.Context, campaignID, journeyID string) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT entity_id FROM journey_days WHERE journey_id = ? AND event_kind = ? AND entity_id != ''`,
		journeyID, EventRumor)
	if err != nil {
		return fmt.Errorf("read road rumours: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		var factID sql.NullString
		err := s.db.QueryRowContext(ctx,
			`SELECT fact_id FROM rumors WHERE id = ? AND campaign_id = ?`, id, campaignID).Scan(&factID)
		if errors.Is(err, sql.ErrNoRows) {
			continue // the rumour is gone; the event already carries what was heard
		}
		if err != nil {
			return fmt.Errorf("read rumour %s: %w", id, err)
		}
		if factID.Valid && factID.String != "" {
			continue // its discovery rode the batch
		}
		if _, err := s.knowledge.RumorHeard(ctx, campaignID, id, campaign.PartyKnower, ""); err != nil {
			return fmt.Errorf("hear rumour %s on the road: %w", id, err)
		}
	}
	return nil
}

/* ---------- the batch items ---------- */

func travelItemID() string      { return "trv" }
func dayItemID(i int64) string  { return fmt.Sprintf("trvday-%d", i) }
func findItemID(i int64) string { return fmt.Sprintf("trvfind-%d", i) }
func hearItemID(i int64) string { return fmt.Sprintf("trvhear-%d", i) }

// batchItems turns the journey's outcomes into one proposal batch's
// items:
//
//   - the travel event — the anchor everything else depends on, dated on
//     the clock at the journey's end, the destination its location;
//   - one event per non-uneventful day, dated on the day it happened,
//     dependent on the travel event;
//   - a discovery per rumour day whose rumour carries a fact — awareness
//     for the party and nobody else, the same stance the heard path
//     writes (suspects for a true rumour, believes_false for a false or
//     distorted one), dependent on the day it was heard;
//   - a fact per discovery day — the genuinely new record that the party
//     found the place, public, dependent on the day event.
//
// depends_on is the causality: dismissing a day refuses what it carried,
// MAD-359's cascade consumed as designed.
func batchItems(j *JourneyRow, days []DayPlan, snap *canon.Snapshot) []canon.BatchItemInput {
	items := []canon.BatchItemInput{{
		ID: travelItemID(), Kind: "event",
		Subject: fmt.Sprintf("%s to %s: the journey", nameIn(snap, j.FromEntity), nameIn(snap, j.ToEntity)),
		Summary: fmt.Sprintf("The party travels from %s to %s: %d days.", nameIn(snap, j.FromEntity), nameIn(snap, j.ToEntity), j.Days),
		Payload: map[string]any{
			"local_id": travelItemID(),
			"summary":  fmt.Sprintf("The party travels from %s to %s: %d days.", nameIn(snap, j.FromEntity), nameIn(snap, j.ToEntity), j.Days),
			"clock_at": j.StartDay + j.Days,
			"location": j.ToEntity,
		},
	}}
	for _, d := range days {
		if d.EventKind == EventUneventful {
			continue
		}
		summary := d.Detail
		if summary == "" {
			summary = fmt.Sprintf("Day %d: %s.", d.Index+1, d.EventKind)
		}
		payload := map[string]any{
			"local_id": dayItemID(d.Index),
			"summary":  summary,
			"clock_at": d.ClockDay,
		}
		if d.Leg != "" {
			payload["location"] = d.Leg
		}
		if d.Encounter != "" {
			payload["encounter"] = d.Encounter
		}
		items = append(items, canon.BatchItemInput{
			ID: dayItemID(d.Index), Kind: "event",
			Subject:   fmt.Sprintf("Day %d: %s", d.Index+1, d.EventKind),
			Summary:   summary,
			Payload:   payload,
			DependsOn: []string{travelItemID()},
		})
		switch d.EventKind {
		case EventRumor:
			if r := rumorByID(snap, d.EntityID); r != nil && r.FactID != "" {
				stance := "suspects"
				if r.Truth != campaign.RumorTruthTrue {
					stance = "believes_false"
				}
				items = append(items, canon.BatchItemInput{
					ID: hearItemID(d.Index), Kind: "discovery",
					Subject: fmt.Sprintf("The party hears: %s", r.Statement),
					Summary: fmt.Sprintf("%s — heard on the road", stance),
					Payload: map[string]any{
						"fact":          r.FactID,
						"discovered_by": campaign.PartyKnower,
						"method":        fmt.Sprintf("heard on the road from %s to %s", nameIn(snap, j.FromEntity), nameIn(snap, j.ToEntity)),
						"confidence":    0.6,
						"stance":        stance,
						"since_event":   dayItemID(d.Index),
					},
					DependsOn: []string{dayItemID(d.Index)},
				})
			}
		case EventDiscovery:
			name := nameIn(snap, d.EntityID)
			statement := fmt.Sprintf("The party discovers %s on the road to %s.", name, nameIn(snap, j.ToEntity))
			items = append(items, canon.BatchItemInput{
				ID: findItemID(d.Index), Kind: "fact",
				Subject: fmt.Sprintf("%s found", name),
				Summary: statement,
				Payload: map[string]any{
					"local_id":       findItemID(d.Index),
					"statement":      statement,
					"subject":        d.EntityID,
					"predicate":      "discovered_by",
					"object_literal": "the party on the road",
					"visibility":     campaign.VisibilityPublic,
				},
				DependsOn: []string{dayItemID(d.Index)},
			})
		}
	}
	return items
}

// rumorByID finds a rumour in the DM snapshot by id.
func rumorByID(snap *canon.Snapshot, id string) *campaign.Rumor {
	for i := range snap.Rumors {
		if snap.Rumors[i].ID == id {
			return &snap.Rumors[i]
		}
	}
	return nil
}

/* ---------- small helpers ---------- */

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
