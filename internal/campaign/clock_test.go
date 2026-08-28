package campaign

import (
	"context"
	"math/rand"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/clock"
)

/* ---------- the ledger ---------- */

// TestAdvanceLedgerNeverDisagrees is the acceptance property: after a random
// sequence of advances — backwards moves included — campaigns.clock and the
// head of clock_advances never disagree.
func TestAdvanceLedgerNeverDisagrees(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 25; i++ {
		// A mix of absolute jumps and relative moves, forward and back.
		var (
			adv *Advance
			got *Campaign
			err error
		)
		if i%2 == 0 {
			to := rng.Int63n(500) - 100
			adv, got, err = s.AdvanceClock(ctx, c.ID, to, AdvanceManual, "", "", "keeper")
		} else {
			by := rng.Int63n(60) - 30
			adv, got, err = s.AdvanceClockBy(ctx, c.ID, by, AdvanceDowntime, "testing", "", "keeper")
		}
		if err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		if got.Clock != adv.ToDay {
			t.Fatalf("advance %d: campaigns.clock is %d, ledger head is %d", i, got.Clock, adv.ToDay)
		}
		// And the stored campaign row agrees too, not just the response.
		stored, err := s.GetCampaign(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Clock != adv.ToDay {
			t.Fatalf("advance %d: stored clock %d, ledger head %d", i, stored.Clock, adv.ToDay)
		}
	}
	ledger, err := s.ClockLedger(ctx, ScopeDM, c.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 25 {
		t.Fatalf("ledger has %d rows, want 25", len(ledger))
	}
	// The newest row is the head.
	head := ledger[0]
	stored, _ := s.GetCampaign(ctx, c.ID)
	if head.ToDay != stored.Clock {
		t.Fatalf("ledger head says day %d, campaign says %d", head.ToDay, stored.Clock)
	}
}

// TestPatchClockRecordsManualAdvance: a PATCH that changes the clock keeps
// working — and records a manual advance. A backwards move is legal and
// recorded rather than rejected; a no-op change records nothing.
func TestPatchClockRecordsManualAdvance(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	forward := int64(40)
	if _, err := s.UpdateCampaign(ctx, "keeper", c.ID, nil, nil, nil, &forward, nil); err != nil {
		t.Fatalf("patch forward: %v", err)
	}
	ledger, err := s.ClockLedger(ctx, ScopeDM, c.ID, 10)
	if err != nil || len(ledger) != 1 {
		t.Fatalf("forward patch must record one advance: %v %v", ledger, err)
	}
	if ledger[0].Reason != AdvanceManual || ledger[0].FromDay != 0 || ledger[0].ToDay != 40 {
		t.Fatalf("manual advance row wrong: %+v", ledger[0])
	}

	back := int64(12)
	if _, err := s.UpdateCampaign(ctx, "keeper", c.ID, nil, nil, nil, &back, nil); err != nil {
		t.Fatalf("patch backwards: %v", err)
	}
	ledger, err = s.ClockLedger(ctx, ScopeDM, c.ID, 10)
	if err != nil || len(ledger) != 2 {
		t.Fatalf("backwards patch must record too: %v %v", ledger, err)
	}
	if ledger[0].ToDay != 12 || ledger[0].FromDay != 40 {
		t.Fatalf("backwards advance row wrong: %+v", ledger[0])
	}
	stored, _ := s.GetCampaign(ctx, c.ID)
	if stored.Clock != 12 {
		t.Fatalf("clock after backwards patch: %d, want 12", stored.Clock)
	}

	same := int64(12)
	if _, err := s.UpdateCampaign(ctx, "keeper", c.ID, nil, nil, nil, &same, nil); err != nil {
		t.Fatalf("patch same: %v", err)
	}
	ledger, _ = s.ClockLedger(ctx, ScopeDM, c.ID, 10)
	if len(ledger) != 2 {
		t.Fatalf("a no-op patch must not append: %d rows", len(ledger))
	}
}

func TestAdvanceRejectsBadReasonAndForeignSession(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)
	if _, _, err := s.AdvanceClock(ctx, c.ID, 10, "teleport", "", "", "keeper"); err == nil {
		t.Fatal("unknown reason must be rejected")
	}
	if _, _, err := s.AdvanceClock(ctx, c.ID, 10, AdvanceSession, "", "no-such-session", "keeper"); err == nil {
		t.Fatal("foreign session must be rejected")
	}
}

/* ---------- the calendar ---------- */

func TestCalendarDefaultsAndPersists(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	// No row yet: the default Common Reckoning, seeded by campaign id.
	cal, seed, err := s.GetCalendar(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cal.Months) != 12 || cal.YearDays(1) != 360 {
		t.Fatalf("default calendar wrong: %d months, %d days", len(cal.Months), cal.YearDays(1))
	}
	if seed != c.ID {
		t.Fatalf("default seed %q, want the campaign id", seed)
	}

	custom := clock.Default()
	custom.Name = "The Wheel"
	custom.Months = []clock.Month{{Name: "Sun", Days: 91}, {Name: "Rain", Days: 120}, {Name: "Harvest", Days: 60}}
	custom.Seasons = []clock.Season{{Name: "green", StartDay: 1, EndDay: 91}}
	custom.EpochLabel = "W"
	stored, seed, err := s.PutCalendar(ctx, c.ID, custom, "")
	if err != nil {
		t.Fatal(err)
	}
	if seed != c.ID {
		t.Fatalf("empty seed must keep the current one, got %q", seed)
	}
	if len(stored.Months) != 3 {
		t.Fatalf("custom calendar did not persist: %+v", stored)
	}

	again, _, err := s.GetCalendar(ctx, c.ID)
	if err != nil || len(again.Months) != 3 || again.Months[1].Name != "Rain" {
		t.Fatalf("re-read after put: %+v %v", again, err)
	}

	// A new seed overwrites; that is the recorded decision that re-rolls.
	if _, seed, _ = s.PutCalendar(ctx, c.ID, custom, "new-weather-seed"); seed != "new-weather-seed" {
		t.Fatalf("seed change did not stick: %q", seed)
	}

	broken := clock.Default()
	broken.Months[0].Days = 0
	if _, _, err := s.PutCalendar(ctx, c.ID, broken, ""); err == nil {
		t.Fatal("an invalid calendar must be rejected")
	}
}

/* ---------- the schedule ---------- */

func mkEntry(name string, day int64) *string { return &name }

func TestScheduleCRUDAndRecurrence(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	day := int64(120)
	name := "The Night Market"
	e, err := s.CreateScheduledEvent(ctx, c.ID, ScheduleInput{
		Name: &name, Day: &day,
		Recurrence: strp("every_n_days:7"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Recurrence != clock.RecurEveryNDays || e.EveryNDays != 7 || e.Status != SchedulePending {
		t.Fatalf("entry wrong: %+v", e)
	}

	// A bad recurrence is a clean error.
	bad := strp("fortnightly")
	if _, err := s.CreateScheduledEvent(ctx, c.ID, ScheduleInput{Name: mkEntry("x", 1), Day: &day, Recurrence: bad}); err == nil {
		t.Fatal("unknown recurrence must be rejected")
	}
	if _, err := s.CreateScheduledEvent(ctx, c.ID, ScheduleInput{Name: mkEntry("x", 1), Day: &day, Recurrence: strp("every_n_days:0")}); err == nil {
		t.Fatal("every_n_days:0 must be rejected")
	}

	// Status moves are the DM recording what happened.
	fired := ScheduleFired
	updated, err := s.UpdateScheduledEvent(ctx, c.ID, e.ID, ScheduleInput{Status: &fired})
	if err != nil || updated.Status != ScheduleFired {
		t.Fatalf("status patch: %+v %v", updated, err)
	}

	if err := s.DeleteScheduledEvent(ctx, c.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteScheduledEvent(ctx, c.ID, e.ID); err == nil {
		t.Fatal("second delete must be not-found")
	}
}

func strp(v string) *string { return &v }

// TestScheduleDueExpandsAndFilters: ScheduleDue is the one expansion, and the
// visibility filter is applied before it — a player's due list never carries
// a secret entry.
func TestScheduleDueExpandsAndFilters(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	mk := func(name string, day int64, rec string, vis string) *ScheduledEvent {
		e, err := s.CreateScheduledEvent(ctx, c.ID, ScheduleInput{
			Name: &name, Day: &day, Recurrence: strp(rec), Visibility: &vis,
		})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	festival := mk("Festival of Lamps", 100, "yearly", "public")
	mk("The Duke's hidden council", 110, "none", "secret")
	market := mk("Night Market", 5, "every_n_days:10", "public")

	dmDue, err := s.ScheduleDue(ctx, c.ID, true, 90, 130)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range dmDue {
		names = append(names, d.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "Festival of Lamps") || !strings.Contains(joined, "Night Market") || !strings.Contains(joined, "hidden council") {
		t.Fatalf("DM due list missing entries: %v", names)
	}
	// The festival lands exactly once in a 40-day window (the clock package
	// test pins the twice-in-400 case).
	once, err := s.ScheduleDue(ctx, c.ID, true, festival.Day, festival.Day+40)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, d := range once {
		if d.EntryID == festival.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("yearly festival due %d times in 40 days, want 1", count)
	}
	_ = market

	playerDue, err := s.ScheduleDue(ctx, c.ID, false, 90, 130)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range playerDue {
		if d.Visibility == VisibilitySecret {
			t.Fatalf("secret entry leaked into the player's due list: %+v", d)
		}
	}
}

/* ---------- integrity: the new checks ---------- */

func TestClockIntegrityChecks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	// Sessions played, clock never advanced.
	if _, err := s.db.Exec(`INSERT INTO game_sessions (id, campaign_id, ordinal, name, status, created_at, updated_at)
		VALUES ('sess-1', ?, 1, 'Session One', 'done', 0, 0)`, c.ID); err != nil {
		t.Fatal(err)
	}

	// An event dated past the clock, and a pending schedule entry behind it.
	if _, err := s.CreateEvent(ctx, c.ID, "", "A vision of tomorrow", intPtr(30), ""); err != nil {
		t.Fatal(err)
	}
	day := int64(5)
	if _, err := s.CreateScheduledEvent(ctx, c.ID, ScheduleInput{Name: mkEntry("The levy arrives", 5), Day: &day}); err != nil {
		t.Fatal(err)
	}

	snap, err := LoadSnapshot(ctx, ScopeDM, s.db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SessionCount != 1 || snap.AdvanceCount != 0 || snap.Clock != 0 {
		t.Fatalf("snapshot clock face wrong: %+v", snap)
	}
	if len(snap.Schedule) != 1 {
		t.Fatalf("snapshot schedule: %+v", snap.Schedule)
	}

	findings := Check(snap)
	got := map[string]Finding{}
	for _, f := range findings {
		got[f.Check] = f
	}
	if f, ok := got[CheckEventAfterClock]; !ok || f.Severity != SeverityWarn {
		t.Fatalf("event_after_clock missing or wrong: %+v", f)
	}
	if f, ok := got[CheckMissedSchedule]; ok {
		t.Fatalf("day 5 pending behind a clock at 0 is due, not missed: %+v", f)
	}
	if f, ok := got[CheckClockNeverAdvanced]; !ok || f.Severity != SeverityInfo {
		t.Fatalf("clock_never_advanced missing or wrong: %+v", f)
	}

	// Advance the clock past the schedule day: the entry becomes missed, and
	// clock_never_advanced stops firing.
	if _, _, err := s.AdvanceClock(ctx, c.ID, 10, AdvanceSession, "", "sess-1", "keeper"); err != nil {
		t.Fatal(err)
	}
	snap, err = LoadSnapshot(ctx, ScopeDM, s.db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	findings = Check(snap)
	sawMissed, sawNever := false, false
	for _, f := range findings {
		if f.Check == CheckMissedSchedule {
			sawMissed = true
		}
		if f.Check == CheckClockNeverAdvanced {
			sawNever = true
		}
	}
	if !sawMissed {
		t.Fatal("pending entry behind the clock must be missed_schedule")
	}
	if sawNever {
		t.Fatal("clock_never_advanced must stop once the clock has moved")
	}
}

func intPtr(v int64) *int64 { return &v }

/* ---------- travel ---------- */

func mkLocation(t *testing.T, s *Store, c *Campaign, name string, routes string) *Entity {
	t.Helper()
	payload := map[string]any{}
	if routes != "" {
		payload["travel"] = map[string]any{"routes": []any{}}
		// Parse a compact "to=days,to=days" spec into route objects.
		for _, leg := range splitRoutes(routes) {
			payload["travel"].(map[string]any)["routes"] = append(
				payload["travel"].(map[string]any)["routes"].([]any), leg)
		}
	}
	e, err := s.CreateEntity(context.Background(), c.ID, KindLocation, name, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func splitRoutes(spec string) []map[string]any {
	var out []map[string]any
	for _, part := range strings.Split(spec, ",") {
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		days := int64(0)
		if len(kv) == 2 {
			days = mustAtoi(kv[1])
		}
		out = append(out, map[string]any{"to": kv[0], "days": float64(days), "terrain": "road"})
	}
	return out
}

func mustAtoi(v string) int64 {
	var n int64
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int64(ch-'0')
	}
	return n
}

func TestShortestRoutePicksCheapestPath(t *testing.T) {
	s := newStore(t)
	c := seedCampaign(t, s)

	a := mkLocation(t, s, c, "Ashford", "")
	b := mkLocation(t, s, c, "Blackwater", "")
	d := mkLocation(t, s, c, "Duskmere", "")
	a.Payload["travel"] = map[string]any{"routes": []any{
		map[string]any{"to": b.ID, "days": 10.0},
		map[string]any{"to": d.ID, "days": 3.0},
	}}
	b.Payload["travel"] = map[string]any{"routes": []any{
		map[string]any{"to": d.ID, "days": 4.0},
	}}
	locations := []Entity{*a, *b, *d}

	// Direct Ashford→Blackwater is 10; via Duskmere it is 3+4=7.
	days, path, ok := ShortestRoute(locations, a.ID, b.ID)
	if !ok || days != 7 {
		t.Fatalf("shortest route: %d days, ok=%v; want 7", days, ok)
	}
	if len(path) != 3 || path[0] != a.ID || path[1] != d.ID || path[2] != b.ID {
		t.Fatalf("path wrong: %v", path)
	}
	// Undirected: the same cost backwards.
	if days, _, ok := ShortestRoute(locations, b.ID, a.ID); !ok || days != 7 {
		t.Fatalf("reverse route: %d, %v", days, ok)
	}
	// Same place costs nothing.
	if days, path, ok := ShortestRoute(locations, a.ID, a.ID); !ok || days != 0 || len(path) != 1 {
		t.Fatalf("self route: %d %v %v", days, path, ok)
	}
}

func TestTravelAdvancesClockAndAsksForDays(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	a := mkLocation(t, s, c, "Ashford", "")
	island := mkLocation(t, s, c, "The Isle", "") // no routes anywhere

	// No route and no day count: the store asks, it never guesses.
	_, err := s.Travel(ctx, c.ID, a.ID, island.ID, nil, "keeper")
	if err == nil || !strings.Contains(err.Error(), "no route") {
		t.Fatalf("unrouted travel must ask for days: %v", err)
	}

	// The DM supplies the day count; the clock moves by exactly that.
	days := int64(5)
	res, err := s.Travel(ctx, c.ID, a.ID, island.ID, &days, "keeper")
	if err != nil {
		t.Fatal(err)
	}
	if res.Days != 5 || res.Clock != 5 {
		t.Fatalf("travel result: %+v", res)
	}
	ledger, err := s.ClockLedger(ctx, ScopeDM, c.ID, 10)
	if err != nil || len(ledger) != 1 {
		t.Fatalf("travel must record an advance: %v %v", ledger, err)
	}
	if ledger[0].Reason != AdvanceTravel || ledger[0].ToDay != 5 {
		t.Fatalf("travel advance wrong: %+v", ledger[0])
	}

	// A routed pair takes the computed cost.
	b := mkLocation(t, s, c, "Blackwater", "")
	a2, err := s.GetEntity(ctx, ScopeDM, c.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	a2.Payload["travel"] = map[string]any{"routes": []any{
		map[string]any{"to": b.ID, "days": 4.0},
	}}
	if _, err := s.UpdateEntity(ctx, c.ID, a.ID, nil, nil, nil, a2.Payload); err != nil {
		t.Fatal(err)
	}
	res, err = s.Travel(ctx, c.ID, a.ID, b.ID, nil, "keeper")
	if err != nil {
		t.Fatal(err)
	}
	if res.Days != 4 || res.Clock != 9 {
		t.Fatalf("routed travel: %+v", res)
	}
	if len(res.Names) != 2 || res.Names[0] != "Ashford" || res.Names[1] != "Blackwater" {
		t.Fatalf("travel names: %v", res.Names)
	}

	// Non-locations are not travel endpoints.
	pc, err := s.CreateEntity(ctx, c.ID, KindPC, "Mira", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Travel(ctx, c.ID, a.ID, pc.ID, nil, "keeper"); err == nil {
		t.Fatal("travel to a pc must be rejected")
	}
}
