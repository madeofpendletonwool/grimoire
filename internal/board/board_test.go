package board

// The board's tests (MAD-423): the config vocabulary, the health words,
// and the snapshot builder's visibility contract over a private
// migrated database wired the way runServe wires it. The leak posture
// is asserted on serialized JSON, not struct fields — absence is the
// guarantee, and a zero is not an absence.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/effects"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/pubsub"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// harness is one private database with the mechanical stack wired.
type harness struct {
	db        *sql.DB
	campaigns *campaign.Store
	ledgers   *ledger.Store
	fx        *effects.Store
	combats   *combat.Store
	board     *Store
	broker    *pubsub.Broker
	campaign  string
	wizard    string // Velren: wizard 5, hp 32, slots 4/3/2
	fighter   string // Brak: fighter 5, hp 44, no slots
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testdb.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	must := func(err error) {
		if err != nil {
			t.Fatalf("harness: %v", err)
		}
	}
	for _, u := range []struct{ id, name string }{
		{"keeper", "keeper"}, {"dm", "dm"}, {"p1", "mira"}, {"p2", "brak"},
	} {
		admin := 0
		if u.id == "keeper" {
			admin = 1
		}
		if _, err := db.Exec(
			`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES (?, ?, 'x', ?, 0)`,
			u.id, u.name, admin); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}

	campaigns, err := campaign.New(db)
	must(err)
	sessions, err := gamesession.New(db)
	must(err)
	roller, err := dice.New(db, campaigns, sessions)
	must(err)
	fx, err := effects.New(db, campaigns, nil)
	must(err)
	offlineCanon, err := canon.NewOffline(db)
	must(err)
	ledgers, err := ledger.New(db, campaigns, offlineCanon)
	must(err)
	combats, err := combat.New(db, campaigns, sessions, roller)
	must(err)
	combats = combats.WithEffects(fx).WithHitPoints(ledgers)

	broker := pubsub.New()
	campaigns.WithBroker(broker)
	ledgers.WithBroker(broker)
	roller.WithBroker(broker)
	fx.WithBroker(broker)
	combats.WithBroker(broker)

	boards, err := New(campaigns, ledgers, fx, combats, userNames{})
	must(err)
	boards.WithBroker(broker)

	ctx := context.Background()
	c, err := campaigns.CreateCampaign(ctx, "keeper", "The Ashen Court", "D&D 5e", "")
	must(err)
	must(campaigns.AddMember(ctx, c.ID, "dm", campaign.RoleDM, ""))
	must(campaigns.AddMember(ctx, c.ID, "p1", campaign.RolePlayer, ""))
	must(campaigns.AddMember(ctx, c.ID, "p2", campaign.RolePlayer, ""))

	mkPC := func(name string, s sheet.Sheet) string {
		e, err := campaigns.CreateEntity(ctx, c.ID, campaign.KindPC, name, "", campaign.WithSheet(nil, s))
		must(err)
		must(ledgers.SyncEntity(ctx, c.ID, e.ID))
		return e.ID
	}
	wizard := mkPC("Velren", sheet.Sheet{
		Classes:      []sheet.ClassLevel{{Class: "Wizard", Level: 5}},
		Spellcasting: &sheet.Spellcasting{Slots: map[string]int{"1": 4, "2": 3, "3": 2}},
		MaxHP:        32, AC: 13,
	})
	fighter := mkPC("Brak", sheet.Sheet{
		Classes: []sheet.ClassLevel{{Class: "Fighter", Level: 5}},
		MaxHP:   44, AC: 18,
	})
	// The player plays the wizard.
	// The players play the wizard and the fighter.
	must(campaigns.SetMemberCharacter(ctx, c.ID, "p1", wizard))
	must(campaigns.SetMemberCharacter(ctx, c.ID, "p2", fighter))

	return &harness{
		db: db, campaigns: campaigns, ledgers: ledgers, fx: fx, combats: combats,
		board: boards, broker: broker, campaign: c.ID,
		wizard: wizard, fighter: fighter,
	}
}

// userNames is the Users double: ids are already usernames here.
type userNames struct{}

func (userNames) Usernames(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		out[id] = id
	}
	return out, nil
}

// mustJSON serializes a member strip for absence assertions.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

/* ---------- the config ---------- */

