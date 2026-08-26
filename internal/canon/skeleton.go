package canon

// The campaign skeleton generator (MAD-361, stage 2 of MAD-314's design
// tools): a premise becomes an editable campaign.
//
// This generator does not ask a model for a campaign. Before any prompt is
// built, the structure is computed, the way internal/encounter's designer
// computes a budget before it offers creatures:
//
//   - story.Pace and story.Shape fix the act count, every act's level band
//     and its session count from the requested level range. The model fills
//     the skeleton; it does not decide how many acts there are or where they
//     sit.
//   - The premise is read deterministically for tone, threat and setting
//     signal — the same mechanism encounter.ReadIdea uses for an encounter
//     idea, with a different vocabulary.
//   - The faction web is structural: N factions produce a legal edge set
//     drawn from the controlled relationship types, with at least one
//     secretly_controls edge reaching the central secret. A faction graph
//     with no antagonistic edge is not a campaign, and that is a rule here,
//     not a hope.
//   - The central secret is staged as a secret-visibility fact WITH a clue
//     path — a discovery that grants its keeper knowledge — because
//     unreachable_secret would otherwise flag it the moment it landed.
//
// The model's contribution is names, prose and specificity, filling a shape
// it was handed. The graph objects (factions, NPCs, the secret, the hooks,
// the edges) are staged as one proposal batch behind the review gate; the
// acts and session plans are spine rows — plan, not canon — and are written
// straight into the narrative spine the way a DM writing them by hand would,
// editable through the same story endpoints.
//
// Regeneration is part-addressed (acts, factions, secret, hooks): re-rolling
// the factions keeps the acts by simply not naming them. When the factions
// part is skipped, the secret and hooks anchor to the faction web already in
// the campaign graph — its roles re-derived from the accepted
// secretly_controls edge — so a re-rolled secret points at the real factions,
// not at names a fresh model call invented.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

/* ---------- the deterministic premise read ---------- */

// PremiseHints is the signal pulled out of a premise: which tones it names,
// which threat classes its words imply, which faction archetypes and setting
// words it carries. The analogue of encounter.Hints for a campaign.
type PremiseHints struct {
	Tones      []string `json:"tones"`
	Threats    []string `json:"threats"`
	Archetypes []string `json:"archetypes"`
	Settings   []string `json:"settings"`
}

// toneWords maps premise words onto tone vocabulary.
var toneWords = map[string]string{
	"dark": "dark", "grim": "dark", "grimdark": "dark", "bleak": "dark",
	"gritty": "gritty", "grounded": "gritty",
	"horror": "horror", "scary": "horror", "dread": "horror", "creepy": "horror",
	"nasty": "horror", "nightmare": "horror",
	"intrigue": "intrigue", "political": "intrigue", "politics": "intrigue",
	"court": "intrigue", "scheme": "intrigue", "schemes": "intrigue",
	"conspiracy": "intrigue", "betrayal": "intrigue",
	"mystery": "mystery", "mysterious": "mystery", "secret": "mystery", "secrets": "mystery",
	"comedic": "comedic", "funny": "comedic", "lighthearted": "comedic",
	"tragic": "tragic", "tragedy": "tragic",
	"epic": "epic", "heroic": "epic",
	"survival": "survival", "desperate": "survival",
	"exploration": "exploration", "wilderness": "exploration",
	"war": "war", "invasion": "war", "rebellion": "war", "revolution": "war",
	"heist": "heist", "caper": "heist",
	"sandbox": "sandbox",
}

// threatWords maps premise words onto threat classes: what the campaign is
// threatened by.
var threatWords = map[string][]string{
	"forest": {"ancient forest"}, "woods": {"ancient forest"}, "wildwood": {"ancient forest"},
	"undead": {"the undead"}, "skeletons": {"the undead"}, "vampire": {"the undead"},
	"vampires": {"the undead"}, "necromancer": {"the undead"}, "necromancy": {"the undead"},
	"dragon": {"dragons"}, "dragons": {"dragons"}, "wyrm": {"dragons"}, "drakes": {"dragons"},
	"plague": {"plague"}, "pestilence": {"plague"}, "blight": {"plague"}, "rot": {"plague"},
	"god": {"divine"}, "gods": {"divine"}, "deity": {"divine"}, "deities": {"divine"},
	"prophecy": {"prophecy"}, "prophecies": {"prophecy"}, "fate": {"prophecy"},
	"fey": {"the fey"}, "fae": {"the fey"}, "fairy": {"the fey"}, "fairies": {"the fey"},
	"demon": {"fiends"}, "demons": {"fiends"}, "devil": {"fiends"}, "devils": {"fiends"},
	"fiend": {"fiends"}, "hell": {"fiends"},
	"eldritch": {"the eldritch"}, "aberration": {"the eldritch"}, "tentacled": {"the eldritch"},
	"abyss": {"the eldritch"}, "void": {"the eldritch"},
	"winter": {"endless winter"}, "fimbul": {"endless winter"}, "frozen": {"endless winter"},
	"sea": {"the drowned sea"}, "ocean": {"the drowned sea"}, "islands": {"the drowned sea"},
	"island": {"the drowned sea"}, "archipelago": {"the drowned sea"}, "drowned": {"the drowned sea"},
	"empire": {"empire"}, "imperial": {"empire"}, "tyrant": {"empire"}, "tyranny": {"empire"},
	"inquisition": {"the inquisition"},
}

