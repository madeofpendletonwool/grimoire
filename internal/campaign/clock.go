package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/madeofpendletonwool/grimoire/internal/clock"
)

// The campaign clock (MAD-365). Three things live here: the calendar a
// campaign reckons time by, the advance ledger that makes campaigns.clock a
// cached head rather than a settable integer, and the schedule. The pure
// arithmetic — day↔date, recurrence expansion, weather — is internal/clock;
// this file owns the rows.

/* ---------- vocabularies ---------- */

// Reasons a clock advances. 'manual' is what a plain PATCH on the campaign
// records; the others are the features that move time deliberately. 'rest'
// is the resource ledger's long rest (MAD-419): the day the party slept
// through, recorded like any other move.
const (
	AdvanceTravel   = "travel"
	AdvanceDowntime = "downtime"
	AdvanceSession  = "session"
	AdvanceTick     = "tick"
	AdvanceRest     = "rest"
	AdvanceManual   = "manual"
)

// Scheduled-event status. 'missed' is what integrity calls a pending entry
// the clock has passed; setting it is a DM decision, not automatic.
const (
	SchedulePending   = "pending"
	ScheduleFired     = "fired"
	ScheduleCancelled = "cancelled"
	ScheduleMissed    = "missed"
)

var validAdvanceReasons = map[string]bool{
	AdvanceTravel: true, AdvanceDowntime: true, AdvanceSession: true,
	AdvanceTick: true, AdvanceRest: true, AdvanceManual: true,
}

var validScheduleStatuses = map[string]bool{
	SchedulePending: true, ScheduleFired: true, ScheduleCancelled: true, ScheduleMissed: true,
}

/* ---------- the calendar ---------- */

// GetCalendar returns the campaign's calendar and weather seed. A campaign
// with no row yet gets the default Common Reckoning and its own id as seed —
// the calendar editor's starting point, not a special case callers branch on.
func (s *Store) GetCalendar(ctx context.Context, campaignID string) (*clock.Calendar, string, error) {
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, "", err
	}
	var definition, seed string
	err := s.db.QueryRowContext(ctx,
		`SELECT definition, seed FROM campaign_calendars WHERE campaign_id = ?`, campaignID).
		Scan(&definition, &seed)
	if errors.Is(err, sql.ErrNoRows) {
		return clock.Default(), campaignID, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("load calendar: %w", err)
	}
	cal := &clock.Calendar{}
	if err := json.Unmarshal([]byte(definition), cal); err != nil {
		// A stored definition that no longer parses is data loss, not a
		// defaulting opportunity; surface it.
		return nil, "", fmt.Errorf("decode calendar: %w", err)
	}
	if cal.Months == nil || len(cal.Months) == 0 {
		return nil, "", fmt.Errorf("stored calendar for campaign %s has no months", campaignID)
	}
	if seed == "" {
		seed = campaignID
	}
	return cal, seed, nil
}

