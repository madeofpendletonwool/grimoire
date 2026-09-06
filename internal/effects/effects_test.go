package effects

// The duration engine's unit tests (MAD-421). The load-bearing fixture is
// the round/minute boundary the issue names: {10 rounds} and {1 minute}
// are the same duration — 60 seconds — and must count down identically on
// both clocks and render the same bytes at every step. The golden file
// pins the whole countdown story: a turn-advance sequence, a world-clock
// sequence, and a rest, over a target carrying overlapping effects.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDurationValidate(t *testing.T) {
	if err := (Duration{Amount: 1, Unit: UnitMinute}).Validate(); err != nil {
		t.Fatalf("legal duration refused: %v", err)
	}
	if err := (Duration{Amount: 0, Unit: UnitMinute}).Validate(); err == nil {
		t.Fatalf("zero-amount timed duration accepted")
	}
	if err := (Duration{Amount: -3, Unit: UnitHour}).Validate(); err == nil {
		t.Fatalf("negative duration accepted")
	}
	if err := (Duration{Unit: UnitUntilDispelled}).Validate(); err != nil {
		t.Fatalf("until-dispelled refused: %v", err)
	}
	if err := (Duration{Amount: 8, Unit: UnitUntilRest}).Validate(); err == nil {
		t.Fatalf("until-rest with an amount accepted")
	}
	if err := (Duration{Amount: 5, Unit: "fortnights"}).Validate(); err == nil {
		t.Fatalf("unknown unit accepted")
	}
}

func TestDurationSeconds(t *testing.T) {
	cases := []struct {
		d    Duration
		want int64
	}{
		{Duration{Amount: 10, Unit: UnitRound}, 60},
		{Duration{Amount: 1, Unit: UnitMinute}, 60},
		{Duration{Amount: 8, Unit: UnitHour}, 8 * HourSeconds},
		{Duration{Amount: 10, Unit: UnitDay}, 10 * DaySeconds},
	}
	for _, c := range cases {
		got, ok := c.d.Seconds()
		if !ok || got != c.want {
			t.Fatalf("%s.Seconds() = %d, %t; want %d, true", c.d.String(), got, ok, c.want)
		}
	}
	if _, ok := (Duration{Unit: UnitUntilDispelled}).Seconds(); ok {
		t.Fatalf("until-dispelled reported a clock amount")
	}
}

func TestConditionVocabulary(t *testing.T) {
	if !IsCondition("Poisoned") || !IsCondition(" POISONED ") {
		t.Fatalf("poisoned not recognized case-insensitively")
	}
	if got := ConditionName("POISONED"); got != "poisoned" {
		t.Fatalf("ConditionName = %q, want the vocabulary's own spelling", got)
	}
	for _, bad := range []string{"dizzy", "on fire", "", "poison"} {
		if IsCondition(bad) {
			t.Fatalf("%q accepted as a condition — the vocabulary is declared, not free text", bad)
		}
	}
	if len(Conditions()) != 15 {
		t.Fatalf("the game has fifteen conditions, got %d", len(Conditions()))
	}
}

func TestEffectValidate(t *testing.T) {
	if err := (Effect{Kind: KindCondition, Name: "poisoned", Duration: Duration{Unit: UnitUntilRest}}).Validate(); err != nil {
		t.Fatalf("legal condition refused: %v", err)
	}
	if err := (Effect{Kind: KindCondition, Name: "dizzy", Duration: Duration{Unit: UnitUntilRest}}).Validate(); err == nil {
		t.Fatalf("free-text condition accepted")
	}
	if err := (Effect{Kind: KindCondition, Name: "poisoned", Concentration: true, Duration: Duration{Unit: UnitUntilRest}}).Validate(); err == nil {
		t.Fatalf("concentration on a bare condition accepted")
	}
	if err := (Effect{Kind: KindSpell, Name: ""}).Validate(); err == nil {
		t.Fatalf("nameless spell accepted")
	}
	if err := (Effect{Kind: KindSpell, Name: "Hex", Duration: Duration{Amount: 1, Unit: UnitHour}}).Validate(); err != nil {
		t.Fatalf("concentration-free spell refused: %v", err)
	}
	if err := (Effect{Kind: "curse", Name: "Maldiction", Duration: Duration{Unit: UnitUntilDispelled}}).Validate(); err == nil {
		t.Fatalf("unknown kind accepted")
	}
	if err := (Effect{Kind: KindSpell, Name: "Hex"}).Validate(); err == nil {
		t.Fatalf("durationless spell accepted — every effect carries one of the six units")
	}
}

