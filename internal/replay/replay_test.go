package replay

// The fold's tests (MAD-426): one whole battle written as a journal —
// the same payloads the tracker writes — folded to its end, scrubbed
// at points along the way, and pinned against a golden canonical
// state. Drift is asserted loudly: a journal that disagrees with the
// grammar it records is an error, never a patch-up. No database, no
// clock, no network.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/combat"
)

func velren() combat.Combatant {
	return combat.Combatant{
		ID: "velren", Name: "Velren", Side: combat.SideParty, Kind: combat.KindPC,
		Initiative: 22, AC: 15, MaxHP: 28, HP: 28, Position: 0,
		Snapshot: combat.Snapshot{Resist: []string{"fire"}},
	}
}

func goblinA() combat.Combatant {
	return combat.Combatant{
		ID: "goblin-a", Name: "Goblin A", Side: combat.SideFoe, Kind: combat.KindMonster,
		Initiative: 15, AC: 15, MaxHP: 7, HP: 7, Position: 1,
	}
}

func goblinB() combat.Combatant {
	return combat.Combatant{
		ID: "goblin-b", Name: "Goblin B", Side: combat.SideFoe, Kind: combat.KindMonster,
		Initiative: 12, AC: 15, MaxHP: 7, HP: 7, Position: 2,
		Snapshot: combat.Snapshot{LegendaryMax: 2},
	}
}

func whiskers() combat.Combatant {
	return combat.Combatant{
		ID: "whiskers", Name: "Whiskers", Side: combat.SideParty, Kind: combat.KindCompanion,
		Initiative: 9, AC: 13, MaxHP: 11, HP: 11, Position: 3,
	}
}

// journal is the battle the tracker would have written: every payload
// exactly as it lands in combat_log (numbers as JSON decodes them).
func journal() []Event {
	return []Event{
		{Seq: 1, Kind: "start", Payload: map[string]any{"round": float64(1), "order": "Velren 22, Goblin A 15, Goblin B 12, Whiskers 9"}},
		{Seq: 2, Kind: "turn", CombatantID: "velren", Payload: map[string]any{"round": float64(1), "turn_index": float64(0), "combatant": "Velren"}},
		{Seq: 3, Kind: "damage", CombatantID: "goblin-a", Amount: 10, Payload: map[string]any{
			"asked": float64(10), "effective": float64(10), "before": float64(7), "after": float64(0), "died": true}},
		{Seq: 4, Kind: "damage", CombatantID: "velren", Amount: 3, Payload: map[string]any{
			"asked": float64(6), "damage_type": "fire", "effective": float64(3), "before": float64(28), "after": float64(25)}},
		{Seq: 5, Kind: "turn", CombatantID: "goblin-b", Payload: map[string]any{"round": float64(1), "turn_index": float64(2), "combatant": "Goblin B"}},
		{Seq: 6, Kind: "damage", CombatantID: "velren", Amount: 34, Payload: map[string]any{
			"asked": float64(34), "effective": float64(34), "before": float64(25), "after": float64(0), "carried_past_zero": float64(9), "went_down": true}},
		{Seq: 7, Kind: "death_save", CombatantID: "velren", Payload: map[string]any{
			"result": "success", "successes": float64(1), "failures": float64(0), "stable": false, "dead": false}},
		{Seq: 8, Kind: "heal", CombatantID: "velren", Amount: 5, Payload: map[string]any{
			"before": float64(0), "after": float64(5), "woke": true}},
		{Seq: 9, Kind: "condition", CombatantID: "goblin-b", Payload: map[string]any{
			"condition": "poisoned", "rounds": float64(2), "id": "goblin-b-cond-1"}, Actor: "keeper"},
		{Seq: 10, Kind: "temp_hp", CombatantID: "velren", Amount: 6, Payload: map[string]any{"temp_hp": float64(6)}},
		{Seq: 11, Kind: "turn", CombatantID: "whiskers", Payload: map[string]any{"round": float64(1), "turn_index": float64(3), "combatant": "Whiskers"}},
		{Seq: 12, Kind: "damage", CombatantID: "whiskers", Amount: 4, Payload: map[string]any{
			"asked": float64(4), "effective": float64(4), "before": float64(11), "after": float64(7)}},
		{Seq: 13, Kind: "legendary", CombatantID: "goblin-b", Payload: map[string]any{
			"ability": "Tail Attack", "cost": float64(1), "used": float64(1), "max": float64(2)}},
		{Seq: 14, Kind: "turn", CombatantID: "velren", Payload: map[string]any{
			"round": float64(2), "turn_index": float64(0), "new_round": float64(2), "combatant": "Velren"}},
		{Seq: 15, Kind: "reaction", CombatantID: "velren", Payload: map[string]any{"reaction_spent": true}},
		{Seq: 16, Kind: "legendary", CombatantID: "goblin-b", Payload: map[string]any{
			"ability": "Wing Attack", "cost": float64(1), "used": float64(2), "max": float64(2)}},
		{Seq: 17, Kind: "reveal", CombatantID: "goblin-b", Payload: map[string]any{"reveal": "hp"}},
		{Seq: 18, Kind: "condition", CombatantID: "whiskers", Payload: map[string]any{
			"condition": "frightened", "rounds": float64(1), "id": "whiskers-cond-1"}, Actor: "keeper"},
		{Seq: 19, Kind: "condition_end", CombatantID: "whiskers", Payload: map[string]any{
			"condition": "frightened", "id": "whiskers-cond-1"}},
		{Seq: 20, Kind: "end", Payload: map[string]any{
			"round": float64(2), "alive": map[string]any{"party": float64(2), "foe": float64(1)}}},
	}
}

