package canon

// The location and settlement generator (MAD-372, stage 5.2 of MAD-316): a
// premise becomes a village — the place, its notable people, its
// sub-locations, its services, its hooks and its secrets — as one proposal
// batch through the MAD-359 review gate.
//
// Like the quest designer, this generator does not ask a model for
// structure. Before any prompt exists:
//
//   - place.ShapeFor fixes the structural budget from the settlement kind
//     and scale band: how many notable NPCs, services, sub-locations, hooks
//     and secrets, and which sub-location kinds the settlement type
//     requires (a village has an inn, a temple and a market; it has no
//     cathedral district). A city gets districts and delegates their
//     interiors to a later flesh-out request. A generation that would
//     exceed its band is refused with the count, never silently truncated.
//   - The premise is read deterministically for terrain, threat and tone
//     signal, the way encounter.ReadIdea and ReadQuestHook read theirs.
//   - Reuse before invention: every proposed name resolves against
//     entity_aliases through campaign.ResolveName first, and the reuse
//     slots are offered scored candidates out of the campaign. Generating
//     a village near an existing town proposes a route and a located_in
//     edge to that town, not a second copy of it.
//   - Secrets arrive with a way to be found: every visibility='secret'
//     fact stages a sibling discovery item that grants its holder an
//     awareness stance, so nothing is born flagged unreachable_secret.
//   - Routes are written even if unread: the near anchor and the parent
//     get travel.routes entries in the travel payload block, so the place
//     is navigable the moment it is accepted.
//
// The flesh-out path (POST .../locations/{eid}/design/flesh-out) proposes
// around an existing location — the block's empty fields, the missing
// people, the missing secrets — never replacing what is already there.
// Regeneration is part-addressed (parts: place, sublocations, npcs, hooks,
// secrets) because a batch is item-addressable: dismiss the people items,
// keep the geography, flesh out again.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/place"
)

/* ---------- the deterministic premise read ---------- */

// PlaceSignals is what a free-text premise deterministically implies: the
// terrain the place sits in, threat classes, the tone, and the leftover
// words to score candidate entities with. The analogue of encounter.Hints
// and QuestHookSignals for a place.
type PlaceSignals struct {
	Terrain string   `json:"terrain,omitempty"`
	Threats []string `json:"threats,omitempty"`
	Tone    string   `json:"tone,omitempty"`
	Terms   []string `json:"terms,omitempty"`
}

// premiseTerrainWords maps premise words onto the terrain a place sits in.
var premiseTerrainWords = map[string]string{
	"forest": "forest", "woods": "forest", "woodland": "forest", "timber": "forest",
	"wildwood": "forest", "trees": "forest",
	"coast": "coast", "coastal": "coast", "shore": "coast", "seaside": "coast",
	"sea": "coast", "harbour": "coast", "harbor": "coast", "cliffs": "coast",
	"mountain": "mountains", "mountains": "mountains", "pass": "mountains",
	"highland": "mountains", "valley": "mountains", "hills": "mountains",
	"swamp": "swamp", "marsh": "swamp", "mire": "swamp", "fen": "swamp",
	"bog": "swamp", "wetland": "swamp",
	"river": "river", "riverbank": "river", "ford": "river", "bridge": "river",
	"lake": "river", "island": "island", "isle": "island", "islands": "island",
	"road": "road", "roadside": "road", "crossroads": "road", "highroad": "road",
	"plains": "plains", "grassland": "plains", "steppe": "plains", "prairie": "plains",
	"desert": "desert", "dunes": "desert", "wastes": "desert", "waste": "desert",
	"snow": "tundra", "tundra": "tundra", "arctic": "tundra", "frozen": "tundra",
	"mine": "mining hills", "mines": "mining hills", "mining": "mining hills",
	"underdark": "underdark", "caverns": "underdark",
}

// premiseToneWords maps premise words onto the place's tone: what is in
// the trees, as the premise example has it.
var premiseToneWords = map[string]string{
	"uneasy": "uneasy", "unease": "uneasy", "wary": "uneasy", "afraid": "uneasy",
	"fearful": "uneasy", "nervous": "uneasy", "distrust": "uneasy",
	"lurking": "uneasy", "watching": "uneasy", "vanished": "uneasy",
	"disappearances": "uneasy", "disappeared": "uneasy", "missing": "uneasy",
	"besieged": "besieged", "siege": "besieged", "raided": "besieged",
	"raiders": "besieged", "attacks": "besieged",
	"prosperous": "prosperous", "thriving": "prosperous", "wealthy": "prosperous",
	"growing": "prosperous", "boomtown": "prosperous",
	"devout": "devout", "pious": "devout", "pilgrims": "devout", "shrine": "devout",
	"haunted": "haunted", "cursed": "haunted", "ruined": "haunted",
	"corrupt": "corrupt", "corruption": "corrupt", "venal": "corrupt",
	"fading": "fading", "declining": "fading", "dying": "fading", "emptying": "fading",
	"frontier": "frontier", "remote": "frontier", "isolated": "frontier",
}

// premiseStopwords carry no place signal.
var premiseStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"they": true, "them": true, "their": true, "there": true, "here": true,
	"want": true, "like": true, "some": true, "something": true, "anything": true,
	"place": true, "town": true, "village": true, "hamlet": true, "city": true,
	"settlement": true, "location": true, "small": true, "little": true,
	"party": true, "players": true, "player": true, "characters": true,
	"character": true, "campaign": true, "game": true, "session": true,
	"make": true, "build": true, "give": true, "generate": true, "design": true,
	"need": true, "would": true, "could": true, "really": true, "very": true,
	"just": true, "maybe": true, "should": true, "about": true, "into": true,
	"from": true, "over": true, "than": true, "then": true, "when": true,
	"were": true, "been": true, "have": true, "has": true, "had": true, "was": true,
	"will": true, "must": true, "what": true, "whats": true, "its": true,
}

var placeWordRE = regexp.MustCompile(`[a-z][a-z'-]+`)

// premiseTones is the tone vocabulary in deterministic tie-break order.
var premiseTones = []string{
	"uneasy", "besieged", "prosperous", "devout",
	"haunted", "corrupt", "fading", "frontier",
}

// premiseTerrainPoint marks the terrains a settlement sits ON rather than
// IN: a road, a river, a mine, a coast. When a premise names both kinds —
// "a village on the forest road" — the point feature wins: the village is
// on the road, and the forest is what the road runs through.
var premiseTerrainPoint = map[string]bool{
	"road": true, "river": true, "mining hills": true, "coast": true, "island": true,
}

// betterTerrain picks between an earlier terrain read and a later one: a
// point feature beats a region; otherwise the first read stands.
func betterTerrain(current, next string) string {
	if current == "" {
		return next
	}
	if premiseTerrainPoint[next] && !premiseTerrainPoint[current] {
		return next
	}
	return current
}

