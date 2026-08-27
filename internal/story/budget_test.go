package story

// The session budget's tests (MAD-362): the scene count off the crossing
// arithmetic at its boundaries, and the kind mix for every act position of
// every legal shape — our arithmetic, no model in the picture.

import (
	"reflect"
	"testing"
)

func TestScenesPerSession(t *testing.T) {
	cases := []struct {
		name                 string
		start, end, sessions int
		want                 int
	}{
		{"gauntlet: one session per crossing", 1, 4, 3, 6},
		{"climax density: two sessions per crossing", 10, 12, 4, 5},
		{"brisk: 2.25 sessions per crossing", 4, 6, 5, 5},
		{"typical: three sessions per crossing", 1, 3, 6, 4},
		{"slow burn: 4.5 sessions per crossing", 1, 3, 9, 4},
		{"glacial: six sessions per crossing", 1, 2, 6, 3},
		{"floor: nine sessions per crossing", 1, 2, 9, 3},
		{"a band with no crossings still budgets", 5, 5, 3, 4},
		{"fewer sessions than one is treated as one", 1, 6, 0, 6},
	}
	for _, tc := range cases {
		if got := ScenesPerSession(tc.start, tc.end, tc.sessions); got != tc.want {
			t.Errorf("%s: ScenesPerSession(%d, %d, %d) = %d, want %d",
				tc.name, tc.start, tc.end, tc.sessions, got, tc.want)
		}
	}
	// The bounds hold across every ratio a real band produces.
	for sessions := 1; sessions <= 26; sessions++ {
		for crossings := 1; crossings <= 19; crossings++ {
			got := ScenesPerSession(1, 1+crossings, sessions)
			if got < minScenesPerSession || got > maxScenesPerSession {
				t.Fatalf("ScenesPerSession(crossings=%d, sessions=%d) = %d, outside 3..6", crossings, sessions, got)
			}
		}
	}
	// The issue's own bar: a paced 1-12 campaign's acts never budget eleven
	// scenes — or anything past six — for one session.
	for _, ap := range Pace(1, 12, 4).PerAct {
		if got := ScenesPerSession(ap.LevelStart, ap.LevelEnd, ap.Sessions); got > 6 {
			t.Errorf("act %d (%d-%d, %d sessions) budgets %d scenes", ap.Act, ap.LevelStart, ap.LevelEnd, ap.Sessions, got)
		}
	}
}

func TestSceneMix(t *testing.T) {
	// A setup act leads social; a climax act leads combat; the mid turn
	// leads revelation — each position's signature scene.
	if got := SceneMix(0, 3, 4); got[0] != KindSocial {
		t.Errorf("three-act setup leads with %s, want social", got[0])
	}
	if got := SceneMix(2, 4, 4); got[0] != KindCombat {
		t.Errorf("four-act climax leads with %s, want combat", got[0])
	}
	if got := SceneMix(2, 5, 4); got[0] != KindRevelation {
		t.Errorf("five-act mid turn leads with %s, want revelation", got[0])
	}
	// The resolution template carries downtime; the middle ones do not.
	resolution := SceneMix(2, 3, 6)
	hasDowntime := false
	for _, k := range resolution {
		if k == KindDowntime {
			hasDowntime = true
		}
	}
	if !hasDowntime {
		t.Errorf("resolution mix %v carries no downtime", resolution)
	}

	// Every mix draws only from the six legal kinds, at every position and
	// length, for every shape and for act counts with no named shape.
	legal := map[string]bool{
		KindSocial: true, KindExploration: true, KindCombat: true,
		KindRevelation: true, KindDowntime: true, KindTravel: true,
	}
	for _, actCount := range []int{3, 4, 5, 6, 0} {
		for idx := 0; idx < actCount+2; idx++ {
			for n := 1; n <= 8; n++ {
				mix := SceneMix(idx, actCount, n)
				if len(mix) != n {
					t.Fatalf("SceneMix(%d, %d, %d) returned %d kinds", idx, actCount, n, len(mix))
				}
				for _, k := range mix {
					if !legal[k] {
						t.Fatalf("SceneMix(%d, %d, %d) returned illegal kind %q", idx, actCount, n, k)
					}
				}
			}
		}
	}

	// Deterministic and stable: the same position yields the same mix.
	if !reflect.DeepEqual(SceneMix(1, 4, 5), SceneMix(1, 4, 5)) {
		t.Error("SceneMix is not deterministic")
	}
	if SceneMix(0, 3, 0) != nil {
		t.Error("a zero-length mix is nil")
	}
}
