package place

// Settlement shapes (MAD-372): the deterministic structural budget the
// location generator fills, computed from the settlement kind and the scale
// band before any prompt exists — the direct analogue of encounter.Plan's
// budget and campaign.BuildQuestTopology's machine. A hamlet gets two
// notable people, one service and one gathering place; a village gets its
// inn, temple and market; a town adds a watch; a city gets districts and
// delegates their interiors to a later request. The model names things and
// writes their prose; it does not decide how many of anything there are.
//
// The shape is also the refusal arithmetic: a generation that would exceed
// its band is refused with the count, never silently truncated, and a
// full batch's item count must sit inside MinItems..MaxItems.

import (
	"fmt"
	"strings"
)

// Settlement kinds. The place block's Kind is free prose, but the generator
// only runs on these four — a ruin or a dungeon is a different proposal
// (dungeon interiors are MAD-373).
const (
	KindHamlet  = "hamlet"
	KindVillage = "village"
	KindTown    = "town"
	KindCity    = "city"
)

// Scale bands: how big the place reads at the table within its kind.
const (
	ScaleSmall  = "small"
	ScaleMedium = "medium"
	ScaleLarge  = "large"
)

// placeKinds is the kind catalogue, in the fixed order surfaces offer it.
var placeKinds = []string{KindHamlet, KindVillage, KindTown, KindCity}

// placeScales is the scale catalogue.
var placeScales = []string{ScaleSmall, ScaleMedium, ScaleLarge}

// Kinds lists every settlement kind the generator runs on.
func Kinds() []string {
	out := make([]string, len(placeKinds))
	copy(out, placeKinds)
	return out
}

// Scales lists every scale band.
func Scales() []string {
	out := make([]string, len(placeScales))
	copy(out, placeScales)
	return out
}

// SubSlot is one sub-location the shape requires: its stable internal id,
// the role it plays (a village has an inn, a temple and a market; it has no
// cathedral district), and the brief the model fills against. Every slot
// arrives with its located_in edge to the parent already implied — a room
// that is nowhere is the thing the graph is for.
type SubSlot struct {
	ID    string `json:"id"`
	Role  string `json:"role"`
	Brief string `json:"brief"`
}

// Shape is one settlement kind and scale band's whole structural budget:
// how many notable NPCs, services, sub-locations, hooks and secrets a
// generation may propose, and which sub-location kinds it must include.
// Everything is arithmetic — the counts are fixed before the prompt.
type Shape struct {
	Kind     string `json:"kind"`
	Scale    string `json:"scale"`
	Label    string `json:"label"`
	NPCs     int    `json:"npcs"`     // notable NPCs
	Services int    `json:"services"` // service strings in the place block
	Hooks    int    `json:"hooks"`    // public hook facts
	Secrets  int    `json:"secrets"`  // secret facts, each with a holder
	// Subs are the required sub-locations; SubLocations == len(Subs).
	Subs []SubSlot `json:"subs"`
	// Districts marks a city: the sub-locations are districts, and their
	// interiors are delegated to a later flesh-out request.
	Districts bool `json:"districts"`
}

// SubLocations is how many sub-locations the band fixes.
func (s Shape) SubLocations() int { return len(s.Subs) }

/* ---------- the batch item-count band ---------- */

// The item count of a full generation, as the batch stages it:
//
//	1               the place entity (block and routes in its payload)
//	2 × subs        each sub-location entity + its located_in edge
//	2 × npcs        each notable NPC entity + its located_in edge
//	0..npcs         each NPC's faction edge, when one is attached
//	hooks           each hook fact
//	2 × secrets     each secret fact + its holder's discovery
//
// MinItems assumes no faction edges; MaxItems allows one per NPC. A parts
// run stages fewer items by construction — its per-part counts are the band.

// MinItems is the smallest batch a full generation of this shape stages.
func (s Shape) MinItems() int {
	return 1 + 2*len(s.Subs) + 2*s.NPCs + s.Hooks + 2*s.Secrets
}

// MaxItems is the largest batch a full generation of this shape stages.
func (s Shape) MaxItems() int { return s.MinItems() + s.NPCs }

// InsideBand reports whether a full batch's item count sits inside the
// shape's band — the assertion every generation must satisfy.
func (s Shape) InsideBand(items int) bool {
	return items >= s.MinItems() && items <= s.MaxItems()
}

/* ---------- the catalogue ---------- */

// sub is a shorthand builder for SubSlot.
func sub(id, role, brief string) SubSlot {
	return SubSlot{ID: id, Role: role, Brief: brief}
}

