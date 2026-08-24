package canon

// The flag ledger's semantics, tested over a real migrated database:
// refresh, clear, reopen, and — the load-bearing one — human-decision
// preservation across re-runs.

import (
	"context"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// offlineStore builds the offline store over a seeded campaign. The seed's
// secret Silver Key fact carries no awareness row at all, so the first run
// always reports unreachable_secret on it — a ready-made finding to refresh,
// clear, decide and reopen.
func offlineStore(t *testing.T) (*Store, *campaign.Fixture, string) {
	t.Helper()
	db, fx, _ := seeded(t)
	s, err := NewOffline(db)
	if err != nil {
		t.Fatalf("offline store: %v", err)
	}
	return s, fx, fx.Campaign.ID
}

func flagFor(t *testing.T, flags []Flag, check, recordID string) Flag {
	t.Helper()
	for _, f := range flags {
		if f.CheckCode == check && f.RecordID == recordID {
			return f
		}
	}
	t.Fatalf("flag %s/%s not found in %d flags", check, recordID, len(flags))
	return Flag{}
}

func TestOfflineStoreRunsEngineAndRefusesModelStages(t *testing.T) {
	s, fx, cid := offlineStore(t)
	ctx := context.Background()

	flags, err := s.CheckCampaign(ctx, cid, DefaultCheckOptions())
	if err != nil {
		t.Fatalf("check campaign: %v", err)
	}
	if len(flags) == 0 {
		t.Fatal("the seeded campaign must produce at least the unreachable_secret flag")
	}
	if f := flagFor(t, flags, CheckUnreachableSecret, fx.FactKeyOpensCrypt); f.Status != FlagOpen {
		t.Fatalf("fresh finding status %q, want open", f.Status)
	}

	// The offline store refuses the model-driven stages with a clear error,
	// never a nil-pointer panic.
	if _, err := s.Extract(ctx, ExtractInput{CampaignID: cid}); err == nil {
		t.Fatal("extract on an offline store must fail")
	}
	if _, err := s.Validate(ctx, ValidateInput{CampaignID: cid}); err == nil {
		t.Fatal("validate on an offline store must fail")
	}
}

func TestFlagLedgerClearsAndReopens(t *testing.T) {
	s, fx, cid := offlineStore(t)
	ctx := context.Background()
	db := s.db

	// 1. First run reports the unreachable secret.
	flags, err := s.CheckCampaign(ctx, cid, DefaultCheckOptions())
	if err != nil {
		t.Fatal(err)
	}
	first := flagFor(t, flags, CheckUnreachableSecret, fx.FactKeyOpensCrypt)
	if first.Status != FlagOpen || first.ClearedAt.After(first.LastSeenAt) {
		t.Fatalf("first run: status %q, want open", first.Status)
	}

	// 2. Cure it: give the secret a deliberate unaware marker — a modeled
	// clue opportunity — and re-run. The engine stops reporting; the open
	// flag must be cleared.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO awareness (campaign_id, knower, fact_id, stance, confidence, created_at, updated_at)
		VALUES (?, 'party', ?, 'unaware', 1, ?, ?)`,
		cid, fx.FactKeyOpensCrypt, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("plant unaware marker: %v", err)
	}
	flags, err = s.CheckCampaign(ctx, cid, DefaultCheckOptions())
	if err != nil {
		t.Fatal(err)
	}
	cleared := flagFor(t, flags, CheckUnreachableSecret, fx.FactKeyOpensCrypt)
	if cleared.Status != FlagCleared || cleared.ClearedAt.IsZero() {
		t.Fatalf("after cure: status %q, want cleared with a timestamp", cleared.Status)
	}

	// 3. The finding reappears: the flag reopens, and first_seen is kept —
	// the ledger remembers it has seen this before.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM awareness WHERE campaign_id = ? AND fact_id = ?`, cid, fx.FactKeyOpensCrypt); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	flags, err = s.CheckCampaign(ctx, cid, DefaultCheckOptions())
	if err != nil {
		t.Fatal(err)
	}
	reopened := flagFor(t, flags, CheckUnreachableSecret, fx.FactKeyOpensCrypt)
	if reopened.Status != FlagOpen {
		t.Fatalf("after reappear: status %q, want open", reopened.Status)
	}
	if !reopened.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Fatal("first_seen must survive a clear/reopen cycle")
	}
}

