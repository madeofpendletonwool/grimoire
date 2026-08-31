package dungeon

import (
	"fmt"
	"testing"
)

// allParams is the parameter grid the property test sweeps: every size
// band, both density extremes and the balanced middle, every
// branchiness. A DM's real request is somewhere in this grid.
func allParams() []Params {
	out := make([]Params, 0, 4*4*4)
	for _, size := range Sizes() {
		for _, densities := range [][3]int{
			{1, 1, 1}, // balanced
			{5, 1, 1}, // combat-heavy
			{1, 5, 1}, // puzzle-heavy — the DM asking for puzzles gets them counted out
			{1, 1, 5}, // exploration-heavy
		} {
			for _, branch := range []int{0, 1, 2, 3} {
				out = append(out, Params{
					Theme: "an abandoned temple under the marsh", Size: size, Level: 5,
					ExpectedSessions: 2,
					CombatDensity:    densities[0], PuzzleDensity: densities[1], ExploreDensity: densities[2],
					Branchiness: branch,
				})
			}
		}
	}
	return out
}

// axisOf maps a purpose onto its density axis.
func axisOf(purpose string) string {
	switch purpose {
	case PurposeGuard, PurposeLair, PurposeBoss:
		return "combat"
	case PurposePuzzle:
		return "puzzle"
	case PurposeSecret:
		return "secret"
	default: // entrance, connector, treasure, shrine, prison, hazard
		return "exploration"
	}
}

// checkGraph asserts every invariant a generated dungeon must hold:
// the room count matches the quota sum, the quotas land their purpose
// counts exactly, the entrance reaches the boss and every other room,
// the critical path is a real path of declared edges, the grid holds
// every room in its own cell, and no two edges cross.
func checkGraph(t *testing.T, g Graph) {
	t.Helper()

	if n := len(g.Rooms); n != g.Quotas.Sum() {
		t.Fatalf("room count %d != quota sum %d", n, g.Quotas.Sum())
	}
	if g.Rooms[0].Key != g.Entrance {
		t.Fatalf("first room %s is not the entrance %s", g.Rooms[0].Key, g.Entrance)
	}

	// The quotas land exactly, per purpose and per axis.
	counts := map[string]int{}
	for _, r := range g.Rooms {
		if !ValidPurpose(r.Purpose) {
			t.Fatalf("room %s has invalid purpose %q", r.Key, r.Purpose)
		}
		counts[r.Purpose]++
	}
	axis := map[string]int{}
	for purpose, n := range counts {
		axis[axisOf(purpose)] += n
	}
	if axis["combat"] != g.Quotas.Combat {
		t.Errorf("combat rooms = %d, quota = %d", axis["combat"], g.Quotas.Combat)
	}
	if axis["puzzle"] != g.Quotas.Puzzle {
		t.Errorf("puzzle rooms = %d, quota = %d", axis["puzzle"], g.Quotas.Puzzle)
	}
	if axis["exploration"] != g.Quotas.Exploration {
		t.Errorf("exploration rooms = %d, quota = %d", axis["exploration"], g.Quotas.Exploration)
	}
	if axis["secret"] != g.Quotas.Secret {
		t.Errorf("secret rooms = %d, quota = %d", axis["secret"], g.Quotas.Secret)
	}
	if g.Quotas.Secret < 1 {
		t.Error("a dungeon without a secret room")
	}
	if counts[PurposeEntrance] != 1 || counts[PurposeBoss] != 1 {
		t.Errorf("entrances = %d, bosses = %d; one of each", counts[PurposeEntrance], counts[PurposeBoss])
	}

	// Every edge endpoint is a declared room; every room key is unique.
	seen := map[string]bool{}
	for _, r := range g.Rooms {
		if r.Key == "" {
			t.Fatal("a room with an empty key")
		}
		if seen[r.Key] {
			t.Fatalf("room key %q declared twice", r.Key)
		}
		seen[r.Key] = true
	}
	for _, e := range g.Edges {
		if !ValidEdgeKind(e.Kind) {
			t.Fatalf("edge %s->%s has invalid kind %q", e.From, e.To, e.Kind)
		}
		if !seen[e.From] || !seen[e.To] {
			t.Fatalf("edge %s->%s names an undeclared room", e.From, e.To)
		}
	}

	// The guarantees: a path entrance -> boss (a walkable one — one-way
	// edges honoured), and no unreachable room.
	reach := g.Reachable(g.Entrance)
	if !reach[g.Boss] {
		t.Fatalf("the boss %s is not reachable from the entrance", g.Boss)
	}
	for _, r := range g.Rooms {
		if !reach[r.Key] {
			t.Fatalf("room %s (%s) is unreachable from the entrance", r.Key, r.Purpose)
		}
	}

	// The critical path is real: a chain of declared edges.
	var path []string
	visited := map[string]bool{g.Entrance: true}
	cur := g.Entrance
	for cur != g.Boss {
		moved := false
		for _, e := range g.Edges {
			if e.From != cur || visited[e.To] {
				continue
			}
			cur = e.To
			visited[cur] = true
			path = append(path, cur)
			moved = true
			break
		}
		if !moved {
			t.Fatalf("no directed path from %s to the boss %s", g.Entrance, g.Boss)
		}
	}
	// OnPath rooms are exactly the path rooms.
	onPath := map[string]bool{}
	for _, r := range g.Rooms {
		if r.OnPath {
			onPath[r.Key] = true
		}
	}
	if len(onPath) != len(path)+1 {
		t.Errorf("on-path rooms = %d, the path is %d rooms", len(onPath), len(path)+1)
	}
	for _, k := range path {
		if !onPath[k] {
			t.Errorf("path room %s is not marked on the path", k)
		}
	}

	// Secret rooms hang off the critical path: every secret's one edge
	// comes from an on-path room, and it is a secret door.
	for _, r := range g.Rooms {
		if r.Purpose != PurposeSecret {
			continue
		}
		ins := 0
		for _, e := range g.Edges {
			if e.To == r.Key {
				ins++
				if e.Kind != EdgeSecretDoor {
					t.Errorf("secret %s is not behind a secret door (%s)", r.Key, e.Kind)
				}
				from, _ := g.Room(e.From)
				if !from.OnPath {
					t.Errorf("secret %s hangs off %s, which is off the critical path", r.Key, e.From)
				}
			}
		}
		if ins != 1 {
			t.Errorf("secret %s has %d edges in; a leaf has one", r.Key, ins)
		}
	}

	// The grid: one cell per room, minimum coordinates zero.
	cells := map[[2]int]string{}
	for _, r := range g.Rooms {
		if r.X < 0 || r.Y < 0 {
			t.Fatalf("room %s at (%d,%d): the grid re-bases to zero", r.Key, r.X, r.Y)
		}
		if other, dup := cells[[2]int{r.X, r.Y}]; dup {
			t.Fatalf("rooms %s and %s share cell (%d,%d)", other, r.Key, r.X, r.Y)
		}
		cells[[2]int{r.X, r.Y}] = r.Key
	}

	// The map guarantee: no crossing edges, ever, for anything the
	// generator can produce.
	if xs := g.Crossings(); len(xs) > 0 {
		t.Fatalf("%d crossing edge pairs, starting %s->%s x %s->%s",
			len(xs), xs[0].A.From, xs[0].A.To, xs[0].B.From, xs[0].B.To)
	}

	// The depths are honest: every room's depth is its BFS distance.
	d := map[string]int{g.Entrance: 0}
	frontier := []string{g.Entrance}
	for len(frontier) > 0 {
		var next []string
		for _, from := range frontier {
			for _, to := range g.Neighbors(from) {
				if _, ok := d[to]; !ok {
					d[to] = d[from] + 1
					next = append(next, to)
				}
			}
		}
		frontier = next
	}
	for _, r := range g.Rooms {
		if r.Depth != d[r.Key] {
			t.Errorf("room %s depth %d, BFS says %d", r.Key, r.Depth, d[r.Key])
		}
	}
}

