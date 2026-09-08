package combat

// The tracker's row-level tests (MAD-422): a full battle on a private
// migrated database — pcs from real sheets, a companion wolf beside the
// party, goblins on the other side, initiative through the dice engine,
// damage and deaths and healing, durations expiring on both engines, the
// journal and the session log telling one story, and the reload reading
// the battle exactly where the table left it. The pure grammar has its
// own tests.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/effects"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// statblocks is the fake resolver's shelf: the wolf the ranger rides
// with, the goblins on the other side, and a dragon that brings
// legendary actions, a lair and a recharge along.
type statblocks map[string]encounter.Creature

// lookup implements the resolver with a squashed-name match — enough
// tolerance for the store's calls, none of the catalog's machinery.
type lookup struct{ shelf statblocks }

func (l lookup) ResolveStatblock(_ context.Context, _, _, name string) (encounter.Creature, bool) {
	for _, c := range l.shelf {
		if strings.EqualFold(strings.Join(strings.Fields(c.Name), ""), strings.Join(strings.Fields(name), "")) {
			return c, true
		}
	}
	return encounter.Creature{}, false
}

func wolf() encounter.Creature {
	return encounter.Creature{
		Slug: "wolf", Name: "Wolf", CR: "1/4", XP: 50, AC: 13, HP: 11,
		Abilities: &statblock.Abilities{Str: 12, Dex: 15, Con: 12, Int: 3, Wis: 12, Cha: 6},
		Actions:   []encounter.NamedText{{Name: "Bite", Kind: "ACTION"}},
	}
}

func goblin() encounter.Creature {
	return encounter.Creature{
		Slug: "goblin", Name: "Goblin", CR: "1/4", XP: 50, AC: 15, HP: 7,
		Abilities: &statblock.Abilities{Str: 8, Dex: 14, Con: 10, Int: 10, Wis: 8, Cha: 8},
		Actions:   []encounter.NamedText{{Name: "Scimitar", Kind: "ACTION"}},
	}
}

func dragon() encounter.Creature {
	return encounter.Creature{
		Slug: "adult-black-dragon", Name: "Adult Black Dragon", CR: "14", XP: 11500, AC: 19, HP: 195,
		LairAction: true,
		Abilities:  &statblock.Abilities{Str: 23, Dex: 14, Con: 21, Int: 14, Wis: 13, Cha: 17},
		Actions: []encounter.NamedText{
			{Name: "Bite", Kind: "ACTION"},
			{Name: "Acid Breath", Kind: "ACTION", Usage: "Recharge 5-6"},
			{Name: "Detect", Kind: "LEGENDARY_ACTION", Cost: 1},
			{Name: "Tail Attack", Kind: "LEGENDARY_ACTION", Cost: 1},
			{Name: "Wing Attack", Kind: "LEGENDARY_ACTION", Cost: 2},
		},
	}
}

// harness is one private database with the stack wired the way play
// wires it: campaigns, sessions, the dice engine, the effect engine and
// the combat store over a two-creature shelf, one live sitting, and a
// ranger with a real sheet.
type harness struct {
	db        *sql.DB
	campaigns *campaign.Store
	sessions  *gamesession.Store
	effects   *effects.Store
	store     *Store
	campaign  string
	session   string
	velren    string // the ranger
	nyx       string // the warlock, unstructured
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
	effectEngine, err := effects.New(db, campaigns, nil)
	if err != nil {
		t.Fatalf("effects store: %v", err)
	}
	store, err := New(db, campaigns, sessions, roller)
	if err != nil {
		t.Fatalf("combat store: %v", err)
	}
	store = store.WithEffects(effectEngine).WithResolver(lookup{shelf: statblocks{
		"Wolf": wolf(), "Goblin": goblin(), "Adult Black Dragon": dragon(),
	}})

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
	nyx, err := campaigns.CreateEntity(ctx, c.ID, campaign.KindPC, "Nyx", "", nil)
	if err != nil {
		t.Fatalf("create nyx: %v", err)
	}
	return &harness{
		db: db, campaigns: campaigns, sessions: sessions, effects: effectEngine, store: store,
		campaign: c.ID, session: live.ID, velren: velren.ID, nyx: nyx.ID,
	}
}

