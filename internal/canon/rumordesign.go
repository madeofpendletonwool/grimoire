package canon

/*
The rumour mill's generator (MAD-374, stage 5.2 of MAD-316).

Deterministic before the prompt, as everywhere in stage 5:

  - The truth mix is a parameter, not the model's mood: how many true, how
    many false, how many distorted — the counts are arithmetic, and the
    model fills words into exactly that many slots, no more.
  - The true ones are drawn from the campaign's own secret facts. A rumour
    that is true and about nothing in the graph is a rumour that leads
    nowhere — the single most common way a generated rumour table wastes a
    session — so a request for more true rumours than secret facts exist
    is refused with the count, never padded.
  - The false and distorted ones are filtered against facts the party
    already holds a granting stance on: a false rumour the party can
    instantly disprove is noise. Distorted rumours name the fact they
    distort; false ones fall back to invented-whole when the
    contradictable pool runs dry, because the requested mix is honoured
    exactly.
  - Distribution is a join, not a model call: candidate holders are the
    NPCs with located_in edges into the area and the faction edges that
    would carry the talk, first-K in stable order, weighted by spread.

The model writes statements and nothing else — one field per computed
slot, each slot's description carrying the fact it attests or contradicts,
so a true rumour is worded against the real secret and a distorted one
against the real fact it twists.
*/

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/* ---------- the input and the result ---------- */

// RumorDesignInput is one generation request. About names the entity or
// location the rumours circle; the three counts are the truth mix and are
// honoured exactly.
type RumorDesignInput struct {
	CampaignID string
	About      string
	TrueCount  int
	FalseCount int
	// DistortedCount twists an existing fact rather than contradicting or
	// inventing; each one names the fact it distorts.
	DistortedCount int
	Premise        string
	CreatedBy      string
}

// RumorDesignResult is the arithmetic plus the staged batch: nothing is in
// the mill until the batch is decided.
type RumorDesignResult struct {
	Batch *Batch
	// Facts carries the fact statements each slot drew, keyed by slot id
	// (t1, f1, d1...); empty for an invented-whole false rumour. The
	// response renders it so the DM sees what each rumour plays against.
	Facts map[string]string
	// Holders counts the distribution the join proposed.
	Holders int
}

// maxRumorSlots bounds one request: a mill table for one area never needs
// more than this, and the review queue is the scarce resource.
const maxRumorSlots = 12

// holdersForSpread is how many holders the join proposes per rumour,
// weighted by its spread: local talk is one or two voices, widespread is
// half the tavern.
var holdersForSpread = map[string]int{
	"local": 2, "regional": 3, "widespread": 5,
}

// rumorSlot is one computed slot: the truth it carries, the fact it plays
// against (empty for invented-whole), and its model-filled statement.
type rumorSlot struct {
	ID   string // t1, f1, d1... — the field key, stable across the exchange
	Kind string // true | false | distorted
	Fact string // fact id; "" for invented whole
}

/* ---------- the generator ---------- */

