package stats

// The reads' tests (MAD-428): the logs the earlier stages wrote — dice
// rows, the combat journal, the ledger's transactions — folded into the
// same numbers from either scope (campaign or session), with the
// player's fold excluding a secret roll in the query, the way every
// leak test in this codebase insists.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

type harness struct {
	db        *sql.DB
	campaigns *campaign.Store
	sessions  *gamesession.Store
	roller    *dice.Store
	combats   *combat.Store
	ledgers   *ledger.Store
	stats     *Store

	campaign string
	session  string
	velren   string
	nyx      string
	goblin   string // an npc entity rolls aim at
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
	knowledgeStore, err := knowledge.New(db)
	if err != nil {
		t.Fatalf("knowledge store: %v", err)
	}
	sessions, err := gamesession.New(db)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	roller, err := dice.New(db, campaigns, sessions)
	if err != nil {
		t.Fatalf("dice store: %v", err)
	}
	engine, err := canon.NewOffline(db)
	if err != nil {
		t.Fatalf("canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	ledgers, err := ledger.New(db, campaigns, engine)
	if err != nil {
		t.Fatalf("ledger store: %v", err)
	}
	combats, err := combat.New(db, campaigns, sessions, roller)
	if err != nil {
		t.Fatalf("combat store: %v", err)
	}
	statsStore, err := New(db)
	if err != nil {
		t.Fatalf("stats store: %v", err)
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
	mkPC := func(name string, s sheet.Sheet) string {
		e, err := campaigns.CreateEntity(ctx, c.ID, campaign.KindPC, name, "", campaign.WithSheet(nil, s))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return e.ID
	}
	goblin, err := campaigns.CreateEntity(ctx, c.ID, campaign.KindNPC, "Goblin", "", nil)
	if err != nil {
		t.Fatalf("create goblin: %v", err)
	}
	return &harness{
		db: db, campaigns: campaigns, sessions: sessions, roller: roller,
		combats: combats, ledgers: ledgers, stats: statsStore,
		campaign: c.ID, session: live.ID,
		velren: mkPC("Velren", sheet.Sheet{Classes: []sheet.ClassLevel{{Class: "Ranger", Level: 3}}, MaxHP: 28, AC: 15}),
		nyx:    mkPC("Nyx", sheet.Sheet{Classes: []sheet.ClassLevel{{Class: "Warlock", Level: 3}}, MaxHP: 24, AC: 13}),
		goblin: goblin.ID,
	}
}

func byCombatantName(t *testing.T, order []combat.Combatant, name string) combat.Combatant {
	t.Helper()
	for _, c := range order {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no combatant %s", name)
	return combat.Combatant{}
}

// seedNight writes one sitting's mechanical history: a fight between
// the two pcs with attributed damage, attack rolls aimed at the goblin
// (one of them secret), an open check, and an inspiration spend linked
// to the roll that spent it. Returns the inspiration transaction.
func (h *harness) seedNight(t *testing.T) *ledger.TxnRow {
	t.Helper()
	ctx := context.Background()

	start, err := h.combats.Start(ctx, h.campaign, combat.StartInput{
		PCs: []string{h.velren, h.nyx}, SessionID: h.session,
	}, "keeper")
	if err != nil {
		t.Fatalf("start combat: %v", err)
	}
	velren := byCombatantName(t, start.Order, "Velren")
	nyx := byCombatantName(t, start.Order, "Nyx")
	// Velren takes 10 from Nyx's hex and 6 from an unnamed source; Nyx
	// takes 15 from Velren's blade.
	if _, err := h.combats.Damage(ctx, h.campaign, start.Combat.ID, velren.ID, 10, "", "", false, nyx.ID, "keeper"); err != nil {
		t.Fatalf("hex damage: %v", err)
	}
	if _, err := h.combats.Damage(ctx, h.campaign, start.Combat.ID, velren.ID, 6, "", "", false, "", "keeper"); err != nil {
		t.Fatalf("trap damage: %v", err)
	}
	if _, err := h.combats.Damage(ctx, h.campaign, start.Combat.ID, nyx.ID, 15, "", "", false, velren.ID, "keeper"); err != nil {
		t.Fatalf("blade damage: %v", err)
	}

	// Rolls: two public attacks at the goblin, one secret attack (the
	// DM's hidden archer), one open check — plus the two initiative
	// rolls the tracker already rolled in Start.
	var anAttack *dice.RollRow
	for _, in := range []dice.Input{
		{Formula: "1d20+5", ContextKind: dice.ContextAttack, CharacterID: h.velren, TargetID: h.goblin, Actor: "keeper"},
		{Formula: "1d20+3", ContextKind: dice.ContextAttack, CharacterID: h.nyx, TargetID: h.goblin, Actor: "keeper"},
		{Formula: "1d20+7", ContextKind: dice.ContextAttack, CharacterID: h.velren, TargetID: h.goblin, Actor: "keeper", Visibility: dice.VisibilitySecret},
		{Formula: "1d20+2", ContextKind: dice.ContextCheck, CharacterID: h.nyx, Actor: "keeper"},
	} {
		row, err := h.roller.Roll(ctx, h.campaign, in)
		if err != nil {
			t.Fatalf("roll %+v: %v", in, err)
		}
		if anAttack == nil {
			anAttack = row
		}
	}

	// Inspiration: awarded, spent on the roll that carried advantage,
	// linked to its event the way the roll flow links it.
	if _, _, err := h.ledgers.AwardInspiration(ctx, h.campaign, h.velren, "", "keeper"); err != nil {
		t.Fatalf("award inspiration: %v", err)
	}
	txn, err := h.ledgers.TrySpendInspiration(ctx, h.campaign, h.velren, "", "keeper")
	if err != nil {
		t.Fatalf("spend inspiration: %v", err)
	}
	if err := h.ledgers.LinkTxnEvent(ctx, txn.ID, anAttack.SessionEventID, anAttack.SessionID); err != nil {
		t.Fatalf("link spend: %v", err)
	}
	return txn
}

func TestSessionFold(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNight(t)

	st, err := h.stats.Session(ctx, h.session, true)
	if err != nil {
		t.Fatalf("session fold: %v", err)
	}
	// 4 explicit rolls plus the tracker's initiative (one per combatant,
	// more when the seeded dice tie and demand a tiebreak).
	if st.Rolls.ByContext["attack"] != 3 || st.Rolls.ByContext["check"] != 1 || st.Rolls.ByContext["initiative"] < 2 {
		t.Fatalf("roll counts: %+v", st.Rolls)
	}
	if st.Rolls.Total != 4+st.Rolls.ByContext["initiative"] {
		t.Fatalf("roll total %d does not match its contexts %+v", st.Rolls.Total, st.Rolls.ByContext)
	}
	if st.PartyDamageTaken != 31 || st.PartyDamageDealt != 25 {
		t.Fatalf("party totals: taken %d dealt %d, want 31/25", st.PartyDamageTaken, st.PartyDamageDealt)
	}
	var velren, nyx *CharacterStat
	for i := range st.Characters {
		switch st.Characters[i].Name {
		case "Velren":
			velren = &st.Characters[i]
		case "Nyx":
			nyx = &st.Characters[i]
		}
	}
	if velren == nil || nyx == nil {
		t.Fatalf("characters: %+v", st.Characters)
	}
	if velren.Taken != 16 || velren.Dealt != 15 || velren.Share != 51.6 {
		t.Fatalf("velren: %+v", velren)
	}
	if nyx.Taken != 15 || nyx.Dealt != 10 || nyx.Share != 48.4 {
		t.Fatalf("nyx: %+v", nyx)
	}
	if len(st.Targets) != 1 || st.Targets[0].Name != "Goblin" || st.Targets[0].Rolls != 3 {
		t.Fatalf("targets: %+v", st.Targets)
	}
	if st.InspirationSpends != 1 {
		t.Fatalf("inspiration spends: %d", st.InspirationSpends)
	}
	if st.Empty() || st.Markdown() == "" {
		t.Fatal("a full session must render numbers")
	}
}

// TestPlayerFoldExcludesSecretRolls is the leak line: the player's
// numbers come from the same queries the feed uses — a secret roll is
// absent from the fold, not merely unprinted.
func TestPlayerFoldExcludesSecretRolls(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNight(t)

	dm, err := h.stats.Session(ctx, h.session, true)
	if err != nil {
		t.Fatalf("dm fold: %v", err)
	}
	player, err := h.stats.Session(ctx, h.session, false)
	if err != nil {
		t.Fatalf("player fold: %v", err)
	}
	if dm.Rolls.Total-player.Rolls.Total != 1 {
		t.Fatalf("dm sees %d rolls, player %d — the secret is not the difference", dm.Rolls.Total, player.Rolls.Total)
	}
	if dm.Rolls.ByContext["attack"] != 3 || player.Rolls.ByContext["attack"] != 2 {
		t.Fatalf("attack counts: dm %d player %d", dm.Rolls.ByContext["attack"], player.Rolls.ByContext["attack"])
	}
	for _, tg := range player.Targets {
		if tg.Rolls > 2 {
			t.Fatalf("player target counts include the secret roll: %+v", player.Targets)
		}
	}
}

// TestCampaignFoldSeesEverySession: the endcap is the campaign's whole
// history — the seeded sitting plus a roll that happened with no
// session open (a quieter moment between sittings).
func TestCampaignFoldSeesEverySession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNight(t)

	// End the sitting, then roll out-of-session.
	done := gamesession.StatusDone
	if _, err := h.sessions.UpdateSession(ctx, h.session, nil, &done); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if _, err := h.roller.Roll(ctx, h.campaign, dice.Input{
		Formula: "1d20+4", ContextKind: dice.ContextCheck, CharacterID: h.velren, Actor: "keeper",
	}); err != nil {
		t.Fatalf("quiet roll: %v", err)
	}

	session, err := h.stats.Session(ctx, h.session, true)
	if err != nil {
		t.Fatalf("session fold: %v", err)
	}
	whole, err := h.stats.Campaign(ctx, h.campaign, true)
	if err != nil {
		t.Fatalf("campaign fold: %v", err)
	}
	if whole.Rolls.Total != session.Rolls.Total+1 {
		t.Fatalf("campaign rolls %d, session %d + 1 quiet", whole.Rolls.Total, session.Rolls.Total)
	}
	// The quiet roll has no session: the sitting's fold does not see it.
	if quiet, err := h.stats.Session(ctx, h.session, false); err != nil || quiet.Rolls.Total != session.Rolls.Total-1 {
		t.Fatalf("quiet roll leaked into the session fold: %d (err %v)", quiet.Rolls.Total, err)
	}
}

func TestEmptySessionFoldsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	other, err := h.sessions.CreateSession(ctx, h.campaign, "The Quiet One")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	st, err := h.stats.Session(ctx, other.ID, true)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if !st.Empty() || st.Markdown() != "" {
		t.Fatalf("a quiet session rendered %q", st.Markdown())
	}
}

func TestUnknownSessionIsAnError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.stats.Session(context.Background(), "no-such-session", true); err == nil {
		t.Fatal("folding an unknown session must error")
	}
}