func byName(t *testing.T, order []Combatant, name string) Combatant {
	t.Helper()
	for _, c := range order {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no combatant named %s in %d", name, len(order))
	return Combatant{}
}

/* ---------- the whole battle ---------- */

func TestFullCombatRunsEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Roll initiative: the ranger, her wolf, and two goblins.
	start, err := h.store.Start(ctx, h.campaign, StartInput{
		PCs:        []string{h.velren},
		Companions: []CompanionLine{{Statblock: "Wolf", Name: "Whiskers"}},
		Monsters:   []MonsterLine{{Name: "Goblin", Count: 2}},
	}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(start.Order) != 4 {
		t.Fatalf("order: %d combatants, want 4", len(start.Order))
	}
	velren := byName(t, start.Order, "Velren")
	if velren.MaxHP != 28 || velren.AC != 15 || velren.Snapshot.InitBonus != 3 || velren.Downed {
		t.Fatalf("velren's sheet numbers: %+v", velren)
	}
	if velren.EntityID != h.velren {
		t.Fatalf("velren's entity link: %s", velren.EntityID)
	}
	whiskers := byName(t, start.Order, "Whiskers")
	if whiskers.Kind != KindCompanion || whiskers.Side != SideParty || whiskers.MaxHP != 11 || whiskers.Snapshot.InitBonus != 2 {
		t.Fatalf("the wolf beside the party: %+v", whiskers)
	}
	if whiskers.EntityID != "" {
		t.Fatalf("the wolf carries no entity: %q", whiskers.EntityID)
	}
	ga, gb := byName(t, start.Order, "Goblin A"), byName(t, start.Order, "Goblin B")
	if ga.Side != SideFoe || gb.MaxHP != 7 || ga.Snapshot.InitBonus != 2 {
		t.Fatalf("the goblins: %+v %+v", ga, gb)
	}

	// The order is the initiative order, and every position is distinct.
	for i := 1; i < len(start.Order); i++ {
		if start.Order[i-1].Initiative < start.Order[i].Initiative {
			t.Fatalf("order not sorted: %+v", start.Order)
		}
		if start.Order[i-1].Initiative == start.Order[i].Initiative {
			t.Fatalf("tie survived the re-rolls: %+v", start.Order)
		}
	}

	// Initiative is rolls: one public initiative roll per combatant in
	// the dice feed, provenance for every number on the rows.
	var rolls int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dice_rolls WHERE campaign_id = ? AND context_kind = 'initiative'`,
		h.campaign).Scan(&rolls); err != nil {
		t.Fatalf("count rolls: %v", err)
	}
	if rolls < 4 {
		t.Fatalf("initiative rolls: %d, want at least 4", rolls)
	}

	// The battle survives reload exactly where it stands: round 1,
	// nobody has acted.
	again, order, err := h.store.Active(ctx, h.campaign)
	if err != nil || again == nil {
		t.Fatalf("reload active: %v %v", again, err)
	}
	if again.Round != 1 || again.TurnIndex != -1 || len(order) != 4 {
		t.Fatalf("reload state: round %d turn %d n %d", again.Round, again.TurnIndex, len(order))
	}
	combatID := again.ID

	// The first turn is the order's own top.
	turn, err := h.store.NextTurn(ctx, h.campaign, combatID, "keeper")
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if turn.Combat.TurnIndex != 0 || turn.Combat.Round != 1 || turn.RoundWrapped {
		t.Fatalf("first turn state: %+v", turn.Combat)
	}
	if got := turn.Order[turn.Combat.TurnIndex].Name; got != order[0].Name {
		t.Fatalf("the order's top opens the battle: %s", got)
	}

	// A goblin hits Velren for 8; his fire resistance does not apply to
	// a blade, and he takes all 8.
	velrenID := byName(t, turn.Order, "Velren").ID
	hit, err := h.store.Damage(ctx, h.campaign, combatID, velrenID, 8, "slashing", "scimitar", false, "", "keeper")
	if err != nil {
		t.Fatalf("damage: %v", err)
	}
	if hit.Outcome.Effective != 8 || hit.Combatant.HP != 20 {
		t.Fatalf("velren after the hit: %+v", hit)
	}
	// The fire resistance halves what it should.
	hit, err = h.store.Damage(ctx, h.campaign, combatID, velrenID, 10, "fire", "torch", false, "", "keeper")
	if err != nil {
		t.Fatalf("fire damage: %v", err)
	}
	if hit.Outcome.Effective != 5 || hit.Combatant.HP != 15 {
		t.Fatalf("resistance: %+v", hit)
	}

	// Velren cuts Goblin A down; the wolf kills Goblin B.
	gaID, gbID := byName(t, turn.Order, "Goblin A").ID, byName(t, turn.Order, "Goblin B").ID
	kill, err := h.store.Damage(ctx, h.campaign, combatID, gaID, 9, "slashing", "longsword", false, "", "keeper")
	if err != nil {
		t.Fatalf("kill goblin a: %v", err)
	}
	if !kill.Outcome.Died || !kill.Combatant.Dead {
		t.Fatalf("goblin a: %+v", kill)
	}
	if _, err := h.store.Damage(ctx, h.campaign, combatID, gbID, 11, "piercing", "bite", false, "", "keeper"); err != nil {
		t.Fatalf("kill goblin b: %v", err)
	}

	// The journal and the session log tell one story: every write is a
	// combat_log row, and the live sitting carries the mirrors.
	logRows, err := h.store.Log(ctx, h.campaign, combatID, 0, 0)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	kinds := map[string]int{}
	for _, e := range logRows {
		kinds[e.Kind]++
	}
	if kinds["start"] != 1 || kinds["turn"] != 1 || kinds["damage"] != 4 {
		t.Fatalf("journal kinds: %v", kinds)
	}
	var events int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_events WHERE session_id = ? AND kind = 'combat'`, h.session).Scan(&events); err != nil {
		t.Fatalf("count combat events: %v", err)
	}
	if events != 6 { // start + turn + 4 damage
		t.Fatalf("session mirrors: %d, want 6", events)
	}
	var mirrored int
	if err := h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM combat_log WHERE session_event_id IS NOT NULL AND combat_id = ?`, combatID).Scan(&mirrored); err != nil {
		t.Fatalf("count mirrored rows: %v", err)
	}
	if mirrored != 6 {
		t.Fatalf("journal rows linked to their mirrors: %d", mirrored)
	}

	// End: the battle closes with the survivor counts.
	end, err := h.store.End(ctx, h.campaign, combatID, "the goblins are down", "keeper")
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if end.Combat.Status != StatusEnded || end.Alive[SideFoe] != 0 || end.Alive[SideParty] != 2 {
		t.Fatalf("end state: %+v %v", end.Combat, end.Alive)
	}
	if _, err := h.store.End(ctx, h.campaign, combatID, "", "keeper"); err == nil {
		t.Fatalf("ended twice")
	}
	// One battle at a time — and after the end, a new one may start.
	if _, err := h.store.Start(ctx, h.campaign, StartInput{PCs: []string{h.velren}}, "keeper"); err != nil {
		t.Fatalf("next battle: %v", err)
	}
}

func TestOneActiveCombatPerCampaign(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.Start(ctx, h.campaign, StartInput{PCs: []string{h.velren}}, "keeper"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := h.store.Start(ctx, h.campaign, StartInput{PCs: []string{h.nyx}}, "keeper"); err == nil {
		t.Fatalf("a second battle started while one ran")
	}
}

func TestStartRejectsUnknownStatblocksAndNonPCs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.store.Start(ctx, h.campaign, StartInput{
		PCs: []string{h.velren}, Monsters: []MonsterLine{{Name: "Beholder", Count: 1}},
	}, "keeper"); err == nil {
		t.Fatalf("an unknown statblock joined the battle")
	}
	npc, err := h.campaigns.CreateEntity(ctx, h.campaign, campaign.KindNPC, "Duke", "", nil)
	if err != nil {
		t.Fatalf("create npc: %v", err)
	}
	if _, err := h.store.Start(ctx, h.campaign, StartInput{PCs: []string{npc.ID}}, "keeper"); err == nil {
		t.Fatalf("an npc joined as a pc")
	}
}

/* ---------- death saves and healing ---------- */

func TestDownedDeathSavesAndHealing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start, err := h.store.Start(ctx, h.campaign, StartInput{PCs: []string{h.velren}}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	combatID := start.Combat.ID
	velrenID := byName(t, start.Order, "Velren").ID

	// Dropped, not dead; damage at zero fails saves; healing wakes.
	if _, err := h.store.Damage(ctx, h.campaign, combatID, velrenID, 28, "slashing", "", false, "", "keeper"); err != nil {
		t.Fatalf("damage: %v", err)
	}
	down, err := h.store.Damage(ctx, h.campaign, combatID, velrenID, 4, "slashing", "", false, "", "keeper")
	if err != nil {
		t.Fatalf("damage at zero: %v", err)
	}
	if !down.Combatant.Downed || down.Combatant.DeathFailures != 1 || down.Combatant.Dead {
		t.Fatalf("down and failing: %+v", down.Combatant)
	}
	if _, err := h.store.DeathSave(ctx, h.campaign, combatID, velrenID, SaveFail, "keeper"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := h.store.DeathSave(ctx, h.campaign, combatID, velrenID, SaveCritSuccess, "keeper"); err != nil {
		t.Fatalf("nat 20: %v", err)
	}
	woke, _, err := h.store.Active(ctx, h.campaign)
	if err != nil || woke == nil {
		t.Fatalf("reload: %v", err)
	}
	velren := byName(t, mustOrder(t, h, woke.ID), "Velren")
	if velren.HP != 1 || velren.Downed || velren.DeathSuccesses != 0 || velren.DeathFailures != 0 {
		t.Fatalf("awake at 1 hp with the ledger cleared: %+v", velren)
	}

	// Healing after the wake, capped at the max.
	if _, err := h.store.Heal(ctx, h.campaign, woke.ID, velren.ID, 99, "", "keeper"); err != nil {
		t.Fatalf("heal: %v", err)
	}
	healed, _, _ := h.store.Active(ctx, h.campaign)
	velren = byName(t, mustOrder(t, h, healed.ID), "Velren")
	if velren.HP != 28 {
		t.Fatalf("healed to the max: %+v", velren)
	}

	// Death saves are a downed pc's grammar, nobody else's.
	if _, err := h.store.DeathSave(ctx, h.campaign, woke.ID, velren.ID, SaveSuccess, "keeper"); err == nil {
		t.Fatalf("a standing pc saved")
	}
}

func mustOrder(t *testing.T, h *harness, combatID string) []Combatant {
	t.Helper()
	_, order, err := h.store.Get(context.Background(), h.campaign, combatID)
	if err != nil {
		t.Fatalf("get combat: %v", err)
	}
	return order
}

/* ---------- durations on both engines ---------- */

func TestDurationsExpireOnBothEngines(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start, err := h.store.Start(ctx, h.campaign, StartInput{
		PCs:      []string{h.velren},
		Monsters: []MonsterLine{{Name: "Goblin", Count: 1}},
	}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	combatID := start.Combat.ID
	velrenID := byName(t, start.Order, "Velren").ID
	goblinID := byName(t, start.Order, "Goblin").ID

	// A two-round condition on the goblin (combat-local) and a two-round
	// effect on Velren (a duration-engine row, the pc's own store).
	if _, err := h.store.ApplyCondition(ctx, h.campaign, combatID, goblinID, "poisoned", 2, "keeper"); err != nil {
		t.Fatalf("condition: %v", err)
	}
	if _, err := h.store.ApplyCondition(ctx, h.campaign, combatID, velrenID, "blinded", 2, "keeper"); err == nil {
		t.Fatalf("a pc's condition went combat-local")
	}
	if _, err := h.effects.Apply(ctx, h.campaign, effects.ApplyInput{
		TargetID: h.velren, Kind: effects.KindCondition, Name: "blinded",
		Duration: effects.Duration{Amount: 2, Unit: effects.UnitRound},
	}, "keeper"); err != nil {
		t.Fatalf("effect: %v", err)
	}

	// Two combatants walk a round in two turns; the third turn wraps to
	// round 2 and wears one round off both engines; the fifth turn wraps
	// to round 3 and expires both.
	for i := 0; i < 3; i++ {
		if _, err := h.store.NextTurn(ctx, h.campaign, combatID, "keeper"); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	_, order := mustActive(t, h, ctx)
	if len(byName(t, order, "Goblin").Conditions) != 1 || byName(t, order, "Goblin").Conditions[0].Rounds != 1 {
		t.Fatalf("goblin's condition after one wrap: %+v", byName(t, order, "Goblin").Conditions)
	}

	var expired effects.Row
	for i := 0; i < 2; i++ {
		turn, err := h.store.NextTurn(ctx, h.campaign, combatID, "keeper")
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if turn.RoundWrapped && turn.Combat.Round == 3 {
			if len(turn.ExpiredConditions) != 1 || turn.ExpiredConditions[0].Name != "poisoned" {
				t.Fatalf("wrap 3 expired conditions: %+v", turn.ExpiredConditions)
			}
			if len(turn.ExpiredEffects) != 1 || turn.ExpiredEffects[0].Name != "blinded" {
				t.Fatalf("wrap 3 expired effects: %+v", turn.ExpiredEffects)
			}
			expired = turn.ExpiredEffects[0]
		}
	}
	if expired.Name == "" {
		t.Fatalf("the wrap to round 3 never expired the blinded row")
	}
	_, order = mustActive(t, h, ctx)
	if len(byName(t, order, "Goblin").Conditions) != 0 {
		t.Fatalf("goblin's condition outlived its rounds: %+v", byName(t, order, "Goblin").Conditions)
	}
}

func mustActive(t *testing.T, h *harness, ctx context.Context) (*Combat, []Combatant) {
	t.Helper()
	c, order, err := h.store.Active(ctx, h.campaign)
	if err != nil || c == nil {
		t.Fatalf("active: %v %v", c, err)
	}
	return c, order
}

/* ---------- legendary actions, recharge prompts, the lair ---------- */

func TestLegendaryRechargeAndLairAutomation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start, err := h.store.Start(ctx, h.campaign, StartInput{
		PCs:      []string{h.velren},
		Monsters: []MonsterLine{{Name: "Adult Black Dragon", Count: 1}},
	}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	combatID := start.Combat.ID
	dragon := byName(t, start.Order, "Adult Black Dragon")
	if dragon.Snapshot.LegendaryMax != 3 || !dragon.Snapshot.Lair {
		t.Fatalf("the dragon's snapshot: %+v", dragon.Snapshot)
	}

	// Velren acts first or second; walk turns until the dragon's turn
	// starts, then read its prompts and spend its legend.
	var prompts []TurnPrompt
	for i := 0; i < 2; i++ {
		turn, err := h.store.NextTurn(ctx, h.campaign, combatID, "keeper")
		if err != nil {
			t.Fatalf("turn: %v", err)
		}
		if turn.Order[turn.Combat.TurnIndex].Name == "Adult Black Dragon" {
			prompts = turn.Prompts
		}
	}
	found := false
	for _, p := range prompts {
		if p.Kind == "recharge" && p.Name == "Acid Breath" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no recharge prompt on the dragon's turn: %+v", prompts)
	}

	// Legendary spends respect the derived budget: Detect (1) then Wing
	// Attack (2) fills it, and a third spend refuses.
	if _, err := h.store.Legendary(ctx, h.campaign, combatID, dragon.ID, "Detect", 1, "keeper"); err != nil {
		t.Fatalf("legendary: %v", err)
	}
	if _, err := h.store.Legendary(ctx, h.campaign, combatID, dragon.ID, "Wing Attack", 2, "keeper"); err != nil {
		t.Fatalf("legendary to budget: %v", err)
	}
	if _, err := h.store.Legendary(ctx, h.campaign, combatID, dragon.ID, "Detect", 1, "keeper"); err == nil {
		t.Fatalf("legendary over budget")
	}
	// Walk to the dragon's turn end (the next turn after its own), then
	// the budget is back.
	for {
		turn, err := h.store.NextTurn(ctx, h.campaign, combatID, "keeper")
		if err != nil {
			t.Fatalf("turn: %v", err)
		}
		if turn.Order[turn.Combat.TurnIndex].Name == "Velren" {
			break
		}
	}
	if _, err := h.store.Legendary(ctx, h.campaign, combatID, dragon.ID, "Detect", 1, "keeper"); err != nil {
		t.Fatalf("legendary after replenish: %v", err)
	}

	// Keep walking rounds until the count-20 crossing fires; the dragon
	// has a lair, so it must.
	fired := false
	for i := 0; i < 12 && !fired; i++ {
		turn, err := h.store.NextTurn(ctx, h.campaign, combatID, "keeper")
		if err != nil {
			t.Fatalf("turn: %v", err)
		}
		fired = turn.LairReminder
	}
	if !fired {
		t.Fatalf("the lair never fired")
	}
}

/* ---------- concentration prompts on damage ---------- */

func TestDamagePromptsConcentrationChecks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Velren concentrates on hunter's mark.
	if _, err := h.effects.Apply(ctx, h.campaign, effects.ApplyInput{
		TargetID: h.velren, Kind: effects.KindSpell, Name: "hunter's mark",
		SourceID: h.velren, Concentration: true,
		Duration: effects.Duration{Amount: 10, Unit: effects.UnitRound},
	}, "keeper"); err != nil {
		t.Fatalf("concentrate: %v", err)
	}
	start, err := h.store.Start(ctx, h.campaign, StartInput{PCs: []string{h.velren}}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	velrenID := byName(t, start.Order, "Velren").ID
	// 22 damage: DC is half, 11, over the floor of 10.
	hit, err := h.store.Damage(ctx, h.campaign, start.Combat.ID, velrenID, 22, "slashing", "", false, "", "keeper")
	if err != nil {
		t.Fatalf("damage: %v", err)
	}
	if len(hit.Concentration) != 1 || hit.Concentration[0].Spell != "hunter's mark" || hit.Concentration[0].DC != 11 {
		t.Fatalf("concentration prompt: %+v", hit.Concentration)
	}
	// A small hit stays at the floor.
	hit, err = h.store.Damage(ctx, h.campaign, start.Combat.ID, velrenID, 5, "slashing", "", false, "", "keeper")
	if err != nil {
		t.Fatalf("damage: %v", err)
	}
	if len(hit.Concentration) != 1 || hit.Concentration[0].DC != 10 {
		t.Fatalf("floor: %+v", hit.Concentration)
	}
}

/* ---------- the session export carries the battle ---------- */

func TestCombatExportsIntoTheSessionLog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start, err := h.store.Start(ctx, h.campaign, StartInput{
		PCs:      []string{h.velren},
		Monsters: []MonsterLine{{Name: "Goblin", Count: 1}},
	}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	goblinID := byName(t, start.Order, "Goblin").ID
	if _, err := h.store.Damage(ctx, h.campaign, start.Combat.ID, goblinID, 9, "slashing", "bite", false, "", "keeper"); err != nil {
		t.Fatalf("damage: %v", err)
	}

	// The export renders the battle's events like every other kind.
	export, err := h.sessions.ExportMarkdown(ctx, h.session)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	for _, want := range []string{"combat", "Velren", "order:", "Goblin takes 9 slashing", "dies"} {
		if !strings.Contains(export, want) {
			t.Fatalf("export missing %q:\n%s", want, clip(export, 2000))
		}
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

/* ---------- reload folds the journal to the same state ---------- */

func TestJournalFoldsToTheColumns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start, err := h.store.Start(ctx, h.campaign, StartInput{
		PCs:      []string{h.velren},
		Monsters: []MonsterLine{{Name: "Goblin", Count: 2}},
	}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	combatID := start.Combat.ID
	velrenID := byName(t, start.Order, "Velren").ID

	// A dense stream: temp hp, damage through it, healing, a kill.
	if _, err := h.store.TempHP(ctx, h.campaign, combatID, velrenID, 5, "", "keeper"); err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := h.store.Damage(ctx, h.campaign, combatID, velrenID, 9, "fire", "", false, "", "keeper"); err != nil {
		t.Fatalf("damage: %v", err)
	}
	if _, err := h.store.Heal(ctx, h.campaign, combatID, velrenID, 4, "", "keeper"); err != nil {
		t.Fatalf("heal: %v", err)
	}
	gaID := byName(t, start.Order, "Goblin A").ID
	if _, err := h.store.Damage(ctx, h.campaign, combatID, gaID, 7, "slashing", "", false, "", "keeper"); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// Fold Velren's own entries: temp hp grants 5, fire 9 halves to 4
	// and the temp coat drinks all 4, healing 4 caps at the max — the
	// arithmetic the columns must reproduce from the journal alone.
	entries, err := h.store.Log(ctx, h.campaign, combatID, 0, 0)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	velren := Combatant{MaxHP: 28, HP: 28}
	var foldTemp int
	for _, e := range entries {
		if e.CombatantID != velrenID {
			continue
		}
		switch e.Kind {
		case "temp_hp":
			foldTemp = max(foldTemp, e.Amount)
		case "damage":
			eff, _ := e.Payload["effective"].(float64)
			absorbed, _ := e.Payload["absorbed_by_temp_hp"].(float64)
			foldTemp -= int(absorbed)
			if land := int(eff) - int(absorbed); land > 0 {
				if velren.HP-land < 0 {
					velren.HP = 0
				} else {
					velren.HP -= land
				}
			}
		case "heal":
			velren.HP += e.Amount
			if velren.HP > velren.MaxHP {
				velren.HP = velren.MaxHP
			}
		}
	}
	_, order := mustActive(t, h, ctx)
	live := byName(t, order, "Velren")
	if live.HP != velren.HP {
		t.Fatalf("fold %d != column %d", velren.HP, live.HP)
	}
	if live.TempHP != 1 { // 5 granted, 4 drunk
		t.Fatalf("temp fold: %d", live.TempHP)
	}
	if !byName(t, order, "Goblin A").Dead {
		t.Fatalf("the fold lost a death")
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestUnstructuredPCFightsOnZerosWithAWarning(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	start, err := h.store.Start(ctx, h.campaign, StartInput{PCs: []string{h.nyx}}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(start.Warnings) != 1 || !strings.Contains(start.Warnings[0], "Nyx") {
		t.Fatalf("warnings: %v", start.Warnings)
	}
	nyx := byName(t, start.Order, "Nyx")
	if nyx.MaxHP != 0 || nyx.AC != 0 || nyx.Snapshot.InitBonus != 0 {
		t.Fatalf("zeros: %+v", nyx)
	}
	// Nyx still rolls initiative and fights.
	if nyx.Initiative < 1 || nyx.Initiative > 20 {
		t.Fatalf("initiative: %d", nyx.Initiative)
	}
}

func TestCombatNamesAndLettering(t *testing.T) {
	if instanceName("Goblin", 1, 0) != "Goblin" {
		t.Fatalf("one goblin is Goblin")
	}
	if instanceName("Goblin", 3, 1) != "Goblin B" {
		t.Fatalf("the second of three is Goblin B")
	}
	if letters(25) != "Z" || letters(26) != "AA" || letters(51) != "AZ" || letters(52) != "BA" {
		t.Fatalf("lettering: %s %s %s %s", letters(25), letters(26), letters(51), letters(52))
	}
	h := newHarness(t)
	start, err := h.store.Start(context.Background(), h.campaign, StartInput{
		Monsters: []MonsterLine{{Name: "Goblin", Count: 2}},
	}, "keeper")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := start.Combat.Name; got != fmt.Sprintf("Combat — %d on the other side", 2) {
		t.Fatalf("default name: %q", got)
	}
}
