package effects

// The effects store's row-level tests (MAD-421): apply with the
// concentration rule and supersession, the combat tick's persisting pass,
// and the world-clock derivation — day advances and real rests (written
// by the ledger, exactly as play writes them) expiring things correctly —
// over a private migrated database. The pure engine has its own tests.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// harness is one private database with the stack wired the way play wires
// it: campaigns, the ledger (whose rests are the world-clock events the
// derivation judges against), and the effects store with an index holding
// one seeded SRD condition entry.
type harness struct {
	campaigns *campaign.Store
	ledgers   *ledger.Store
	effects   *Store
	idx       *index.Store
	campaign  string
	thalia    string // the fighter things happen to
	evendur   string // the cleric who concentrates
	nyx       string // the warlock who also concentrates
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
	engine, err := canon.NewOffline(db)
	if err != nil {
		t.Fatalf("canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	ledgers, err := ledger.New(db, campaigns, engine)
	if err != nil {
		t.Fatalf("ledger store: %v", err)
	}
	engine = engine.WithRestFinalizer(ledgers)

	// The index on its own file, seeded with the SRD's Poisoned entry —
	// the real rules text the grounded condition must surface.
	idx, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("index store: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if _, err := idx.DB().Exec(`INSERT INTO docs (corpus, number, title, body, source) VALUES
		('dnd', 'conditions/0001', 'Conditions — Poisoned',
		 'A poisoned creature has disadvantage on attack rolls and ability checks.',
		 'SRD 5.1')`); err != nil {
		t.Fatalf("seed condition doc: %v", err)
	}

	effectsStore, err := New(db, campaigns, idx)
	if err != nil {
		t.Fatalf("effects store: %v", err)
	}

	ctx := context.Background()
	c, err := campaigns.CreateCampaign(ctx, "keeper", "The Ashen Court", "D&D 5e", "")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	mk := func(name string) string {
		e, err := campaigns.CreateEntity(ctx, c.ID, campaign.KindPC, name, "", nil)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return e.ID
	}
	return &harness{
		campaigns: campaigns, ledgers: ledgers, effects: effectsStore, idx: idx, campaign: c.ID,
		thalia: mk("Thalia"), evendur: mk("Evendur"), nyx: mk("Nyx"),
	}
}

func TestApplyGroundsConditionText(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	out, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindCondition, Name: "  POISONED ",
		Duration: Duration{Amount: 10, Unit: UnitRound},
	}, "keeper")
	if err != nil {
		t.Fatalf("apply poisoned: %v", err)
	}
	if out.Applied.Name != "poisoned" {
		t.Fatalf("condition name not canonicalized to the vocabulary's spelling: %q", out.Applied.Name)
	}
	if out.Applied.TargetName != "Thalia" {
		t.Fatalf("target name not resolved: %q", out.Applied.TargetName)
	}
	srd := h.effects.GroundCondition(ctx, "poisoned")
	if srd.Body == "" || srd.Ref == "" {
		t.Fatalf("the poisoned condition did not ground in the indexed SRD: %+v", srd)
	}
	if !strings.Contains(srd.Body, "disadvantage on attack rolls") {
		t.Fatalf("grounded text is not the real rules text: %q", srd.Body)
	}

	// The vocabulary is declared, not free text.
	if _, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindCondition, Name: "dizzy",
		Duration: Duration{Unit: UnitUntilRest},
	}, "keeper"); err == nil {
		t.Fatalf("free-text condition accepted")
	}
	// A bare condition is not concentrated on; the spell carries that.
	if _, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindCondition, Name: "grappled", Concentration: true,
		Duration: Duration{Unit: UnitUntilDispelled},
	}, "keeper"); err == nil {
		t.Fatalf("concentration on a bare condition accepted")
	}
	// Concentration needs its concentrator.
	if _, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindSpell, Name: "Hex", Concentration: true,
		Duration: Duration{Amount: 1, Unit: UnitHour},
	}, "keeper"); err == nil {
		t.Fatalf("sourceless concentration accepted")
	}
}

// TestConcentrationOneLinkAtATime is the acceptance fixture: a source
// concentrating on something new ends their previous link; a second
// source's link is untouched.
func TestConcentrationOneLinkAtATime(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	bless, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindSpell, Name: "Bless", SourceID: h.evendur, Concentration: true,
		Duration: Duration{Amount: 1, Unit: UnitMinute},
	}, "keeper")
	if err != nil {
		t.Fatalf("apply bless: %v", err)
	}
	hex, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.nyx, Kind: KindSpell, Name: "Hex", SourceID: h.evendur, Concentration: true,
		Duration: Duration{Amount: 1, Unit: UnitHour},
	}, "keeper")
	if err != nil {
		t.Fatalf("apply hex: %v", err)
	}
	if len(hex.BrokeConcentration) != 1 || hex.BrokeConcentration[0].ID != bless.Applied.ID {
		t.Fatalf("new concentration did not break the old link: %+v", hex.BrokeConcentration)
	}
	if hex.BrokeConcentration[0].EndReason != "" { // the returned row predates its own update
		t.Fatalf("pre-update row should not carry its end reason yet")
	}

	// The broken link is persisted as ended with its reason.
	broken, err := h.effects.Get(ctx, h.campaign, bless.Applied.ID, true)
	if err != nil {
		t.Fatalf("get bless: %v", err)
	}
	if broken.Status != StatusEnded || broken.EndReason != EndConcentrationBroken {
		t.Fatalf("bless after the break: %+v", broken)
	}

	// One link per source, but Nyx concentrating beside Evendur is fine.
	if _, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindSpell, Name: "Hold Person", SourceID: h.nyx, Concentration: true,
		Duration: Duration{Amount: 1, Unit: UnitMinute},
	}, "keeper"); err != nil {
		t.Fatalf("nyx concentrating beside evendur: %v", err)
	}
	links, err := h.effects.Concentrations(ctx, h.campaign)
	if err != nil {
		t.Fatalf("concentrations: %v", err)
	}
	sources := map[string]int{}
	for _, l := range links {
		sources[l.SourceID]++
	}
	if sources[h.evendur] != 1 || sources[h.nyx] != 1 {
		t.Fatalf("concentration links: %v", sources)
	}
}

func TestSupersedeLatestApplicationWins(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindCondition, Name: "poisoned",
		Duration: Duration{Amount: 10, Unit: UnitRound},
	}, "keeper")
	if err != nil {
		t.Fatalf("apply poisoned: %v", err)
	}
	second, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindCondition, Name: "Poisoned",
		Duration: Duration{Amount: 1, Unit: UnitHour},
	}, "keeper")
	if err != nil {
		t.Fatalf("re-apply poisoned: %v", err)
	}
	if len(second.Superseded) != 1 || second.Superseded[0].ID != first.Applied.ID {
		t.Fatalf("re-application did not supersede: %+v", second.Superseded)
	}
	old, err := h.effects.Get(ctx, h.campaign, first.Applied.ID, true)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if old.Status != StatusEnded || old.EndReason != EndSuperseded {
		t.Fatalf("superseded row: %+v", old)
	}

	// The active list holds exactly one poisoned, and it is the new one.
	active, err := h.effects.List(ctx, h.campaign, h.thalia, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var poisoned []*Row
	for i := range active {
		if active[i].Name == "poisoned" {
			poisoned = append(poisoned, &active[i])
		}
	}
	if len(poisoned) != 1 || poisoned[0].ID != second.Applied.ID {
		t.Fatalf("active poisoned rows: %+v", poisoned)
	}
	// A different effect on the same target was never involved.
	if _, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindSpell, Name: "Mage Armor", SourceID: h.evendur,
		Duration: Duration{Amount: 8, Unit: UnitHour},
	}, "keeper"); err != nil {
		t.Fatalf("apply mage armor: %v", err)
	}
	active, err = h.effects.List(ctx, h.campaign, h.thalia, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("overlapping effects on one target did not compose: %d rows", len(active))
	}
}

// TestAdvanceRoundsPersistsTheCombatClock: ten turn-ticks expire both
// spellings of sixty seconds and leave the persisted rows ended.
func TestAdvanceRoundsPersistsTheCombatClock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, fx := range []ApplyInput{
		{TargetID: h.thalia, Kind: KindSpell, Name: "Bless", SourceID: h.evendur, Concentration: true,
			Duration: Duration{Amount: 10, Unit: UnitRound}},
		{TargetID: h.thalia, Kind: KindSpell, Name: "Guiding Bolt", SourceID: h.evendur,
			Duration: Duration{Amount: 1, Unit: UnitMinute}},
		{TargetID: h.nyx, Kind: KindSpell, Name: "Hex", SourceID: h.nyx, Concentration: true,
			Duration: Duration{Amount: 1, Unit: UnitHour}},
	} {
		if _, err := h.effects.Apply(ctx, h.campaign, fx, "keeper"); err != nil {
			t.Fatalf("apply %s: %v", fx.Name, err)
		}
	}

	active, expired, err := h.effects.AdvanceRounds(ctx, h.campaign, 9)
	if err != nil {
		t.Fatalf("advance 9: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("nine turns expired something: %+v", expired)
	}
	byName := rowsByName(active)
	if rem := byName["Bless"].Running.Remaining; rem != 6 {
		t.Fatalf("bless after 9 rounds: %ds, want 6", rem)
	}
	if disp := byName["Bless"].Running.Display(); disp != "1 round" {
		t.Fatalf("bless display: %q", disp)
	}
	if rem := byName["Guiding Bolt"].Running.Remaining; rem != 6 {
		t.Fatalf("guiding bolt (1 minute) after 9 rounds: %ds, want 6 — the boundary must hold on the store path too", rem)
	}

	active, expired, err = h.effects.AdvanceRounds(ctx, h.campaign, 1)
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if len(expired) != 2 {
		t.Fatalf("the tenth turn must expire both sixty-second spellings: %+v", expired)
	}
	for _, r := range expired {
		if r.EndReason != EndExpired {
			t.Fatalf("tick expiry reason: %q", r.EndReason)
		}
	}
	if len(active) != 1 || active[0].Name != "Hex" {
		t.Fatalf("hex must outlive ten turns: %+v", active)
	}
	if rem := active[0].Running.Remaining; rem != HourSeconds-60 {
		t.Fatalf("hex remaining after ten turns: %d", rem)
	}

	// The expiries are persisted, not just derived.
	ended, err := h.effects.List(ctx, h.campaign, h.thalia, true)
	if err != nil {
		t.Fatalf("list ended: %v", err)
	}
	endedCount := 0
	for _, r := range ended {
		if r.Status == StatusEnded && r.EndReason == EndExpired {
			endedCount++
		}
	}
	if endedCount != 2 {
		t.Fatalf("persisted tick expiries: %d", endedCount)
	}

	// A turn dispels nothing: until-dispelled survives any number of ticks.
	if _, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindSpell, Name: "Unseen Servant", SourceID: h.evendur,
		Duration: Duration{Unit: UnitUntilDispelled},
	}, "keeper"); err != nil {
		t.Fatalf("apply unseen servant: %v", err)
	}
	if _, _, err := h.effects.AdvanceRounds(ctx, h.campaign, 1000); err != nil {
		t.Fatalf("advance 1000: %v", err)
	}
	active, err = h.effects.List(ctx, h.campaign, h.thalia, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !hasName(active, "Unseen Servant") {
		t.Fatalf("an until-dispelled effect expired on turns alone: %+v", active)
	}
}

// TestWorldClockDerivation is the world-clock acceptance fixture: a day
// of travel expires the hours, decrements the days, and touches nothing
// that only a rest or a dispel can end; the ledger's real long rest
// (clock advance and rest row, written exactly as play writes them) ends
// the until-rest effect.
func TestWorldClockDerivation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	mk := func(in ApplyInput) *Row {
		out, err := h.effects.Apply(ctx, h.campaign, in, "keeper")
		if err != nil {
			t.Fatalf("apply %s: %v", in.Name, err)
		}
		return &out.Applied
	}
	mk(ApplyInput{TargetID: h.thalia, Kind: KindSpell, Name: "Mage Armor", SourceID: h.evendur,
		Duration: Duration{Amount: 8, Unit: UnitHour}})
	mk(ApplyInput{TargetID: h.thalia, Kind: KindSpell, Name: "Geas", SourceID: h.evendur,
		Duration: Duration{Amount: 10, Unit: UnitDay}})
	mk(ApplyInput{TargetID: h.thalia, Kind: KindFeature, Name: "Blessed by the spring",
		Duration: Duration{Unit: UnitUntilRest}})
	mk(ApplyInput{TargetID: h.thalia, Kind: KindSpell, Name: "Unseen Servant", SourceID: h.evendur,
		Duration: Duration{Unit: UnitUntilDispelled}})

	// A day of travel on the campaign clock.
	if _, _, err := h.campaigns.AdvanceClockBy(ctx, h.campaign, 1, campaign.AdvanceTravel, "the road to Emberhollow", "", "keeper"); err != nil {
		t.Fatalf("travel: %v", err)
	}
	rows, err := h.effects.List(ctx, h.campaign, h.thalia, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byName := rowsByName(rows)
	if r := byName["Mage Armor"]; !r.Running.Ended || r.Running.EndReason != EndExpired {
		t.Fatalf("mage armor survived a day of travel: %+v", r.Running)
	}
	if r := byName["Geas"]; r.Running.Ended || r.Running.Remaining != 9*DaySeconds || r.Running.Display() != "9 days" {
		t.Fatalf("geas after a day of travel: %+v", r.Running)
	}
	for _, name := range []string{"Blessed by the spring", "Unseen Servant"} {
		if r := byName[name]; r.Running.Ended {
			t.Fatalf("%s ended without its event: %+v", name, r.Running)
		}
	}

	// The night: a real long rest through the ledger — the clock advance
	// and the rest row, exactly as play writes them. The rest must land
	// strictly after the effects' applied_at (ms-resolution clocks), hence
	// the beat of sleep: a rest ties no one.
	time.Sleep(2 * time.Millisecond)
	if _, _, err := h.ledgers.Rest(ctx, h.campaign, []string{h.thalia}, ledger.RestLong, "", "the party sleeps", "keeper"); err != nil {
		t.Fatalf("long rest: %v", err)
	}
	rows, err = h.effects.List(ctx, h.campaign, h.thalia, true)
	if err != nil {
		t.Fatalf("list after rest: %v", err)
	}
	byName = rowsByName(rows)
	if r := byName["Blessed by the spring"]; !r.Running.Ended || r.Running.EndReason != EndRest {
		t.Fatalf("until-rest effect survived the rest: %+v", r.Running)
	}
	if r := byName["Unseen Servant"]; r.Running.Ended {
		t.Fatalf("until-dispelled effect ended against time: %+v", r.Running)
	}
	if r := byName["Geas"]; r.Running.Ended || r.Running.Remaining != 8*DaySeconds {
		t.Fatalf("geas after the night: %+v", r.Running)
	}

	// A short rest alone (no clock move) still ends until-rest rows:
	// apply a fresh one, rest short, and watch it go.
	fresh := mk(ApplyInput{TargetID: h.nyx, Kind: KindCondition, Name: "poisoned",
		Duration: Duration{Unit: UnitUntilRest}})
	time.Sleep(2 * time.Millisecond)
	if _, _, err := h.ledgers.Rest(ctx, h.campaign, []string{h.nyx}, ledger.RestShort, "", "", "keeper"); err != nil {
		t.Fatalf("short rest: %v", err)
	}
	row, err := h.effects.Get(ctx, h.campaign, fresh.ID, true)
	if err != nil {
		t.Fatalf("get fresh: %v", err)
	}
	if !row.Running.Ended || row.Running.EndReason != EndRest {
		t.Fatalf("until-rest row after a short rest: %+v", row.Running)
	}
}

func TestEndByHand(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	out, err := h.effects.Apply(ctx, h.campaign, ApplyInput{
		TargetID: h.thalia, Kind: KindSpell, Name: "Unseen Servant", SourceID: h.evendur,
		Duration: Duration{Unit: UnitUntilDispelled},
	}, "keeper")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	ended, err := h.effects.End(ctx, h.campaign, out.Applied.ID, "dispelled", "nyx-player")
	if err != nil {
		t.Fatalf("dispel: %v", err)
	}
	if ended.Status != StatusEnded || ended.EndReason != "dispelled" || ended.EndedBy != "nyx-player" {
		t.Fatalf("dispelled row: %+v", ended)
	}
	if !ended.EndedAt.After(time.Time{}) {
		t.Fatalf("ended_at not set")
	}

	// Ending is not deleting: the row reads in history.
	if _, err := h.effects.End(ctx, h.campaign, out.Applied.ID, "dispelled", "keeper"); err == nil {
		t.Fatalf("double end accepted")
	}
	rows, err := h.effects.List(ctx, h.campaign, h.thalia, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != StatusEnded {
		t.Fatalf("history after dispel: %+v", rows)
	}
	if _, err := h.effects.End(ctx, h.campaign, out.Applied.ID, "vibes", "keeper"); err == nil {
		t.Fatalf("unknown end reason accepted")
	}
}

func TestListScopesByTarget(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, fx := range []ApplyInput{
		{TargetID: h.thalia, Kind: KindCondition, Name: "prone", Duration: Duration{Amount: 1, Unit: UnitRound}},
		{TargetID: h.nyx, Kind: KindCondition, Name: "frightened", Duration: Duration{Amount: 1, Unit: UnitRound}},
		{TargetID: h.nyx, Kind: KindSpell, Name: "Hex", SourceID: h.nyx, Concentration: true,
			Duration: Duration{Amount: 1, Unit: UnitHour}},
	} {
		if _, err := h.effects.Apply(ctx, h.campaign, fx, "keeper"); err != nil {
			t.Fatalf("apply %s: %v", fx.Name, err)
		}
	}
	thalias, err := h.effects.List(ctx, h.campaign, h.thalia, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(thalias) != 1 || thalias[0].Name != "prone" {
		t.Fatalf("thalia's rows: %+v", thalias)
	}
	all, err := h.effects.List(ctx, h.campaign, "", false)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("campaign-wide rows: %d", len(all))
	}
}

/* ---------- helpers ---------- */

func rowsByName(rows []Row) map[string]Row {
	out := make(map[string]Row, len(rows))
	for _, r := range rows {
		out[r.Name] = r
	}
	return out
}

func hasName(rows []Row, name string) bool {
	for _, r := range rows {
		if r.Name == name {
			return true
		}
	}
	return false
}