// archetypeWords maps premise words onto faction archetypes the factions may
// take.
var archetypeWords = map[string]string{
	"cult": "cult", "sect": "cult", "worshippers": "cult",
	"guild": "guild", "guilds": "guild",
	"order": "knightly order", "knights": "knightly order", "knightly": "knightly order",
	"house": "noble house", "houses": "noble house", "noble": "noble house",
	"nobles": "noble house", "dynasty": "noble house", "baron": "noble house",
	"court": "the court", "crown": "the court", "throne": "the court", "king": "the court",
	"queen": "the court", "regent": "the court",
	"clan": "clan", "clans": "clan", "tribe": "clan", "tribes": "clan",
	"circle": "circle", "coven": "circle", "druids": "circle",
	"church": "church", "temple": "church", "faith": "church", "religion": "church",
	"syndicate": "syndicate", "thieves": "syndicate", "gang": "syndicate",
	"mafia": "syndicate", "cartel": "syndicate",
	"merchants": "trading company", "company": "trading company", "trade": "trading company",
	"bank": "trading company", "bankers": "trading company",
	"college": "academy", "academy": "academy", "university": "academy",
	"wizards": "academy", "magi": "academy",
}

// settingWords maps premise words onto setting vocabulary.
var settingWords = map[string]string{
	"kingdom": "kingdom", "kingdoms": "kingdom", "realm": "kingdom", "realms": "kingdom",
	"empire": "empire",
	"city":   "city", "cities": "city", "town": "city", "metropolis": "city",
	"village": "village", "villages": "village", "hamlet": "village",
	"frontier": "frontier", "borderlands": "frontier", "marches": "frontier", "march": "frontier",
	"desert": "desert", "wastes": "desert",
	"mountains": "mountains", "mountain": "mountains", "peaks": "mountains", "highlands": "mountains",
	"swamp": "swamp", "marsh": "swamp", "fen": "swamp", "mire": "swamp",
	"underdark": "the underdark", "deeps": "the underdark",
	"plains": "plains", "grasslands": "plains", "steppe": "plains",
	"river": "river", "rivers": "river", "valley": "river",
	"castle": "citadel", "citadel": "citadel", "fortress": "citadel", "keep": "citadel",
	"ruins": "ruins", "ruined": "ruins",
}

var premiseWordRE = regexp.MustCompile(`[a-z][a-z'-]+`)

// ReadPremise pulls the deterministic signal out of a free-text premise:
// which tones, threat classes, faction archetypes and setting words it
// carries. The same mechanism encounter.ReadIdea applies to an encounter
// idea, with the campaign's vocabulary.
func ReadPremise(premise string) PremiseHints {
	words := premiseWordRE.FindAllString(strings.ToLower(premise), -1)
	var h PremiseHints
	seen := [4]map[string]bool{{}, {}, {}, {}}
	add := func(i int, v string) {
		if v == "" || seen[i][v] {
			return
		}
		seen[i][v] = true
		switch i {
		case 0:
			h.Tones = append(h.Tones, v)
		case 1:
			h.Threats = append(h.Threats, v)
		case 2:
			h.Archetypes = append(h.Archetypes, v)
		case 3:
			h.Settings = append(h.Settings, v)
		}
	}
	for _, w := range words {
		singular := strings.TrimSuffix(w, "s")
		if t, ok := toneWords[w]; ok {
			add(0, t)
		} else if t, ok := toneWords[singular]; ok {
			add(0, t)
		}
		for _, t := range threatWords[w] {
			add(1, t)
		}
		for _, t := range threatWords[singular] {
			add(1, t)
		}
		if a, ok := archetypeWords[w]; ok {
			add(2, a)
		} else if a, ok := archetypeWords[singular]; ok {
			add(2, a)
		}
		if s, ok := settingWords[w]; ok {
			add(3, s)
		} else if s, ok := settingWords[singular]; ok {
			add(3, s)
		}
	}
	sort.Strings(h.Tones)
	sort.Strings(h.Threats)
	sort.Strings(h.Archetypes)
	sort.Strings(h.Settings)
	return h
}

// political reports whether the premise points at political intrigue — the
// signal that widens the faction web from four factions to five.
func (h PremiseHints) political() bool {
	for _, t := range h.Tones {
		if t == "intrigue" {
			return true
		}
	}
	return false
}

/* ---------- structure: bands, roles, edges ---------- */

// Band is one act's level band as the acts table wants it: neighbours chain
// plus-one, neither overlapping nor leaving a gap.
type Band struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// ActBands turns a Pacing — whose acts share boundary levels, because a
// crossing belongs to the act that pays for it — into the tiling bands the
// acts table carries: act i+1 starts at act i's end plus one, the first act
// starts at the band's floor, and the last act always ends at the ceiling
// (Pace never leaves the last slice empty). An act whose Pacing slice was
// empty settles on its start level alone. With actCount at most the span,
// empty slices never neighbour each other and the tiling is exact.
func ActBands(p story.Pacing) []Band {
	out := make([]Band, len(p.PerAct))
	for i, ap := range p.PerAct {
		start := p.LevelStart
		if i > 0 {
			start = out[i-1].End + 1
		}
		end := start
		if ap.LevelEnd > ap.LevelStart { // a non-empty slice of crossings
			end = ap.LevelEnd
		}
		out[i] = Band{Start: start, End: end}
	}
	return out
}