func lineup() []combat.Combatant {
	return []combat.Combatant{velren(), goblinA(), goblinB(), whiskers()}
}

func foldAt(t *testing.T, events []Event, at int64) State {
	t.Helper()
	st, err := Fold(Seeds(lineup(), events), events, at)
	if err != nil {
		t.Fatalf("fold to %d: %v", at, err)
	}
	return st
}

func pick(t *testing.T, st State, id string) *combat.Combatant {
	t.Helper()
	for i := range st.Combatants {
		if st.Combatants[i].ID == id {
			return &st.Combatants[i]
		}
	}
	t.Fatalf("no combatant %s in the folded state", id)
	return nil
}

/* ---------- scrubbing ---------- */

func TestFoldOpeningLineup(t *testing.T) {
	events := journal()
	st := foldAt(t, events, 0)
	if st.Round != 1 || st.TurnIndex != -1 || st.Turn() != "" || st.Status != combat.StatusActive {
		t.Fatalf("opening state: round %d turn %d (%q) status %s", st.Round, st.TurnIndex, st.Turn(), st.Status)
	}
	for _, id := range []string{"velren", "goblin-a", "goblin-b", "whiskers"} {
		c := pick(t, st, id)
		if c.HP != c.MaxHP || c.Dead || c.Downed || c.ReactionSpent || c.LegendaryUsed != 0 || len(c.Conditions) != 0 {
			t.Fatalf("%s's opening state: %+v", id, c)
		}
	}
}

func TestFoldScrubPoints(t *testing.T) {
	events := journal()

	// After the first hit: Goblin A is dead, Velren untouched.
	st := foldAt(t, events, 3)
	if c := pick(t, st, "goblin-a"); !c.Dead || c.HP != 0 {
		t.Fatalf("goblin A after seq 3: %+v", c)
	}
	if c := pick(t, st, "velren"); c.HP != 28 {
		t.Fatalf("velren after seq 3: %+v", c)
	}

	// After the fire hit: resistance halved it.
	st = foldAt(t, events, 4)
	if c := pick(t, st, "velren"); c.HP != 25 {
		t.Fatalf("velren after seq 4 (resisted fire): %d hp, want 25", c.HP)
	}

	// After the big hit: down and dying, not dead — 9 past zero is not
	// massive against a 28 max.
	st = foldAt(t, events, 6)
	c := pick(t, st, "velren")
	if !c.Downed || c.Dead || c.HP != 0 {
		t.Fatalf("velren after seq 6: %+v", c)
	}

	// One death save in.
	st = foldAt(t, events, 7)
	if c := pick(t, st, "velren"); c.DeathSuccesses != 1 || c.DeathFailures != 0 || !c.Downed {
		t.Fatalf("velren after seq 7: %+v", c)
	}

	// The heal wakes and clears the ledger.
	st = foldAt(t, events, 8)
	if c := pick(t, st, "velren"); c.HP != 5 || c.Downed || c.DeathSuccesses != 0 || c.DeathFailures != 0 {
		t.Fatalf("velren after seq 8 (healed and woke): %+v", c)
	}

	// The condition stands at two rounds; the temp coat is on.
	st = foldAt(t, events, 10)
	if c := pick(t, st, "goblin-b"); len(c.Conditions) != 1 || c.Conditions[0].Name != "poisoned" || c.Conditions[0].Rounds != 2 {
		t.Fatalf("goblin B after seq 10: %+v", c.Conditions)
	}
	if c := pick(t, st, "velren"); c.TempHP != 6 {
		t.Fatalf("velren after seq 10: temp %d, want 6", c.TempHP)
	}

	// The legendary budget spent once, off-turn.
	st = foldAt(t, events, 13)
	if c := pick(t, st, "goblin-b"); c.LegendaryUsed != 1 {
		t.Fatalf("goblin B after seq 13: legendary %d, want 1", c.LegendaryUsed)
	}

	// The wrap: round 2, the condition worn to one round, the wolf
	// healed earlier hurt, and the turn pointer back on Velren.
	st = foldAt(t, events, 14)
	if st.Round != 2 || st.Turn() != "Velren" {
		t.Fatalf("after seq 14: round %d turn %q", st.Round, st.Turn())
	}
	if c := pick(t, st, "goblin-b"); len(c.Conditions) != 1 || c.Conditions[0].Rounds != 1 {
		t.Fatalf("goblin B's condition after the wrap: %+v", c.Conditions)
	}

	// The reaction, spent after the turn that would have reset it.
	st = foldAt(t, events, 15)
	if c := pick(t, st, "velren"); !c.ReactionSpent {
		t.Fatal("velren's reaction after seq 15: not spent")
	}

	// The reveal folds (it is on the row) but the fight is still live.
	st = foldAt(t, events, 17)
	if c := pick(t, st, "goblin-b"); c.Reveal != "hp" {
		t.Fatalf("goblin B after seq 17: reveal %q", c.Reveal)
	}
	if st.Status != combat.StatusActive {
		t.Fatalf("status before the end row: %s", st.Status)
	}
}