// ReadPlacePremise pulls the deterministic signal out of a free-text premise:
// the terrain it names (a point feature beats a region — "a village on the
// forest road" sits on a road through forest),
// the threat classes its words imply, the dominant tone, and the leftover
// terms to score candidate entities with.
func ReadPlacePremise(premise string) PlaceSignals {
	words := placeWordRE.FindAllString(strings.ToLower(premise), -1)
	var s PlaceSignals
	toneCounts := map[string]int{}
	seenThreat := map[string]bool{}
	seenTerm := map[string]bool{}
	for _, w := range words {
		singular := strings.TrimSuffix(w, "s")
		if t, ok := premiseTerrainWords[w]; ok {
			s.Terrain = betterTerrain(s.Terrain, t)
		} else if t, ok := premiseTerrainWords[singular]; ok {
			s.Terrain = betterTerrain(s.Terrain, t)
		}
		for _, th := range threatWords[w] {
			if !seenThreat[th] {
				seenThreat[th] = true
				s.Threats = append(s.Threats, th)
			}
		}
		if tone, ok := premiseToneWords[w]; ok {
			toneCounts[tone]++
		} else if tone, ok := premiseToneWords[singular]; ok {
			toneCounts[tone]++
		}
		if len(w) < 4 || premiseStopwords[w] || seenTerm[w] {
			continue
		}
		seenTerm[w] = true
		s.Terms = append(s.Terms, w)
	}
	best, bestN := "", 0
	for _, tone := range premiseTones {
		if toneCounts[tone] > bestN {
			best, bestN = tone, toneCounts[tone]
		}
	}
	s.Tone = best
	sort.Strings(s.Threats)
	return s
}

/* ---------- parts ---------- */

// Location design parts: which pieces of the shape a run proposes. Empty
// means everything a flesh-out target still has room for; a fresh design
// always stages the place itself.
const (
	LocationPartPlace        = "place"
	LocationPartSublocations = "sublocations"
	LocationPartNPCs         = "npcs"
	LocationPartHooks        = "hooks"
	LocationPartSecrets      = "secrets"
)

var locationPartsKnown = map[string]bool{
	LocationPartPlace: true, LocationPartSublocations: true, LocationPartNPCs: true,
	LocationPartHooks: true, LocationPartSecrets: true,
}

/* ---------- the input and the result ---------- */

// LocationDesignInput is one design request: the premise, the settlement
// kind and scale band, an optional parent location and near anchor, and —
// for the flesh-out path — the existing location to propose around, with
// optional parts. CreatedBy is the DM asking.
type LocationDesignInput struct {
	CampaignID string
	Premise    string
	Kind       string
	Scale      string
	Parent     string
	Near       string
	Location   string
	Parts      []string
	CreatedBy  string
}

// LocationDesignResult is what one generator run produced: the staged
// batch, the scale band that fixed the budget, and the entities reused
// rather than duplicated. The graph itself changes only when the batch is
// decided.
type LocationDesignResult struct {
	Batch     *Batch         `json:"batch,omitempty"`
	Shape     place.Shape    `json:"shape"`
	Reused    []ReusedEntity `json:"reused,omitempty"`
	Generated time.Time      `json:"generated_at"`
}

// maxPlacePremiseLen bounds the premise text.
const maxPlacePremiseLen = 2000

// placeCandidateCap is how many scored candidates each reuse slot offers.
const placeCandidateCap = 6

// placeRosterCap is how many of the campaign's entities ride along as
// context.
const placeRosterCap = 12

// placeSlotCandidate is one scored entity for a reuse slot.
type placeSlotCandidate struct {
	ID     string
	Name   string
	Kind   string
	Score  int
	Reason string
}

/* ---------- the generator ---------- */

// placeWant is what one run proposes: the shape's budget minus what a
// flesh-out target already has, per part.
type placeWant struct {
	fresh   bool
	place   bool
	subs    int
	npcs    int
	hooks   int
	secrets int
}