// PutCalendar validates and stores a campaign's calendar. An empty seed keeps
// the current one (or defaults to the campaign id for a first save): changing
// it is the recorded decision that re-rolls the weather. Validation failures
// are re-wrapped as this package's ErrInvalid so the API maps them to 400s.
func (s *Store) PutCalendar(ctx context.Context, campaignID string, cal *clock.Calendar, seed string) (*clock.Calendar, string, error) {
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, "", err
	}
	if err := cal.Validate(); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if strings.TrimSpace(seed) == "" {
		if _, existing, err := s.GetCalendar(ctx, campaignID); err == nil {
			seed = existing
		} else {
			return nil, "", err
		}
	}
	definition, err := json.Marshal(cal)
	if err != nil {
		return nil, "", fmt.Errorf("encode calendar: %w", err)
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO campaign_calendars (campaign_id, definition, epoch_label, seed, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(campaign_id) DO UPDATE SET
			definition = excluded.definition, epoch_label = excluded.epoch_label,
			seed = excluded.seed, updated_at = excluded.updated_at`,
		campaignID, string(definition), cal.EpochLabel, seed, now); err != nil {
		return nil, "", fmt.Errorf("store calendar: %w", err)
	}
	return cal, seed, nil
}

/* ---------- the ledger ---------- */

// Advance is one recorded movement of the clock. from_day..to_day is the span
// it covered; to_day may be less than from_day (a DM fixing a typo), recorded
// exactly like a forward move.
type Advance struct {
	ID         string
	CampaignID string
	FromDay    int64
	ToDay      int64
	Reason     string
	Note       string
	SessionID  string
	CreatedBy  string
	CreatedAt  time.Time
}

// AdvanceClock moves the campaign clock to toDay and records the movement.
// campaigns.clock and the clock_advances head are written in one transaction,
// so they cannot disagree. Any toDay — backwards included — is legal; the
// ledger is the point.
func (s *Store) AdvanceClock(ctx context.Context, campaignID string, toDay int64, reason, note, sessionID, userID string) (*Advance, *Campaign, error) {
	if !validAdvanceReasons[reason] {
		return nil, nil, fmt.Errorf("%w: advance reason %q", ErrInvalid, reason)
	}
	current, err := currentClockOn(ctx, s.db, campaignID)
	if err != nil {
		return nil, nil, err
	}
	return s.advance(ctx, campaignID, toDay-current, reason, note, sessionID, userID)
}

// AdvanceClockBy moves the clock a relative number of days (negative allowed)
// and records the movement, reading the current head inside the same
// transaction — the shape travel and downtime want.
func (s *Store) AdvanceClockBy(ctx context.Context, campaignID string, delta int64, reason, note, sessionID, userID string) (*Advance, *Campaign, error) {
	if !validAdvanceReasons[reason] {
		return nil, nil, fmt.Errorf("%w: advance reason %q", ErrInvalid, reason)
	}
	return s.advance(ctx, campaignID, delta, reason, note, sessionID, userID)
}

func (s *Store) advance(ctx context.Context, campaignID string, delta int64, reason, note, sessionID, userID string) (*Advance, *Campaign, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("advance tx: %w", err)
	}
	defer tx.Rollback()
	current, err := currentClockOn(ctx, tx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	toDay := current + delta
	if sessionID != "" {
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM game_sessions WHERE id = ? AND campaign_id = ?`, sessionID, campaignID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("check session: %w", err)
		}
	}
	adv := &Advance{
		ID: uuid.NewString(), CampaignID: campaignID,
		FromDay: current, ToDay: toDay, Reason: reason, Note: note,
		SessionID: sessionID, CreatedBy: userID, CreatedAt: time.Now().UTC(),
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO clock_advances (id, campaign_id, from_day, to_day, reason, note, session_id, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		adv.ID, adv.CampaignID, adv.FromDay, adv.ToDay, adv.Reason, adv.Note,
		nullString(adv.SessionID), adv.CreatedBy, adv.CreatedAt.UnixMilli()); err != nil {
		return nil, nil, fmt.Errorf("insert advance: %w", err)
	}
	c, err := s.updateCampaignClockTx(ctx, tx, campaignID, toDay)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("advance commit: %w", err)
	}
	return adv, c, nil
}

// currentClockOn reads the campaign's clock over a runner (*sql.DB or *sql.Tx).
func currentClockOn(ctx context.Context, q dbRunner, campaignID string) (int64, error) {
	var current int64
	err := q.QueryRowContext(ctx,
		`SELECT clock FROM campaigns WHERE id = ?`, campaignID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: campaign %s", ErrNotFound, campaignID)
	}
	if err != nil {
		return 0, fmt.Errorf("read clock: %w", err)
	}
	return current, nil
}