// TestRoundMinuteBoundary is the acceptance fixture: the same sixty
// seconds, declared both ways, tick for turn on the combat clock and
// render identically at every step.
func TestRoundMinuteBoundary(t *testing.T) {
	asRounds := Effect{ID: "a", TargetID: "t", Kind: KindSpell, Name: "Bless", Duration: Duration{Amount: 10, Unit: UnitRound}}
	asMinute := Effect{ID: "b", TargetID: "t", Kind: KindSpell, Name: "Bless", Duration: Duration{Amount: 1, Unit: UnitMinute}}

	rs := []Running{Run(asRounds, World{}), Run(asMinute, World{})}
	for turn := 1; turn <= 10; turn++ {
		rs = TickTurns(rs, 1)
		a, b := rs[0], rs[1]
		if a.Remaining != b.Remaining || a.Ended != b.Ended {
			t.Fatalf("turn %d: %d rounds and 1 minute disagree: %+v vs %+v", turn, 10, a, b)
		}
		if turn < 10 {
			if a.Ended {
				t.Fatalf("ended a turn early at %d", turn)
			}
			if a.Display() != Render(int64(10-turn)*RoundSeconds) {
				t.Fatalf("turn %d display = %q", turn, a.Display())
			}
		}
	}
	if !rs[0].Ended || rs[0].EndReason != EndExpired || rs[1].Ended != true {
		t.Fatalf("both spellings must expire on the tenth turn: %+v", rs)
	}
	// Mid-countdown renders in the game's own words: 30 seconds is
	// "5 rounds" — wait, it is 1 minute's half; sixty seconds renders
	// "1 minute" from either spelling.
	if got := Render(MinuteSeconds); got != "1 minute" {
		t.Fatalf("60 seconds renders %q, want \"1 minute\"", got)
	}
	if got := Render(10 * RoundSeconds); got != "1 minute" {
		t.Fatalf("ten rounds renders %q, want \"1 minute\" (the boundary)", got)
	}
}

func TestRender(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{6, "1 round"},
		{54, "9 rounds"},
		{60, "1 minute"},
		{90, "1 minute 5 rounds"},
		{120, "2 minutes"},
		{3600, "1 hour"},
		{4500, "1 hour 15 minutes"},
		{8 * HourSeconds, "8 hours"},
		{DaySeconds, "1 day"},
		{10 * DaySeconds, "10 days"},
		{9*DaySeconds + 23*HourSeconds, "9 days 23 hours"},
		{3, "expired"}, // less than a round is spent
		{0, "expired"},
		{-5, "expired"},
	}
	for _, c := range cases {
		if got := Render(c.sec); got != c.want {
			t.Fatalf("Render(%d) = %q, want %q", c.sec, got, c.want)
		}
	}
}