// GenerateLocation runs one design exchange: compute the shape, one
// structured-generation call to fill it, validate the fill, one repair
// retry, the reuse read-back, then stage the place, its people, its
// sub-locations, its hooks and its secrets as a proposal batch. Nothing is
// written to the graph until DecideBatch accepts it.
func (s *Store) GenerateLocation(ctx context.Context, in LocationDesignInput) (*LocationDesignResult, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	in.Premise = strings.TrimSpace(in.Premise)
	if in.Premise == "" {
		return nil, fmt.Errorf("%w: a location needs a premise", ErrInvalid)
	}
	if len([]rune(in.Premise)) > maxPlacePremiseLen {
		return nil, fmt.Errorf("%w: the premise is longer than %d characters", ErrInvalid, maxPlacePremiseLen)
	}

	// Parts: empty means everything with room.
	parts := map[string]bool{}
	for _, p := range in.Parts {
		p = strings.TrimSpace(p)
		if !locationPartsKnown[p] {
			return nil, fmt.Errorf("%w: %q is not a location part (place, sublocations, npcs, hooks, secrets)", ErrInvalid, p)
		}
		parts[p] = true
	}
	allParts := len(parts) == 0

	if _, err := s.loadCampaign(ctx, in.CampaignID); err != nil {
		return nil, err
	}
	snap, err := LoadSnapshot(ctx, s.db, in.CampaignID)
	if err != nil {
		return nil, err
	}
	signals := ReadPlacePremise(in.Premise)

	// The flesh-out target: an existing location proposed around, never
	// replaced.
	var target *campaign.Entity
	if in.Location != "" {
		for i := range snap.Entities {
			e := &snap.Entities[i]
			if e.ID == in.Location && e.Status != campaign.StatusDeleted {
				target = e
				break
			}
		}
		if target == nil {
			return nil, fmt.Errorf("%w: location %s", ErrNotFound, in.Location)
		}
		if target.Kind != campaign.KindLocation {
			return nil, fmt.Errorf("%w: %s is a %s, not a location", ErrInvalid, target.Name, target.Kind)
		}
	}

	// The parent and the near anchor: live locations of this campaign (a
	// road leads to a place).
	findLoc := func(id, what string) (*campaign.Entity, error) {
		if strings.TrimSpace(id) == "" {
			return nil, nil
		}
		for i := range snap.Entities {
			e := &snap.Entities[i]
			if e.ID == id && e.Status != campaign.StatusDeleted {
				if e.Kind != campaign.KindLocation {
					return nil, fmt.Errorf("%w: the %s %s is a %s, not a location", ErrInvalid, what, e.Name, e.Kind)
				}
				return e, nil
			}
		}
		return nil, fmt.Errorf("%w: %s entity %s", ErrNotFound, what, id)
	}
	parent, err := findLoc(in.Parent, "parent")
	if err != nil {
		return nil, err
	}
	near, err := findLoc(in.Near, "near")
	if err != nil {
		return nil, err
	}

	// Kind and scale: the flesh-out reads the existing block first (never
	// replacing it), the request second, village/medium last.
	kind, scale := in.Kind, in.Scale
	block := campaign.Place{}
	if target != nil {
		block = campaign.PlaceOf(target)
		if kind == "" {
			if _, err := place.ShapeFor(block.Kind, ""); err == nil {
				kind = block.Kind
			}
		}
		if scale == "" {
			for _, band := range place.Scales() {
				if strings.EqualFold(block.Scale, band) {
					scale = band
					break
				}
			}
		}
	}
	if kind == "" {
		kind = place.KindVillage
	}
	shape, err := place.ShapeFor(kind, scale)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	// The budget: what this run proposes, per part. A fresh design always
	// stages the place itself; a flesh-out subtracts what the target
	// already has — a default run skips full parts, an explicitly
	// requested full part is refused with the count, never silently
	// truncated.
	want := placeWant{fresh: target == nil}
	if target == nil {
		if !allParts && !parts[LocationPartPlace] {
			return nil, fmt.Errorf("%w: a fresh design stages the place itself — add the place part or flesh out an existing location", ErrInvalid)
		}
		want.place = true
		if allParts || parts[LocationPartSublocations] {
			want.subs = shape.SubLocations()
		}
		if allParts || parts[LocationPartNPCs] {
			want.npcs = shape.NPCs
		}
		if allParts || parts[LocationPartHooks] {
			want.hooks = shape.Hooks
		}
		if allParts || parts[LocationPartSecrets] {
			want.secrets = shape.Secrets
		}
	} else {
		existing := placeExistingCounts(snap, target)
		take := func(requested string, band, have int, label string) (int, error) {
			if !parts[requested] && !allParts {
				return 0, nil
			}
			room := band - have
			if room <= 0 {
				if allParts {
					return 0, nil // the default run proposes what is missing, skips the rest
				}
				return 0, fmt.Errorf("%w: %s is full: %s already has %d and the %s %s band allows %d — dismiss some before re-rolling",
					ErrInvalid, label, target.Name, have, shape.Kind, shape.Scale, band)
			}
			return room, nil
		}
		if want.subs, err = take(LocationPartSublocations, shape.SubLocations(), existing.children, "the sub-locations part"); err != nil {
			return nil, err
		}
		if want.npcs, err = take(LocationPartNPCs, shape.NPCs, existing.npcs, "the people part"); err != nil {
			return nil, err
		}
		if want.hooks, err = take(LocationPartHooks, shape.Hooks, existing.hooks, "the hooks part"); err != nil {
			return nil, err
		}
		if want.secrets, err = take(LocationPartSecrets, shape.Secrets, existing.secrets, "the secrets part"); err != nil {
			return nil, err
		}
		want.place = parts[LocationPartPlace] || allParts
		if want.place && !blockHasRoom(block) {
			if allParts {
				want.place = false // the block is written; the default run leaves it alone
			} else {
				return nil, fmt.Errorf("%w: the place block is full: %s's block is already written — edit it by hand instead",
					ErrInvalid, target.Name)
			}
		}
	}
	if !want.place && want.subs == 0 && want.npcs == 0 && want.hooks == 0 && want.secrets == 0 {
		return nil, fmt.Errorf("%w: nothing to propose — every part is off or full", ErrInvalid)
	}

	// The candidate pools, scored out of the campaign: reuse before
	// invention, the quest designer's cast pattern applied to a place. The
	// target, its anchors and its existing placements never appear in the
	// pools — an edge back to itself is not reuse.
	anchors := map[string]bool{}
	for _, e := range []*campaign.Entity{target, parent, near} {
		if e != nil {
			anchors[e.ID] = true
		}
	}
	npcPool := scorePlaceCandidates(snap, target, anchors, campaign.KindNPC, signals)
	subPool := scorePlaceCandidates(snap, target, anchors, campaign.KindLocation, signals)
	var factionPool []string
	for i := range snap.Entities {
		e := &snap.Entities[i]
		if e.Status == campaign.StatusDeleted {
			continue
		}
		if e.Kind == campaign.KindFaction || e.Kind == campaign.KindOrganization {
			factionPool = append(factionPool, e.Name)
		}
	}
	sort.Strings(factionPool)
	if len(factionPool) > placeCandidateCap+2 {
		factionPool = factionPool[:placeCandidateCap+2]
	}
	// The people already at the target (flesh-out): legal secret holders.
	var presentNames []string
	presentIDs := map[string]string{} // lowercased name -> real id
	if target != nil {
		kinds := map[string]string{}
		for i := range snap.Entities {
			kinds[snap.Entities[i].ID] = snap.Entities[i].Kind
		}
		for _, id := range placeEdgeEnds(snap, target.ID) {
			if kinds[id] == campaign.KindNPC || kinds[id] == campaign.KindCreature {
				for i := range snap.Entities {
					if snap.Entities[i].ID == id {
						presentNames = append(presentNames, snap.Entities[i].Name)
						presentIDs[strings.ToLower(snap.Entities[i].Name)] = id
					}
				}
			}
		}
		sort.Strings(presentNames)
	}

	fields, structure := locationDesignPrompt(locationPromptIn{
		premise: in.Premise, signals: signals, shape: shape, want: want,
		target: target, parent: parent, near: near, block: block,
		npcPool: npcPool, subPool: subPool, factionPool: factionPool,
		presentNames: presentNames, entities: snap.Entities,
	})

	note := "Fill every declared field. The structure block is the place you are designing — its kind, its scale band and every count are already law. " +
		"Reuse the existing candidates and the campaign's places where they fit: a place generated into a campaign attaches to the people and places it already has."

	fill := func(note string) (*Generated, error) {
		return s.Generate(ctx, GenerateInput{
			System: placeSystemPrompt, Structure: structure, Fields: fields, Note: note,
		})
	}
	gen, err := fill(note)
	if err != nil {
		return nil, err
	}

	// Assemble; on failure, exactly one repair retry with the failures
	// appended — the generate.go contract, one retry, no second mechanism.
	// The reuse read-back runs inside the same loop: a name the campaign
	// already resolves is the second-Blackwater failure this generator
	// exists to prevent (for the place) or an automatic reuse (for
	// everyone else).
	var reused []ReusedEntity
	assemble := func(values map[string]any) (*locationAssembled, []string) {
		a := assembleLocationDesign(shape, want, npcPool, subPool, factionPool, presentNames, presentIDs, values)
		return a, s.placeReadbacks(ctx, in.CampaignID, a, &reused)
	}
	assembled, problems := assemble(gen.Values)
	if len(problems) > 0 {
		reused = nil
		gen, err = fill(note + "\nYour previous response had these problems:\n- " + strings.Join(problems, "\n- "))
		if err != nil {
			return nil, err
		}
		assembled, problems = assemble(gen.Values)
		if len(problems) > 0 {
			return nil, fmt.Errorf("%w: the generated place failed validation twice: %s",
				ErrInvalid, strings.Join(problems, "; "))
		}
	}

	/* ---------- the batch, in dependency order ---------- */

	var items []BatchItemInput
	// placeRef is how sibling items reference the place: the real id when
	// it already exists, the batch item id when this batch creates it.
	placeRef := "place"
	if target != nil {
		placeRef = target.ID
	}
	// placeDeps is the dependency on the place item siblings carry, when
	// this batch creates the place.
	placeDeps := func() []string {
		if target == nil && want.place {
			return []string{"place"}
		}
		return nil
	}

	// 1. The place: its entity (fresh) with the read-aloud in
	//    entities.summary where the prose index reads it, or the merged
	//    block update (flesh-out) — never replacing what is already there.
	if want.place {
		mergedBlock := mergePlaceBlock(block, assembled.block)
		payload := campaign.WithPlace(targetPayload(target), mergedBlock)
		// Routes are written even if unread: the near anchor and the
		// parent get travel.routes entries against their real ids, so the
		// place is navigable the moment it is accepted.
		if near != nil && assembled.routeDays > 0 {
			payload = withRoute(payload, near.ID, assembled.routeDays, assembled.routeTerrain)
		}
		if parent != nil && assembled.routeDays > 0 && (near == nil || parent.ID != near.ID) {
			payload = withRoute(payload, parent.ID, assembled.routeDays, assembled.routeTerrain)
		}
		if target == nil {
			items = append(items, BatchItemInput{
				ID: "place", Kind: "entity", Subject: assembled.name, Summary: assembled.summary,
				Payload: map[string]any{
					"local_id": "place", "kind": campaign.KindLocation,
					"name": assembled.name, "summary": assembled.summary,
					"payload": payload,
				},
			})
		} else {
			items = append(items, BatchItemInput{
				ID: "place", Kind: "entity", Subject: target.Name,
				Summary: "The place block and roads out, proposed around what is already here.",
				Payload: map[string]any{
					"local_id": "place", "entity_update": target.ID, "payload": payload,
				},
			})
		}
	}

	// The anchor edges: a village generated near an existing town (or
	// inside a parent) is placed — located_in the anchor, an edge to the
	// entity that already exists, never a second copy of it.
	for _, anchor := range []*campaign.Entity{near, parent} {
		if anchor == nil {
			continue
		}
		if near != nil && parent != nil && anchor.ID == near.ID && parent.ID == near.ID {
			continue // the same entity named twice anchors once
		}
		items = append(items, BatchItemInput{
			ID: "edge-anchor-" + anchor.ID, Kind: "relationship",
			Subject: fmt.Sprintf("%s located_in %s", assembled.nameOr(target), anchor.Name),
			Summary: fmt.Sprintf("%s sits at %s.", assembled.nameOr(target), anchor.Name),
			Payload: map[string]any{
				"from_entity": placeRef, "rel_type": "located_in", "to_entity": anchor.ID,
			},
			DependsOn: placeDeps(),
		})
	}

	// 2. The sub-locations: entity items, then their located_in edges to
	//    the place — every generated sub-location arrives with the edge
	//    already specified, because a room that is nowhere is the thing
	//    the graph is for.
	//
	// Sibling references carry names (or real ids), not batch-local ids:
	// the batch map resolves names case-insensitively, and names keep a
	// second run's payloads distinct from an undecided first run's —
	// StageBatch's dedup would otherwise read "sub-1 located_in place" as
	// a re-proposal and silently skip the second village's edges.
	subSlots := shape.Subs
	if len(subSlots) > want.subs {
		subSlots = subSlots[:want.subs]
	}
	for i, slot := range subSlots {
		pick := assembled.subs[i]
		ref := pick.existingID
		edgeDeps := placeDeps()
		if ref == "" {
			ref = pick.name
			items = append(items, BatchItemInput{
				ID: slot.ID, Kind: "entity", Subject: pick.name, Summary: pick.summary,
				Payload: map[string]any{
					"local_id": slot.ID, "kind": campaign.KindLocation,
					"name": pick.name, "summary": pick.summary,
				},
				DependsOn: placeDeps(),
			})
			edgeDeps = append(edgeDeps, slot.ID)
		}
		items = append(items, BatchItemInput{
			ID: "edge-" + slot.ID, Kind: "relationship",
			Subject: fmt.Sprintf("%s located_in %s", pick.name, assembled.nameOr(target)),
			Summary: fmt.Sprintf("The %s of %s.", slot.Role, assembled.nameOr(target)),
			Payload: map[string]any{
				"from_entity": ref, "rel_type": "located_in", "to_entity": placeRef,
			},
			DependsOn: edgeDeps,
		})
	}

	// 3. The notable people: entity items with their MAD-313 agent block,
	//    their located_in edges, and a faction edge when one is attached.
	for i := 0; i < want.npcs; i++ {
		pick := assembled.npcs[i]
		itemID := fmt.Sprintf("npc-%d", i+1)
		ref := pick.existingID
		var ownDeps []string // the npc item, when this batch creates it
		if ref == "" {
			ref = pick.name
			agent := map[string]any{}
			if pick.summary != "" {
				agent["public_identity"] = pick.summary
			}
			if pick.goal != "" {
				agent["goals"] = []string{pick.goal}
			}
			if pick.voice != "" {
				agent["voice"] = pick.voice
			}
			itemPayload := map[string]any{
				"local_id": itemID, "kind": campaign.KindNPC,
				"name": pick.name, "summary": pick.summary,
			}
			if len(agent) > 0 {
				itemPayload["payload"] = map[string]any{"agent": agent}
			}
			items = append(items, BatchItemInput{
				ID: itemID, Kind: "entity", Subject: pick.name, Summary: pick.summary,
				Payload:   itemPayload,
				DependsOn: placeDeps(),
			})
			ownDeps = []string{itemID}
		}
		items = append(items, BatchItemInput{
			ID: "edge-npc-" + itemID, Kind: "relationship",
			Subject: fmt.Sprintf("%s located_in %s", pick.name, assembled.nameOr(target)),
			Summary: fmt.Sprintf("%s keeps %s.", pick.name, assembled.nameOr(target)),
			Payload: map[string]any{
				"from_entity": ref, "rel_type": "located_in", "to_entity": placeRef,
			},
			DependsOn: append(append([]string{}, placeDeps()...), ownDeps...),
		})
		if pick.faction != "" && pick.faction != "none" {
			items = append(items, BatchItemInput{
				ID: "edge-faction-" + itemID, Kind: "relationship",
				Subject: fmt.Sprintf("%s member_of %s", pick.name, pick.faction),
				Summary: fmt.Sprintf("%s answers to %s.", pick.name, pick.faction),
				Payload: map[string]any{
					"from_entity": ref, "rel_type": "member_of", "to_entity": pick.faction,
				},
				DependsOn: ownDeps,
			})
		}
	}

	// 4. The hooks: public facts with ai_proposed provenance, subject the
	//    place — a live fact touching the new place is what keeps
	//    dormant_region quiet.
	for i := 0; i < want.hooks; i++ {
		pick := assembled.hooks[i]
		id := fmt.Sprintf("hook-%d", i+1)
		items = append(items, BatchItemInput{
			ID: id, Kind: "fact",
			Subject: fmt.Sprintf("A hook at %s", assembled.nameOr(target)),
			Summary: pick.statement,
			Payload: map[string]any{
				"local_id":  id,
				"statement": pick.statement, "subject": placeRef,
				"predicate": "needs", "object_literal": pick.thread,
				"visibility": campaign.VisibilityPublic,
			},
			DependsOn: placeDeps(),
		})
	}

	// 5. The secrets: the fact, then the holder's discovery — a secret
	//    with no one holding it is born unreachable, so every secret
	//    stages the awareness stance that makes it findable. The holder
	//    reference is the holder's name (or real id): the batch map
	//    resolves it at apply time, and the name keeps a second run's
	//    payloads distinct from an undecided first run's.
	for i := 0; i < want.secrets; i++ {
		pick := assembled.secrets[i]
		secretID := fmt.Sprintf("secret-%d", i+1)
		holderRef := pick.holderID
		if holderRef == "" {
			holderRef = pick.holderName // resolves by name through the batch map
		}
		deps := placeDeps()
		if pick.holderItem != "" {
			deps = append(deps, pick.holderItem)
		}
		items = append(items, BatchItemInput{
			ID: secretID, Kind: "fact",
			Subject: fmt.Sprintf("A secret %s hides", assembled.nameOr(target)),
			Summary: pick.statement,
			Payload: map[string]any{
				"local_id":  secretID,
				"statement": pick.statement, "subject": placeRef,
				"predicate": "hides", "object_entity": holderRef,
				"visibility": campaign.VisibilitySecret,
			},
			DependsOn: deps,
		})
		holderDeps := []string{secretID}
		if pick.holderItem != "" {
			holderDeps = append(holderDeps, pick.holderItem)
		}
		items = append(items, BatchItemInput{
			ID: secretID + "-holder", Kind: "discovery",
			Subject: fmt.Sprintf("Who holds %s's secret", assembled.nameOr(target)),
			Summary: pick.statement,
			Payload: map[string]any{
				"fact": secretID, "discovered_by": holderRef,
				"stance": "knows", "method": "holds this secret close",
			},
			DependsOn: holderDeps,
		})
	}

	// The arithmetic gate: a full fresh generation's item count sits inside
	// the band OUR structure says it should — the shape's counts plus the
	// reuse this run actually did (a reused sub-location or person stages
	// its edge but not its entity), plus the anchor edges, checked before
	// anything is staged. A parts run stages fewer items by construction;
	// its per-part counts are the band.
	if target == nil && allParts {
		newPicks := func(picks []placeSlotPick) int {
			n := 0
			for i := range picks {
				if picks[i].existingID == "" {
					n++
				}
			}
			return n
		}
		anchors := 0
		for _, a := range []*campaign.Entity{near, parent} {
			if a != nil {
				anchors++
			}
		}
		min := 1 + anchors +
			len(assembled.subs) + newPicks(assembled.subs) +
			len(assembled.npcs) + newPicks(assembled.npcs) +
			want.hooks + 2*want.secrets
		max := min + want.npcs // a faction edge per notable person
		if len(items) < min || len(items) > max {
			return nil, fmt.Errorf("%w: the batch would stage %d items and the %s %s structure allows %d-%d",
				ErrInvalid, len(items), shape.Kind, shape.Scale, min, max)
		}
	}

	promptRecord := fmt.Sprintf("premise: %s | kind: %s, scale: %s | signals: terrain %q, tone %q",
		in.Premise, shape.Kind, shape.Scale, signals.Terrain, signals.Tone)
	if near != nil {
		promptRecord += " | near: " + near.Name
	}
	if parent != nil {
		promptRecord += " | parent: " + parent.Name
	}
	if target != nil {
		promptRecord += " | fleshing out: " + target.Name
	}
	if !allParts {
		promptRecord += " | parts: " + strings.Join(in.Parts, ", ")
	}
	if len(reused) > 0 {
		var names []string
		for _, r := range reused {
			names = append(names, r.Name)
		}
		promptRecord += " | reused: " + strings.Join(names, ", ")
	}

	batch, err := s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceLocation,
		Prompt: promptRecord, CreatedBy: in.CreatedBy, Items: items,
	})
	if err != nil {
		return nil, err
	}
	return &LocationDesignResult{
		Batch: batch, Shape: shape, Reused: reused, Generated: s.now(),
	}, nil
}