// factionRole is one faction's structural slot: what it is for in the web,
// independent of the name the model will give it.
type factionRole struct {
	Key   string
	Label string
	Brief string
}

// factionRoles is the role catalogue, in the fixed index order the generator
// hands the model: faction 1 is the power, faction 2 the hidden hand, and so
// on. The first three are the minimum web; four and five widen it.
var factionRoles = []factionRole{
	{Key: "power", Label: "The power",
		Brief: "The establishment the campaign's world turns on — the throne, the council, the church that owns the room."},
	{Key: "hidden_hand", Label: "The hidden hand",
		Brief: "The faction that secretly controls the power and keeps the campaign's central secret. Nobody says its name aloud."},
	{Key: "opposition", Label: "The opposition",
		Brief: "Openly against the power. The party's obvious ally — and the obvious tool of someone else."},
	{Key: "wildcard", Label: "The wildcard",
		Brief: "Plays its own game, allied with the opposition for now, loyal to the tide."},
	{Key: "insider", Label: "The insider",
		Brief: "Serves the power from within, honestly or otherwise — the web's loose thread."},
}

// webEdge is one structural edge of the faction web: the roles it joins and
// its type from the controlled relationship vocabulary.
type webEdge struct {
	FromRole int // index into factionRoles
	ToRole   int
	RelType  string
}

// webEdges is the legal edge set for n factions: the secretly_controls edge
// that reaches the central secret (the hidden hand's grip on the power), an
// open antagonism, and one widening edge per faction past three. Every type
// is drawn from the seeded relationship vocabulary.
func webEdges(n int) []webEdge {
	if n < 3 {
		n = 3
	}
	if n > len(factionRoles) {
		n = len(factionRoles)
	}
	edges := []webEdge{
		{FromRole: 1, ToRole: 0, RelType: "secretly_controls"},
		{FromRole: 2, ToRole: 0, RelType: "enemy_of"},
	}
	if n >= 4 {
		edges = append(edges, webEdge{FromRole: 3, ToRole: 2, RelType: "allied_with"})
	}
	if n >= 5 {
		edges = append(edges, webEdge{FromRole: 4, ToRole: 0, RelType: "serves"})
	}
	return edges
}

/* ---------- the generator ---------- */

// The regenerable parts of a skeleton. A re-roll names the parts it wants;
// everything else is left alone.
const (
	PartActs     = "acts"
	PartFactions = "factions"
	PartSecret   = "secret"
	PartHooks    = "hooks"
)

// Input bounds.
const (
	maxPremiseLen = 4000
	maxToneLen    = 500
	rosterCap     = 40
	hookCount     = 4
)

// SkeletonInput is one design request: a premise, a level range, an optional
// act count, tone knobs, and the parts to (re)generate — everything when
// empty.
type SkeletonInput struct {
	CampaignID string
	Premise    string
	Tone       string
	LevelStart int
	LevelEnd   int
	ActCount   int
	Parts      []string
	CreatedBy  string
}