func TestConfigParse(t *testing.T) {
	if cfg, err := Parse(nil); err != nil || cfg != Default() {
		t.Fatalf("absent config = %+v, %v; want default", cfg, err)
	}
	cfg, err := Parse(map[string]any{"hp": "word", "slots": "private"})
	if err != nil || cfg.HP != HPWord || cfg.Slots != SlotsPrivate {
		t.Fatalf("valid config = %+v, %v", cfg, err)
	}
	for _, bad := range []any{
		"nope", 7,
		map[string]any{"hp": "numbers"},
		map[string]any{"hp": "word", "slots": "sometimes"},
		map[string]any{"hp": true},
	} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("parse(%v) accepted a bad config", bad)
		}
	}
	// Failing closed: an unparseable stored value hides.
	if cfg := ConfigOf(map[string]any{"hp": "whenever"}); cfg != Closed() {
		t.Fatalf("bad stored config = %+v, want closed", cfg)
	}
	// A partial config keeps the named fields and defaults the rest.
	if cfg := ConfigOf(map[string]any{"hp": "word"}); cfg.HP != HPWord || cfg.Slots != SlotsVisible {
		t.Fatalf("partial config = %+v", cfg)
	}
}

/* ---------- the health words ---------- */

func TestHealthWord(t *testing.T) {
	cases := []struct {
		hp, max int
		down    bool
		dead    bool
		want    string
	}{
		{32, 32, false, false, "unhurt"},
		{31, 32, false, false, "hurt"},
		{16, 32, false, false, "bloodied"}, // exactly half is bloodied
		{2, 32, false, false, "bloodied"},
		{0, 32, false, false, "down"},
		{5, 32, true, false, "down"},
		{5, 32, false, true, "dead"},
		{0, 0, false, false, ""}, // no known max: no word to say
	}
	for _, tc := range cases {
		if got := HealthWord(tc.hp, tc.max, tc.down, tc.dead); got != tc.want {
			t.Fatalf("HealthWord(%d/%d down=%v dead=%v) = %q, want %q",
				tc.hp, tc.max, tc.down, tc.dead, got, tc.want)
		}
	}
}

/* ---------- the snapshot's shapes ---------- */

func TestSnapshotDMSeesExact(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Velren is hurt and out of 3rd-level slots: the warnings carry it.
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, poolID(t, h, h.wizard, "hp:hp"),
		ledger.TxnInput{Kind: ledger.TxnSpend, Amount: 10, Note: "goblin arrow"}, "dm"); err != nil {
		t.Fatalf("hurt velren: %v", err)
	}
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, poolID(t, h, h.wizard, "slot:3"),
		ledger.TxnInput{Kind: ledger.TxnSpend, Amount: 2, Note: "fireball x2"}, "p1"); err != nil {
		t.Fatalf("spend slots: %v", err)
	}

	snap, err := h.board.Snapshot(ctx, h.campaign, DMStanding())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.DM || snap.Config != Default() {
		t.Fatalf("dm snapshot header = %+v", snap)
	}
	if len(snap.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(snap.Members))
	}
	var velren *Member
	for i := range snap.Members {
		if snap.Members[i].CharacterID == h.wizard {
			velren = &snap.Members[i]
		}
	}
	if velren == nil {
		t.Fatal("velren missing from the board")
	}
	if velren.HP == nil || *velren.HP != 22 || velren.MaxHP == nil || *velren.MaxHP != 32 {
		t.Fatalf("velren hp = %+v", velren)
	}
	if velren.Classes == "" || velren.Level != 5 || velren.AC != 13 {
		t.Fatalf("velren sheet line = %+v", velren)
	}
	if len(velren.Slots) != 3 || velren.Slots[2].Left != 0 || velren.Slots[2].Max != 2 {
		t.Fatalf("velren slots = %+v", velren.Slots)
	}
	joined := strings.Join(velren.Warnings, "; ")
	if !strings.Contains(joined, "3rd-level slots") {
		t.Fatalf("velren warnings = %q, want out-of-3rd", joined)
	}
}

func TestSnapshotWordModeHidesNumbers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setConfig(t, h, Config{HP: HPWord, Slots: SlotsPrivate})

	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, poolID(t, h, h.wizard, "hp:hp"),
		ledger.TxnInput{Kind: ledger.TxnSpend, Amount: 18, Note: "trap"}, "dm"); err != nil {
		t.Fatalf("hurt velren: %v", err)
	}

	// A player views the party: nobody's strip carries numbers, and the
	// wizard is bloodied (14/32 ≤ half).
	snap, err := h.board.Snapshot(ctx, h.campaign, PlayerStanding(h.wizard))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, m := range snap.Members {
		if m.CharacterID == h.wizard {
			continue // the viewer's own strip is exact by design
		}
		body := mustJSON(t, m)
		for _, leak := range []string{`"hp":`, `"max_hp":`, `"temp_hp":`, `"slots":`, `"warnings":`} {
			if strings.Contains(body, leak) {
				t.Fatalf("word+private mode: %s's strip leaked %q: %s", m.Name, leak, body)
			}
		}
	}
	// The wizard is bloodied (14/32 <= half) — their own word read is
	// the one place the word appears for them; here the other viewer
	// sees it via a fresh standing.
	otherSnap, err := h.board.Snapshot(ctx, h.campaign, PlayerStanding(h.fighter))
	if err != nil {
		t.Fatalf("other snapshot: %v", err)
	}
	for _, m := range otherSnap.Members {
		if m.CharacterID == h.wizard && m.Health != "bloodied" {
			t.Fatalf("velren health = %q, want bloodied", m.Health)
		}
	}

	// The DM's read is untouched by the config: every number present.
	dmSnap, err := h.board.Snapshot(ctx, h.campaign, DMStanding())
	if err != nil {
		t.Fatalf("dm snapshot: %v", err)
	}
	for _, m := range dmSnap.Members {
		if m.HP == nil {
			t.Fatalf("dm strip for %s carries no hp", m.Name)
		}
	}
}