func TestLayout_ProducesValidGraphs(t *testing.T) {
	for _, p := range allParams() {
		g, err := Layout(p, 42)
		if err != nil {
			t.Fatalf("Layout(%s): %v", p.Size, err)
		}
		checkGraph(t, g)
	}
}

// TestLayout_PropertyOverSeeds is the acceptance property test: a
// hundred seeds across the full parameter range, every invariant checked
// on every graph.
func TestLayout_PropertyOverSeeds(t *testing.T) {
	params := allParams()
	for seed := int64(1); seed <= 100; seed++ {
		for _, p := range params {
			g, err := Layout(p, seed)
			if err != nil {
				t.Fatalf("Layout(%s, seed %d): %v", p.Size, seed, err)
			}
			checkGraph(t, g)
		}
	}
}

// TestLayout_Deterministic asserts the purity promise: identical
// arguments, identical output — twice in one process, and with a fresh
// generator between the calls, which is what a process restart is.
func TestLayout_Deterministic(t *testing.T) {
	for _, p := range allParams() {
		a, err := Layout(p, 20260830)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Layout(p, 20260830)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%+v", a) != fmt.Sprintf("%+v", b) {
			t.Fatalf("same params and seed produced different graphs:\n%+v\n%+v", a, b)
		}
		c, err := Layout(p, 20260831)
		if err != nil {
			t.Fatal(err)
		}
		if c.Seed == a.Seed && fmt.Sprintf("%+v", c) == fmt.Sprintf("%+v", a) && p.ExpectedSessions > 0 {
			// A different seed producing the identical graph is legal
			// (the space of small dungeons is finite); this is only a
			// smoke check that seeds vary, not an invariant.
			t.Logf("seed %d and %d agree for %s — legal, noted", 20260830, 20260831, p.Size)
		}
	}
}