// ReusedEntity is one model-named entity that resolved against the campaign's
// existing graph, so the web links to it instead of proposing a second Duke.
type ReusedEntity struct {
	Input string `json:"input"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
}

// SkeletonResult is what one generator run produced: the staged batch (the
// canon objects behind the review gate), the acts and session plans written
// to the spine when the acts part ran, and the entities reused rather than
// duplicated.
type SkeletonResult struct {
	Batch     *Batch              `json:"batch,omitempty"`
	Pacing    story.Pacing        `json:"pacing"`
	ActShape  story.ActShape      `json:"act_shape"`
	Bands     []Band              `json:"bands"`
	Acts      []story.Act         `json:"acts,omitempty"`
	Plans     []story.SessionPlan `json:"plans,omitempty"`
	Reused    []ReusedEntity      `json:"reused,omitempty"`
	Generated time.Time           `json:"generated_at"`
}

// defaultActCount derives the act count from the level span: short bands
// sustain three acts, a full 1-12 arc four, the longest arcs five.
func defaultActCount(span int) int {
	switch {
	case span <= 6:
		return 3
	case span <= 12:
		return 4
	default:
		return 5
	}
}

// skeletonFaction is one faction as the generator holds it: either proposed
// by the model this run (existing == nil) or already in the campaign graph —
// reused by name in the first case, anchored by id in the second.
type skeletonFaction struct {
	name     string
	summary  string
	localID  string
	role     int // index into factionRoles
	existing *campaign.Entity
}

// ref is how a sibling item references this faction: the real id when the
// faction already exists, the model's name when the batch itself will create
// it (the batch resolution map resolves names case-insensitively).
func (f *skeletonFaction) ref() string {
	if f.existing != nil {
		return f.existing.ID
	}
	return f.name
}

// itemID is the faction's batch item id, empty when it needs no item because
// it already exists.
func (f *skeletonFaction) itemID() string {
	if f.existing != nil {
		return ""
	}
	return f.localID
}

// GenerateSkeleton runs one design exchange: compute the structure, one
// structured-generation call to fill it, stage the canon objects as a
// proposal batch, and — when the acts part ran — write the acts and session
// plans into the narrative spine. Nothing the batch carries is canon until
// DecideBatch accepts it.
func (s *Store) GenerateSkeleton(ctx context.Context, in SkeletonInput) (*SkeletonResult, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	in.Premise = strings.TrimSpace(in.Premise)
	if in.Premise == "" {
		return nil, fmt.Errorf("%w: a skeleton needs a premise", ErrInvalid)
	}
	if len([]rune(in.Premise)) > maxPremiseLen {
		return nil, fmt.Errorf("%w: the premise is longer than %d characters", ErrInvalid, maxPremiseLen)
	}
	in.Tone = strings.TrimSpace(in.Tone)
	if len([]rune(in.Tone)) > maxToneLen {
		return nil, fmt.Errorf("%w: the tone knobs are longer than %d characters", ErrInvalid, maxToneLen)
	}
	if in.LevelStart == 0 {
		in.LevelStart = 1
	}
	if in.LevelEnd == 0 {
		in.LevelEnd = 10
	}
	if in.LevelStart < 1 || in.LevelEnd > 20 {
		return nil, fmt.Errorf("%w: a campaign lives inside levels 1-20 (got %d-%d)", ErrInvalid, in.LevelStart, in.LevelEnd)
	}
	if in.LevelStart > in.LevelEnd {
		in.LevelStart, in.LevelEnd = in.LevelEnd, in.LevelStart
	}
	span := in.LevelEnd - in.LevelStart + 1

	actCount := in.ActCount
	if actCount == 0 {
		actCount = defaultActCount(span)
	}
	if _, ok := story.Shape(actCount); !ok {
		return nil, fmt.Errorf("%w: %d acts is not a legal structure — the shapes are three_act, four_act and five_act",
			ErrInvalid, actCount)
	}
	if actCount > span {
		return nil, fmt.Errorf("%w: levels %d-%d sustain at most %d acts; a %d-act structure needs a wider band",
			ErrInvalid, in.LevelStart, in.LevelEnd, span, actCount)
	}

	parts := map[string]bool{}
	for _, p := range in.Parts {
		p = strings.TrimSpace(p)
		switch p {
		case PartActs, PartFactions, PartSecret, PartHooks:
			parts[p] = true
		default:
			return nil, fmt.Errorf("%w: %q is not a skeleton part (acts, factions, secret, hooks)", ErrInvalid, p)
		}
	}
	if len(parts) == 0 {
		parts = map[string]bool{PartActs: true, PartFactions: true, PartSecret: true, PartHooks: true}
	}

	if _, err := s.loadCampaign(ctx, in.CampaignID); err != nil {
		return nil, err
	}

	stories, err := story.New(s.db)
	if err != nil {
		return nil, err
	}
	sessions, err := gamesession.New(s.db)
	if err != nil {
		return nil, err
	}

	// The acts part refuses a campaign that already has acts: regenerating
	// factions keeps them precisely because the acts part was not named.
	if parts[PartActs] {
		existing, err := stories.ListActs(ctx, campaign.ScopeDM, in.CampaignID)
		if err != nil {
			return nil, err
		}
		if len(existing) > 0 {
			return nil, fmt.Errorf("%w: the campaign already has %d acts; delete them or regenerate without the acts part",
				ErrInvalid, len(existing))
		}
	}

	// The structure: pacing, shape, tiling bands, the faction web.
	pacing := story.Pace(in.LevelStart, in.LevelEnd, actCount)
	shape, _ := story.Shape(actCount)
	bands := ActBands(pacing)
	hints := ReadPremise(in.Premise + " " + in.Tone)

	// The factions: model-named when the factions part runs, read back from
	// the accepted graph when it does not — a re-rolled secret must point at
	// the real factions, not at names a fresh model call invented.
	factionCount := 4
	if hints.political() {
		factionCount = 5
	}
	var factions []*skeletonFaction
	var edges []webEdge
	if parts[PartFactions] {
		edges = webEdges(factionCount)
		for i := 0; i < factionCount; i++ {
			factions = append(factions, &skeletonFaction{
				role: i, localID: fmt.Sprintf("faction-%d", i+1),
			})
		}
	} else if parts[PartSecret] || parts[PartHooks] {
		// Anchoring the secret or the hooks to the accepted web — an
		// acts-only rerun needs no factions at all.
		factions, err = s.acceptedFactions(ctx, in.CampaignID)
		if err != nil {
			return nil, err
		}
		if len(factions) < 2 {
			return nil, fmt.Errorf("%w: the campaign has no accepted faction web to anchor to; regenerate with the factions part",
				ErrInvalid)
		}
		factionCount = len(factions)
	}

	relTypes, err := s.RelationshipTypes(ctx, in.CampaignID)
	if err != nil {
		return nil, err
	}
	legalRel := map[string]bool{}
	for _, t := range relTypes {
		legalRel[t] = true
	}
	for _, e := range edges {
		if !legalRel[e.RelType] {
			return nil, fmt.Errorf("%w: the relationship vocabulary has no %q type", ErrInvalid, e.RelType)
		}
	}

	// The campaign's existing entities: the model reuses them where they fit,
	// and the generator enforces it after the fact through ResolveName.
	existing, err := s.campaigns.ListEntities(ctx, campaign.ScopeDM, in.CampaignID, "")
	if err != nil {
		return nil, err
	}
	roster := existing
	if len(roster) > rosterCap {
		roster = roster[:rosterCap]
	}

	// One structured-generation call: the schema is the structure. Field
	// count follows the computed shape, so the model cannot return a campaign
	// of a different size than the one asked for.
	fields := skeletonFields(skeletonFieldsIn{
		parts: parts, factionCount: factionCount, factions: factions,
		actCount: actCount, bands: bands, pacing: pacing, shape: shape,
	})
	structure := skeletonStructure{
		Premise: in.Premise, Tone: in.Tone, Hints: hints,
		Levels: skeletonLevels{Start: in.LevelStart, End: in.LevelEnd},
	}
	for i := range shape.Acts {
		structure.Acts = append(structure.Acts, skeletonAct{
			Index: i + 1, Role: shape.Acts[i].Key, Label: shape.Acts[i].Label,
			Purpose: shape.Acts[i].Purpose, LevelStart: bands[i].Start, LevelEnd: bands[i].End,
			Sessions: pacing.PerAct[i].Sessions,
		})
	}
	for i, f := range factions {
		fv := skeletonFactionView{
			Index: i + 1, Role: factionRoles[f.role].Key,
			Label: factionRoles[f.role].Label, Brief: factionRoles[f.role].Brief,
		}
		if f.existing != nil {
			fv.Name = f.existing.Name
		}
		structure.Factions = append(structure.Factions, fv)
		if parts[PartFactions] {
			structure.NPCs = append(structure.NPCs, skeletonNPC{Index: i + 1, LeadsFaction: i + 1})
		}
	}
	for _, e := range edges {
		structure.Web = append(structure.Web, skeletonEdgeView{
			From: factionRoles[e.FromRole].Key, RelType: e.RelType, To: factionRoles[e.ToRole].Key,
		})
	}
	structure.Hooks = hookCount
	for _, e := range roster {
		structure.Existing = append(structure.Existing, skeletonExisting{Name: e.Name, Kind: e.Kind, Summary: e.Summary})
	}

	gen, err := s.Generate(ctx, GenerateInput{
		System: skeletonSystemPrompt, Structure: structure, Fields: fields,
		Note: "Fill every declared field. The structure block is the campaign you are designing — its counts and edges are already law.",
	})
	if err != nil {
		return nil, err
	}
	val := func(key string) string {
		sv, _ := gen.Values[key].(string)
		return strings.TrimSpace(sv)
	}

	// The model-named factions and their leaders: collected, checked for
	// duplicates, and resolved against the campaign graph — a name that
	// resolves is reused, not duplicated. The entity_merge_candidate check
	// exists precisely because this goes wrong when a generator skips it.
	npcs := make([]npcRefSlot, factionCount)
	var reused []ReusedEntity
	if parts[PartFactions] {
		seen := map[string]string{}
		claim := func(name, what string) error {
			key := strings.ToLower(name)
			if key == "" {
				return fmt.Errorf("%w: %s has no name", ErrInvalid, what)
			}
			if prev, dup := seen[key]; dup {
				return fmt.Errorf("%w: %q is named twice (as %s and %s) — every name must be distinct",
					ErrInvalid, name, prev, what)
			}
			seen[key] = what
			return nil
		}
		for i := 1; i <= factionCount; i++ {
			f := factions[i-1]
			f.name = val(fmt.Sprintf("faction_%d_name", i))
			f.summary = val(fmt.Sprintf("faction_%d_summary", i))
			if err := claim(f.name, fmt.Sprintf("faction %d", i)); err != nil {
				return nil, err
			}
			n := &npcs[i-1]
			n.name = val(fmt.Sprintf("npc_%d_name", i))
			n.summary = val(fmt.Sprintf("npc_%d_summary", i))
			n.localID = fmt.Sprintf("npc-%d", i)
			if err := claim(n.name, fmt.Sprintf("npc %d", i)); err != nil {
				return nil, err
			}
		}
		resolve := func(name string) (*campaign.Entity, error) {
			hits, err := s.campaigns.ResolveName(ctx, campaign.ScopeDM, in.CampaignID, name)
			if err != nil {
				return nil, err
			}
			if len(hits) == 0 {
				return nil, nil
			}
			hit := hits[0]
			reused = append(reused, ReusedEntity{Input: name, ID: hit.ID, Name: hit.Name, Kind: hit.Kind})
			return &hit, nil
		}
		for _, f := range factions {
			if hit, err := resolve(f.name); err != nil {
				return nil, err
			} else if hit != nil {
				f.existing = hit
			}
		}
		for i := range npcs {
			n := &npcs[i]
			if hit, err := resolve(n.name); err != nil {
				return nil, err
			} else if hit != nil {
				n.existing = hit
			}
		}
	}

	// The batch: entities first, then the secret and hooks with their clue
	// paths, then the web that joins everything. depends_on names the
	// siblings that must apply first; StageBatch turns them into review ids.
	var items []BatchItemInput
	factionDeps := func(fs ...*skeletonFaction) []string {
		var deps []string
		for _, f := range fs {
			if id := f.itemID(); id != "" {
				deps = append(deps, id)
			}
		}
		return deps
	}
	if parts[PartFactions] {
		for _, f := range factions {
			if f.existing != nil {
				continue
			}
			items = append(items, BatchItemInput{
				ID: f.localID, Kind: "entity", Subject: f.name, Summary: f.summary,
				Payload: map[string]any{
					"local_id": f.localID, "kind": campaign.KindFaction,
					"name": f.name, "summary": f.summary,
				},
			})
		}
		for i := range npcs {
			n := &npcs[i]
			if n.existing != nil {
				continue
			}
			items = append(items, BatchItemInput{
				ID: n.localID, Kind: "entity", Subject: n.name, Summary: n.summary,
				Payload: map[string]any{
					"local_id": n.localID, "kind": campaign.KindNPC,
					"name": n.name, "summary": n.summary,
				},
			})
		}
		for _, e := range edges {
			items = append(items, BatchItemInput{
				ID:      fmt.Sprintf("web-%s-%s", factionRoles[e.FromRole].Key, factionRoles[e.ToRole].Key),
				Kind:    "relationship",
				Subject: fmt.Sprintf("%s %s %s", factionRoles[e.FromRole].Label, e.RelType, factionRoles[e.ToRole].Label),
				Summary: "The structural edge of the faction web.",
				Payload: map[string]any{
					"from_entity": factions[e.FromRole].ref(), "rel_type": e.RelType,
					"to_entity": factions[e.ToRole].ref(),
				},
				DependsOn: factionDeps(factions[e.FromRole], factions[e.ToRole]),
			})
		}
		for i := range npcs {
			n := &npcs[i]
			deps := factionDeps(factions[i])
			if n.existing == nil {
				deps = append(deps, n.localID)
			}
			items = append(items, BatchItemInput{
				ID: fmt.Sprintf("npc-leads-%d", i+1), Kind: "relationship",
				Subject: fmt.Sprintf("%s leads %s", n.name, factions[i].name),
				Summary: fmt.Sprintf("The leader of %s.", factions[i].name),
				Payload: map[string]any{
					"from_entity": npcRef(npcs, i), "rel_type": "leads", "to_entity": factions[i].ref(),
				},
				DependsOn: deps,
			})
		}
	}
	if parts[PartSecret] {
		statement := val("secret_statement")
		keeper, victim := factions[1], factions[0]
		cluePath := fmt.Sprintf("held by the hidden hand; its clues surface across the %d acts", actCount)
		items = append(items, BatchItemInput{
			ID: "central-secret", Kind: "fact", Subject: "The central secret", Summary: statement,
			Payload: map[string]any{
				"local_id": "central-secret", "statement": statement,
				"subject": keeper.ref(), "predicate": "secretly_controls", "object_entity": victim.ref(),
				"visibility": campaign.VisibilitySecret, "clue_path": cluePath,
			},
			DependsOn: factionDeps(keeper, victim),
		})
		// The clue path: the hidden hand knows its own secret, which is what
		// keeps unreachable_secret quiet the moment the fact lands. The
		// scenes the clues surface in are the spine's to place (MAD-362).
		items = append(items, BatchItemInput{
			ID: "central-secret-clue", Kind: "discovery",
			Subject: "The central secret's clue path", Summary: statement,
			Payload: map[string]any{
				"fact": "central-secret", "discovered_by": keeper.ref(), "stance": "knows",
				"method": "Keeper of the central secret (" + cluePath + ")",
			},
			DependsOn: append([]string{"central-secret"}, factionDeps(keeper)...),
		})
	}
	if parts[PartHooks] {
		for i := 1; i <= hookCount; i++ {
			statement := val(fmt.Sprintf("hook_%d_statement", i))
			thread := val(fmt.Sprintf("hook_%d_thread", i))
			lead := val(fmt.Sprintf("hook_%d_lead", i))
			subject := factions[(i-1)%len(factions)]
			hookID := fmt.Sprintf("hook-%d", i)
			items = append(items, BatchItemInput{
				ID: hookID, Kind: "fact", Subject: fmt.Sprintf("Opening hook %d", i), Summary: statement,
				Payload: map[string]any{
					"local_id": hookID, "statement": statement,
					"subject": subject.ref(), "predicate": "is_behind", "object_literal": thread,
					"visibility": campaign.VisibilitySecret, "lead": lead,
				},
				DependsOn: factionDeps(subject),
			})
			// Not born orphaned: the party already suspects the thread — an
			// awareness row the moment the hook lands.
			items = append(items, BatchItemInput{
				ID: hookID + "-lead", Kind: "discovery",
				Subject: fmt.Sprintf("Opening hook %d's lead", i), Summary: statement,
				Payload: map[string]any{
					"fact": hookID, "discovered_by": campaign.PartyKnower, "stance": "suspects",
					"method": lead,
				},
				DependsOn: []string{hookID},
			})
		}
	}

	res := &SkeletonResult{
		Pacing: pacing, ActShape: shape, Bands: bands,
		Reused: reused, Generated: s.now(),
	}
	if len(items) > 0 {
		var partNames []string
		for _, p := range in.Parts {
			partNames = append(partNames, p)
		}
		sort.Strings(partNames)
		promptRecord := fmt.Sprintf("premise: %s", in.Premise)
		if in.Tone != "" {
			promptRecord += " | tone: " + in.Tone
		}
		promptRecord += fmt.Sprintf(" | levels %d-%d, %d acts, %d sessions | parts: %s | signals: tones [%s], threats [%s], archetypes [%s], settings [%s]",
			in.LevelStart, in.LevelEnd, actCount, pacing.TotalSessions, strings.Join(partNames, "+"),
			strings.Join(hints.Tones, ", "), strings.Join(hints.Threats, ", "),
			strings.Join(hints.Archetypes, ", "), strings.Join(hints.Settings, ", "))
		if len(reused) > 0 {
			var names []string
			for _, r := range reused {
				names = append(names, r.Name)
			}
			promptRecord += " | reused: " + strings.Join(names, ", ")
		}
		res.Batch, err = s.StageBatch(ctx, BatchInput{
			CampaignID: in.CampaignID, Source: BatchSourceSkeleton,
			Prompt: promptRecord, CreatedBy: in.CreatedBy, Items: items,
		})
		if err != nil {
			return nil, err
		}
	}

	// The spine: acts and session plans are plan rows, written the way a DM
	// writes them by hand — no gate, fully editable.
	if parts[PartActs] {
		for i := 1; i <= actCount; i++ {
			premise := val(fmt.Sprintf("act_%d_premise", i))
			if premise == "" {
				premise = shape.Acts[i-1].Purpose
			}
			act, err := stories.CreateAct(ctx, in.CampaignID,
				val(fmt.Sprintf("act_%d_name", i)), premise, bands[i-1].Start, bands[i-1].End)
			if err != nil {
				return nil, err
			}
			res.Acts = append(res.Acts, *act)
			for j := 0; j < pacing.PerAct[i-1].Sessions; j++ {
				sess, err := sessions.CreateSession(ctx, in.CampaignID, "")
				if err != nil {
					return nil, err
				}
				planned := story.PlanStatusPlanned
				plan, err := stories.PutPlan(ctx, in.CampaignID, sess.ID, act.ID, premise, "", &planned)
				if err != nil {
					return nil, err
				}
				res.Plans = append(res.Plans, *plan)
			}
		}
	}
	return res, nil
}

// npcRefSlot is one model-named leader NPC: its name, summary and batch
// local id, and the entity it resolved against when the name was reused.
type npcRefSlot struct {
	name     string
	summary  string
	localID  string
	existing *campaign.Entity
}

// npcRef is how a sibling item references NPC i: the real id when the leader
// already exists, the model's name when the batch itself will create it.
func npcRef(npcs []npcRefSlot, i int) string {
	if npcs[i].existing != nil {
		return npcs[i].existing.ID
	}
	return npcs[i].name
}

// acceptedFactions reads the campaign's faction web back from the graph in
// role order: the secretly_controls edge names the hidden hand and the power,
// the remaining factions fill the widening roles in name order.
func (s *Store) acceptedFactions(ctx context.Context, campaignID string) ([]*skeletonFaction, error) {
	entities, err := s.campaigns.ListEntities(ctx, campaign.ScopeDM, campaignID, campaign.KindFaction)
	if err != nil {
		return nil, err
	}
	if len(entities) < 2 {
		return nil, nil
	}
	byID := map[string]int{}
	for i := range entities {
		byID[entities[i].ID] = i
	}
	rels, err := s.campaigns.ListRelationships(ctx, campaign.ScopeDM, campaignID)
	if err != nil {
		return nil, err
	}
	var hiddenHand, power *campaign.Entity
	used := map[string]bool{}
	for _, r := range rels {
		if r.RelType != "secretly_controls" {
			continue
		}
		from, okFrom := byID[r.FromEntity]
		to, okTo := byID[r.ToEntity]
		if !okFrom || !okTo {
			continue
		}
		hiddenHand, power = &entities[from], &entities[to]
		used[hiddenHand.ID] = true
		used[power.ID] = true
		break
	}
	if hiddenHand == nil {
		return nil, fmt.Errorf("%w: the campaign's factions have no secretly_controls edge to hang a secret on; regenerate with the factions part",
			ErrInvalid)
	}
	out := []*skeletonFaction{
		{role: 0, existing: power},
		{role: 1, existing: hiddenHand},
	}
	for i := range entities {
		if used[entities[i].ID] {
			continue
		}
		if len(out) >= len(factionRoles) {
			break
		}
		out = append(out, &skeletonFaction{role: len(out), existing: &entities[i]})
	}
	return out, nil
}

/* ---------- the prompt ---------- */

// The prompt-side shapes. Kept separate from the store types so the wire
// contract can carry display text the model needs (role briefs, purposes)
// without polluting the structures the generator computes from.
type skeletonStructure struct {
	Premise  string                `json:"premise"`
	Tone     string                `json:"tone_knobs,omitempty"`
	Hints    PremiseHints          `json:"signals"`
	Levels   skeletonLevels        `json:"levels"`
	Acts     []skeletonAct         `json:"acts"`
	Factions []skeletonFactionView `json:"factions"`
	NPCs     []skeletonNPC         `json:"npcs,omitempty"`
	Web      []skeletonEdgeView    `json:"faction_web,omitempty"`
	Hooks    int                   `json:"hooks"`
	Existing []skeletonExisting    `json:"existing_entities,omitempty"`
}

type skeletonLevels struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type skeletonAct struct {
	Index      int    `json:"index"`
	Role       string `json:"role"`
	Label      string `json:"label"`
	Purpose    string `json:"purpose"`
	LevelStart int    `json:"level_start"`
	LevelEnd   int    `json:"level_end"`
	Sessions   int    `json:"sessions"`
}

type skeletonFactionView struct {
	Index int    `json:"index"`
	Role  string `json:"role"`
	Label string `json:"label"`
	Brief string `json:"brief"`
	Name  string `json:"name,omitempty"` // set when anchoring to an existing faction
}

type skeletonNPC struct {
	Index        int `json:"index"`
	LeadsFaction int `json:"leads_faction"`
}

type skeletonEdgeView struct {
	From    string `json:"from_role"`
	RelType string `json:"rel_type"`
	To      string `json:"to_role"`
}

type skeletonExisting struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Summary string `json:"summary,omitempty"`
}

// skeletonFieldsIn carries what the field builder needs.
type skeletonFieldsIn struct {
	parts        map[string]bool
	factionCount int
	factions     []*skeletonFaction
	actCount     int
	bands        []Band
	pacing       story.Pacing
	shape        story.ActShape
}

// skeletonFields builds the declared schema — the structure the model fills.
// Field count follows the computed shape and the requested parts.
func skeletonFields(in skeletonFieldsIn) []FieldSpec {
	fields := make([]FieldSpec, 0, in.factionCount*4+hookCount*3+in.actCount*2+1)
	if in.parts[PartFactions] {
		for i := 1; i <= in.factionCount; i++ {
			fields = append(fields,
				FieldSpec{Key: fmt.Sprintf("faction_%d_name", i), Required: true, MaxLen: 60,
					Desc: fmt.Sprintf("Name of faction %d — %s. %s", i, factionRoles[i-1].Label, factionRoles[i-1].Brief)},
				FieldSpec{Key: fmt.Sprintf("faction_%d_summary", i), Required: true, MaxLen: 280,
					Desc: fmt.Sprintf("Two sentences on faction %d: what it wants, what it does, what it smells like.", i)},
				FieldSpec{Key: fmt.Sprintf("npc_%d_name", i), Required: true, MaxLen: 60,
					Desc: fmt.Sprintf("Name of the major NPC who leads faction %d — its face and voice.", i)},
				FieldSpec{Key: fmt.Sprintf("npc_%d_summary", i), Required: true, MaxLen: 280,
					Desc: fmt.Sprintf("Two sentences on the leader of faction %d: how they carry themselves and what they want.", i)},
			)
		}
	}
	if in.parts[PartSecret] {
		fields = append(fields, FieldSpec{Key: "secret_statement", Required: true, MaxLen: 400,
			Desc: "The central secret, one sentence, naming what the hidden hand secretly controls and why. This is the fact the whole campaign turns on."})
	}
	if in.parts[PartHooks] {
		for i := 1; i <= hookCount; i++ {
			fi := (i-1)%in.factionCount + 1
			hookDesc := fmt.Sprintf("Opening hook %d of %d: one concrete sentence the party can chase from session one, pointing at faction %d.", i, hookCount, fi)
			if !in.parts[PartFactions] && in.factions[fi-1].existing != nil {
				hookDesc = fmt.Sprintf("Opening hook %d of %d: one concrete sentence the party can chase from session one, pointing at %s.", i, hookCount, in.factions[fi-1].existing.Name)
			}
			fields = append(fields,
				FieldSpec{Key: fmt.Sprintf("hook_%d_statement", i), Required: true, MaxLen: 300, Desc: hookDesc},
				FieldSpec{Key: fmt.Sprintf("hook_%d_thread", i), Required: true, MaxLen: 60,
					Desc: fmt.Sprintf("Hook %d in three to six words — the thing the faction is behind (e.g. \"the road disappearances\").", i)},
				FieldSpec{Key: fmt.Sprintf("hook_%d_lead", i), Required: true, MaxLen: 200,
					Desc: fmt.Sprintf("How the party first brushes hook %d: who carries the rumour, where it is heard.", i)},
			)
		}
	}
	if in.parts[PartActs] {
		for i := 1; i <= in.actCount; i++ {
			fields = append(fields,
				FieldSpec{Key: fmt.Sprintf("act_%d_name", i), Required: true, MaxLen: 80,
					Desc: fmt.Sprintf("Name of act %d (levels %d-%d, about %d sessions) — %s: %s",
						i, in.bands[i-1].Start, in.bands[i-1].End, in.pacing.PerAct[i-1].Sessions,
						in.shape.Acts[i-1].Label, in.shape.Acts[i-1].Purpose)},
				FieldSpec{Key: fmt.Sprintf("act_%d_premise", i), Required: true, MaxLen: 400,
					Desc: fmt.Sprintf("Act %d's premise in two or three sentences: what the party is doing at these levels and how the campaign's pressure grows.", i)},
			)
		}
	}
	return fields
}

// skeletonSystemPrompt is the generator's system prompt.
const skeletonSystemPrompt = `You are Grimoire's campaign skeleton designer. You are handed the complete structure of a campaign — computed by the server and authoritative: the level range, every act's level band and session count, the faction web's roles and edges, the hook count, and the campaign's existing entities — plus the DM's premise. Your job is to fill that structure with names and specificity, nothing more.

STRICT RULES
1. Every field is a plain string. Fill every one. No lists, no markdown, no commentary.
2. The structure is fixed. Do not add, merge or reorder factions, acts or hooks. Faction 2 is always the hidden hand; the web's edges are already decided.
3. Reuse the existing entities where they fit. If the campaign already has a Duke, a town or a cult, name it again exactly and build on it — never invent a second version of someone who exists.
4. The central secret belongs to the hidden hand and must make the secretly_controls edge true. The hooks are leads toward the factions they point at, concrete enough to run in session one.
5. Names are evocative and specific (not "The Evil Cult"); prose is present tense, concrete, no filler.

Honour the DM's premise: its genre, its tone, its setting. Where it names things, use them.`