// TestRunWorldClock pins the world-clock half: a day advance spends 24
// hours of every timed effect; an until-rest effect ends on the rest and
// on nothing else; an until-dispelled effect ends on nothing time can do.
func TestRunWorldClock(t *testing.T) {
	mageArmor := Effect{ID: "ma", TargetID: "t", Kind: KindSpell, Name: "Mage Armor", Duration: Duration{Amount: 8, Unit: UnitHour}}
	hex := Effect{ID: "hex", TargetID: "t", Kind: KindSpell, Name: "Hex", Duration: Duration{Amount: 24, Unit: UnitHour}}
	geas := Effect{ID: "geas", TargetID: "t", Kind: KindSpell, Name: "Geas", Duration: Duration{Amount: 30, Unit: UnitDay}}
	blessed := Effect{ID: "blessed", TargetID: "t", Kind: KindFeature, Name: "Blessed by the spring", Duration: Duration{Unit: UnitUntilRest}}
	watchful := Effect{ID: "watch", TargetID: "t", Kind: KindSpell, Name: "Unseen Servant", Duration: Duration{Unit: UnitUntilDispelled}}

	// Same day, no rest: everything at full strength.
	for _, e := range []Effect{mageArmor, hex, geas, blessed, watchful} {
		if r := Run(e, World{}); r.Ended {
			t.Fatalf("%s ended without any time passing", e.Name)
		}
	}

	// A night's long rest: one day elapses. Mage armor (8h) dies, Hex
	// (24h) dies on the day boundary, geas (30d) loses a day, the
	// until-rest blessing dies on the rest, the until-dispelled effect
	// survives both.
	if r := Run(mageArmor, World{DaysSinceAnchor: 1}); !r.Ended || r.EndReason != EndExpired {
		t.Fatalf("mage armor survived a day: %+v", r)
	}
	if r := Run(hex, World{DaysSinceAnchor: 1}); !r.Ended || r.EndReason != EndExpired {
		t.Fatalf("hex survived its own 24 hours: %+v", r)
	}
	if r := Run(geas, World{DaysSinceAnchor: 1}); r.Ended || r.Remaining != 29*DaySeconds {
		t.Fatalf("geas after one day = %+v, want 29 days left", r)
	}
	if r := Run(blessed, World{DaysSinceAnchor: 1, RestSinceApply: true}); !r.Ended || r.EndReason != EndRest {
		t.Fatalf("until-rest effect survived a rest: %+v", r)
	}
	if r := Run(blessed, World{RestSinceApply: false}); r.Ended {
		t.Fatalf("until-rest effect ended without a rest: %+v", r)
	}
	if r := Run(watchful, World{DaysSinceAnchor: 1, RestSinceApply: true}); r.Ended {
		t.Fatalf("until-dispelled effect ended against time: %+v", r)
	}

	// A backward clock (a DM fixing a typo) gives time back — the clock
	// ledger's own rule, honoured rather than second-guessed.
	if r := Run(geas, World{DaysSinceAnchor: -1}); r.Ended || r.Remaining != 31*DaySeconds {
		t.Fatalf("geas after a backward day = %+v, want 31 days", r)
	}
}

// TestTickTurnsDoesNotTouchUntilUnits: a turn cannot dispel.
func TestTickTurnsDoesNotTouchUntilUnits(t *testing.T) {
	rs := []Running{
		Run(Effect{ID: "a", Kind: KindSpell, Name: "Unseen Servant", Duration: Duration{Unit: UnitUntilDispelled}}, World{}),
		Run(Effect{ID: "b", Kind: KindSpell, Name: "Hex", Duration: Duration{Amount: 1, Unit: UnitHour}}, World{}),
	}
	for i := 0; i < 600; i++ {
		rs = TickTurns(rs, 1)
	}
	if rs[0].Ended {
		t.Fatalf("an until-dispelled effect expired on turns alone")
	}
	if !rs[1].Ended {
		t.Fatalf("an hour effect survived 600 turns (3600 seconds)")
	}
	if rs[0].Remaining != 0 {
		t.Fatalf("until-units carry no clock amount, got %d", rs[0].Remaining)
	}
}

