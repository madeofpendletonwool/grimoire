package downtime

// The downtime rows and orchestration (MAD-368). The pure function is
// downtime.go; this file is everything that touches a database:
//
//   - Request records one question — whose downtime, doing what, about
//     what, over which window, under which seed — computes the
//     deterministic answer once for the caller the DM scope belongs to, and
//     writes one downtime_requests row. Nothing else: a request is a
//     question, and the answer it returns must never cross to a player
//     surface — it names secrets the character has no path to yet.
//   - Stage re-derives the answer (the result is a pure function of the
//     recorded inputs, never cached), checks the digest still matches, and
//     stages one proposal batch: the downtime event, the activity fact, one
//     proposed discovery per finding (awareness for the requesting
//     character and nobody else), and the window's tick outcomes under
//     their own item prefix.
//   - FinalizeDowntimeBatch is canon's DowntimeFinalizer: when the batch is
//     decided, the campaign clock moves by exactly the window exactly once,
//     reason 'downtime', and the row lands on applied or discarded.
//
// The clock does not move until the batch is accepted — the same rule the
// tick follows, ADR 3 applied to the character's own time.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/sim"
)

// Request statuses.
const (
	RequestRecorded  = "requested"
	RequestStaged    = "staged"
	RequestApplied   = "applied"
	RequestDiscarded = "discarded"
)

// Input bounds: the window is a downtime window, at least one day and at
// most a year — the same bounds the tick enforces.
const (
	MinDays = sim.MinDays
	MaxDays = sim.MaxDays
)