func TestSnapshotOwnStripAlwaysExact(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setConfig(t, h, Config{HP: HPWord, Slots: SlotsPrivate})

	// The wizard's player: their own strip is exact even in word mode —
	// 5e players know their own numbers — and their own slots ride even
	// in private mode.
	snap, err := h.board.Snapshot(ctx, h.campaign, PlayerStanding(h.wizard))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, m := range snap.Members {
		if m.CharacterID != h.wizard {
			continue
		}
		if m.HP == nil || *m.HP != 32 {
			t.Fatalf("own strip hp = %+v, want exact 32", m.HP)
		}
		if len(m.Slots) != 3 {
			t.Fatalf("own strip slots = %+v, want its own summary", m.Slots)
		}
		if m.Health != "" {
			t.Fatalf("own strip health word = %q, want empty (numbers already there)", m.Health)
		}
	}
}

func TestSnapshotSlotsPrivateHidesOthers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setConfig(t, h, Config{HP: HPExact, Slots: SlotsPrivate})

	snap, err := h.board.Snapshot(ctx, h.campaign, PlayerStanding(h.wizard))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, m := range snap.Members {
		if m.CharacterID == h.wizard && len(m.Slots) != 3 {
			t.Fatalf("own slots hidden in private mode: %+v", m)
		}
		if m.CharacterID == h.fighter {
			if strings.Contains(mustJSON(t, m), `"slots"`) {
				t.Fatal("another character's slots visible in private mode — a leak")
			}
		}
	}
}

/* ---------- conditions, concentration, combat ---------- */

func TestSnapshotConditionsAndConcentration(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.fx.Apply(ctx, h.campaign, effects.ApplyInput{
		TargetID: h.wizard, Kind: effects.KindCondition, Name: "poisoned",
		Duration: effects.Duration{Amount: 10, Unit: effects.UnitMinute},
	}, "dm"); err != nil {
		t.Fatalf("poison velren: %v", err)
	}
	if _, err := h.fx.Apply(ctx, h.campaign, effects.ApplyInput{
		TargetID: h.fighter, Kind: effects.KindSpell, Name: "bless",
		SourceID: h.wizard, Concentration: true,
		Duration: effects.Duration{Amount: 1, Unit: effects.UnitMinute},
	}, "dm"); err != nil {
		t.Fatalf("bless brak: %v", err)
	}

	snap, err := h.board.Snapshot(ctx, h.campaign, DMStanding())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, m := range snap.Members {
		if m.CharacterID == h.wizard {
			if len(m.Conditions) != 1 || m.Conditions[0].Name != "poisoned" {
				t.Fatalf("velren conditions = %+v", m.Conditions)
			}
			if m.Concentrating != "bless" {
				t.Fatalf("velren concentrating = %q, want bless", m.Concentrating)
			}
		}
	}
}