// settlementShapes is the catalogue: kind → scale → the concrete budget.
// The bands are deliberate arithmetic, not derived: a hamlet is a handful of
// families, a village is the smallest place that has an inn and something to
// hide, a town defends itself, a city divides into districts whose interiors
// are later work.
var settlementShapes = map[string]map[string]Shape{
	KindHamlet: {
		ScaleSmall: {
			Kind: KindHamlet, Scale: ScaleSmall, Label: "A small hamlet",
			NPCs: 1, Services: 1, Hooks: 2, Secrets: 1,
			Subs: []SubSlot{sub("sub-1", "commons", "The one shared roof: where everyone eats and argues.")},
		},
		ScaleMedium: {
			Kind: KindHamlet, Scale: ScaleMedium, Label: "A hamlet",
			NPCs: 2, Services: 1, Hooks: 2, Secrets: 1,
			Subs: []SubSlot{sub("sub-1", "commons", "The one shared roof: where everyone eats and argues.")},
		},
		ScaleLarge: {
			Kind: KindHamlet, Scale: ScaleLarge, Label: "A large hamlet",
			NPCs: 2, Services: 2, Hooks: 3, Secrets: 1,
			Subs: []SubSlot{
				sub("sub-1", "commons", "The shared roof: where everyone eats and argues."),
				sub("sub-2", "shrine", "A modest shrine — whatever the hamlet prays to, it prays to it here."),
			},
		},
	},
	KindVillage: {
		ScaleSmall: {
			Kind: KindVillage, Scale: ScaleSmall, Label: "A small village",
			NPCs: 2, Services: 2, Hooks: 2, Secrets: 1,
			Subs: []SubSlot{
				sub("sub-1", "inn", "The village inn: beds, ale, and everything anyone will say out loud."),
				sub("sub-2", "market", "The market square: what the village sells, and what it buys."),
			},
		},
		ScaleMedium: {
			Kind: KindVillage, Scale: ScaleMedium, Label: "A village",
			NPCs: 3, Services: 2, Hooks: 2, Secrets: 2,
			Subs: []SubSlot{
				sub("sub-1", "inn", "The village inn: beds, ale, and everything anyone will say out loud."),
				sub("sub-2", "temple", "The temple or shrine: what the village believes, and who enforces it."),
				sub("sub-3", "market", "The market square: what the village sells, and what it buys."),
			},
		},
		ScaleLarge: {
			Kind: KindVillage, Scale: ScaleLarge, Label: "A large village",
			NPCs: 4, Services: 3, Hooks: 3, Secrets: 2,
			Subs: []SubSlot{
				sub("sub-1", "inn", "The village inn: beds, ale, and everything anyone will say out loud."),
				sub("sub-2", "temple", "The temple or shrine: what the village believes, and who enforces it."),
				sub("sub-3", "market", "The market square: what the village sells, and what it buys."),
			},
		},
	},
	KindTown: {
		ScaleSmall: {
			Kind: KindTown, Scale: ScaleSmall, Label: "A small town",
			NPCs: 3, Services: 3, Hooks: 2, Secrets: 2,
			Subs: []SubSlot{
				sub("sub-1", "inn", "The town's best inn: where strangers sleep and rumours start."),
				sub("sub-2", "market", "The market: the town's trade in one square."),
				sub("sub-3", "temple", "The temple: the town's conscience, or its cover."),
			},
		},
		ScaleMedium: {
			Kind: KindTown, Scale: ScaleMedium, Label: "A town",
			NPCs: 4, Services: 3, Hooks: 3, Secrets: 2,
			Subs: []SubSlot{
				sub("sub-1", "inn", "The town's best inn: where strangers sleep and rumours start."),
				sub("sub-2", "market", "The market: the town's trade in one square."),
				sub("sub-3", "temple", "The temple: the town's conscience, or its cover."),
				sub("sub-4", "watch", "The watch house: who keeps the town's peace, such as it is."),
			},
		},
		ScaleLarge: {
			Kind: KindTown, Scale: ScaleLarge, Label: "A large town",
			NPCs: 5, Services: 4, Hooks: 3, Secrets: 2,
			Subs: []SubSlot{
				sub("sub-1", "inn", "The town's best inn: where strangers sleep and rumours start."),
				sub("sub-2", "market", "The market: the town's trade in one square."),
				sub("sub-3", "temple", "The temple: the town's conscience, or its cover."),
				sub("sub-4", "watch", "The watch house: who keeps the town's peace, such as it is."),
			},
		},
	},
	KindCity: {
		ScaleSmall: {
			Kind: KindCity, Scale: ScaleSmall, Label: "A small city",
			NPCs: 4, Services: 4, Hooks: 3, Secrets: 2, Districts: true,
			Subs: []SubSlot{
				sub("sub-1", "district", "One district: its character, its trade, its gates."),
				sub("sub-2", "district", "A second district, unlike the first."),
				sub("sub-3", "district", "A third district — where the city keeps what it hides."),
			},
		},
		ScaleMedium: {
			Kind: KindCity, Scale: ScaleMedium, Label: "A city",
			NPCs: 5, Services: 4, Hooks: 3, Secrets: 2, Districts: true,
			Subs: []SubSlot{
				sub("sub-1", "district", "One district: its character, its trade, its gates."),
				sub("sub-2", "district", "A second district, unlike the first."),
				sub("sub-3", "district", "A third district — where the city keeps what it hides."),
				sub("sub-4", "district", "A fourth district: the one that runs the other three."),
			},
		},
		ScaleLarge: {
			Kind: KindCity, Scale: ScaleLarge, Label: "A great city",
			NPCs: 6, Services: 5, Hooks: 3, Secrets: 2, Districts: true,
			Subs: []SubSlot{
				sub("sub-1", "district", "One district: its character, its trade, its gates."),
				sub("sub-2", "district", "A second district, unlike the first."),
				sub("sub-3", "district", "A third district — where the city keeps what it hides."),
				sub("sub-4", "district", "A fourth district: the one that runs the other three."),
				sub("sub-5", "district", "A fifth district: outside the walls, outside the law."),
			},
		},
	},
}

// ShapeFor fixes the structural budget for a settlement kind and scale
// band. An empty scale is the kind's medium band; an unknown kind or scale
// is refused with the legal values — the generator's callers surface that
// error, and nothing is generated from a shape that does not exist.
func ShapeFor(kind, scale string) (Shape, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	byScale, ok := settlementShapes[kind]
	if !ok {
		return Shape{}, fmt.Errorf("%q is not a settlement kind — the kinds are %s",
			kind, strings.Join(placeKinds, ", "))
	}
	scale = strings.ToLower(strings.TrimSpace(scale))
	if scale == "" {
		scale = ScaleMedium
	}
	shape, ok := byScale[scale]
	if !ok {
		return Shape{}, fmt.Errorf("%q is not a scale band — the scales are %s",
			scale, strings.Join(placeScales, ", "))
	}
	return shape, nil
}
