// Package dungeon implements the dungeon designer's seeded room graph
// (MAD-373): the topology a dungeon's dressing hangs on, as arithmetic.
//
// The package is deliberately pure — no database, no network, no clock —
// the same rule internal/clock follows, and the reason a whole dungeon
// can be generated, laid out and drawn without an API key. Same seed,
// same params, same dungeon, forever: re-rolling means changing the seed,
// which is a recorded decision in the dungeons row, not a refresh button.
//
// What Layout computes, in order:
//
//   - The room count from the size band and the expected session count,
//     against the DMG's adventuring-day arithmetic (six-to-eight
//     meaningful encounters a session — internal/encounter carries those
//     tables for combat; a dungeon reads them as rooms): a size band
//     scales how many of them one session burns.
//   - The purpose quotas from the density knobs: combat, puzzle and
//     exploration densities become integer room counts that sum to the
//     room count exactly, counted out before any model sees anything.
//   - A guaranteed critical path entrance -> boss, every room reachable
//     off it, loops added to the branchiness knob rather than to taste,
//     and secret rooms hung off the critical path so finding one is a
//     reward and missing one is survivable.
//   - A grid assignment that keeps the graph drawable without crossing
//     edges: the critical path runs down one column, each side gallery
//     runs along its own row to the right of the path, secrets sit in
//     their own columns to the left, and a loop edge is only added when
//     it crosses nothing already drawn and passes through no room. The
//     map is a rendering of the graph, not a second artefact.
//
// The model dresses rooms it is handed. It may name them and describe
// them; it may not add a room, remove a room or reconnect anything —
// which is enforced by the dressing pass in internal/canon, not here.
package dungeon

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

/* ---------- vocabularies ---------- */

// Room purposes. The CHECK on dungeon_rooms enforces this list; the
// generator only ever emits these.
const (
	PurposeEntrance  = "entrance"
	PurposeGuard     = "guard"
	PurposeHazard    = "hazard"
	PurposePuzzle    = "puzzle"
	PurposeTreasure  = "treasure"
	PurposeShrine    = "shrine"
	PurposePrison    = "prison"
	PurposeLair      = "lair"
	PurposeBoss      = "boss"
	PurposeSecret    = "secret"
	PurposeConnector = "connector"
)

// purposes is the full vocabulary, in declaration order.
var purposes = []string{
	PurposeEntrance, PurposeGuard, PurposeHazard, PurposePuzzle, PurposeTreasure,
	PurposeShrine, PurposePrison, PurposeLair, PurposeBoss, PurposeSecret, PurposeConnector,
}

// ValidPurpose reports whether s is a legal room purpose.
func ValidPurpose(s string) bool {
	for _, p := range purposes {
		if p == s {
			return true
		}
	}
	return false
}

// Purposes lists the legal room purposes.
func Purposes() []string {
	out := make([]string, len(purposes))
	copy(out, purposes)
	return out
}

// Edge kinds — how one room connects to the next. A locked door may name
// the key item that opens it (key_item_entity on the stored edge); a
// secret door hides a secret room; a collapse is one-way.
const (
	EdgeDoor       = "door"
	EdgeLockedDoor = "locked_door"
	EdgeSecretDoor = "secret_door"
	EdgeStair      = "stair"
	EdgeShaft      = "shaft"
	EdgePassage    = "passage"
	EdgeCollapse   = "collapse"
)

// edgeKinds is the legal vocabulary, the CHECK on dungeon_edges.
var edgeKinds = []string{
	EdgeDoor, EdgeLockedDoor, EdgeSecretDoor, EdgeStair, EdgeShaft, EdgePassage, EdgeCollapse,
}

// ValidEdgeKind reports whether s is a legal edge kind.
func ValidEdgeKind(s string) bool {
	for _, k := range edgeKinds {
		if k == s {
			return true
		}
	}
	return false
}

// EdgeKinds lists the legal edge kinds.
func EdgeKinds() []string {
	out := make([]string, len(edgeKinds))
	copy(out, edgeKinds)
	return out
}