// RequestRow is one downtime_requests row: the question's identity and
// where its proposal went.
type RequestRow struct {
	ID             string
	CampaignID     string
	CharacterID    string
	Activity       string // the mapped vocabulary word
	Subject        string // entity id, empty when the activity names none
	ActivityText   string // the free text as given
	Days           int
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

// Store resolves downtime over the shared database handle.
type Store struct {
	db        *sql.DB
	campaigns *campaign.Store
	factions  *faction.Store
	canon     *canon.Store
	now       func() time.Time
}

// New builds a downtime store on an open, migrated database handle with the
// campaign, faction plan and canon stores it orchestrates. The canon store
// may be the offline one: downtime is deterministic and needs no model.
func New(db *sql.DB, campaigns *campaign.Store, factions *faction.Store, canonStore *canon.Store) (*Store, error) {
	if db == nil {
		return nil, errors.New("downtime: nil database handle")
	}
	if campaigns == nil || factions == nil || canonStore == nil {
		return nil, errors.New("downtime: the campaign, faction and canon stores are all required")
	}
	return &Store{db: db, campaigns: campaigns, factions: factions, canon: canonStore, now: time.Now().UTC}, nil
}

/* ---------- loading the inputs ---------- */

// loadInputs is everything Resolve consumes, loaded in one place so Request
// and Stage derive from exactly the same reads the tick's store uses.
func (s *Store) loadInputs(ctx context.Context, campaignID string) (*Inputs, error) {
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
	return &Inputs{Snapshot: snap, Calendar: cal, Plans: plans, Entries: entries}, nil
}

/* ---------- the request ---------- */

// Request records one downtime question and computes its deterministic
// answer. days is clamped to 1..365; a nil seed derives a deterministic
// default from the campaign, the day, the window, the character, the
// activity and the subject, so re-asking the same question re-derives the
// same answer. activity is free text, read through ReadActivity: an
// unmappable activity comes back a *ClarifyError and nothing is recorded.
//
// The returned Result is DM material — it names secrets the character has
// no path to yet. Player-facing surfaces record the request and return the
// row, never the result.
func (s *Store) Request(ctx context.Context, campaignID, characterID, activity, subject string, days int, seed *int64, userID string) (*RequestRow, *Result, error) {
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	if days < MinDays || days > MaxDays {
		return nil, nil, fmt.Errorf("%w: the window is %d days; it must be %d..%d",
			campaign.ErrInvalid, days, MinDays, MaxDays)
	}
	mapped, err := ReadActivity(activity)
	if err != nil {
		return nil, nil, err
	}
	in, err := s.loadInputs(ctx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	in.Character, in.Activity, in.Subject, in.Days = characterID, activity, subject, days
	if seed == nil {
		derived := DefaultSeed(campaignID, c.Clock, days, characterID, mapped, subject)
		seed = &derived
	}
	in.Seed = *seed
	res, err := Resolve(*in)
	if err != nil {
		return nil, nil, err
	}

	row := &RequestRow{
		ID: uuid.NewString(), CampaignID: campaignID,
		CharacterID: characterID, Activity: mapped, Subject: subject,
		ActivityText: activity, Days: days,
		FromDay: res.FromDay, ToDay: res.ToDay, Seed: *seed,
		SnapshotDigest: res.Digest, Status: RequestRecorded,
		CreatedBy: userID,
	}
	now := s.now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO downtime_requests (id, campaign_id, character_id, activity, subject, activity_text,
		                               days, from_day, to_day, seed, snapshot_digest, status, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.CampaignID, row.CharacterID, row.Activity, nullString(row.Subject), row.ActivityText,
		row.Days, row.FromDay, row.ToDay, row.Seed, row.SnapshotDigest,
		row.Status, row.CreatedBy, now, now); err != nil {
		return nil, nil, fmt.Errorf("insert downtime request: %w", err)
	}
	row.CreatedAt, row.UpdatedAt = s.now(), s.now()
	return row, res, nil
}

// DefaultSeed derives the deterministic default a caller-omitted seed falls
// back to: the campaign, the day and the window — the same fold the tick
// uses — mixed with the character, the mapped activity and the subject, so
// two characters asking the same question at the same moment of the
// campaign get different honest dice.
func DefaultSeed(campaignID string, fromDay int64, days int, characterID, activity, subject string) int64 {
	h := fnv64(campaignID)
	h = mix(h, uint64(fromDay))
	h = mix(h, uint64(days))
	h = mix(h, fnv64(characterID))
	h = mix(h, fnv64(activity))
	h = mix(h, fnv64(subject))
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

/* ---------- staging ---------- */

// Stage turns one recorded request into a proposal batch: re-derives the
// answer from the stored window, character, activity, subject and seed,
// refuses if the campaign has changed since the request (the digest no
// longer matches) or the clock has moved off the window's start, and stages
// the outcomes — the downtime event, the activity fact, one proposed
// discovery per finding, and the window's tick outcomes — as one batch
// behind the review gate, source "downtime".
func (s *Store) Stage(ctx context.Context, campaignID, requestID, userID string) (*canon.Batch, *Result, error) {
	row, err := s.requestInCampaign(ctx, requestID, campaignID)
	if err != nil {
		return nil, nil, err
	}
	if row.Status != RequestRecorded {
		return nil, nil, fmt.Errorf("%w: downtime request %s is %s; only a recorded request can be staged",
			campaign.ErrInvalid, requestID, row.Status)
	}
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	if c.Clock != row.FromDay {
		return nil, nil, fmt.Errorf("%w: the clock sits at day %d, not %d where this window began; record a new downtime request",
			campaign.ErrInvalid, c.Clock, row.FromDay)
	}
	in, err := s.loadInputs(ctx, campaignID)
	if err != nil {
		return nil, nil, err
	}
	in.Character, in.Activity, in.Subject = row.CharacterID, row.Activity, row.Subject
	in.Days, in.Seed = row.Days, row.Seed
	res, err := Resolve(*in)
	if err != nil {
		return nil, nil, err
	}
	if res.Digest != row.SnapshotDigest {
		return nil, nil, fmt.Errorf("%w: the campaign has changed since this request; record a new downtime request",
			campaign.ErrInvalid)
	}
	items := batchItems(res)
	items = append(items, sim.BatchItems(res.Tick, "dtk")...)
	subjectName := res.SubjectName
	if subjectName == "" {
		subjectName = "nothing in particular"
	}
	batch, err := s.canon.StageBatch(ctx, canon.BatchInput{
		CampaignID: campaignID, Source: canon.BatchSourceDowntime,
		Prompt: fmt.Sprintf("downtime %s: %s spends %d days %s %s (day %d -> %d, seed %d, digest %s) | %d findings, %d reachable locations",
			shortID(row.ID), res.CharacterName, row.Days, res.Activity, subjectName,
			row.FromDay, row.ToDay, row.Seed, row.SnapshotDigest,
			len(res.Findings), len(res.Reachable)),
		CreatedBy: userID, Items: items,
	})
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE downtime_requests SET batch_id = ?, status = ?, updated_at = ? WHERE id = ? AND campaign_id = ?`,
		batch.ID, RequestStaged, s.now().UnixMilli(), requestID, campaignID); err != nil {
		return nil, nil, fmt.Errorf("stage downtime request: %w", err)
	}
	return batch, res, nil
}

/* ---------- completion: canon.DowntimeFinalizer ---------- */

// FinalizeDowntimeBatch completes a decided downtime batch: the one write
// the batch cannot carry itself. An accepted (or partially accepted) batch
// moves the campaign clock by exactly the window, once — reason 'downtime'
// — and the row lands on applied; a dismissed batch lands on discarded and
// the character's time does not pass. Idempotent: a row not in staged
// status is a no-op, so a failed completion heals on the retry a decided
// batch allows.
func (s *Store) FinalizeDowntimeBatch(ctx context.Context, batch *canon.Batch) error {
	if batch == nil || batch.Source != canon.BatchSourceDowntime {
		return nil
	}
	var (
		id     string
		from   int64
		to     int64
		status string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, from_day, to_day, status FROM downtime_requests WHERE batch_id = ? AND campaign_id = ?`,
		batch.ID, batch.CampaignID).Scan(&id, &from, &to, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // a foreign downtime-source batch; nothing of ours to finish
	}
	if err != nil {
		return fmt.Errorf("load downtime request: %w", err)
	}
	if status != RequestStaged {
		return nil
	}
	next := RequestDiscarded
	if batch.Status == canon.BatchAccepted || batch.Status == canon.BatchPartiallyAccepted {
		delta := to - from
		note := fmt.Sprintf("downtime %s (day %d -> %d)", shortID(id), from, to)
		if _, _, err := s.campaigns.AdvanceClockBy(ctx, batch.CampaignID, delta,
			campaign.AdvanceDowntime, note, "", batch.CreatedBy); err != nil {
			return fmt.Errorf("advance clock: %w", err)
		}
		next = RequestApplied
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE downtime_requests SET status = ?, updated_at = ? WHERE id = ? AND campaign_id = ? AND status = ?`,
		next, s.now().UnixMilli(), id, batch.CampaignID, RequestStaged); err != nil {
		return fmt.Errorf("finish downtime request: %w", err)
	}
	return nil
}

/* ---------- reads ---------- */

const requestCols = `id, campaign_id, character_id, activity,
                     COALESCE(subject, ''), activity_text, days, from_day, to_day, seed, snapshot_digest,
                     COALESCE(batch_id, ''), status, created_by, created_at, updated_at`

func scanRequest(row interface{ Scan(...any) error }) (*RequestRow, error) {
	var (
		r       RequestRow
		created int64
		updated int64
	)
	if err := row.Scan(&r.ID, &r.CampaignID, &r.CharacterID, &r.Activity,
		&r.Subject, &r.ActivityText, &r.Days, &r.FromDay, &r.ToDay, &r.Seed, &r.SnapshotDigest,
		&r.BatchID, &r.Status, &r.CreatedBy, &created, &updated); err != nil {
		return nil, err
	}
	r.CreatedAt = time.UnixMilli(created).UTC()
	r.UpdatedAt = time.UnixMilli(updated).UTC()
	return &r, nil
}

func (s *Store) requestInCampaign(ctx context.Context, id, campaignID string) (*RequestRow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+requestCols+` FROM downtime_requests WHERE id = ? AND campaign_id = ?`, id, campaignID)
	r, err := scanRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: downtime request %s", campaign.ErrNotFound, id)
	}
	return r, err
}

// Requests lists a campaign's downtime requests, newest first.
func (s *Store) Requests(ctx context.Context, campaignID string) ([]RequestRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+requestCols+` FROM downtime_requests WHERE campaign_id = ? ORDER BY created_at DESC, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list downtime requests: %w", err)
	}
	defer rows.Close()
	var out []RequestRow
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

/* ---------- the batch items ---------- */

// The activity fact's predicate, per activity.
var activityPredicates = map[string]string{
	ActivityResearch: "researched", ActivityCraft: "crafted", ActivityTrain: "trained",
	ActivityCarouse: "caroused", ActivityWork: "worked", ActivityTravel: "traveled_to",
	ActivityRecuperate: "recuperated", ActivityScheme: "schemed",
}

func findingItemID(factID string) string { return "dtfind-" + factID }

// batchItems turns the downtime's own outcomes into proposal items:
//
//   - the downtime event — the anchor everything else depends on, dated on
//     the clock at the window's end, the character its participant;
//   - the activity fact — the genuinely new record that the time was spent
//     ("Thalia researches the Cult of the Root for 21 days."), public,
//     dependent on the event;
//   - one proposed discovery per finding — RecordDiscovery through the
//     queue, awareness for the requesting character and nobody else,
//     dependent on the event the discovery happened at.
//
// The window's tick outcomes are appended by the caller under their own
// prefix, so one batch carries both halves of the answer.
func batchItems(res *Result) []canon.BatchItemInput {
	subjectTail := ""
	if res.SubjectName != "" {
		subjectTail = " " + fmt.Sprintf(activityWithSubject[res.Activity], res.SubjectName)
	}
	summary := fmt.Sprintf("%s spends %d days %s%s.", res.CharacterName, res.Days, activityPlain[res.Activity], subjectTail)
	eventPayload := map[string]any{
		"local_id": "dt",
		"summary":  summary,
		"clock_at": res.ToDay,
		"participants": []map[string]any{
			{"entity": res.Character, "role": "actor"},
		},
	}
	if res.Location != "" {
		eventPayload["location"] = res.Location
	}
	items := []canon.BatchItemInput{{
		ID: "dt", Kind: "event",
		Subject: fmt.Sprintf("%s: %s", res.CharacterName, res.Activity), Summary: summary,
		Payload: eventPayload,
	}}
	factPayload := map[string]any{
		"local_id":   "dtfact",
		"statement":  summary,
		"subject":    res.Character,
		"predicate":  activityPredicates[res.Activity],
		"visibility": campaign.VisibilityPublic,
	}
	if res.Subject != "" {
		factPayload["object_entity"] = res.Subject
	}
	if res.TravelDays > 0 {
		factPayload["object_literal"] = fmt.Sprintf("%d days", res.TravelDays)
	}
	items = append(items, canon.BatchItemInput{
		ID: "dtfact", Kind: "fact",
		Subject: summary, Summary: summary,
		Payload: factPayload, DependsOn: []string{"dt"},
	})
	for _, f := range res.Findings {
		items = append(items, canon.BatchItemInput{
			ID: findingItemID(f.FactID), Kind: "discovery",
			Subject: fmt.Sprintf("%s learns: %s", res.CharacterName, f.Statement),
			Summary: fmt.Sprintf("%s — %s", f.Stance, f.Method),
			Payload: map[string]any{
				"fact":          f.FactID,
				"discovered_by": res.Character,
				"method":        f.Method,
				"confidence":    f.Confidence,
				"stance":        f.Stance,
				"since_event":   "dt",
			},
			DependsOn: []string{"dt"},
		})
	}
	return items
}

// shortID renders a request id for human-readable notes without leaking the
// whole uuid into the ledger prose.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
