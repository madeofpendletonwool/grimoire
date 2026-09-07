package replay

// The replay's row-level tests (MAD-426): one real battle through the
// tracker — the same harness shape the tracker's own tests use — then
// replayed: the fold reproduces the recorded rows exactly (the
// checksum assertion, mid-battle and after the end), scrubbing lands
// on the states the table saw, the timeline indexes the fight out of
// the session log, and a journal tampered with under the fold is
// reported, never absorbed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

type shelf struct{}

func (shelf) ResolveStatblock(_ context.Context, _, _, name string) (encounter.Creature, bool) {
	creature := map[string]encounter.Creature{
		"Wolf": {
			Slug: "wolf", Name: "Wolf", CR: "1/4", XP: 50, AC: 13, HP: 11,
			Abilities: &statblock.Abilities{Str: 12, Dex: 15, Con: 12},
		},
		"Goblin": {
			Slug: "goblin", Name: "Goblin", CR: "1/4", XP: 50, AC: 15, HP: 7,
			Abilities: &statblock.Abilities{Str: 8, Dex: 14, Con: 10},
		},
	}[name]
	return creature, creature.Name != ""
}

type harness struct {
	db        *sql.DB
	campaigns *campaign.Store
	sessions  *gamesession.Store
	combats   *combat.Store
	replay    *Store
	campaign  string
	session   string
	velren    string
	other     string // a second campaign, outside everything
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testdb.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('keeper', 'keeper', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	campaigns, err := campaign.New(db)
	if err != nil {
		t.Fatalf("campaign store: %v", err)
	}
	sessions, err := gamesession.New(db)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	roller, err := dice.New(db, campaigns, sessions)
	if err != nil {
		t.Fatalf("dice store: %v", err)
	}
	combats, err := combat.New(db, campaigns, sessions, roller)
	if err != nil {
		t.Fatalf("combat store: %v", err)
	}
	combats = combats.WithResolver(shelf{})
	replays, err := New(combats, sessions)
	if err != nil {
		t.Fatalf("replay store: %v", err)
	}

	ctx := context.Background()
	c, err := campaigns.CreateCampaign(ctx, "keeper", "The Ashen Court", "D&D 5e", "")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	live, err := sessions.CreateSession(ctx, c.ID, "The Ambush")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	live.Status = gamesession.StatusLive
	if _, err := sessions.UpdateSession(ctx, live.ID, nil, &live.Status); err != nil {
		t.Fatalf("go live: %v", err)
	}
	ranger := sheet.Sheet{
		Classes:   []sheet.ClassLevel{{Class: "Ranger", Level: 3}},
		Abilities: sheet.Abilities{STR: 12, DEX: 17, CON: 14, INT: 10, WIS: 14, CHA: 10},
		AC:        15, MaxHP: 28,
		Resistances: []string{"fire"},
	}
	velren, err := campaigns.CreateEntity(ctx, c.ID, campaign.KindPC, "Velren", "", campaign.WithSheet(nil, ranger))
	if err != nil {
		t.Fatalf("create velren: %v", err)
	}
	second, err := campaigns.CreateCampaign(ctx, "keeper", "Elsewhere", "D&D 5e", "")
	if err != nil {
		t.Fatalf("create second campaign: %v", err)
	}
	return &harness{
		db: db, campaigns: campaigns, sessions: sessions, combats: combats, replay: replays,
		campaign: c.ID, session: live.ID, velren: velren.ID, other: second.ID,
	}
}

// start opens the fight: Velren and her wolf against two goblins, with
// the session live so every journal row mirrors into the log.
func (h *harness) start(t *testing.T) *combat.StartResult {
	t.Helper()
	start, err := h.combats.Start(context.Background(), h.campaign, combat.StartInput{
		SessionID:  h.session,
		PCs:        []string{h.velren},
		Companions: []combat.CompanionLine{{Statblock: "Wolf", Name: "Whiskers"}},
		Monsters:   []combat.MonsterLine{{Name: "Goblin", Count: 2}},
	}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return start
}

func orderID(t *testing.T, start *combat.StartResult, name string) string {
	t.Helper()
	for _, c := range start.Order {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("no combatant named %s in the order", name)
	return ""
}

func seqOf(t *testing.T, events []Event, kind, combatantID string) int64 {
	t.Helper()
	for _, ev := range events {
		if ev.Kind == kind && ev.CombatantID == combatantID {
			return ev.Seq
		}
	}
	t.Fatalf("no %s row for %s in the journal", kind, combatantID)
	return 0
}

func frameFighter(t *testing.T, f *Frame, id string) *combat.Combatant {
	t.Helper()
	for i := range f.Order {
		if f.Order[i].ID == id {
			return &f.Order[i]
		}
	}
	t.Fatalf("no combatant %s in the frame", id)
	return nil
}

/* ---------- the checksum assertion ---------- */

func TestReplayReproducesTheRecordedStateMidBattle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start := h.start(t)

	// Before any end row exists, the fold already has to match the
	// live rows — the journal and the state it underlies agree at
	// every moment, or the replay says so.
	if _, err := h.combats.Damage(ctx, h.campaign, start.Combat.ID, orderID(t, start, "Goblin A"), 6, "", "", false, "keeper"); err != nil {
		t.Fatalf("damage goblin A: %v", err)
	}
	if _, err := h.combats.NextTurn(ctx, h.campaign, start.Combat.ID, "keeper"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	b, err := h.replay.Battle(ctx, h.campaign, start.Combat.ID)
	if err != nil {
		t.Fatalf("battle: %v", err)
	}
	if vr := b.Verify(); !vr.Matches {
		t.Fatalf("mid-battle verify failed: %v", vr.Differences)
	}
}

func TestReplayReproducesTheRecordedEndState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start := h.start(t)
	velren := orderID(t, start, "Velren")

	// A fight with a death, a death save, a healing, a standing
	// condition, and round wraps — order-agnostic, because initiative
	// is the dice's.
	if _, err := h.combats.Damage(ctx, h.campaign, start.Combat.ID, orderID(t, start, "Goblin A"), 10, "", "", false, "keeper"); err != nil {
		t.Fatalf("goblin A dies: %v", err)
	}
	if _, err := h.combats.Damage(ctx, h.campaign, start.Combat.ID, velren, 34, "", "", false, "keeper"); err != nil {
		t.Fatalf("velren goes down: %v", err)
	}
	if _, err := h.combats.DeathSave(ctx, h.campaign, start.Combat.ID, velren, combat.SaveSuccess, "keeper"); err != nil {
		t.Fatalf("death save: %v", err)
	}
	if _, err := h.combats.Heal(ctx, h.campaign, start.Combat.ID, velren, 5, "", "keeper"); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if _, err := h.combats.ApplyCondition(ctx, h.campaign, start.Combat.ID, orderID(t, start, "Goblin B"), "poisoned", 3, "keeper"); err != nil {
		t.Fatalf("condition: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := h.combats.NextTurn(ctx, h.campaign, start.Combat.ID, "keeper"); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if _, err := h.combats.End(ctx, h.campaign, start.Combat.ID, "the goblins had enough", "keeper"); err != nil {
		t.Fatalf("end: %v", err)
	}

	b, err := h.replay.Battle(ctx, h.campaign, start.Combat.ID)
	if err != nil {
		t.Fatalf("battle: %v", err)
	}
	vr := b.Verify()
	if !vr.Matches {
		t.Fatalf("end-state verify failed: %v", vr.Differences)
	}
	if len(vr.Checksum) != 64 {
		t.Fatalf("checksum: %q", vr.Checksum)
	}

	// The final frame and the recorded rows agree on every number.
	frame, err := b.Frame(b.MaxSeq())
	if err != nil {
		t.Fatalf("final frame: %v", err)
	}
	if frame.Status != combat.StatusEnded || frame.Round < 2 {
		t.Fatalf("final frame: status %s round %d", frame.Status, frame.Round)
	}
	for i, row := range b.Rows {
		f := frame.Order[i]
		if f.ID != row.ID || f.HP != row.HP || f.TempHP != row.TempHP || f.Dead != row.Dead ||
			f.Downed != row.Downed || f.LegendaryUsed != row.LegendaryUsed || f.Reveal != row.Reveal {
			t.Fatalf("final frame disagrees with the rows: frame %+v, row %+v", f, row)
		}
	}
}

/* ---------- scrubbing a real journal ---------- */

func TestReplayScrubsTheRealBattle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start := h.start(t)
	velren, ga := orderID(t, start, "Velren"), orderID(t, start, "Goblin A")

	if _, err := h.combats.Damage(ctx, h.campaign, start.Combat.ID, ga, 10, "", "", false, "keeper"); err != nil {
		t.Fatalf("goblin A dies: %v", err)
	}
	if _, err := h.combats.Damage(ctx, h.campaign, start.Combat.ID, velren, 34, "", "", false, "keeper"); err != nil {
		t.Fatalf("velren goes down: %v", err)
	}
	if _, err := h.combats.DeathSave(ctx, h.campaign, start.Combat.ID, velren, combat.SaveSuccess, "keeper"); err != nil {
		t.Fatalf("death save: %v", err)
	}
	if _, err := h.combats.Heal(ctx, h.campaign, start.Combat.ID, velren, 5, "", "keeper"); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if _, err := h.combats.End(ctx, h.campaign, start.Combat.ID, "done", "keeper"); err != nil {
		t.Fatalf("end: %v", err)
	}

	b, err := h.replay.Battle(ctx, h.campaign, start.Combat.ID)
	if err != nil {
		t.Fatalf("battle: %v", err)
	}

	// The opening lineup: everyone at their starting numbers, nobody's
	// turn yet.
	frame, err := b.Frame(0)
	if err != nil {
		t.Fatalf("frame 0: %v", err)
	}
	if frame.Round != 1 || frame.Turn != "" || frame.Status != combat.StatusActive {
		t.Fatalf("opening frame: round %d turn %q status %s", frame.Round, frame.Turn, frame.Status)
	}
	if g := frameFighter(t, frame, ga); g.HP != 7 || g.Dead {
		t.Fatalf("goblin A's opening frame: %+v", g)
	}

	// Scrub exactly onto the rows the table remembers.
	down, heal := seqOf(t, b.Events, "damage", velren), seqOf(t, b.Events, "heal", velren)
	frame, err = b.Frame(down)
	if err != nil {
		t.Fatalf("frame at the downing blow: %v", err)
	}
	if v := frameFighter(t, frame, velren); !v.Downed || v.HP != 0 || v.Dead {
		t.Fatalf("velren at the downing blow: %+v", v)
	}
	frame, err = b.Frame(heal)
	if err != nil {
		t.Fatalf("frame at the heal: %v", err)
	}
	if v := frameFighter(t, frame, velren); v.Downed || v.HP != 5 || v.DeathSuccesses != 0 {
		t.Fatalf("velren after the heal: %+v", v)
	}

	// The dead goblin is dead from its row onward, and the journal
	// ends on the end row.
	goblinDead := seqOf(t, b.Events, "damage", ga)
	frame, err = b.Frame(goblinDead)
	if err != nil {
		t.Fatalf("frame at goblin A's death: %v", err)
	}
	if g := frameFighter(t, frame, ga); !g.Dead {
		t.Fatalf("goblin A after the killing blow: %+v", g)
	}
	if last := b.Events[len(b.Events)-1]; last.Kind != "end" {
		t.Fatalf("journal does not end on the end row: %+v", last)
	}
}

/* ---------- the timeline ---------- */

func TestReplaySessionTimeline(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start := h.start(t)

	velren := orderID(t, start, "Velren")
	if _, err := h.combats.Damage(ctx, h.campaign, start.Combat.ID, velren, 10, "fire", "", false, "keeper"); err != nil {
		t.Fatalf("damage: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := h.combats.NextTurn(ctx, h.campaign, start.Combat.ID, "keeper"); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if _, err := h.combats.End(ctx, h.campaign, start.Combat.ID, "", "keeper"); err != nil {
		t.Fatalf("end: %v", err)
	}

	tl, err := h.replay.Session(ctx, h.campaign, h.session)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if tl.Session.ID != h.session || tl.Session.Status != gamesession.StatusLive {
		t.Fatalf("timeline session: %+v", tl.Session)
	}
	if len(tl.Fights) != 1 {
		t.Fatalf("fights: %+v", tl.Fights)
	}
	f := tl.Fights[0]
	if f.CombatID != start.Combat.ID || f.Status != combat.StatusEnded || f.LogLen == 0 || f.Round < 1 {
		t.Fatalf("fight index: %+v", f)
	}
	var rolls, combats int
	for _, ev := range tl.Events {
		switch ev.Kind {
		case gamesession.EventRoll:
			rolls++
		case gamesession.EventCombat:
			combats++
			if ev.CombatID != start.Combat.ID {
				t.Fatalf("combat event points elsewhere: %+v", ev)
			}
		}
	}
	if rolls == 0 {
		t.Fatal("the initiative rolls never made the timeline")
	}
	if combats != f.LogLen {
		t.Fatalf("combat events %d, journal rows %d — the mirror and journal disagree", combats, f.LogLen)
	}

	// A session outside the campaign is not found.
	if _, err := h.replay.Session(ctx, h.other, h.session); err == nil {
		t.Fatal("a session leaked across campaigns")
	}
}

/* ---------- drift under the fold ---------- */

func TestReplayReportsTamperedJournals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start := h.start(t)
	velren := orderID(t, start, "Velren")
	if _, err := h.combats.Damage(ctx, h.campaign, start.Combat.ID, velren, 10, "", "", false, "keeper"); err != nil {
		t.Fatalf("damage: %v", err)
	}
	if _, err := h.combats.End(ctx, h.campaign, start.Combat.ID, "", "keeper"); err != nil {
		t.Fatalf("end: %v", err)
	}

	b, err := h.replay.Battle(ctx, h.campaign, start.Combat.ID)
	if err != nil {
		t.Fatalf("battle: %v", err)
	}
	hit := seqOf(t, b.Events, "damage", velren)

	// Bend the journal under the fold: the hit's "after" becomes a lie
	// the rows never recorded.
	var payload string
	if err := h.db.QueryRowContext(ctx,
		`SELECT payload FROM combat_log WHERE combat_id = ? AND seq = ?`, start.Combat.ID, hit).
		Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	m["after"] = 17
	bent, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	if _, err := h.db.ExecContext(ctx,
		`UPDATE combat_log SET payload = ? WHERE combat_id = ? AND seq = ?`,
		string(bent), start.Combat.ID, hit); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	b, err = h.replay.Battle(ctx, h.campaign, start.Combat.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	vr := b.Verify()
	if vr.Matches {
		t.Fatal("a tampered journal verified clean")
	}
	joined := strings.Join(vr.Differences, "; ")
	if joined == "" || !strings.Contains(strings.ToLower(joined), "velren") {
		t.Fatalf("differences point nowhere near the lie: %v", vr.Differences)
	}
	if _, err := b.Frame(b.MaxSeq()); err == nil {
		t.Fatal("scrubbing a tampered journal folded without complaint")
	}
}

/* ---------- scope and wiring ---------- */

func TestReplayScopesToTheCampaign(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start := h.start(t)
	if _, err := h.replay.Battle(ctx, h.other, start.Combat.ID); err == nil {
		t.Fatal("a battle leaked across campaigns")
	}
	if _, err := h.replay.Battle(ctx, h.campaign, "no-such-combat"); err == nil {
		t.Fatal("a missing battle loaded")
	}
}

func TestNewRefusesHalfWiring(t *testing.T) {
	h := newHarness(t)
	if _, err := New(nil, h.sessions); err == nil {
		t.Fatal("a replay without the combat store built")
	}
	if _, err := New(h.combats, nil); err == nil {
		t.Fatal("a replay without the session store built")
	}
}