// GenerateRumors stages one rumour batch about an entity or a location.
func (s *Store) GenerateRumors(ctx context.Context, in RumorDesignInput) (*RumorDesignResult, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.About) == "" {
		return nil, fmt.Errorf("%w: a rumour needs a subject — an entity or a location", ErrInvalid)
	}
	if in.TrueCount < 0 || in.FalseCount < 0 || in.DistortedCount < 0 {
		return nil, fmt.Errorf("%w: counts cannot be negative", ErrInvalid)
	}
	total := in.TrueCount + in.FalseCount + in.DistortedCount
	if total == 0 {
		return nil, fmt.Errorf("%w: a rumour batch needs at least one slot", ErrInvalid)
	}
	if total > maxRumorSlots {
		return nil, fmt.Errorf("%w: %d slots is more than one mill table (%d max)", ErrInvalid, total, maxRumorSlots)
	}
	if _, err := s.loadCampaign(ctx, in.CampaignID); err != nil {
		return nil, err
	}
	snap, err := LoadSnapshot(ctx, s.db, in.CampaignID)
	if err != nil {
		return nil, err
	}

	// The subject: any live entity — a location, an NPC, even an item
	// people gossip about.
	var about *campaignEntityLite
	for i := range snap.Entities {
		e := &snap.Entities[i]
		if e.ID == in.About && e.Status != "deleted" {
			about = &campaignEntityLite{ID: e.ID, Name: e.Name, Kind: e.Kind}
			break
		}
	}
	if about == nil {
		return nil, fmt.Errorf("%w: entity %s", ErrNotFound, in.About)
	}

	/* ---------- the pools, deterministically ---------- */

	// What the party holds: a fact granted to the party or a pc cannot
	// back a false rumour — the party would disprove it on sight.
	partyHolds := map[string]bool{}
	isPC := map[string]bool{}
	for _, e := range snap.Entities {
		if e.Kind == "pc" && e.Status != "deleted" {
			isPC[e.ID] = true
		}
	}
	for _, a := range snap.Awareness {
		if a.Knower == "party" || isPC[a.Knower] {
			if a.Stance == "knows" || a.Stance == "suspects" || a.Stance == "believes_false" {
				partyHolds[a.FactID] = true
			}
		}
	}
	liveAbout := func(f campaign.Fact) bool {
		return f.SubjectEntity == about.ID || f.ObjectEntity == about.ID
	}

	var secrets, contradictable []rumorFactRef
	for _, f := range snap.Facts {
		if f.Confidence != "canon" && f.Confidence != "derived" {
			continue // proposed is invisible; contested is nobody's to lean on
		}
		if f.SupersededBy != "" || !liveAbout(f) {
			continue
		}
		ref := rumorFactRef{ID: f.ID, Statement: f.Statement, Subject: f.SubjectEntity, Object: f.ObjectEntity}
		if f.Visibility == "secret" {
			secrets = append(secrets, ref)
		}
		if !partyHolds[f.ID] {
			contradictable = append(contradictable, ref)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].ID < secrets[j].ID })
	sort.Slice(contradictable, func(i, j int) bool { return contradictable[i].ID < contradictable[j].ID })

	if len(secrets) < in.TrueCount {
		return nil, fmt.Errorf("%w: asked for %d true rumours but the campaign has only %d live secret fact(s) about %s — true rumours are drawn from the campaign's own secrets, never padded",
			ErrInvalid, in.TrueCount, len(secrets), about.Name)
	}
	if len(contradictable) < in.DistortedCount {
		return nil, fmt.Errorf("%w: asked for %d distorted rumours but only %d fact(s) about %s are not already held by the party — distort fewer, or add facts",
			ErrInvalid, in.DistortedCount, len(contradictable), about.Name)
	}

	/* ---------- the slots ---------- */

	var slots []rumorSlot
	facts := map[string]string{}
	for i := 0; i < in.TrueCount; i++ {
		id := fmt.Sprintf("t%d", i+1)
		slots = append(slots, rumorSlot{ID: id, Kind: "true", Fact: secrets[i].ID})
		facts[id] = secrets[i].Statement
	}
	// The distorted slots draw from the pool's tail, the false ones from
	// the head: one fact carries at most one contradicting rumour per
	// batch, and neither side can starve the other. When the head runs
	// dry the remaining false rumours are invented whole — the mix is
	// still honoured exactly.
	falsePool := len(contradictable) - in.DistortedCount
	consumed := 0
	invented := 0
	for i := 0; i < in.FalseCount; i++ {
		id := fmt.Sprintf("f%d", i+1)
		if consumed < falsePool {
			slots = append(slots, rumorSlot{ID: id, Kind: "false", Fact: contradictable[consumed].ID})
			facts[id] = contradictable[consumed].Statement
			consumed++
		} else {
			// The pool ran dry: a false rumour invented whole still
			// honours the mix — it contradicts nothing because it names
			// nothing.
			slots = append(slots, rumorSlot{ID: id, Kind: "false", Fact: ""})
			invented++
		}
	}
	for i := 0; i < in.DistortedCount; i++ {
		id := fmt.Sprintf("d%d", i+1)
		slots = append(slots, rumorSlot{ID: id, Kind: "distorted", Fact: contradictable[falsePool+i].ID})
		facts[id] = contradictable[falsePool+i].Statement
	}

	/* ---------- the distribution join ---------- */

	holders := rumorHolderPool(snap, about)
	pickHolders := func(spread string) []string {
		n := holdersForSpread[spread]
		if n > len(holders) {
			n = len(holders)
		}
		return holders[:n]
	}

	/* ---------- the one model call ---------- */

	fields := make([]FieldSpec, 0, len(slots))
	structureSlots := map[string]any{}
	for _, sl := range slots {
		desc := "the rumour's wording as it circulates"
		switch sl.Kind {
		case "true":
			desc = "a TRUE rumour — gossip that happens to carry this secret: " + facts[sl.ID]
		case "false":
			if sl.Fact != "" {
				desc = "a FALSE rumour contradicting this fact: " + facts[sl.ID]
			} else {
				desc = "a FALSE rumour invented whole — plausible, checkable never, pointing at nothing"
			}
		case "distorted":
			desc = "a DISTORTED rumour — this fact twisted into something adjacent and wrong: " + facts[sl.ID]
		}
		fields = append(fields, FieldSpec{Key: sl.ID, Desc: desc, Required: true, MaxLen: 220})
		structureSlots[sl.ID] = map[string]any{"kind": sl.Kind, "fact": facts[sl.ID]}
	}
	structure := map[string]any{
		"about":      map[string]any{"id": about.ID, "name": about.Name, "kind": about.Kind},
		"slots":      structureSlots,
		"counts":     map[string]int{"true": in.TrueCount, "false": in.FalseCount, "distorted": in.DistortedCount},
		"holderPool": len(holders),
	}
	if in.Premise != "" {
		structure["premise"] = in.Premise
	}
	note := "Fill every declared slot with the wording the rumour actually circulates in. The counts and truth values in the structure are law; you are writing the words, not choosing what is true."
	gen, err := s.Generate(ctx, GenerateInput{
		System: rumorSystemPrompt, Structure: structure, Fields: fields, Note: note,
	})
	if err != nil {
		return nil, err
	}

	/* ---------- assemble, one repair retry ---------- */

	assemble := func(values map[string]any) ([]BatchItemInput, []string) {
		var items []BatchItemInput
		var problems []string
		for _, sl := range slots {
			statement, _ := values[sl.ID].(string)
			statement = strings.TrimSpace(statement)
			if statement == "" {
				problems = append(problems, fmt.Sprintf("slot %s (%s) came back empty", sl.ID, sl.Kind))
				continue
			}
			spread := spreadForSlot(sl, in.FalseCount+in.DistortedCount)
			payload := map[string]any{
				"statement":    statement,
				"truth":        sl.Kind,
				"about_entity": about.ID,
				"origin":       fmt.Sprintf("generated about %s", about.Name),
				"spread":       spread,
			}
			if sl.Fact != "" {
				payload["fact_id"] = sl.Fact
			}
			chosen := pickHolders(spread)
			if len(chosen) > 0 {
				var hs []any
				for _, h := range chosen {
					hs = append(hs, map[string]any{"entity": h})
				}
				payload["holders"] = hs
			}
			items = append(items, BatchItemInput{
				ID: sl.ID, Kind: "rumor", Subject: about.Name,
				Summary: statement, Payload: payload,
			})
		}
		return items, problems
	}
	items, problems := assemble(gen.Values)
	if len(problems) > 0 {
		gen, err = s.Generate(ctx, GenerateInput{
			System: rumorSystemPrompt, Structure: structure, Fields: fields,
			Note: note + "\nYour previous response had these problems:\n- " + strings.Join(problems, "\n- "),
		})
		if err != nil {
			return nil, err
		}
		items, problems = assemble(gen.Values)
		if len(problems) > 0 {
			return nil, fmt.Errorf("%w: the generated rumours failed validation twice: %s", ErrInvalid, strings.Join(problems, "; "))
		}
	}

	/* ---------- the batch ---------- */

	promptRecord := fmt.Sprintf("rumours about %s (%s): %d true, %d false, %d distorted",
		about.Name, about.Kind, in.TrueCount, in.FalseCount, in.DistortedCount)
	if invented > 0 {
		promptRecord += fmt.Sprintf("; %d false invented whole", invented)
	}
	if in.Premise != "" {
		promptRecord += " | premise: " + in.Premise
	}
	batch, err := s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceRumor,
		Prompt: promptRecord, CreatedBy: in.CreatedBy, Items: items,
	})
	if err != nil {
		return nil, err
	}
	holderCount := 0
	for _, sl := range slots {
		holderCount += len(pickHolders(spreadForSlot(sl, in.FalseCount+in.DistortedCount)))
	}
	return &RumorDesignResult{Batch: batch, Facts: facts, Holders: holderCount}, nil
}

