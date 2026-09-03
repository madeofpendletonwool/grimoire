package statblock

import (
	"strconv"
	"strings"
	"testing"
)

// The envelope is read off the same tables ComputeCR computes from, so the
// test pins the DMG's own printed rows: a handful of spot checks per band,
// the proficiency ladder, and the clamp behaviour at the table's ends.
func TestEnvelopeForMatchesDMGRows(t *testing.T) {
	cases := []struct {
		cr     float64
		label  string
		prof   int
		hpMin  int
		hpMax  int
		ac     int
		dprMin int
		dprMax int
		ab     int
		dc     int
	}{
		{0, "0", 2, 1, 6, 13, 0, 1, 3, 13},
		{0.25, "1/4", 2, 36, 49, 13, 4, 5, 3, 13},
		{1, "1", 2, 71, 85, 13, 9, 14, 3, 13},
		{5, "5", 3, 131, 145, 15, 33, 38, 4, 14},
		{7, "7", 3, 161, 175, 15, 45, 50, 4, 14},
		{10, "10", 4, 206, 220, 17, 63, 68, 5, 15},
		{13, "13", 5, 251, 265, 18, 81, 86, 6, 16},
		{20, "20", 6, 356, 400, 19, 123, 140, 8, 18},
		{24, "24", 7, 536, 580, 21, 195, 212, 9, 19},
		{30, "30", 8, 806, 850, 21, 303, 335, 10, 21},
	}
	for _, c := range cases {
		e := EnvelopeFor(c.cr)
		if e.Label != c.label {
			t.Errorf("CR %v: label %q, want %q", c.cr, e.Label, c.label)
		}
		if e.ProfBonus != c.prof {
			t.Errorf("CR %v: prof %d, want %d", c.cr, e.ProfBonus, c.prof)
		}
		if e.HP.Min != c.hpMin || e.HP.Max != c.hpMax {
			t.Errorf("CR %v: hp band [%d,%d], want [%d,%d]", c.cr, e.HP.Min, e.HP.Max, c.hpMin, c.hpMax)
		}
		if e.AC.Assumed != c.ac {
			t.Errorf("CR %v: assumed AC %d, want %d", c.cr, e.AC.Assumed, c.ac)
		}
		if e.DPR.Min != c.dprMin || e.DPR.Max != c.dprMax {
			t.Errorf("CR %v: dpr band [%d,%d], want [%d,%d]", c.cr, e.DPR.Min, e.DPR.Max, c.dprMin, c.dprMax)
		}
		if e.AttackBonus.Assumed != c.ab {
			t.Errorf("CR %v: assumed attack bonus %d, want %d", c.cr, e.AttackBonus.Assumed, c.ab)
		}
		if e.SaveDC.Assumed != c.dc {
			t.Errorf("CR %v: assumed save DC %d, want %d", c.cr, e.SaveDC.Assumed, c.dc)
		}
		if e.LegendaryPoints != 3 {
			t.Errorf("CR %v: legendary budget %d, want 3 (what ComputeCR prices)", c.cr, e.LegendaryPoints)
		}
		if e.HP.Assumed < e.HP.Min || e.HP.Assumed > e.HP.Max {
			t.Errorf("CR %v: assumed hp %d outside its own band [%d,%d]", c.cr, e.HP.Assumed, e.HP.Min, e.HP.Max)
		}
		if e.DPR.Assumed < e.DPR.Min || e.DPR.Assumed > e.DPR.Max {
			t.Errorf("CR %v: assumed dpr %d outside its own band [%d,%d]", c.cr, e.DPR.Assumed, e.DPR.Min, e.DPR.Max)
		}
	}
}

func TestEnvelopeForClamps(t *testing.T) {
	lo := EnvelopeFor(-3)
	if lo.CR != 0 || lo.Label != "0" {
		t.Errorf("negative request should clamp to CR 0, got %v %q", lo.CR, lo.Label)
	}
	hi := EnvelopeFor(45)
	if hi.CR != 30 || hi.Label != "30" {
		t.Errorf("huge request should clamp to CR 30, got %v %q", hi.CR, hi.Label)
	}
}

// parsed fills in what every real caller of ComputeCR does before calling
// it: the deterministic parse of each action's prose into Parsed/Attack —
// the same never-half-parsed contract the mirror keeps.
func parsed(s Statblock) Statblock {
	for i := range s.Actions {
		if atk, ok := ParseAttack(s.Actions[i].Name, s.Actions[i].Desc); ok {
			s.Actions[i].Parsed = true
			s.Actions[i].Attack = atk
		} else {
			s.Actions[i].Unparse = "test: unreadable"
		}
	}
	return s
}