// updateCampaignClockTx rewrites a campaign row inside a caller-owned
// transaction with a new clock value and refreshed updated_at, returning the
// re-read campaign.
func (s *Store) updateCampaignClockTx(ctx context.Context, tx *sql.Tx, campaignID string, toDay int64) (*Campaign, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE campaigns SET clock = ?, updated_at = ? WHERE id = ?`,
		toDay, time.Now().UTC().UnixMilli(), campaignID); err != nil {
		return nil, fmt.Errorf("update clock: %w", err)
	}
	row := tx.QueryRowContext(ctx, `SELECT `+campaignCols+` FROM campaigns WHERE id = ?`, campaignID)
	c, err := scanCampaign(row)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ClockLedger returns the campaign's advances, most recent first — the head
// of the ledger. DM-scope reads only: the ledger is the DM's account of how
// time moved. Ordering is by insertion (rowid): several advances can land in
// the same millisecond, and a timestamp alone would order them arbitrarily.
func (s *Store) ClockLedger(ctx context.Context, scope Scope, campaignID string, limit int) ([]Advance, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, campaign_id, from_day, to_day, reason, note, session_id, created_by, created_at
		  FROM clock_advances WHERE campaign_id = ?
		 ORDER BY rowid DESC LIMIT ?`, campaignID, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	defer rows.Close()
	var out []Advance
	for rows.Next() {
		var (
			a       Advance
			session sql.NullString
			created int64
		)
		if err := rows.Scan(&a.ID, &a.CampaignID, &a.FromDay, &a.ToDay, &a.Reason, &a.Note,
			&session, &a.CreatedBy, &created); err != nil {
			return nil, err
		}
		a.SessionID = session.String
		a.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

/* ---------- the schedule ---------- */

// ScheduledEvent is one entry of the campaign's schedule: a festival, a
// ritual on the new moon, an NPC's weekly routine (entity_id set), a caravan
// arrival. Day is on the campaign clock's axis; recurrence expands through
// clock.Due, never here.
type ScheduledEvent struct {
	ID             string
	CampaignID     string
	Name           string
	Detail         string
	Day            int64
	Recurrence     string // none | yearly | monthly | every_n_days
	EveryNDays     int64  // > 0 exactly when Recurrence is every_n_days
	Status         string
	EntityID       string
	LocationEntity string
	Visibility     string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ScheduleInput is the writable half of a schedule entry. Pointers are nil to
// leave a field alone (update); the zero struct is invalid (create requires a
// name and a day).
type ScheduleInput struct {
	Name           *string
	Detail         *string
	Day            *int64
	Recurrence     *string // none | yearly | monthly | every_n_days:<N>
	Status         *string
	EntityID       *string
	LocationEntity *string
	Visibility     *string
}

// CreateScheduledEvent adds an entry to the schedule.
func (s *Store) CreateScheduledEvent(ctx context.Context, campaignID string, in ScheduleInput) (*ScheduledEvent, error) {
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return nil, fmt.Errorf("%w: schedule entry name is required", ErrInvalid)
	}
	if in.Day == nil {
		return nil, fmt.Errorf("%w: schedule entry day is required", ErrInvalid)
	}
	e := &ScheduledEvent{
		ID: uuid.NewString(), CampaignID: campaignID,
		Name: strings.TrimSpace(*in.Name), Day: *in.Day,
		Recurrence: clock.RecurNone, Status: SchedulePending, Visibility: VisibilityPublic,
	}
	if in.Detail != nil {
		e.Detail = *in.Detail
	}
	if err := e.applyRecurrence(in.Recurrence); err != nil {
		return nil, err
	}
	if in.Visibility != nil {
		e.Visibility = *in.Visibility
	}
	if err := s.finishScheduledEvent(ctx, e, in.EntityID, in.LocationEntity); err != nil {
		return nil, err
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	e.CreatedAt, e.UpdatedAt = now, now
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO scheduled_events (id, campaign_id, name, detail, day, recurrence, every_n_days,
		                              status, entity_id, location_entity, visibility, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.CampaignID, e.Name, e.Detail, e.Day, e.Recurrence, e.EveryNDays,
		e.Status, nullString(e.EntityID), nullString(e.LocationEntity), e.Visibility,
		e.CreatedAt.UnixMilli(), e.UpdatedAt.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert schedule entry: %w", err)
	}
	return e, nil
}

// UpdateScheduledEvent patches an entry. nil fields are left alone; a day or
// recurrence change is just an edit — "did it happen" is the status field.
func (s *Store) UpdateScheduledEvent(ctx context.Context, campaignID, id string, in ScheduleInput) (*ScheduledEvent, error) {
	e, err := s.scheduleInCampaign(ctx, id, campaignID)
	if err != nil {
		return nil, err
	}
	if in.Name != nil {
		e.Name = strings.TrimSpace(*in.Name)
	}
	if in.Detail != nil {
		e.Detail = *in.Detail
	}
	if in.Day != nil {
		e.Day = *in.Day
	}
	if err := e.applyRecurrence(in.Recurrence); err != nil {
		return nil, err
	}
	if in.Status != nil {
		e.Status = *in.Status
	}
	if in.Visibility != nil {
		e.Visibility = *in.Visibility
	}
	if err := s.finishScheduledEvent(ctx, e, in.EntityID, in.LocationEntity); err != nil {
		return nil, err
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	e.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE scheduled_events SET name = ?, detail = ?, day = ?, recurrence = ?, every_n_days = ?,
		                            status = ?, entity_id = ?, location_entity = ?, visibility = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ?`,
		e.Name, e.Detail, e.Day, e.Recurrence, e.EveryNDays, e.Status,
		nullString(e.EntityID), nullString(e.LocationEntity), e.Visibility,
		e.UpdatedAt.UnixMilli(), id, campaignID)
	if err != nil {
		return nil, fmt.Errorf("update schedule entry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: schedule entry %s", ErrNotFound, id)
	}
	return e, nil
}

// applyRecurrence decodes a wire recurrence ("every_n_days:7") onto the row's
// normalized fields. A nil pointer leaves them alone; a bad value is
// re-wrapped as this package's ErrInvalid.
func (e *ScheduledEvent) applyRecurrence(in *string) error {
	if in == nil {
		return nil
	}
	kind, n, err := clock.ParseRecurrence(*in)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	e.Recurrence = kind
	e.EveryNDays = n
	return nil
}

// finishScheduledEvent resolves the entity references: entity_id is whose
// routine this is (nil pointers keep current values), location_entity is
// where it happens. Both must live in the same campaign and not be deleted.
func (s *Store) finishScheduledEvent(ctx context.Context, e *ScheduledEvent, entityID, location *string) error {
	resolve := func(current string, in *string, what string) (string, error) {
		if in == nil {
			return current, nil
		}
		id := strings.TrimSpace(*in)
		if id == "" {
			return "", nil
		}
		ent, err := s.entityInCampaign(ctx, id, e.CampaignID)
		if err != nil {
			return "", err
		}
		if ent.Status == StatusDeleted {
			return "", fmt.Errorf("%w: %s entity %s is deleted", ErrInvalid, what, id)
		}
		return id, nil
	}
	var err error
	if e.EntityID, err = resolve(e.EntityID, entityID, "entity"); err != nil {
		return err
	}
	if e.LocationEntity, err = resolve(e.LocationEntity, location, "location"); err != nil {
		return err
	}
	return nil
}

func (e *ScheduledEvent) validate() error {
	if e.Name == "" {
		return fmt.Errorf("%w: schedule entry name is required", ErrInvalid)
	}
	if !validScheduleStatuses[e.Status] {
		return fmt.Errorf("%w: schedule status %q", ErrInvalid, e.Status)
	}
	if !validVisibility[e.Visibility] {
		return fmt.Errorf("%w: schedule visibility %q", ErrInvalid, e.Visibility)
	}
	if e.Recurrence == clock.RecurEveryNDays && e.EveryNDays < 1 {
		return fmt.Errorf("%w: every_n_days needs N ≥ 1", ErrInvalid)
	}
	if e.Recurrence != clock.RecurEveryNDays && e.EveryNDays != 0 {
		return fmt.Errorf("%w: every_n_days is set for recurrence %q", ErrInvalid, e.Recurrence)
	}
	return nil
}

const scheduleCols = `id, campaign_id, name, detail, day, recurrence, every_n_days, status,
                      entity_id, location_entity, visibility, created_at, updated_at`

func scanScheduledEvent(row interface{ Scan(...any) error }) (*ScheduledEvent, error) {
	var (
		e        ScheduledEvent
		entity   sql.NullString
		location sql.NullString
		created  int64
		updated  int64
	)
	if err := row.Scan(&e.ID, &e.CampaignID, &e.Name, &e.Detail, &e.Day, &e.Recurrence, &e.EveryNDays,
		&e.Status, &entity, &location, &e.Visibility, &created, &updated); err != nil {
		return nil, err
	}
	e.EntityID = entity.String
	e.LocationEntity = location.String
	e.CreatedAt = time.UnixMilli(created).UTC()
	e.UpdatedAt = time.UnixMilli(updated).UTC()
	return &e, nil
}

func (s *Store) scheduleInCampaign(ctx context.Context, id, campaignID string) (*ScheduledEvent, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+scheduleCols+` FROM scheduled_events WHERE id = ? AND campaign_id = ?`, id, campaignID)
	e, err := scanScheduledEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: schedule entry %s", ErrNotFound, id)
	}
	return e, err
}

// ListScheduledEvents returns the campaign's schedule, by day then name.
// includeSecret is the caller's scope decision: the DM's read passes true,
// every non-DM surface passes false and secret entries are not merely hidden
// but absent — a player must not learn a secret entry exists.
func (s *Store) ListScheduledEvents(ctx context.Context, campaignID string, includeSecret bool) ([]ScheduledEvent, error) {
	if err := s.campaignExists(ctx, campaignID); err != nil {
		return nil, err
	}
	q := `SELECT ` + scheduleCols + ` FROM scheduled_events WHERE campaign_id = ?`
	if !includeSecret {
		q += ` AND visibility = 'public'`
	}
	q += ` ORDER BY day, name COLLATE NOCASE, id`
	rows, err := s.db.QueryContext(ctx, q, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list schedule: %w", err)
	}
	defer rows.Close()
	var out []ScheduledEvent
	for rows.Next() {
		e, err := scanScheduledEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// DeleteScheduledEvent removes an entry outright. The 'cancelled' status is
// for plans that were real and did not happen; a delete is a mistake being
// unmade, the same rule relationships follow.
func (s *Store) DeleteScheduledEvent(ctx context.Context, campaignID, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM scheduled_events WHERE id = ? AND campaign_id = ?`, id, campaignID)
	if err != nil {
		return fmt.Errorf("delete schedule entry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: schedule entry %s", ErrNotFound, id)
	}
	return nil
}

// ScheduleDue expands the schedule into occurrences inside [from, to) using
// clock.Due — the one expansion in the codebase — and joins the entry names
// back on. Entries with a terminal status never come due; visibility is the
// caller's decision, made on the entries before they reach here.
func (s *Store) ScheduleDue(ctx context.Context, campaignID string, includeSecret bool, from, to int64) ([]DueEntry, error) {
	entries, err := s.ListScheduledEvents(ctx, campaignID, includeSecret)
	if err != nil {
		return nil, err
	}
	cal, _, err := s.GetCalendar(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	pure := make([]clock.Entry, 0, len(entries))
	byID := make(map[string]*ScheduledEvent, len(entries))
	for i := range entries {
		e := &entries[i]
		if e.Status != SchedulePending && e.Status != ScheduleFired {
			continue
		}
		pure = append(pure, clock.Entry{ID: e.ID, Day: e.Day, Recurrence: e.Recurrence, EveryN: e.EveryNDays})
		byID[e.ID] = e
	}
	var out []DueEntry
	for _, occ := range clock.Due(cal, pure, from, to) {
		e := byID[occ.EntryID]
		out = append(out, DueEntry{EntryID: e.ID, Name: e.Name, Day: occ.Day, Visibility: e.Visibility})
	}
	return out, nil
}

// DueEntry is one schedule occurrence joined with its entry's name.
type DueEntry struct {
	EntryID    string
	Name       string
	Day        int64
	Visibility string
}

/* ---------- travel ---------- */

// Route is one leg of a location's travel block: a neighbour, how many days
// it takes, and what kind of ground it is. It lives in the location entity's
// payload as {"travel": {"routes": [...]}} — the graph is on the entities
// themselves, with no coordinates and no invented geography.
type Route struct {
	To      string `json:"to"`
	Days    int64  `json:"days"`
	Terrain string `json:"terrain,omitempty"`
}

// RoutesOf decodes the travel routes a location entity declares. Malformed
// routes are ignored: the payload is free-form JSON, and one bad hand edit
// must not take the travel endpoint down.
func RoutesOf(e *Entity) []Route {
	if e == nil || e.Kind != KindLocation {
		return nil
	}
	raw, ok := e.Payload["travel"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := raw["routes"].([]any)
	if !ok {
		return nil
	}
	var out []Route
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		to, _ := m["to"].(string)
		days, _ := m["days"].(float64)
		if to == "" || days < 0 || days > math.MaxInt32 {
			continue
		}
		terrain, _ := m["terrain"].(string)
		out = append(out, Route{To: to, Days: int64(days), Terrain: terrain})
	}
	return out
}

// ShortestRoute answers "how many days from one location to another" with a
// shortest path by day cost over the locations' declared routes. Routes are
// undirected — a road from A to B is a road — and every cost is positive, so
// Dijkstra is exact. The path is the entity ids walked, from and to included.
// No path exists (or either end is unknown) returns ok=false, and the caller
// asks the DM for a day count instead of guessing one.
func ShortestRoute(locations []Entity, from, to string) (days int64, path []string, ok bool) {
	if from == to {
		return 0, []string{from}, true
	}
	known := map[string]bool{}
	for i := range locations {
		if locations[i].Status != StatusDeleted {
			known[locations[i].ID] = true
		}
	}
	if !known[from] || !known[to] {
		return 0, nil, false
	}
	adj := map[string][]Route{}
	for i := range locations {
		loc := &locations[i]
		if loc.Status == StatusDeleted {
			continue
		}
		for _, r := range RoutesOf(loc) {
			if !known[r.To] || r.Days < 0 {
				continue
			}
			adj[loc.ID] = append(adj[loc.ID], r)
			// Undirected: the reverse edge joins the same two places.
			adj[r.To] = append(adj[r.To], Route{To: loc.ID, Days: r.Days, Terrain: r.Terrain})
		}
	}
	// Dijkstra with a linear min-scan: campaign route graphs are small, and
	// a container heap buys nothing here.
	dist := map[string]int64{from: 0}
	prev := map[string]string{}
	visited := map[string]bool{}
	for {
		best := ""
		var bestDist int64
		for id, d := range dist {
			if visited[id] {
				continue
			}
			if best == "" || d < bestDist {
				best, bestDist = id, d
			}
		}
		if best == "" || best == to {
			break
		}
		visited[best] = true
		for _, r := range adj[best] {
			if visited[r.To] {
				continue
			}
			if d, seen := dist[r.To]; !seen || bestDist+r.Days < d {
				dist[r.To] = bestDist + r.Days
				prev[r.To] = best
			}
		}
	}
	d, reached := dist[to]
	if !reached {
		return 0, nil, false
	}
	for node := to; node != ""; node = prev[node] {
		path = append(path, node)
		if node == from {
			break
		}
	}
	// path was built to→from; reverse it.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return d, path, true
}

// TravelResult is the answer to a travel request: how many days, which
// locations were walked, and where the clock ended up.
type TravelResult struct {
	Days  int64
	Path  []string
	Names []string
	Clock int64
}

// Travel works out the cost of moving between two location entities and
// advances the clock by that many days, reason 'travel'. daysOverride, when
// set, wins over any computed route — the DM saying "the scenic route takes
// twelve" — and is the required answer when the map holds no route at all.
func (s *Store) Travel(ctx context.Context, campaignID, from, to string, daysOverride *int64, userID string) (*TravelResult, error) {
	locations, err := s.ListEntities(ctx, ScopeDM, campaignID, KindLocation)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Entity, len(locations))
	for _, loc := range locations {
		byID[loc.ID] = loc
	}
	fromEnt, ok := byID[from]
	if !ok {
		return nil, fmt.Errorf("%w: location %s", ErrNotFound, from)
	}
	toEnt, ok := byID[to]
	if !ok {
		return nil, fmt.Errorf("%w: location %s", ErrNotFound, to)
	}
	var (
		days int64
		path []string
	)
	routeDays, routePath, routed := ShortestRoute(locations, from, to)
	switch {
	case daysOverride != nil:
		if *daysOverride < 0 {
			return nil, fmt.Errorf("%w: travel days %d", ErrInvalid, *daysOverride)
		}
		days = *daysOverride
		if routed {
			path = routePath
		} else {
			path = []string{from, to}
		}
	case routed:
		days, path = routeDays, routePath
	default:
		return nil, fmt.Errorf("%w: no route between %s and %s; pass days to record the journey's length",
			ErrInvalid, fromEnt.Name, toEnt.Name)
	}
	note := fmt.Sprintf("%s to %s: %s days", fromEnt.Name, toEnt.Name, strconv.FormatInt(days, 10))
	adv, _, err := s.AdvanceClockBy(ctx, campaignID, days, AdvanceTravel, note, "", userID)
	if err != nil {
		return nil, err
	}
	res := &TravelResult{Days: days, Path: path, Names: []string{}, Clock: adv.ToDay}
	for _, id := range path {
		if ent, ok := byID[id]; ok {
			res.Names = append(res.Names, ent.Name)
		} else {
			res.Names = append(res.Names, id)
		}
	}
	return res, nil
}