func TestFlagDecisionSurvivesReRuns(t *testing.T) {
	s, fx, cid := offlineStore(t)
	ctx := context.Background()
	db := s.db

	if _, err := s.CheckCampaign(ctx, cid, DefaultCheckOptions()); err != nil {
		t.Fatal(err)
	}

	// The DM dismisses the unreachable-secret flag with a note.
	if err := s.DecideFlag(ctx, cid, CheckUnreachableSecret, "fact", fx.FactKeyOpensCrypt,
		DecisionDismissed, "intentional: the key is a late reveal", "keeper"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	flags, err := s.Flags(ctx, cid, FlagDismissed)
	if err != nil {
		t.Fatal(err)
	}
	if len(flags) != 1 || flags[0].RecordID != fx.FactKeyOpensCrypt {
		t.Fatalf("dismissed list: %+v", flags)
	}
	if flags[0].DecisionNote == "" || flags[0].DecidedBy != "keeper" || flags[0].DecidedAt.IsZero() {
		t.Fatalf("decision fields not recorded: %+v", flags[0])
	}

	// A re-run that still reports the finding refreshes the message but
	// never reopens the decided flag.
	if _, err := s.CheckCampaign(ctx, cid, DefaultCheckOptions()); err != nil {
		t.Fatal(err)
	}
	flags, err = s.Flags(ctx, cid, "")
	if err != nil {
		t.Fatal(err)
	}
	f := flagFor(t, flags, CheckUnreachableSecret, fx.FactKeyOpensCrypt)
	if f.Status != FlagDismissed {
		t.Fatalf("re-run clobbered the decision: status %q", f.Status)
	}
	if f.DecisionNote != "intentional: the key is a late reveal" {
		t.Fatalf("decision note lost: %q", f.DecisionNote)
	}

	// And a re-run that stops reporting it leaves the decision in place —
	// decided items never become cleared.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO awareness (campaign_id, knower, fact_id, stance, confidence, created_at, updated_at)
		VALUES (?, 'party', ?, 'unaware', 1, ?, ?)`,
		cid, fx.FactKeyOpensCrypt, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckCampaign(ctx, cid, DefaultCheckOptions()); err != nil {
		t.Fatal(err)
	}
	flags, err = s.Flags(ctx, cid, "")
	if err != nil {
		t.Fatal(err)
	}
	if f := flagFor(t, flags, CheckUnreachableSecret, fx.FactKeyOpensCrypt); f.Status != FlagDismissed {
		t.Fatalf("decided flag must keep its decision forever, got %q", f.Status)
	}

	// Deciding again is an error, not an overwrite.
	err = s.DecideFlag(ctx, cid, CheckUnreachableSecret, "fact", fx.FactKeyOpensCrypt, DecisionAccepted, "", "keeper")
	if err == nil {
		t.Fatal("re-deciding a decided flag must fail")
	}
	// Deciding a flag that does not exist is ErrNotFound.
	if err := s.DecideFlag(ctx, cid, "no_such_check", "fact", "nope", DecisionAccepted, "", "keeper"); err == nil {
		t.Fatal("deciding a missing flag must fail")
	}
	// Bogus decisions are rejected outright.
	if err := s.DecideFlag(ctx, cid, CheckUnreachableSecret, "fact", fx.FactKeyOpensCrypt, "maybe", "", "keeper"); err == nil {
		t.Fatal("a decision outside the vocabulary must fail")
	}
}

func TestFlagRefreshUpdatesMessage(t *testing.T) {
	s, fx, cid := offlineStore(t)
	ctx := context.Background()
	db := s.db

	// A controllable clock: the ledger stores milliseconds, and two runs in
	// the same millisecond must still be distinguishable.
	clock := time.Now().UTC()
	s.now = func() time.Time { return clock }

	if _, err := s.CheckCampaign(ctx, cid, DefaultCheckOptions()); err != nil {
		t.Fatal(err)
	}
	flags, _ := s.Flags(ctx, cid, FlagOpen)
	before := flagFor(t, flags, CheckUnreachableSecret, fx.FactKeyOpensCrypt)

	// Rewrite the secret's statement; the finding's message must refresh on
	// the next run while the flag keeps its identity.
	if _, err := db.ExecContext(ctx,
		`UPDATE facts SET statement = ? WHERE id = ?`,
		"The Silver Key opens the Duke's wine cellar.", fx.FactKeyOpensCrypt); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if _, err := s.CheckCampaign(ctx, cid, DefaultCheckOptions()); err != nil {
		t.Fatal(err)
	}
	flags, _ = s.Flags(ctx, cid, FlagOpen)
	after := flagFor(t, flags, CheckUnreachableSecret, fx.FactKeyOpensCrypt)
	if after.ID != before.ID {
		t.Fatal("the refreshed finding must keep the same flag row")
	}
	if after.Message == before.Message {
		t.Fatalf("message did not refresh: %q", after.Message)
	}
	if !after.LastSeenAt.After(before.LastSeenAt) {
		t.Fatalf("last_seen must move forward on a refresh: %v -> %v", before.LastSeenAt, after.LastSeenAt)
	}
}

// TestLoaderDatesAndEncounters pins the loader's DB-reading half: sessions,
// the planned-encounter payload contract, and the party levels read from pc
// payloads. The rules themselves are covered pure above; this proves the
// snapshot the store hands them matches the database.
func TestLoaderDatesAndEncounters(t *testing.T) {
	db, fx, sid := seeded(t)
	ctx := context.Background()
	cid := fx.Campaign.ID

	// A planned session carrying a planned encounter, and a live one that
	// must not be read as planned material.
	var plannedID string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO game_sessions (id, campaign_id, ordinal, name, status, created_at, updated_at)
		VALUES ('sess-2', ?, 2, 'Session 2', 'planned', ?, ?) RETURNING id`,
		cid, time.Now().UnixMilli(), time.Now().UnixMilli()).Scan(&plannedID); err != nil {
		t.Fatalf("planned session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO session_events (id, session_id, seq, kind, summary, detail, payload, created_at)
		VALUES ('ev-1', ?, 1, 'encounter', 'Roadside ambush', '',
		        '{"name":"Roadside ambush","party":[1,1,1,1],"monsters":[{"name":"Goblin","cr":"1/4","count":6},{"name":"Zorblat","cr":"2","count":1}]}', ?)`,
		plannedID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("planned encounter event: %v", err)
	}

	// A bestiary mirror with the goblin but not zorblat. The table is owned
	// by the encounter package and created on demand; create it the way the
	// catalog would.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS bestiary (
		key TEXT PRIMARY KEY, name TEXT NOT NULL, cr TEXT NOT NULL DEFAULT '', cr_num REAL NOT NULL DEFAULT 0,
		xp INTEGER NOT NULL DEFAULT 0, type TEXT NOT NULL DEFAULT '', data TEXT NOT NULL, synced_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("bestiary table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO bestiary (key, name, cr, cr_num, xp, type, data, synced_at) VALUES ('goblin', 'Goblin', '1/4', 0.25, 50, 'humanoid', '{}', 0)`); err != nil {
		t.Fatalf("bestiary row: %v", err)
	}

	snap, err := LoadSnapshot(ctx, db, cid)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}

	// Sessions: the seed's done session plus the planned one.
	if len(snap.Sessions) != 2 {
		t.Fatalf("sessions loaded: %d, want 2", len(snap.Sessions))
	}
	// The party: four pcs at level 5, in name order.
	want := []int{5, 5, 5, 5}
	if len(snap.Party) != len(want) {
		t.Fatalf("party loaded: %v, want %v", snap.Party, want)
	}
	for i := range want {
		if snap.Party[i] != want[i] {
			t.Fatalf("party loaded: %v, want %v", snap.Party, want)
		}
	}
	// The planned encounter, with its roster parsed and XP derived from CR.
	if len(snap.Encounters) != 1 {
		t.Fatalf("planned encounters: %d, want 1", len(snap.Encounters))
	}
	e := snap.Encounters[0]
	if e.EventID != "ev-1" || len(e.Monsters) != 2 || e.Monsters[0].XP != 50 {
		t.Fatalf("encounter parsed: %+v", e)
	}
	if !snap.Bestiary["goblin"] || snap.Bestiary["zorblat"] {
		t.Fatalf("bestiary loaded: %v", snap.Bestiary)
	}

	// And the rules over the loaded snapshot: the unfinished monster is
	// unresolved and the level-1 plan has drifted.
	s, err := NewOffline(db)
	if err != nil {
		t.Fatal(err)
	}
	_ = s
	_ = sid
	findings := CheckSnapshot(snap, DefaultCheckOptions())
	if has(findings, CheckStatBlockUnresolved, "ev-1") != 1 {
		t.Fatal("the loaded encounter must produce stat_block_unresolved")
	}
	if has(findings, CheckPartyLevelDrift, "ev-1") != 1 {
		t.Fatal("the loaded encounter must produce party_level_drift")
	}
}