/* ---------- the existing-count read ---------- */

// placeExisting is what a flesh-out target already has, per part: the live
// graph rows the band subtracts.
type placeExisting struct {
	children int
	npcs     int
	hooks    int
	secrets  int
}

// placeEdgeEnds lists the live entities the target CONTAINS — children
// and present people, by directed edge: X located_in the target, or the
// target contains X. The target's own anchor edge out (the target
// located_in somewhere else) does not count: Blackwater is not inside the
// village that sits near it.
func placeEdgeEnds(snap *Snapshot, locationID string) []string {
	seen := map[string]bool{}
	live := map[string]bool{}
	for i := range snap.Entities {
		if snap.Entities[i].Status != campaign.StatusDeleted {
			live[snap.Entities[i].ID] = true
		}
	}
	for _, r := range snap.Relationships {
		var other string
		switch {
		case r.RelType == "located_in" && r.ToEntity == locationID:
			other = r.FromEntity
		case r.RelType == "contains" && r.FromEntity == locationID:
			other = r.ToEntity
		default:
			continue
		}
		if other != locationID && live[other] {
			seen[other] = true
		}
	}
	ends := make([]string, 0, len(seen))
	for id := range seen {
		ends = append(ends, id)
	}
	sort.Strings(ends)
	return ends
}

