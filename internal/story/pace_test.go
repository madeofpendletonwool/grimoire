package story

// Pace tests. The load-bearing ones pin the derivation to the encounter
// package's own tables at the band boundaries: every crossing price must
// equal ceil(Hard(next)/Medium(current)) read straight off
// encounter.ThresholdFor, and the per-act split must sum to the total.

import (
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

func TestPaceSingleCrossingMatchesXPTable(t *testing.T) {
	// The band boundaries the derivation is pinned to: the first crossing,
	// the tier-3 jump, the last crossing, and a flat band (no crossings).
	cases := []struct {
		from, to int
	}{
		{1, 2}, {4, 5}, {11, 12}, {19, 20}, {5, 5}, {20, 20},
	}
	for _, c := range cases {
		p := Pace(c.from, c.to, 1)
		if len(p.PerAct) != 1 {
			t.Fatalf("Pace(%d,%d,1): got %d acts, want 1", c.from, c.to, len(p.PerAct))
		}
		if p.TotalSessions != p.PerAct[0].Sessions {
			t.Errorf("Pace(%d,%d,1): total %d != act sessions %d", c.from, c.to, p.TotalSessions, p.PerAct[0].Sessions)
		}
		if c.from == c.to {
			if p.PerAct[0].Sessions != 1 {
				t.Errorf("Pace(%d,%d,1): a flat band still costs one session, got %d", c.from, c.to, p.PerAct[0].Sessions)
			}
			continue
		}
		medium, okM := encounter.ThresholdFor(c.from, encounter.BandMedium)
		hard, okH := encounter.ThresholdFor(c.to, encounter.BandHard)
		if !okM || !okH {
			t.Fatalf("ThresholdFor(%d/%d): the table lacks the band", c.from, c.to)
		}
		want := 1
		if hard > medium {
			want = (hard + medium - 1) / medium
		}
		if got := p.PerAct[0].Sessions; got != want {
			t.Errorf("Pace(%d,%d,1) = %d sessions, want ceil(Hard(%d)/Medium(%d)) = %d", c.from, c.to, got, c.to, c.from, want)
		}
	}
}

func TestPaceLevelsTwelveAcrossFourActs(t *testing.T) {
	// The issue's own example: levels 1-12 across four acts produces real
	// session counts, not a guess. Every crossing price comes off the table,
	// so this total is pinned: 3+3+3+3 for the first four crossings, 2 for
	// each of the remaining seven — 26.
	p := Pace(1, 12, 4)
	if p.TotalSessions != 26 {
		t.Errorf("Pace(1,12,4) total = %d, want 26 (pinned against the XP table)", p.TotalSessions)
	}
	if len(p.PerAct) != 4 {
		t.Fatalf("got %d acts, want 4", len(p.PerAct))
	}
	sum := 0
	for _, ap := range p.PerAct {
		if ap.Sessions < 1 {
			t.Errorf("act %d has %d sessions; every act is floored at one", ap.Act, ap.Sessions)
		}
		sum += ap.Sessions
	}
	if sum != p.TotalSessions {
		t.Errorf("per-act sessions sum to %d, want the total %d", sum, p.TotalSessions)
	}
	// The bands must chain across the acts.
	for i := 1; i < len(p.PerAct); i++ {
		if p.PerAct[i].LevelStart != p.PerAct[i-1].LevelEnd {
			t.Errorf("act %d starts at %d but act %d ends at %d; the split must chain",
				i+1, p.PerAct[i].LevelStart, i, p.PerAct[i-1].LevelEnd)
		}
	}
	if first, last := p.PerAct[0].LevelStart, p.PerAct[3].LevelEnd; first != 1 || last != 12 {
		t.Errorf("the split covers %d-%d, want 1-12", first, last)
	}
}

func TestPaceClampsItsInputs(t *testing.T) {
	// Out-of-range levels clamp to 1-20; an inverted band orders itself; a
	// nonsensical act count falls back to one act.
	clamped := Pace(-3, 99, 2)
	if clamped.LevelStart != 1 || clamped.LevelEnd != 20 {
		t.Errorf("Pace(-3,99,2) band = %d-%d, want 1-20", clamped.LevelStart, clamped.LevelEnd)
	}
	if want := Pace(1, 20, 2).TotalSessions; clamped.TotalSessions != want {
		t.Errorf("clamped total %d, want %d", clamped.TotalSessions, want)
	}
	inv := Pace(12, 1, 3)
	if inv.LevelStart != 1 || inv.LevelEnd != 12 {
		t.Errorf("an inverted band must order itself, got %d-%d", inv.LevelStart, inv.LevelEnd)
	}
	one := Pace(1, 5, 0)
	if len(one.PerAct) != 1 {
		t.Errorf("actCount 0 must fall back to one act, got %d", len(one.PerAct))
	}
}

func TestPaceMoreActsThanCrossings(t *testing.T) {
	// Six acts over three crossings: the later acts own no crossings and
	// still cost one session each.
	p := Pace(1, 4, 6)
	if len(p.PerAct) != 6 {
		t.Fatalf("got %d acts, want 6", len(p.PerAct))
	}
	for _, ap := range p.PerAct {
		if ap.Sessions < 1 {
			t.Errorf("act %d has %d sessions; an act nobody plays is not an act", ap.Act, ap.Sessions)
		}
	}
}

func TestSessionsToLevelUnit(t *testing.T) {
	if got := SessionsToLevel(1); got != 3 {
		t.Errorf("SessionsToLevel(1) = %d, want 3 (ceil(Hard(2)/Medium(1)) = ceil(150/50))", got)
	}
	if got := SessionsToLevel(5); got != 2 {
		t.Errorf("SessionsToLevel(5) = %d, want 2 (ceil(900/500))", got)
	}
}