// Size bands. A delve is one session's crawl; a megadungeon is a
// campaign's obsession.
const (
	SizeDelve   = "delve"
	SizeLair    = "lair"
	SizeComplex = "complex"
	SizeMega    = "megadungeon"
)

// sizes is the band catalogue in declaration order.
var sizes = []string{SizeDelve, SizeLair, SizeComplex, SizeMega}

// ValidSize reports whether s is a legal size band.
func ValidSize(s string) bool {
	for _, k := range sizes {
		if k == s {
			return true
		}
	}
	return false
}

// Sizes lists the legal size bands.
func Sizes() []string {
	out := make([]string, len(sizes))
	copy(out, sizes)
	return out
}

// Input bounds. Level is the 5e ladder; sessions cap where a single
// dungeon stops being a dungeon and becomes a campaign; branchiness is a
// 0-3 knob because a loop count past three in a room graph is a maze,
// not a dungeon.
const (
	MinLevel    = 1
	MaxLevel    = 20
	MinSessions = 1
	MaxSessions = 10
	MinBranch   = 0
	MaxBranch   = 3

	// maxRooms bounds the arithmetic: a megadungeon at ten sessions of
	// expected play. Past this the map stops reading and the quotas stop
	// meaning anything.
	maxRooms = 60

	// minRoomsPerSession and maxRoomsPerSession are the DMG adventuring
	// day's six-to-eight encounters, read as rooms: the range one session
	// of dungeoneering burns. Layout works inside it; the size band picks
	// where.
	minRoomsPerSession = 6
	maxRoomsPerSession = 8
)

// sizeBands carries each size band's arithmetic: a base room count and
// the rooms one further session of expected play adds. A delve burns its
// rooms fast (the whole thing is one session); a megadungeon pays its
// rooms out slowly because a party crosses it in pieces.
var sizeBands = map[string]struct{ base, perSession int }{
	SizeDelve:   {base: 8, perSession: minRoomsPerSession},
	SizeLair:    {base: 12, perSession: 7},
	SizeComplex: {base: 16, perSession: 7},
	SizeMega:    {base: 22, perSession: maxRoomsPerSession},
}

/* ---------- the input and the output ---------- */

// Params is what a dungeon is generated from: the theme (free prose the
// dressing pass reads), the size band, the party level, the expected
// session count, the three density knobs and the branchiness knob. The
// densities are non-negative weights, not percentages that must sum to
// one hundred — what must hold is that the integer quotas they produce
// sum to the room count, which largest-remainder allocation guarantees.
type Params struct {
	Theme            string `json:"theme"`
	Size             string `json:"size"`
	Level            int    `json:"level"`
	ExpectedSessions int    `json:"expected_sessions"`
	CombatDensity    int    `json:"combat_density"`
	PuzzleDensity    int    `json:"puzzle_density"`
	ExploreDensity   int    `json:"explore_density"`
	Branchiness      int    `json:"branchiness"`
}

// Default returns the params a DM who typed nothing gets: a delve for a
// 3rd-level party, one session, the knobs at their balanced settings.
func Default() Params {
	return Params{
		Size: SizeDelve, Level: 3, ExpectedSessions: 1,
		CombatDensity: 1, PuzzleDensity: 1, ExploreDensity: 1, Branchiness: 1,
	}
}