// placeExistingCounts reads a flesh-out target's existing people, children
// and live facts, so the band arithmetic subtracts them.
func placeExistingCounts(snap *Snapshot, target *campaign.Entity) placeExisting {
	var out placeExisting
	kinds := map[string]string{}
	for i := range snap.Entities {
		kinds[snap.Entities[i].ID] = snap.Entities[i].Kind
	}
	for _, id := range placeEdgeEnds(snap, target.ID) {
		switch kinds[id] {
		case campaign.KindLocation:
			out.children++
		case campaign.KindNPC, campaign.KindCreature:
			out.npcs++
		}
	}
	for _, f := range snap.Facts {
		if f.SupersededBy != "" {
			continue
		}
		if f.SubjectEntity != target.ID && f.ObjectEntity != target.ID {
			continue
		}
		if f.Visibility == campaign.VisibilitySecret {
			out.secrets++
		} else {
			out.hooks++
		}
	}
	return out
}

// blockHasRoom reports whether the place block has empty fields a
// flesh-out may propose into.
func blockHasRoom(block campaign.Place) bool {
	return block.Population == "" || block.Government == "" || block.Defences == "" ||
		block.State == "" || len(block.Services) == 0 || len(block.Senses) == 0
}

// mergePlaceBlock merges a proposed block into an existing one: what is
// already there wins — a flesh-out proposes around, never replacing.
func mergePlaceBlock(existing, proposed campaign.Place) campaign.Place {
	out := existing
	overwrite := func(dst, src string) string {
		if dst != "" {
			return dst
		}
		return src
	}
	out.Kind = overwrite(out.Kind, proposed.Kind)
	out.Scale = overwrite(out.Scale, proposed.Scale)
	out.Population = overwrite(out.Population, proposed.Population)
	out.Government = overwrite(out.Government, proposed.Government)
	out.Defences = overwrite(out.Defences, proposed.Defences)
	out.Climate = overwrite(out.Climate, proposed.Climate)
	out.State = overwrite(out.State, proposed.State)
	out.PrivateTruth = overwrite(out.PrivateTruth, proposed.PrivateTruth)
	if out.Danger == 0 {
		out.Danger = proposed.Danger
	}
	appendList := func(dst, src []string) []string {
		seen := map[string]bool{}
		for _, s := range dst {
			seen[strings.ToLower(s)] = true
		}
		for _, s := range src {
			if !seen[strings.ToLower(s)] {
				dst = append(dst, s)
				seen[strings.ToLower(s)] = true
			}
		}
		return dst
	}
	out.Services = appendList(out.Services, proposed.Services)
	out.Senses = appendList(out.Senses, proposed.Senses)
	return out
}

// targetPayload returns a flesh-out target's payload (nil for a fresh
// place), the base the merged block and routes are written onto.
func targetPayload(target *campaign.Entity) map[string]any {
	if target == nil {
		return nil
	}
	return target.Payload
}

// withRoute appends one travel.routes entry to a location payload,
// preserving every other key and any routes already declared. One road per
// pair: a route to somewhere already listed is kept, never duplicated.
func withRoute(payload map[string]any, to string, days int64, terrain string) map[string]any {
	if payload == nil {
		payload = map[string]any{}
	}
	travel, _ := payload["travel"].(map[string]any)
	if travel == nil {
		travel = map[string]any{}
	}
	routes, _ := travel["routes"].([]any)
	entry := map[string]any{"to": to, "days": days}
	if terrain != "" {
		entry["terrain"] = terrain
	}
	for _, r := range routes {
		if m, ok := r.(map[string]any); ok && m["to"] == to {
			return payload
		}
	}
	travel["routes"] = append(routes, entry)
	payload["travel"] = travel
	return payload
}

/* ---------- the candidate pools ---------- */