// TestOverlappingEffectsCompose is the acceptance fixture: several effects
// on one target, of every unit, count down side by side without one
// corrupting another — byte-identical whether they run together or one at
// a time.
func TestOverlappingEffectsCompose(t *testing.T) {
	target := []Effect{
		{ID: "1", TargetID: "t", Kind: KindSpell, Name: "Bless", Duration: Duration{Amount: 10, Unit: UnitRound}},
		{ID: "2", TargetID: "t", Kind: KindCondition, Name: "poisoned", Duration: Duration{Amount: 1, Unit: UnitHour}},
		{ID: "3", TargetID: "t", Kind: KindSpell, Name: "Hex", Concentration: true, Duration: Duration{Amount: 1, Unit: UnitHour}},
		{ID: "4", TargetID: "t", Kind: KindFeature, Name: "Rage", Duration: Duration{Amount: 10, Unit: UnitRound}},
		{ID: "5", TargetID: "t", Kind: KindSpell, Name: "Mage Armor", Duration: Duration{Amount: 8, Unit: UnitHour}},
	}
	together := make([]Running, len(target))
	for i, e := range target {
		together[i] = Run(e, World{})
	}
	for turn := 0; turn < 12; turn++ {
		together = TickTurns(together, 1)
		for i, e := range target {
			alone := TickTurns([]Running{Run(e, World{})}, int64(turn)+1)[0]
			if together[i].Remaining != alone.Remaining || together[i].Ended != alone.Ended {
				t.Fatalf("turn %d: effect %s corrupted by its neighbours: %+v vs %+v", turn+1, e.Name, together[i], alone)
			}
		}
	}
	// Bless and Rage (10 rounds) expired exactly on turn ten; the
	// hour-effects survived twelve turns with 528 seconds worn away.
	if !together[0].Ended || !together[3].Ended {
		t.Fatalf("the round effects should have expired on turn ten")
	}
	if together[1].Ended || together[2].Ended || together[4].Ended {
		t.Fatalf("an hour effect expired in twelve turns")
	}
	if want := HourSeconds - 12*RoundSeconds; together[1].Remaining != want {
		t.Fatalf("poisoned remaining = %d, want %d", together[1].Remaining, want)
	}
}

// TestTickTurnsImmutable: a tick is as re-runnable as a sim step — the
// input slice is never mutated, so replaying the same tick from the same
// state reproduces the same bytes.
func TestTickTurnsImmutable(t *testing.T) {
	rs := []Running{Run(Effect{ID: "a", Kind: KindSpell, Name: "Bless", Duration: Duration{Amount: 10, Unit: UnitRound}}, World{})}
	before, _ := json.Marshal(rs)
	_ = TickTurns(rs, 4)
	after, _ := json.Marshal(rs)
	if string(before) != string(after) {
		t.Fatalf("TickTurns mutated its input")
	}
	first, _ := json.Marshal(TickTurns(rs, 4))
	second, _ := json.Marshal(TickTurns(rs, 4))
	if string(first) != string(second) {
		t.Fatalf("the same tick is not reproducible")
	}
}

/* ---------- the golden file ---------- */

var updateGoldens = flag.Bool("update-golden", false, "rewrite the countdown golden file")