// normalize fills zero fields with their defaults and validates the rest.
func (p Params) normalize() (Params, error) {
	d := Default()
	out := p
	out.Theme = strings.TrimSpace(out.Theme)
	if out.Size == "" {
		out.Size = d.Size
	}
	if !ValidSize(out.Size) {
		return Params{}, fmt.Errorf("size %q is not a band — the bands are delve, lair, complex and megadungeon", out.Size)
	}
	if out.Level == 0 {
		out.Level = d.Level
	}
	if out.Level < MinLevel || out.Level > MaxLevel {
		return Params{}, fmt.Errorf("level %d is outside %d-%d", out.Level, MinLevel, MaxLevel)
	}
	if out.ExpectedSessions == 0 {
		out.ExpectedSessions = d.ExpectedSessions
	}
	if out.ExpectedSessions < MinSessions || out.ExpectedSessions > MaxSessions {
		return Params{}, fmt.Errorf("expected sessions %d is outside %d-%d", out.ExpectedSessions, MinSessions, MaxSessions)
	}
	if out.CombatDensity < 0 || out.PuzzleDensity < 0 || out.ExploreDensity < 0 {
		return Params{}, fmt.Errorf("density knobs cannot be negative")
	}
	if out.CombatDensity == 0 && out.PuzzleDensity == 0 && out.ExploreDensity == 0 {
		out.CombatDensity, out.PuzzleDensity, out.ExploreDensity = 1, 1, 1
	}
	if out.Branchiness == 0 {
		out.Branchiness = d.Branchiness
	}
	if out.Branchiness < MinBranch || out.Branchiness > MaxBranch {
		return Params{}, fmt.Errorf("branchiness %d is outside %d-%d", out.Branchiness, MinBranch, MaxBranch)
	}
	return out, nil
}

// Quotas is the room arithmetic the density knobs resolve to: integer
// counts per axis, counted out before any model sees anything. Combat
// covers guard, lair and boss rooms; exploration covers entrance,
// connector, treasure, shrine, prison and hazard rooms; puzzle covers
// puzzle rooms; secret covers the secret rooms hung off the critical
// path. The four axes sum to the room count exactly.
type Quotas struct {
	Combat      int `json:"combat"`
	Puzzle      int `json:"puzzle"`
	Exploration int `json:"exploration"`
	Secret      int `json:"secret"`
}

// Sum is the total room count the quotas account for.
func (q Quotas) Sum() int {
	return q.Combat + q.Puzzle + q.Exploration + q.Secret
}