// scorePlaceCandidates ranks the campaign's entities of one kind for reuse
// slots: premise terms against name and summary, threat classes for
// flavour. The target, the anchors and (for people) everyone already at
// the target never appear in the pool.
func scorePlaceCandidates(snap *Snapshot, target *campaign.Entity, anchors map[string]bool, kind string, signals PlaceSignals) []placeSlotCandidate {
	exclude := map[string]bool{}
	for id := range anchors {
		exclude[id] = true
	}
	if target != nil && kind != campaign.KindLocation {
		// The people already here are not candidates to be added again;
		// locations may appear as sub-location candidates (a neighbouring
		// ruin can be claimed by an edge) except the anchors themselves.
		for _, id := range placeEdgeEnds(snap, target.ID) {
			exclude[id] = true
		}
	}
	var out []placeSlotCandidate
	for i := range snap.Entities {
		e := &snap.Entities[i]
		if e.Status == campaign.StatusDeleted || e.Kind != kind || e.Kind == campaign.KindPC {
			continue
		}
		if exclude[e.ID] {
			continue
		}
		score, reason := 1, "kind fits"
		lowName := strings.ToLower(e.Name)
		lowSummary := strings.ToLower(e.Summary)
		for _, t := range signals.Terms {
			if strings.Contains(lowName, t) {
				score += 3
				reason = "named by the premise"
				break
			}
		}
		for _, t := range signals.Terms {
			if strings.Contains(lowSummary, t) {
				score += 1
				if reason == "kind fits" {
					reason = "described by the premise"
				}
				break
			}
		}
		for _, th := range signals.Threats {
			if strings.Contains(lowSummary, strings.Split(th, " ")[0]) {
				score += 1
				if reason == "kind fits" {
					reason = "carries the premise's threat"
				}
				break
			}
		}
		out = append(out, placeSlotCandidate{ID: e.ID, Name: e.Name, Kind: e.Kind, Score: score, Reason: reason})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > placeCandidateCap {
		out = out[:placeCandidateCap]
	}
	return out
}

/* ---------- the prompt ---------- */

// locationPromptIn carries everything the prompt builder needs.
type locationPromptIn struct {
	premise      string
	signals      PlaceSignals
	shape        place.Shape
	want         placeWant
	target       *campaign.Entity
	parent       *campaign.Entity
	near         *campaign.Entity
	block        campaign.Place
	npcPool      []placeSlotCandidate
	subPool      []placeSlotCandidate
	factionPool  []string
	presentNames []string
	entities     []campaign.Entity
}

// Numeric field bounds, as package vars so the specs can point at them.
var (
	placeDangerMin = float64(0)
	placeDangerMax = float64(5)
	placeRouteMin  = float64(1)
	placeRouteMax  = float64(60)
)

// locationDesignPrompt builds the declared schema and the structure block.
// Field count follows the computed budget — the model cannot return a
// village with more people than the band allows. A flesh-out declares only
// the block's empty fields: what is written is law and is not asked about.
func locationDesignPrompt(in locationPromptIn) ([]FieldSpec, map[string]any) {
	fields := []FieldSpec{}
	if in.want.fresh {
		fields = append(fields,
			FieldSpec{Key: "place_name", Required: true, MaxLen: 60,
				Desc: "The place's own name — not the name of any existing candidate or entity the campaign already has."},
			FieldSpec{Key: "place_summary", Required: true, MaxLen: 400,
				Desc: "The read-aloud paragraph: what the party sees, hears and feels on arrival. Two or three sentences."},
		)
	}
	// The block, and its services: declared only when the place part runs.
	// A flesh-out declaring only its people (say) never asks about the
	// block — what is written is law and is not asked about.
	if in.want.place {
		blockField := func(key, label, placeholder string, filled string, maxLen int) {
			if filled != "" {
				return // never replacing what is already there
			}
			fields = append(fields, FieldSpec{Key: key, Required: true, MaxLen: maxLen,
				Desc: label + " — " + placeholder})
		}
		blockField("population", "Population", "about 300", in.block.Population, 60)
		blockField("government", "Government", "a reeve answerable to the Duke", in.block.Government, 80)
		blockField("defences", "Defences", "a palisade ditch and a watch of six", in.block.Defences, 80)
		blockField("climate", "Climate", "temperate; damp off the marsh", in.block.Climate, 60)
		blockField("state", "Current state", "the mill burned last month and nobody says why", in.block.State, 120)
		if len(in.block.Senses) == 0 {
			fields = append(fields, FieldSpec{Key: "senses", Required: true, MaxLen: 240,
				Desc: "What the party hears and smells: two or three phrases separated by semicolons."})
		}
		if in.block.Danger == 0 {
			fields = append(fields, FieldSpec{Key: "danger", Required: true, Type: FieldInteger,
				Min: &placeDangerMin, Max: &placeDangerMax,
				Desc: "How dangerous it reads, 0 (a safe hearth) to 5 (do not sleep here)."})
		}
		// Services: the band's count, numbered past what the block lists.
		for i := len(in.block.Services) + 1; i <= in.shape.Services; i++ {
			fields = append(fields, FieldSpec{
				Key: fmt.Sprintf("service_%d", i), Required: true, MaxLen: 60,
				Desc: fmt.Sprintf("Notable service %d: what the place sells or provides (inn, market, smith, ferry…).", i),
			})
		}
	}
	// The sub-location slots.
	subSlots := in.shape.Subs
	if len(subSlots) > in.want.subs {
		subSlots = subSlots[:in.want.subs]
	}
	newLabel := func(prefix string) string {
		return fmt.Sprintf(`Legal values: an existing candidate's exact name, or "new" — then fill %s_new_name and %s_new_summary.`, prefix, prefix)
	}
	subPoolNames := func() []string {
		var pool []string
		for _, c := range in.subPool {
			pool = append(pool, c.Name)
		}
		return pool
	}()
	npcPoolNames := func() []string {
		var pool []string
		for _, c := range in.npcPool {
			pool = append(pool, c.Name)
		}
		return pool
	}()
	for _, slot := range subSlots {
		prefix := slot.ID
		pool := append(append([]string{}, subPoolNames...), "new")
		fields = append(fields,
			FieldSpec{Key: prefix, Required: true, MaxLen: 120, Pool: pool,
				Desc: fmt.Sprintf("The %s: %s %s", slot.Role, slot.Brief, newLabel(prefix))},
			FieldSpec{Key: prefix + "_new_name", Required: false, MaxLen: 60,
				Desc: fmt.Sprintf("Name for a new location, only when %s is \"new\".", prefix)},
			FieldSpec{Key: prefix + "_new_summary", Required: false, MaxLen: 240,
				Desc: fmt.Sprintf("One or two sentences on the place, only when %s is \"new\".", prefix)},
		)
	}
	// The notable people.
	for i := 1; i <= in.want.npcs; i++ {
		prefix := fmt.Sprintf("npc_%d", i)
		pool := append(append([]string{}, npcPoolNames...), "new")
		fields = append(fields,
			FieldSpec{Key: prefix, Required: true, MaxLen: 120, Pool: pool,
				Desc: fmt.Sprintf("Notable person %d: someone worth talking to. %s", i, newLabel(prefix))},
			FieldSpec{Key: prefix + "_new_name", Required: false, MaxLen: 60,
				Desc: fmt.Sprintf("Name, only when %s is \"new\".", prefix)},
			FieldSpec{Key: prefix + "_new_summary", Required: false, MaxLen: 240,
				Desc: fmt.Sprintf("Their public face in a sentence or two, only when %s is \"new\".", prefix)},
			FieldSpec{Key: prefix + "_goal", Required: false, MaxLen: 160,
				Desc: fmt.Sprintf("What they want, one line (their mind's first goal), only when %s is \"new\".", prefix)},
			FieldSpec{Key: prefix + "_voice", Required: false, MaxLen: 120,
				Desc: fmt.Sprintf("How they talk, one line, only when %s is \"new\".", prefix)},
		)
		if len(in.factionPool) > 0 {
			factionPool := append([]string{"none"}, in.factionPool...)
			fields = append(fields, FieldSpec{
				Key: prefix + "_faction", Required: true, MaxLen: 120, Pool: factionPool,
				Desc: fmt.Sprintf("The faction or organization person %d answers to, or \"none\".", i),
			})
		}
	}
	// The hooks.
	for i := 1; i <= in.want.hooks; i++ {
		fields = append(fields,
			FieldSpec{Key: fmt.Sprintf("hook_%d_statement", i), Required: true, MaxLen: 300,
				Desc: fmt.Sprintf("Adventure hook %d: the trouble a party could chase, one or two sentences.", i)},
			FieldSpec{Key: fmt.Sprintf("hook_%d_thread", i), Required: true, MaxLen: 60,
				Desc: fmt.Sprintf("Hook %d's thread in three to six words (\"the missing woodcutters\").", i)},
		)
	}
	// The secrets and their holders.
	for i := 1; i <= in.want.secrets; i++ {
		holderExtra := ""
		if len(in.presentNames) > 0 {
			holderExtra = " or someone already at the place"
		}
		fields = append(fields,
			FieldSpec{Key: fmt.Sprintf("secret_%d_statement", i), Required: true, MaxLen: 300,
				Desc: fmt.Sprintf("Secret %d: what the place is really hiding, one or two sentences.", i)},
			FieldSpec{Key: fmt.Sprintf("secret_%d_holder", i), Required: true, MaxLen: 120,
				Desc: fmt.Sprintf("Who holds secret %d: the exact name of one of this design's notable people%s. A secret with no holder cannot be found.",
					i, holderExtra)},
		)
	}
	// The road out: one route to the near anchor when there is one, else
	// to the parent (both, when they differ). Days and terrain feed the
	// travel block.
	if in.near != nil || in.parent != nil {
		fields = append(fields,
			FieldSpec{Key: "route_days", Required: true, Type: FieldInteger,
				Min: &placeRouteMin, Max: &placeRouteMax,
				Desc: "Days of travel on the road out, 1-60."},
			FieldSpec{Key: "route_terrain", Required: false, MaxLen: 60,
				Desc: "The road's ground in a phrase (\"dirt road through dark pines\")."},
		)
	}

	structure := map[string]any{
		"premise": in.premise,
		"signals": in.signals,
		"shape":   in.shape,
		"budget": map[string]any{
			"sublocations": in.want.subs, "npcs": in.want.npcs,
			"hooks": in.want.hooks, "secrets": in.want.secrets,
		},
	}
	if in.target != nil {
		structure["fleshing_out"] = map[string]any{
			"name": in.target.Name, "summary": in.target.Summary,
			"place_block": in.block,
			"note":        "Propose around what is here; what is already written is law and will not be replaced.",
		}
		if len(in.presentNames) > 0 {
			structure["people_already_here"] = in.presentNames
		}
	}
	if in.parent != nil {
		structure["parent"] = map[string]any{"name": in.parent.Name, "summary": in.parent.Summary}
	}
	if in.near != nil {
		structure["near"] = map[string]any{"name": in.near.Name, "summary": in.near.Summary}
	}
	candView := func(pool []placeSlotCandidate) []map[string]any {
		var out []map[string]any
		for _, c := range pool {
			out = append(out, map[string]any{"name": c.Name, "kind": c.Kind, "why": c.Reason})
		}
		return out
	}
	structure["npc_candidates"] = candView(in.npcPool)
	structure["sub_location_candidates"] = candView(in.subPool)
	structure["factions"] = in.factionPool
	var roster []skeletonExisting
	for i := range in.entities {
		e := &in.entities[i]
		if e.Status == campaign.StatusDeleted || e.Kind == campaign.KindPC {
			continue
		}
		roster = append(roster, skeletonExisting{Name: e.Name, Kind: e.Kind, Summary: e.Summary})
		if len(roster) >= placeRosterCap {
			break
		}
	}
	structure["existing_entities"] = roster
	return fields, structure
}

// placeSystemPrompt is the designer's system prompt.
const placeSystemPrompt = `You are Grimoire's place designer. You are handed the complete structure of a settlement — computed by the server and authoritative: its kind, its scale band, how many notable people, services, sub-locations, hooks and secrets it may have, the roles its sub-locations play, the candidate entities the campaign already has, and the DM's premise. Your job is to fill that structure with names, prose and specificity, nothing more.

STRICT RULES
1. Every field is a plain string (or a whole number where declared). Fill every one. No markdown, no commentary; only a field that says semicolons may carry more than one phrase.
2. The structure is fixed. Do not add, merge or drop sub-locations, people, services, hooks or secrets; the counts are already law.
3. Reuse before invention: pick an existing candidate by its exact name whenever one fits. Only answer "new" when nothing does, and then write the new entity's name and summary. Never invent a second version of someone the campaign already has.
4. The place's own name must be new: a name the campaign already holds is the duplication this designer exists to prevent.
5. Each secret names a holder from this design's notable people (or, when fleshing a place out, someone already at the place). A secret nobody holds can never be found.
6. Honour the DM's premise: its terrain, its threat, its tone. Where it names people or places, use them.
7. When fleshing a place out, what is already written is law: propose around it, never over it.
8. District interiors (a city) are later work: name each district and what it is for; do not detail its inside.`

/* ---------- assembly: values into a design ---------- */

// placeSlotPick is one reuse slot after the model filled it: an existing
// entity id, or the new entity's name and prose.
type placeSlotPick struct {
	existingID string
	name       string
	summary    string
	goal       string
	voice      string
	faction    string
}

// placeHookPick is one hook after the model filled it.
type placeHookPick struct {
	statement string
	thread    string
}

// placeSecretPick is one secret after the model filled it: the statement
// and its holder — the holder's name, the reference the fact's
// object_entity carries (an existing id, a batch item id, or the name for
// the batch map to resolve), and the batch item the holder needs applied
// first when this design creates them.
type placeSecretPick struct {
	statement  string
	holderName string
	holderID   string
	holderItem string
}

// locationAssembled is a design after assembly: the place's headline, its
// block, the slot picks, the hooks and secrets, and the problems that were
// found (empty when the fill is valid).
type locationAssembled struct {
	name         string
	summary      string
	block        campaign.Place
	services     []string
	subs         []placeSlotPick
	npcs         []placeSlotPick
	hooks        []placeHookPick
	secrets      []placeSecretPick
	routeDays    int64
	routeTerrain string
	problems     []string
}

// nameOr returns the design's place name, or the flesh-out target's.
func (a *locationAssembled) nameOr(target *campaign.Entity) string {
	if a.name != "" {
		return a.name
	}
	if target != nil {
		return target.Name
	}
	return "the place"
}

// assembleLocationDesign turns validated values into the design, running
// every check a staged batch must pass: pool legality, holder resolution
// against this design's people, duplicate picks, and the required prose.
func assembleLocationDesign(shape place.Shape, want placeWant, npcPool, subPool []placeSlotCandidate, factionPool, presentNames []string, presentIDs map[string]string, values map[string]any) *locationAssembled {
	a := &locationAssembled{}
	val := func(key string) string {
		sv, _ := values[key].(string)
		return strings.TrimSpace(sv)
	}
	a.name = val("place_name")
	a.summary = val("place_summary")

	// The block. The scale reads as the band's label minus its article.
	a.block = campaign.Place{
		Kind:       shape.Kind,
		Scale:      strings.TrimPrefix(strings.ToLower(shape.Label), "a "),
		Population: val("population"),
		Government: val("government"),
		Defences:   val("defences"),
		Climate:    val("climate"),
		State:      val("state"),
	}
	for _, phrase := range strings.Split(val("senses"), ";") {
		if phrase = strings.TrimSpace(phrase); phrase != "" {
			a.block.Senses = append(a.block.Senses, phrase)
		}
	}
	if v, ok := values["danger"].(float64); ok {
		a.block.Danger = int(v)
	}
	for i := 1; i <= shape.Services; i++ {
		if svc := val(fmt.Sprintf("service_%d", i)); svc != "" {
			a.services = append(a.services, svc)
		}
	}
	a.block.Services = a.services
	if v, ok := values["route_days"].(float64); ok {
		a.routeDays = int64(v)
	}
	a.routeTerrain = val("route_terrain")

	fillSlot := func(prefix string, pool []placeSlotCandidate) placeSlotPick {
		pick := val(prefix)
		for _, c := range pool {
			if c.Name == pick {
				return placeSlotPick{existingID: c.ID, name: c.Name}
			}
		}
		switch {
		case pick == "new" || pick == "":
			name, summary := val(prefix+"_new_name"), val(prefix+"_new_summary")
			if pick == "new" && name == "" {
				a.problems = append(a.problems, fmt.Sprintf("%s is \"new\" but %s_new_name is missing", prefix, prefix))
			}
			return placeSlotPick{name: name, summary: summary}
		default:
			a.problems = append(a.problems, fmt.Sprintf("%s %q is neither an existing candidate nor \"new\"", prefix, pick))
			return placeSlotPick{name: pick}
		}
	}

	// The sub-locations: the same place staged twice is a problem the
	// model repairs.
	subSlots := shape.Subs
	if len(subSlots) > want.subs {
		subSlots = subSlots[:want.subs]
	}
	seenSub := map[string]bool{}
	for _, slot := range subSlots {
		p := fillSlot(slot.ID, subPool)
		key := p.existingID
		if key == "" {
			key = strings.ToLower(p.name)
		}
		if key != "" {
			if seenSub[key] {
				a.problems = append(a.problems, fmt.Sprintf("sub-%s repeats an earlier sub-location (%s)", slot.ID, p.name))
			}
			seenSub[key] = true
		}
		a.subs = append(a.subs, p)
	}

	// The notable people, with their faction pick.
	seenNPC := map[string]bool{}
	for i := 1; i <= want.npcs; i++ {
		prefix := fmt.Sprintf("npc_%d", i)
		p := fillSlot(prefix, npcPool)
		p.faction = val(prefix + "_faction")
		if p.faction != "" && p.faction != "none" && !containsStr(factionPool, p.faction) {
			a.problems = append(a.problems, fmt.Sprintf("%s_faction %q is not one of the factions offered", prefix, p.faction))
		}
		key := p.existingID
		if key == "" {
			key = strings.ToLower(p.name)
		}
		if key != "" {
			if seenNPC[key] {
				a.problems = append(a.problems, fmt.Sprintf("%s repeats an earlier notable person (%s)", prefix, p.name))
			}
			seenNPC[key] = true
		}
		a.npcs = append(a.npcs, p)
	}

	// The hooks.
	for i := 1; i <= want.hooks; i++ {
		p := placeHookPick{
			statement: val(fmt.Sprintf("hook_%d_statement", i)),
			thread:    val(fmt.Sprintf("hook_%d_thread", i)),
		}
		if p.statement == "" {
			a.problems = append(a.problems, fmt.Sprintf("hook_%d_statement is missing", i))
		}
		if p.thread == "" {
			a.problems = append(a.problems, fmt.Sprintf("hook_%d_thread is missing", i))
		}
		a.hooks = append(a.hooks, p)
	}

	// The secrets: the holder must be someone this design can stand
	// behind — one of its notable people (the batch item when new, the id
	// when reused), or (fleshing out) someone already at the place, whose
	// name the batch map resolves.
	for i := 1; i <= want.secrets; i++ {
		p := placeSecretPick{statement: val(fmt.Sprintf("secret_%d_statement", i))}
		holder := val(fmt.Sprintf("secret_%d_holder", i))
		if p.statement == "" {
			a.problems = append(a.problems, fmt.Sprintf("secret_%d_statement is missing", i))
		}
		if holder == "" {
			a.problems = append(a.problems, fmt.Sprintf("secret_%d_holder is missing", i))
		} else {
			resolved := false
			for j, np := range a.npcs {
				if !strings.EqualFold(np.name, holder) {
					continue
				}
				p.holderName = np.name
				p.holderID = np.existingID // the real id when reused, the name when new
				if np.existingID == "" {
					p.holderItem = fmt.Sprintf("npc-%d", j+1)
				}
				resolved = true
				break
			}
			if !resolved {
				for _, present := range presentNames {
					if strings.EqualFold(present, holder) {
						// Someone already at the place: the real id, which
						// the apply path resolves directly.
						p.holderName, p.holderID = present, presentIDs[strings.ToLower(present)]
						resolved = true
						break
					}
				}
			}
			if !resolved {
				a.problems = append(a.problems, fmt.Sprintf(
					"secret_%d_holder %q is not one of this design's notable people or someone already at the place", i, holder))
			}
		}
		a.secrets = append(a.secrets, p)
	}
	return a
}

// placeReadbacks resolves every proposed new name against the campaign
// graph — reuse before invention, the entity_merge_candidate rule — and
// refuses a place name the campaign already holds. Hits convert the pick
// into reuse (reported through reused), except the place's own name, which
// is a problem the model repairs.
func (s *Store) placeReadbacks(ctx context.Context, campaignID string, a *locationAssembled, reused *[]ReusedEntity) []string {
	problems := append([]string{}, a.problems...)
	if a.name != "" {
		hits, err := s.campaigns.ResolveName(ctx, campaign.ScopeDM, campaignID, a.name)
		if err == nil && len(hits) > 0 {
			problems = append(problems, fmt.Sprintf(
				"place_name %q already names %s in this campaign; a generated place needs a name of its own",
				a.name, hits[0].Name))
		}
	}
	convert := func(picks []placeSlotPick) {
		for i := range picks {
			pick := &picks[i]
			if pick.existingID != "" || pick.name == "" {
				continue
			}
			hits, err := s.campaigns.ResolveName(ctx, campaign.ScopeDM, campaignID, pick.name)
			if err != nil || len(hits) == 0 {
				continue // a failed lookup is an infra problem, not a validation one
			}
			pick.existingID = hits[0].ID
			*reused = append(*reused, ReusedEntity{Input: pick.name, ID: hits[0].ID, Name: hits[0].Name, Kind: hits[0].Kind})
		}
	}
	convert(a.subs)
	convert(a.npcs)
	// A holder whose person the read-back turned into an existing entity
	// loses its batch-item dependency and gains the real id.
	for i := range a.secrets {
		sec := &a.secrets[i]
		if sec.holderItem == "" {
			continue
		}
		for _, np := range a.npcs {
			if strings.EqualFold(np.name, sec.holderName) && np.existingID != "" {
				sec.holderID, sec.holderItem = np.existingID, ""
				break
			}
		}
	}
	return problems
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