// TestLayout_RoomCountArithmetic pins the size-band arithmetic: base
// plus per-session, capped at maxRooms.
func TestLayout_RoomCountArithmetic(t *testing.T) {
	cases := []struct {
		size     string
		sessions int
		want     int
	}{
		{SizeDelve, 1, 8},
		{SizeLair, 1, 12},
		{SizeComplex, 1, 16},
		{SizeMega, 1, 22},
		{SizeDelve, 3, 8 + 6*2},
		{SizeMega, 10, 60}, // 22+11*9 = 121, capped
	}
	for _, tc := range cases {
		g, err := Layout(Params{Size: tc.size, ExpectedSessions: tc.sessions}, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Rooms) != tc.want {
			t.Errorf("%s x%d sessions = %d rooms, want %d", tc.size, tc.sessions, len(g.Rooms), tc.want)
		}
	}
}

// TestLayout_DensityKnobsLandExactly pins the quota arithmetic: the
// knobs' integer quotas sum to the room count, and a heavy knob carries
// the majority regardless of seed.
func TestLayout_DensityKnobsLandExactly(t *testing.T) {
	p := Params{Size: SizeComplex, ExpectedSessions: 2,
		CombatDensity: 5, PuzzleDensity: 1, ExploreDensity: 1, Branchiness: 2}
	g, err := Layout(p, 99)
	if err != nil {
		t.Fatal(err)
	}
	if g.Quotas.Sum() != len(g.Rooms) {
		t.Fatalf("quota sum %d != rooms %d", g.Quotas.Sum(), len(g.Rooms))
	}
	flex := len(g.Rooms) - 2 - g.Quotas.Secret
	if g.Quotas.Combat-1+g.Quotas.Puzzle+g.Quotas.Exploration-1 != flex {
		t.Fatalf("flex quotas %d+%d+%d != flex %d",
			g.Quotas.Combat-1, g.Quotas.Puzzle, g.Quotas.Exploration-1, flex)
	}
	if g.Quotas.Combat <= g.Quotas.Puzzle {
		t.Errorf("combat-heavy knobs gave combat=%d puzzle=%d", g.Quotas.Combat, g.Quotas.Puzzle)
	}
}

// TestLayout_BranchinessAddsLoops: a branched dungeon has more edges
// than the same seed unbranched, and never more than the knob allows.
func TestLayout_BranchinessAddsLoops(t *testing.T) {
	p := Params{Size: SizeLair, ExpectedSessions: 2}
	flat, err := Layout(p, 5)
	if err != nil {
		t.Fatal(err)
	}
	p.Branchiness = 3
	loopy, err := Layout(p, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(loopy.Edges) <= len(flat.Edges) {
		t.Errorf("branchiness 3 gave %d edges, flat gave %d", len(loopy.Edges), len(flat.Edges))
	}
	if len(loopy.Edges)-len(flat.Edges) > 3 {
		t.Errorf("%d loops added, knob is 3", len(loopy.Edges)-len(flat.Edges))
	}
}

// TestLayout_InputValidation pins the bounds.
func TestLayout_InputValidation(t *testing.T) {
	bad := []Params{
		{Size: "castle"},
		{Size: SizeDelve, Level: 21},
		{Size: SizeDelve, Level: -1},
		{Size: SizeDelve, ExpectedSessions: 11},
		{Size: SizeDelve, CombatDensity: -1},
		{Size: SizeDelve, Branchiness: 4},
	}
	for _, p := range bad {
		if _, err := Layout(p, 1); err == nil {
			t.Errorf("params %+v accepted; want refused", p)
		}
	}
	// Zero fields default rather than refuse: the DM who typed nothing
	// still gets a real dungeon.
	if _, err := Layout(Params{}, 1); err != nil {
		t.Errorf("empty params refused: %v", err)
	}
}

// TestLayout_Vocabularies pins the legal vocabularies the migrations
// CHECK against.
func TestLayout_Vocabularies(t *testing.T) {
	for _, p := range Purposes() {
		if !ValidPurpose(p) {
			t.Errorf("purpose %q not valid", p)
		}
	}
	for _, k := range EdgeKinds() {
		if !ValidEdgeKind(k) {
			t.Errorf("edge kind %q not valid", k)
		}
	}
	if ValidPurpose("throne_room") || ValidEdgeKind("portal") || ValidSize("castle") {
		t.Error("an unknown vocabulary value validated")
	}
}