// slamAt builds the simplest statblock that carries a given AC, HP and
// damage per round: one melee attack whose prose prints the numbers asked
// of it. The parser reads the printed average, so the dice expression is
// decorative here.
func slamAt(ac, hp, dpr, ab int) Statblock {
	return parsed(Statblock{
		Name: "Test", AC: ac, HP: hp,
		Actions: []Action{{
			Name: "Slam",
			Desc: "Melee Weapon Attack: +" + strconv.Itoa(ab) + " to hit, reach 5 ft., one target. " +
				"Hit: " + strconv.Itoa(dpr) + " (" + strconv.Itoa(dpr) + "d6) bludgeoning damage.",
		}},
	})
}

// The envelope and the calculator must agree with each other: a statblock
// built to the middle of every band computes to the requested CR. This is
// the property the whole designer leans on.
func TestEnvelopeMidpointComputesToRequestedCR(t *testing.T) {
	for cr := 0.0; cr <= 30; cr++ {
		e := EnvelopeFor(cr)
		r := ComputeCR(slamAt(e.AC.Assumed, e.HP.Assumed, e.DPR.Assumed, e.AttackBonus.Assumed))
		if r.CR != e.CR {
			t.Errorf("CR %v: a mid-band design computed %s (offensive %s, defensive %s), want %s",
				cr, r.Label, Label(r.Offensive), Label(r.Defensive), e.Label)
		}
	}
}

// The save-based path must reach the same CR: the same damage delivered
// through a DC at the assumed column prices the same offense.
func TestEnvelopeSaveBasedMatchesAttackBased(t *testing.T) {
	for _, cr := range []float64{1, 5, 9, 16} {
		e := EnvelopeFor(cr)
		// A cone is an area effect, and the DMG prices one at two targets —
		// so the printed damage is half the envelope's per-round figure and
		// the arithmetic lands on it.
		printed := e.DPR.Assumed / 2
		s := parsed(Statblock{
			AC: e.AC.Assumed, HP: e.HP.Assumed,
			Actions: []Action{{
				Name: "Breath",
				Desc: "The test breathes in a 30-foot cone. Dexterity Saving Throw: DC " +
					strconv.Itoa(e.SaveDC.Assumed) + ", each creature in the area. Failure: " +
					strconv.Itoa(printed) + " (" + strconv.Itoa(printed) + "d6) fire damage.",
			}},
		})
		r := ComputeCR(s)
		if r.CR != e.CR {
			t.Errorf("CR %v: save-based mid-band design computed %s, want %s", cr, r.Label, e.Label)
		}
	}
}

func TestParseLabel(t *testing.T) {
	ok := map[string]float64{"0": 0, "7": 7, "1/8": 0.125, "1/4": 0.25, "1/2": 0.5, " 13 ": 13, "0.5": 0.5, "30": 30}
	for in, want := range ok {
		got, valid := ParseLabel(in)
		if !valid || got != want {
			t.Errorf("ParseLabel(%q) = %v, %v; want %v", in, got, valid, want)
		}
	}
	for _, in := range []string{"", "31", "x", "-1", "1/3", "7.5"} {
		if _, valid := ParseLabel(in); valid {
			t.Errorf("ParseLabel(%q) accepted; want refused", in)
		}
	}
}

// Shortfall names the right half and the right numbers.
func TestShortfall(t *testing.T) {
	// A CR 7 request met by a weak offense and an on-target defense.
	e := EnvelopeFor(7)
	r := ComputeCR(slamAt(e.AC.Assumed, e.HP.Assumed, 9, 4))
	if r.CR >= e.CR {
		t.Fatalf("expected the weak offense to compute below CR 7, got %s", r.Label)
	}
	misses := Shortfall(7, r)
	if len(misses) != 1 {
		t.Fatalf("expected exactly one shortfall (offense), got %v", misses)
	}
	if !strings.Contains(misses[0], "offensive CR") || !strings.Contains(misses[0], "damage per round is 36 short") {
		t.Errorf("the offense shortfall should name its half and its number: %q", misses[0])
	}

	// And the mirror case: on-target damage, paper defenses.
	r = ComputeCR(slamAt(10, 30, e.DPR.Assumed, e.AttackBonus.Assumed))
	misses = Shortfall(7, r)
	if len(misses) != 1 || !strings.Contains(misses[0], "defensive CR") {
		t.Fatalf("expected exactly the defensive shortfall, got %v", misses)
	}

	// A mid-band design misses nothing.
	e = EnvelopeFor(10)
	r = ComputeCR(slamAt(e.AC.Assumed, e.HP.Assumed, e.DPR.Assumed, e.AttackBonus.Assumed))
	if misses := Shortfall(10, r); len(misses) != 0 {
		t.Errorf("a mid-band CR 10 design should agree, got %v", misses)
	}
}