func TestSnapshotCombatAndMonsters(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A fight with a goblin on the foe side, resolved through the real
	// tracker. The statblock resolver is nil in this harness: pcs only
	// combat. Start instead with the two pcs and damage the wizard.
	res, err := h.combats.Start(ctx, h.campaign, combat.StartInput{
		Name: "The ambush", PCs: []string{h.wizard, h.fighter},
	}, "dm")
	if err != nil {
		t.Fatalf("start combat: %v", err)
	}
	var wizID string
	for _, c := range res.Order {
		if c.EntityID == h.wizard {
			wizID = c.ID
		}
	}
	if _, err := h.combats.Damage(ctx, h.campaign, res.Combat.ID, wizID, 26, "slashing", "goblin", false, "dm"); err != nil {
		t.Fatalf("damage velren: %v", err)
	}

	// Out of combat the wizard was full; in combat the strip reads the
	// fight: 12/32.
	playerSnap, err := h.board.Snapshot(ctx, h.campaign, PlayerStanding(h.wizard))
	if err != nil {
		t.Fatalf("player snapshot: %v", err)
	}
	if playerSnap.Combat == nil || playerSnap.Combat.Name != "The ambush" {
		t.Fatalf("combat summary = %+v", playerSnap.Combat)
	}
	if strings.Contains(mustJSON(t, playerSnap), `"monsters"`) {
		t.Fatal("a player snapshot carries the monster side — a leak")
	}
	for _, m := range playerSnap.Members {
		if m.CharacterID == h.wizard {
			if m.HP == nil || *m.HP != 6 {
				t.Fatalf("in-combat hp = %+v, want the combat row's 6", m.HP)
			}
			if len(m.Warnings) == 0 || !strings.Contains(m.Warnings[0], "6 hp") {
				t.Fatalf("low-hp warning = %v", m.Warnings)
			}
		}
	}

	if _, err := h.combats.NextTurn(ctx, h.campaign, res.Combat.ID, "dm"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	dmSnap, err := h.board.Snapshot(ctx, h.campaign, DMStanding())
	if err != nil {
		t.Fatalf("dm snapshot: %v", err)
	}
	if dmSnap.Combat == nil || dmSnap.Combat.Round != 1 || dmSnap.Combat.Turn == "" {
		t.Fatalf("dm combat summary = %+v", dmSnap.Combat)
	}
	var active int
	for _, e := range dmSnap.Combat.Order {
		if e.Active {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active entries = %d, want exactly one", active)
	}

	// Ending the fight writes the survivors' hp back to the ledger; the
	// next snapshot (no combat) reads the same numbers from the pool.
	if _, err := h.combats.End(ctx, h.campaign, res.Combat.ID, "won", "dm"); err != nil {
		t.Fatalf("end combat: %v", err)
	}
	after, err := h.board.Snapshot(ctx, h.campaign, DMStanding())
	if err != nil {
		t.Fatalf("post-combat snapshot: %v", err)
	}
	if after.Combat != nil {
		t.Fatal("the ended combat still shows on the board")
	}
	for _, m := range after.Members {
		if m.CharacterID == h.wizard {
			if m.HP == nil || *m.HP != 6 {
				t.Fatalf("post-combat hp = %+v, want the write-back's 6", m.HP)
			}
		}
	}

	// The next fight starts from the written-back number, not max.
	res2, err := h.combats.Start(ctx, h.campaign, combat.StartInput{
		Name: "Round two", PCs: []string{h.wizard},
	}, "dm")
	if err != nil {
		t.Fatalf("second combat: %v", err)
	}
	for _, c := range res2.Order {
		if c.EntityID == h.wizard && c.HP != 6 {
			t.Fatalf("second combat seeded hp %d, want 6 (the ledger's truth)", c.HP)
		}
	}
}

/* ---------- presence ---------- */

func TestPresenceMarksConnected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	snap, err := h.board.Snapshot(ctx, h.campaign, DMStanding())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Present) != 0 {
		t.Fatalf("presence before anyone connects = %v", snap.Present)
	}

	wake, stop := h.board.SubscribeAs(h.campaign, "p1")
	defer stop()
	snap, err = h.board.Snapshot(ctx, h.campaign, DMStanding())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Present) != 1 || snap.Present[0] != "p1" {
		t.Fatalf("presence = %v, want [p1]", snap.Present)
	}
	for _, m := range snap.Members {
		if m.CharacterID == h.wizard && !m.Present {
			t.Fatal("velren's strip does not show their player present")
		}
	}

	// The wake channel fires when a write lands through the shared
	// broker — the live contract the stream handler sleeps on.
	if _, _, err := h.ledgers.Apply(ctx, h.campaign, h.wizard, poolID(t, h, h.wizard, "hp:hp"),
		ledger.TxnInput{Kind: ledger.TxnSpend, Amount: 1, Note: "chip"}, "dm"); err != nil {
		t.Fatalf("spend: %v", err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("a ledger write did not wake the board subscriber")
	}
}

/* ---------- helpers ---------- */

func setConfig(t *testing.T, h *harness, cfg Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := h.campaigns.GetCampaign(ctx, h.campaign)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	settings := c.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	settings[SettingsKey] = cfg.SettingsValue()
	if _, err := h.campaigns.UpdateCampaign(ctx, "keeper", h.campaign, nil, nil, nil, nil, settings); err != nil {
		t.Fatalf("set config: %v", err)
	}
}

func poolID(t *testing.T, h *harness, entity, key string) string {
	t.Helper()
	ctx := context.Background()
	pools, err := h.ledgers.Pools(ctx, h.campaign, entity)
	if err != nil {
		t.Fatalf("pools: %v", err)
	}
	for _, p := range pools {
		if p.Key() == key {
			return p.ID
		}
	}
	t.Fatalf("no pool %s on %s", key, entity)
	return ""
}

var _ Ledger = (*ledger.Store)(nil)
var _ Effects = (*effects.Store)(nil)
var _ Combats = (*combat.Store)(nil)
var _ Users = userNames{}

// guard the unused-import set the harness's sql handle needs.
var _ = func() *sql.DB { return nil }