// Room is one room of the graph before and after dressing: its stable key
// (r1, r2, ...), its purpose, its grid cell, its depth from the entrance,
// and the name and detail the dressing pass (or the DM) writes. Name is
// empty until someone names the room — the map shows the purpose then.
type Room struct {
	Key     string `json:"key"`
	Name    string `json:"name,omitempty"`
	Purpose string `json:"purpose"`
	Detail  string `json:"detail,omitempty"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Depth   int    `json:"depth"`
	// OnPath marks the critical path: entrance -> boss. Secrets hang off
	// it; wings hang off it; the path itself is what the party must be
	// able to walk.
	OnPath bool `json:"on_path"`
}

// Edge is one connection between rooms. KeyItem is the name of the item
// a locked door needs — filled by the dressing pass, empty on anything
// else. OneWay edges (a collapse, a shaft down) pass one direction only.
type Edge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Kind    string `json:"kind"`
	KeyItem string `json:"key_item,omitempty"`
	OneWay  bool   `json:"one_way,omitempty"`
}

// Graph is a whole dungeon topology: the params and seed it was computed
// from (echoed, so the stored params JSON is the layout's own record),
// the rooms in declaration order, the edges in declaration order, the
// quotas, and the entrance and boss keys.
type Graph struct {
	Params   Params `json:"params"`
	Seed     int64  `json:"seed"`
	Rooms    []Room `json:"rooms"`
	Edges    []Edge `json:"edges"`
	Quotas   Quotas `json:"quotas"`
	Entrance string `json:"entrance"`
	Boss     string `json:"boss"`
}

// Room returns the room with this key.
func (g Graph) Room(key string) (Room, bool) {
	for _, r := range g.Rooms {
		if r.Key == key {
			return r, true
		}
	}
	return Room{}, false
}

// Neighbors lists the rooms reachable from key along edges, honouring
// one-way edges (a collapse leads on, not back).
func (g Graph) Neighbors(key string) []string {
	var out []string
	for _, e := range g.Edges {
		if e.From == key {
			out = append(out, e.To)
		}
		if e.To == key && !e.OneWay {
			out = append(out, e.From)
		}
	}
	return out
}

// Reachable returns the set of room keys reachable from start along
// declared edges (one-way honoured), start included.
func (g Graph) Reachable(start string) map[string]bool {
	out := map[string]bool{start: true}
	frontier := []string{start}
	for len(frontier) > 0 {
		var next []string
		for _, from := range frontier {
			for _, to := range g.Neighbors(from) {
				if !out[to] {
					out[to] = true
					next = append(next, to)
				}
			}
		}
		frontier = next
	}
	return out
}

/* ---------- the generator ---------- */

// wing is one side area: a chain of rooms hung off a critical-path
// anchor. The anchor feeds the chain's first room; the chain runs on.
type wing struct {
	anchor string
	chain  []string
}

// Layout computes a dungeon's whole topology from its params and seed:
// room count, purpose quotas, the critical path, the wings, the secrets,
// the loops and the grid. Identical arguments produce an identical graph,
// across calls, across process restarts, forever.
func Layout(p Params, seed int64) (Graph, error) {
	p, err := p.normalize()
	if err != nil {
		return Graph{}, err
	}
	rng := newRNG(seed)

	// 1. The room count: the size band's base plus its per-session rate.
	band := sizeBands[p.Size]
	roomCount := band.base + band.perSession*(p.ExpectedSessions-1)
	if roomCount > maxRooms {
		roomCount = maxRooms
	}

	// 2. The secrets: one always, more with branchiness, never so many
	// the dungeon is all reward. The count is drawn from the seed.
	secrets := 1 + rng.intn(1+p.Branchiness)
	if cap := roomCount / 6; secrets > cap {
		secrets = cap
	}
	if secrets < 1 {
		secrets = 1
	}

	// 3. The quotas: largest-remainder allocation of the flexible rooms
	// over the three density knobs. The flexible rooms are every room
	// but the entrance, the boss and the secrets; the structural rooms
	// join their axes after, so the four quotas always sum to the room
	// count.
	flex := roomCount - 2 - secrets
	flexCombat, flexPuzzle, flexExplore := largestRemainder(
		p.CombatDensity, p.PuzzleDensity, p.ExploreDensity, flex)

	// 4. The skeleton: a critical path entrance -> ... -> boss, with the
	// flexible rooms split between inner path rooms and wings off them.
	// The path is long enough that wings are wings, short enough that
	// the path is a path; the wings cluster toward the boss because the
	// boss's door is guarded by what stands before it.
	pathLen := 3 + flex/5
	if pathLen > 7 {
		pathLen = 7
	}
	if pathLen > flex+2 {
		pathLen = flex + 2
	}
	wingRooms := flex - (pathLen - 2)

	g := Graph{Params: p, Seed: seed}
	pathKeys := make([]string, pathLen)
	for i := range pathKeys {
		pathKeys[i] = g.newKey()
		g.Rooms = append(g.Rooms, Room{Key: pathKeys[i], OnPath: true})
	}
	g.Entrance, g.Boss = pathKeys[0], pathKeys[len(pathKeys)-1]
	inner := pathKeys[1 : len(pathKeys)-1]

	// The side galleries: one per inner path room — two galleries off one
	// anchor would share a row — dealt deepest-first (the boss's
	// neighbourhood fills before the entrance's). A gallery is a chain;
	// its rooms run outward along the anchor's row. When every anchor has
	// its gallery and rooms remain, the galleries extend, round-robin
	// from a seeded start — a megadungeon's wings are long walks.
	var wings []wing
	anchorAt := len(inner) - 1
	for wingRooms > 0 && anchorAt >= 0 {
		n := 1 + rng.intn(3)
		if n > wingRooms {
			n = wingRooms
		}
		w := wing{anchor: inner[anchorAt]}
		for i := 0; i < n; i++ {
			k := g.newKey()
			g.Rooms = append(g.Rooms, Room{Key: k})
			w.chain = append(w.chain, k)
		}
		wings = append(wings, w)
		wingRooms -= n
		anchorAt--
	}
	for i := 0; wingRooms > 0; i++ {
		at := i % len(wings)
		k := g.newKey()
		g.Rooms = append(g.Rooms, Room{Key: k})
		wings[at].chain = append(wings[at].chain, k)
		wingRooms--
	}

	// The secret rooms: leaves hung off the critical path, off inner
	// path rooms only — never off the entrance (too cheap) and never off
	// the boss (the reward is what the boss guards, not a side door).
	secretKeys := make([]string, secrets)
	secretAnchors := make([]string, secrets)
	for i := 0; i < secrets; i++ {
		k := g.newKey()
		g.Rooms = append(g.Rooms, Room{Key: k})
		secretKeys[i] = k
		secretAnchors[i] = inner[rng.intn(len(inner))]
	}

	// 5. The edges, kinds chosen now so the draw order is fixed: the
	// path (the boss's door is locked — the key is the point), the wings
	// (a collapse is one-way), the secrets (behind secret doors).
	pathKinds := []string{EdgeDoor, EdgePassage}
	for i := 0; i+1 < len(pathKeys); i++ {
		kind := pathKinds[rng.intn(len(pathKinds))]
		if i == len(pathKeys)-2 {
			kind = EdgeLockedDoor
		}
		g.Edges = append(g.Edges, Edge{From: pathKeys[i], To: pathKeys[i+1], Kind: kind})
	}
	wingKinds := []string{EdgeDoor, EdgePassage, EdgeStair}
	for _, w := range wings {
		g.Edges = append(g.Edges, Edge{From: w.anchor, To: w.chain[0], Kind: wingKinds[rng.intn(len(wingKinds))]})
		for i := 0; i+1 < len(w.chain); i++ {
			kind := EdgeDoor
			switch rng.intn(8) {
			case 0:
				kind = EdgeShaft
			case 1:
				kind = EdgeCollapse
			case 2:
				kind = EdgeStair
			}
			g.Edges = append(g.Edges, Edge{From: w.chain[i], To: w.chain[i+1], Kind: kind, OneWay: kind == EdgeCollapse})
		}
	}
	for i, k := range secretKeys {
		g.Edges = append(g.Edges, Edge{From: secretAnchors[i], To: k, Kind: EdgeSecretDoor})
	}

	// 6. The depths (BFS from the entrance), then the purposes: the
	// quota pools realized as room purposes, dealt deepest-first so
	// combat sits deep and connectors sit shallow. Entrance and boss are
	// already spoken for; secrets keep their own purpose.
	g = assignDepths(g)
	pools := make([]string, 0, flex)
	for i := 0; i < flexCombat; i++ {
		pools = append(pools, PurposeGuard)
	}
	for i := 0; i < flexPuzzle; i++ {
		pools = append(pools, PurposePuzzle)
	}
	explorePurposes := []string{PurposeConnector, PurposeTreasure, PurposeShrine, PurposePrison, PurposeHazard}
	for i := 0; i < flexExplore; i++ {
		pools = append(pools, explorePurposes[rng.intn(len(explorePurposes))])
	}
	flexIdx := make([]int, 0, flex)
	for i := range g.Rooms {
		k := g.Rooms[i].Key
		if k == g.Entrance || k == g.Boss || containsStr(secretKeys, k) {
			continue
		}
		flexIdx = append(flexIdx, i)
	}
	order := make([]int, len(flexIdx))
	copy(order, flexIdx)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := g.Rooms[order[i]], g.Rooms[order[j]]
		if a.Depth != b.Depth {
			return a.Depth > b.Depth
		}
		return a.Key > b.Key
	})
	// The deepest flex rooms become lairs rather than guards: the things
	// that live deepest live largest.
	lairs := 0
	if flexCombat > 2 {
		lairs = 1 + rng.intn(flexCombat/2)
	}
	for i, idx := range order {
		purpose := pools[i]
		if purpose == PurposeGuard && lairs > 0 && g.Rooms[idx].Depth >= pathLen {
			purpose = PurposeLair
			lairs--
		}
		g.Rooms[idx].Purpose = purpose
	}
	for i := range g.Rooms {
		switch {
		case g.Rooms[i].Key == g.Entrance:
			g.Rooms[i].Purpose = PurposeEntrance
		case g.Rooms[i].Key == g.Boss:
			g.Rooms[i].Purpose = PurposeBoss
		case containsStr(secretKeys, g.Rooms[i].Key):
			g.Rooms[i].Purpose = PurposeSecret
		}
	}

	g.Quotas = Quotas{
		Combat:      flexCombat + 1, // + the boss
		Puzzle:      flexPuzzle,
		Exploration: flexExplore + 1, // + the entrance
		Secret:      secrets,
	}

	// 7. The grid, then the loops against it. The grid: the path runs
	// down column 0, each wing owns its own column to the right, each
	// secret owns its own column to the left; loop edges are added only
	// when they cross nothing already drawn.
	assignGrid(&g, pathKeys, wings, secretKeys, secretAnchors)
	addLoops(&g, rng, p.Branchiness, pathKeys, wings)

	// The depths are final now (a loop can shorten some walks).
	return assignDepths(g), nil
}

// newKey mints the next stable room key: r1, r2, ...
func (g *Graph) newKey() string {
	return fmt.Sprintf("r%d", len(g.Rooms)+1)
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// largestRemainder splits total over three non-negative weights so the
// parts sum to total exactly: proportional floors first, the remainders
// to the largest fractional parts, ties broken by weight order (combat,
// puzzle, exploration) so the split is deterministic.
func largestRemainder(w1, w2, w3, total int) (int, int, int) {
	sum := w1 + w2 + w3
	shares := []float64{
		float64(w1) * float64(total) / float64(sum),
		float64(w2) * float64(total) / float64(sum),
		float64(w3) * float64(total) / float64(sum),
	}
	out := []int{int(math.Floor(shares[0])), int(math.Floor(shares[1])), int(math.Floor(shares[2]))}
	order := []int{0, 1, 2}
	sort.SliceStable(order, func(i, j int) bool {
		fracI := shares[order[i]] - math.Floor(shares[order[i]])
		fracJ := shares[order[j]] - math.Floor(shares[order[j]])
		if fracI != fracJ {
			return fracI > fracJ
		}
		return order[i] < order[j]
	})
	for left := total - out[0] - out[1] - out[2]; left > 0; left-- {
		out[order[left%3]]++
	}
	return out[0], out[1], out[2]
}

// assignDepths sets every room's depth: graph distance from the entrance
// along edges, one-way honoured (a one-way edge leads on; the way back is
// the way you came).
func assignDepths(g Graph) Graph {
	depth := map[string]int{g.Entrance: 0}
	frontier := []string{g.Entrance}
	for len(frontier) > 0 {
		var next []string
		for _, from := range frontier {
			for _, to := range g.Neighbors(from) {
				if _, seen := depth[to]; !seen {
					depth[to] = depth[from] + 1
					next = append(next, to)
				}
			}
		}
		frontier = next
	}
	for i := range g.Rooms {
		g.Rooms[i].Depth = depth[g.Rooms[i].Key]
	}
	return g
}

// assignGrid places the rooms on the integer grid with no crossing
// edges by construction — a dungeon the way a keyed side-view reads:
//
//   - the critical path runs down column 0, one room per row;
//   - every side gallery runs along its own row, one room per column to
//     the right of the path — a gallery's row belongs to its anchor, and
//     no two galleries share a row, so nothing on the right collides;
//   - every secret room sits in its own column to the left of the path,
//     staggered one row down per secret so two secrets off one anchor
//     fan out instead of stacking.
//
// Path edges are vertical at column 0; gallery edges horizontal at their
// row; secret edges fan left from their anchor. The only interiors that
// could ever meet are on the right between a gallery row and the spine,
// which touch at their shared anchor alone. The result is re-based so
// the minimum x and y are 0.
func assignGrid(g *Graph, pathKeys []string, wings []wing, secretKeys, secretAnchors []string) {
	pos := map[string][2]int{}
	pathRow := map[string]int{}
	for i, k := range pathKeys {
		pos[k] = [2]int{0, i}
		pathRow[k] = i
	}
	for _, w := range wings {
		for j, k := range w.chain {
			pos[k] = [2]int{1 + j, pathRow[w.anchor]}
		}
	}
	for i, k := range secretKeys {
		pos[k] = [2]int{-1 - i, pathRow[secretAnchors[i]] + i}
	}
	minX, minY := 1<<30, 1<<30
	for _, p := range pos {
		if p[0] < minX {
			minX = p[0]
		}
		if p[1] < minY {
			minY = p[1]
		}
	}
	for i := range g.Rooms {
		p := pos[g.Rooms[i].Key]
		g.Rooms[i].X = p[0] - minX
		g.Rooms[i].Y = p[1] - minY
	}
}

// addLoops adds up to branchiness loop edges — shortcuts and second
// routes — each one only when it crosses nothing already drawn and
// passes through no room's cell. The candidates are seeded, ordered
// shortest first (a short hop is likelier to be drawable), and every
// accepted edge is checked against every drawn edge. What the generator
// emits, Crossings can certify: zero.
func addLoops(g *Graph, rng *rng, branchiness int, pathKeys []string, wings []wing) {
	if branchiness <= 0 || len(g.Rooms) < 4 {
		return
	}
	roomAt := map[string][2]int{}
	for _, r := range g.Rooms {
		roomAt[r.Key] = [2]int{r.X, r.Y}
	}
	existing := map[string]bool{}
	for _, e := range g.Edges {
		existing[edgeID(e.From, e.To)] = true
		existing[edgeID(e.To, e.From)] = true
	}
	// The candidate pairs: one gallery's tail to the next gallery's tail
	// (a crawl-round between wings), and a gallery's tail to the
	// critical path room below its anchor (a second way down).
	type pair struct{ a, b string }
	var candidates []pair
	for i := 0; i < len(wings); i++ {
		for j := i + 1; j < len(wings); j++ {
			candidates = append(candidates, pair{
				wings[i].chain[len(wings[i].chain)-1],
				wings[j].chain[len(wings[j].chain)-1],
			})
		}
	}
	for _, w := range wings {
		tail := w.chain[len(w.chain)-1]
		for i, k := range pathKeys {
			if k == w.anchor && i+2 < len(pathKeys) {
				candidates = append(candidates, pair{tail, pathKeys[i+2]})
			}
		}
	}
	for i := len(candidates) - 1; i > 0; i-- {
		j := rng.intn(i + 1)
		candidates[i], candidates[j] = candidates[j], candidates[i]
	}
	dist := func(p pair) float64 {
		a, b := roomAt[p.a], roomAt[p.b]
		dx, dy := float64(a[0]-b[0]), float64(a[1]-b[1])
		return math.Sqrt(dx*dx + dy*dy)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return dist(candidates[i]) < dist(candidates[j])
	})
	drawn := make([]Edge, len(g.Edges))
	copy(drawn, g.Edges)
	added := 0
	for _, p := range candidates {
		if added >= branchiness {
			break
		}
		if p.a == p.b || existing[edgeID(p.a, p.b)] {
			continue
		}
		e := Edge{From: p.a, To: p.b, Kind: EdgePassage}
		if crossesAny(roomAt, drawn, e) || passesThroughRoom(roomAt, e) {
			continue
		}
		g.Edges = append(g.Edges, e)
		drawn = append(drawn, e)
		existing[edgeID(p.a, p.b)] = true
		added++
	}
}

// passesThroughRoom reports whether the edge's rendering runs through
// any room's cell — a crossing the edge-edge test cannot see, because
// the room is a vertex of the edges it touches. The edge's own endpoints
// are its rooms and never count.
func passesThroughRoom(roomAt map[string][2]int, e Edge) bool {
	a, b := roomAt[e.From], roomAt[e.To]
	ax, ay := float64(a[0]), float64(a[1])
	bx, by := float64(b[0]), float64(b[1])
	for key, p := range roomAt {
		if key == e.From || key == e.To {
			continue
		}
		// Point-on-segment: collinear and inside the bounding box.
		px, py := float64(p[0]), float64(p[1])
		cross := (bx-ax)*(py-ay) - (by-ay)*(px-ax)
		if cross != 0 {
			continue
		}
		if px >= math.Min(ax, bx) && px <= math.Max(ax, bx) &&
			py >= math.Min(ay, by) && py <= math.Max(ay, by) {
			return true
		}
	}
	return false
}

func edgeID(a, b string) string { return a + "\x00" + b }

/* ---------- the crossing certification ---------- */

// Crossing names one pair of edges whose straight-line renderings share
// an interior point. Shared endpoints never count — two edges out of one
// room are the graph, not a collision.
type Crossing struct {
	A Edge `json:"a"`
	B Edge `json:"b"`
}

// Crossings returns every pair of edges whose renderings cross. The
// generator's own output must always be empty; the DM's hand edits are
// the DM's to judge, and the map still renders.
func (g Graph) Crossings() []Crossing {
	roomAt := map[string][2]int{}
	for _, r := range g.Rooms {
		roomAt[r.Key] = [2]int{r.X, r.Y}
	}
	var out []Crossing
	for i := 0; i < len(g.Edges); i++ {
		for j := i + 1; j < len(g.Edges); j++ {
			a, b := g.Edges[i], g.Edges[j]
			if a.From == b.From || a.From == b.To || a.To == b.From || a.To == b.To {
				continue
			}
			if segmentsCross(roomAt[a.From], roomAt[a.To], roomAt[b.From], roomAt[b.To]) {
				out = append(out, Crossing{A: a, B: b})
			}
		}
	}
	return out
}

func crossesAny(roomAt map[string][2]int, drawn []Edge, e Edge) bool {
	for _, d := range drawn {
		if e.From == d.From || e.From == d.To || e.To == d.From || e.To == d.To {
			continue
		}
		if segmentsCross(roomAt[e.From], roomAt[e.To], roomAt[d.From], roomAt[d.To]) {
			return true
		}
	}
	return false
}

// segmentsCross reports whether the segment p1-p2 and the segment
// q1-q2 share an interior point, collinear overlaps included.
func segmentsCross(p1, p2, q1, q2 [2]int) bool {
	p1f := [2]float64{float64(p1[0]), float64(p1[1])}
	p2f := [2]float64{float64(p2[0]), float64(p2[1])}
	q1f := [2]float64{float64(q1[0]), float64(q1[1])}
	q2f := [2]float64{float64(q2[0]), float64(q2[1])}
	d1 := cross(sub(q1f, q2f), sub(p1f, q1f))
	d2 := cross(sub(q1f, q2f), sub(p2f, q1f))
	d3 := cross(sub(p1f, p2f), sub(q1f, p1f))
	d4 := cross(sub(p1f, p2f), sub(q2f, p1f))
	if ((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) && ((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0)) {
		return true
	}
	if d1 == 0 && d2 == 0 && d3 == 0 && d4 == 0 {
		return overlap1D(p1[0], p2[0], q1[0], q2[0]) && overlap1D(p1[1], p2[1], q1[1], q2[1])
	}
	return false
}

func overlap1D(a1, a2, b1, b2 int) bool {
	lo, hi := a1, a2
	if lo > hi {
		lo, hi = hi, lo
	}
	blo, bhi := b1, b2
	if blo > bhi {
		blo, bhi = bhi, blo
	}
	return lo <= bhi && blo <= hi
}

func sub(a, b [2]float64) [2]float64 { return [2]float64{a[0] - b[0], a[1] - b[1]} }
func cross(a, b [2]float64) float64  { return a[0]*b[1] - a[1]*b[0] }