func TestFoldEndStateMatchesGolden(t *testing.T) {
	events := journal()
	st := foldAt(t, events, 20)
	if st.Status != combat.StatusEnded {
		t.Fatalf("final status: %s", st.Status)
	}
	got := Canon(st)
	golden, err := os.ReadFile(filepath.Join("testdata", "battle_golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if want := strings.TrimSpace(string(golden)); got != want {
		t.Fatalf("canonical end state drifted from golden:\n got: %s\nwant: %s", got, want)
	}
	sum := sha256.Sum256([]byte(got))
	if Checksum(st) != hex.EncodeToString(sum[:]) {
		t.Fatal("checksum is not the sha256 of the canonical form")
	}
	// The same journal always hashes the same — the assertion's whole
	// point.
	if again := foldAt(t, events, 20); Checksum(again) != Checksum(st) {
		t.Fatal("checksum is not deterministic across folds")
	}
}

/* ---------- drift ---------- */

func TestFoldDriftIsLoud(t *testing.T) {
	events := journal()
	// The journal lies about the hp after the heal.
	events[7].Payload["after"] = float64(99)
	_, err := Fold(Seeds(lineup(), events), events, 20)
	d, ok := err.(Drift)
	if !ok {
		t.Fatalf("want a drift error, got %v", err)
	}
	if d.Seq != 8 || d.Kind != "heal" {
		t.Fatalf("drift points at the wrong row: %+v", d)
	}

	// A row naming nobody in the battle is drift too.
	events = journal()
	events[3].CombatantID = "the-bard-who-left"
	_, err = Fold(Seeds(lineup(), events), events, 4)
	if _, ok := err.(Drift); !ok {
		t.Fatalf("want a drift error for an unknown combatant, got %v", err)
	}

	// And a kind the fold does not know is refused, not skipped.
	events = journal()
	events = append(events, Event{Seq: 21, Kind: "timewarp"})
	if _, err := Fold(Seeds(lineup(), events), events, 21); err == nil {
		t.Fatal("an unknown journal kind folded without complaint")
	}
}

/* ---------- seeding ---------- */

func TestSeedAnchorsOpeningHPFromTheJournal(t *testing.T) {
	// Velren walked in wounded — the rows say 0 hp at the end, but the
	// journal's first "before" says he opened at 12.
	events := []Event{
		{Seq: 1, Kind: "start", Payload: map[string]any{"round": float64(1)}},
		{Seq: 2, Kind: "damage", CombatantID: "velren", Amount: 3, Payload: map[string]any{
			"asked": float64(3), "effective": float64(3), "before": float64(12), "after": float64(9)}},
		{Seq: 3, Kind: "damage", CombatantID: "velren", Amount: 9, Payload: map[string]any{
			"asked": float64(9), "effective": float64(9), "before": float64(9), "after": float64(0)}},
	}
	row := velren()
	row.HP = 0 // what the rows recorded at the end of this short journal
	seed := []combat.Combatant{Seed(row, events)}
	st, err := Fold(seed, events, 3)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if st.Combatants[0].HP != 0 || st.Combatants[0].MaxHP != 28 {
		t.Fatalf("anchored fold: %+v", st.Combatants[0])
	}
	// Scrubbed before any hit, the anchor shows: he opened at 12.
	st, err = Fold(seed, events, 1)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if st.Combatants[0].HP != 12 {
		t.Fatalf("opening hp anchor: %d, want 12", st.Combatants[0].HP)
	}
}

func TestSeedUntouchedCombatantKeepsRowHP(t *testing.T) {
	// Goblin B never appears in an hp row — no damage, no heal: her
	// recorded row *is* her opening state, whatever it says.
	row := goblinB()
	row.HP = 5
	seeded := Seed(row, journal())
	if seeded.HP != 5 {
		t.Fatalf("untouched combatant's opening hp: %d, want the row's 5", seeded.HP)
	}
}