// TestCountdownGolden pins the whole countdown story as one fixture: one
// target carrying overlapping effects across every unit, ten turns of the
// combat clock, a day of travel on the world clock, and a long rest —
// each stage's running state rendered to bytes and compared against
// testdata/countdown_golden.json. The round/minute boundary leads: the
// same sixty seconds declared both ways must produce identical rows.
// Regenerate with -update-golden after an intentional engine change; a
// diff you did not intend is a regression.
func TestCountdownGolden(t *testing.T) {
	fx := func() []Running {
		return []Running{
			Run(Effect{ID: "1", TargetID: "thalia", TargetName: "Thalia", Kind: KindSpell, Name: "Bless", SourceName: "Evendur", Concentration: true, Duration: Duration{Amount: 10, Unit: UnitRound}}, World{}),
			Run(Effect{ID: "2", TargetID: "thalia", TargetName: "Thalia", Kind: KindSpell, Name: "Guiding Bolt", SourceName: "Evendur", Duration: Duration{Amount: 1, Unit: UnitMinute}}, World{}),
			Run(Effect{ID: "3", TargetID: "thalia", TargetName: "Thalia", Kind: KindCondition, Name: "poisoned", Duration: Duration{Amount: 1, Unit: UnitHour}}, World{}),
			Run(Effect{ID: "4", TargetID: "thalia", TargetName: "Thalia", Kind: KindSpell, Name: "Hex", SourceName: "Nyx", Concentration: true, Duration: Duration{Amount: 24, Unit: UnitHour}}, World{}),
			Run(Effect{ID: "5", TargetID: "thalia", TargetName: "Thalia", Kind: KindSpell, Name: "Mage Armor", SourceName: "Evendur", Duration: Duration{Amount: 8, Unit: UnitHour}}, World{}),
			Run(Effect{ID: "6", TargetID: "thalia", TargetName: "Thalia", Kind: KindSpell, Name: "Geas", SourceName: "Evendur", Duration: Duration{Amount: 30, Unit: UnitDay}}, World{}),
			Run(Effect{ID: "7", TargetID: "thalia", TargetName: "Thalia", Kind: KindFeature, Name: "Blessed by the spring", Duration: Duration{Unit: UnitUntilRest}}, World{}),
			Run(Effect{ID: "8", TargetID: "thalia", TargetName: "Thalia", Kind: KindSpell, Name: "Unseen Servant", SourceName: "Evendur", Duration: Duration{Unit: UnitUntilDispelled}}, World{}),
		}
	}

	stage := func(name string, rs []Running) map[string]any {
		rows := make([]map[string]any, 0, len(rs))
		for _, r := range rs {
			rows = append(rows, map[string]any{
				"id": r.ID, "name": r.Name, "unit": r.Unit,
				"remaining_seconds": r.Remaining, "display": r.Display(),
				"ended": r.Ended, "end_reason": r.EndReason,
			})
		}
		return map[string]any{"stage": name, "effects": rows}
	}

	story := []map[string]any{stage("applied", fx())}

	// Ten turns of the combat clock, staged at the moments that matter:
	// the first turn, the half, the ninth, and the tenth — where the
	// round-spelled Bless expires and the minute-spelled Guiding Bolt
	// (the same sixty seconds) expires with it.
	rs := fx()
	for turn := int64(1); turn <= 10; turn++ {
		rs = TickTurns(rs, 1)
		switch turn {
		case 1, 5, 9, 10:
			story = append(story, stage(fmt.Sprintf("combat_turn_%d", turn), rs))
		}
	}

	// Two days of travel on the world clock. The combat clock's wear is
	// part of the persisted state now (the store's shape: ticks persist,
	// days derive), so each timed survivor carries its worn remaining
	// into the world pass — the same arithmetic the store's read
	// performs. Until-units never meet a clock: travel alone ends
	// nothing that only a rest or a dispel can.
	travelled := make([]Running, 0, len(fx()))
	for _, r := range fx() {
		w := Run(r.Effect, World{DaysSinceAnchor: 2})
		if !IsTimed(r.Effect.Unit) {
			travelled = append(travelled, w)
			continue
		}
		w = Run(r.Effect, World{})
		for _, spent := range rs {
			if spent.ID != w.ID {
				continue
			}
			// A row the turns already ended stays ended — the store
			// persists terminal state; the world clock cannot revive.
			if spent.Ended {
				w = spent
				break
			}
			w.Remaining = spent.Remaining - 2*DaySeconds
			if w.Remaining <= 0 {
				w.Remaining, w.Ended, w.EndReason = 0, true, EndExpired
			}
		}
		travelled = append(travelled, w)
	}
	story = append(story, stage("travel_two_days", travelled))

	// The long rest: one more day and a rest event.
	rested := make([]Running, 0, len(travelled))
	for _, r := range travelled {
		if r.Ended {
			rested = append(rested, r)
			continue
		}
		rested = append(rested, Run(r.Effect, World{DaysSinceAnchor: 3, RestSinceApply: true}))
	}
	story = append(story, stage("long_rest", rested))

	gotBytes, err := json.MarshalIndent(story, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	golden := filepath.Join("testdata", "countdown_golden.json")
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(golden, append(gotBytes, '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run -update-golden once): %v", err)
	}
	if string(want) != string(append(gotBytes, '\n')) {
		gotPath := filepath.Join(t.TempDir(), "got.json")
		_ = os.WriteFile(gotPath, gotBytes, 0o644)
		t.Fatalf("countdown golden differs; got %s, want %s", gotPath, golden)
	}
}