// spreadForSlot mirrors the spread the assembler assigned: a mill with a
// broad false side travels further than a local whisper.
func spreadForSlot(sl rumorSlot, falseSide int) string {
	if sl.Kind == "distorted" || falseSide > 2 {
		return "regional"
	}
	return "local"
}

/* ---------- the join ---------- */

// campaignEntityLite is the subject as the prompt and the join read it.
type campaignEntityLite struct {
	ID   string
	Name string
	Kind string
}

// rumorFactRef is one fact the pools carry.
type rumorFactRef struct {
	ID        string
	Statement string
	Subject   string
	Object    string
}

// rumorHolderPool is the distribution join: the NPCs whose position in
// the graph makes them the people who would be saying this — anyone
// located in the area (the subject and every place that contains it, up
// the located_in chain: the monastery's talk is Blackwater's talk), and,
// for a non-location subject, the NPCs who know them or march in their
// factions. Stable order by name; no model anywhere near it.
func rumorHolderPool(snap *Snapshot, about *campaignEntityLite) []string {
	npc := func(id string) bool {
		for _, e := range snap.Entities {
			if e.ID == id {
				return e.Kind == "npc" && e.Status != "deleted"
			}
		}
		return false
	}
	nameOf := func(id string) string {
		for _, e := range snap.Entities {
			if e.ID == id {
				return e.Name
			}
		}
		return id
	}

	locatedIn := map[string][]string{} // entity -> locations it sits in
	knows := map[string][]string{}     // entity -> entities with a knows edge to it
	memberOf := map[string][]string{}  // entity -> factions it belongs to
	for _, r := range snap.Relationships {
		switch r.RelType {
		case "located_in":
			locatedIn[r.FromEntity] = append(locatedIn[r.FromEntity], r.ToEntity)
		case "knows":
			knows[r.ToEntity] = append(knows[r.ToEntity], r.FromEntity)
		case "member_of":
			memberOf[r.FromEntity] = append(memberOf[r.FromEntity], r.ToEntity)
		}
	}

	// The area: the subject plus everywhere that contains it.
	area := map[string]bool{about.ID: true}
	var climb func(id string)
	climb = func(id string) {
		for _, parent := range locatedIn[id] {
			if !area[parent] {
				area[parent] = true
				climb(parent)
			}
		}
	}
	climb(about.ID)

	seen := map[string]bool{}
	add := func(id string) {
		if id != about.ID && npc(id) && !seen[id] {
			seen[id] = true
		}
	}
	for who, locs := range locatedIn {
		for _, l := range locs {
			if area[l] {
				add(who)
				break
			}
		}
	}
	if about.Kind != "location" {
		// Knows the subject: gossip rides acquaintance.
		for _, who := range knows[about.ID] {
			add(who)
		}
		// Fellow members: the faction carries the talk.
		for _, f := range memberOf[about.ID] {
			for who, facs := range memberOf {
				for _, g := range facs {
					if g == f {
						add(who)
					}
				}
			}
		}
	}

	pool := make([]string, 0, len(seen))
	for id := range seen {
		pool = append(pool, id)
	}
	sort.Slice(pool, func(i, j int) bool {
		if nameOf(pool[i]) != nameOf(pool[j]) {
			return nameOf(pool[i]) < nameOf(pool[j])
		}
		return pool[i] < pool[j]
	})
	return pool
}

// rumorSystemPrompt is the one system line the exchange runs under.
const rumorSystemPrompt = `You are Grimoire's rumour mill. You are handed the subject of a town's talk — who or what the rumours circle, the exact mix of true, false and distorted slots, and the real fact each slot plays against. Your job is the wording: how the rumour actually circulates, in one voice a tavern would use. The truth values are already law; write the words that carry them, never a verdict.`
