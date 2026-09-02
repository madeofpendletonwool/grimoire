package canon

// The quest designer (MAD-371, stage 5.2 of MAD-316): a hook becomes a
// branching quest.
//
// This generator does not ask a model for a quest. Before any prompt exists
// the structure is computed, the way internal/encounter's designer computes
// a budget and internal/canon's skeleton generator computes a campaign:
//
//   - campaign.BuildQuestTopology fixes the machine — state count, fork
//     positions, which branches rejoin, the endings — from the kind, the
//     branch count and the depth. The model names states and writes their
//     detail; it does not decide how many branches there are.
//   - The hook is read deterministically for signal — the mode (an
//     investigation or a fight, a betrayal), threat classes, likely giver
//     kinds — the way encounter.ReadIdea reads an encounter idea.
//   - The cast is scored out of the campaign, not invented: the giver, the
//     site and the obstacle are drawn from existing entities, resolved
//     through campaign.ResolveName, so a generated quest attaches to the
//     Duke the campaign already has. Only when nothing fits does the batch
//     propose a new entity, and then it says so.
//   - A branch is exclusive by construction, and that is checked on the
//     built graph (campaign.ForksExclusive) before the batch is staged,
//     not hoped about.
//   - The secret each reveal state surfaces is, preferentially, a secret
//     the campaign already planted and never led anywhere — the engine's
//     own unreachable_secret findings are the candidate pool — because a
//     quest whose states reveal a secret is what makes that secret
//     reachable.
//
// The machine and everything it references are staged as one proposal
// batch behind the review gate, in dependency order through StageBatch's
// depends_on: the entities and facts the quest references are siblings the
// same batch creates. Nothing is written to the graph until DecideBatch
// accepts it.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/* ---------- the deterministic hook read ---------- */

// QuestHookSignals is what a hook deterministically implies: the mode (an
// investigation, a fight, a betrayal), threat classes, likely giver kinds
// and the leftover words to score cast candidates with. The analogue of
// encounter.Hints and PremiseHints for a quest.
type QuestHookSignals struct {
	Mode       string   `json:"mode"`
	Threats    []string `json:"threats,omitempty"`
	GiverKinds []string `json:"giver_kinds,omitempty"`
	Terms      []string `json:"terms,omitempty"`
}

// questModeWords maps hook words onto the coarse mode: what the party will
// mostly be doing.
var questModeWords = map[string]string{
	"missing": campaign.QuestModeInvestigate, "vanished": campaign.QuestModeInvestigate,
	"disappeared": campaign.QuestModeInvestigate, "never": campaign.QuestModeInvestigate,
	"stolen": campaign.QuestModeInvestigate, "theft": campaign.QuestModeInvestigate,
	"murder": campaign.QuestModeInvestigate, "murdered": campaign.QuestModeInvestigate,
	"killed": campaign.QuestModeInvestigate, "who": campaign.QuestModeInvestigate,
	"mystery": campaign.QuestModeInvestigate, "mysterious": campaign.QuestModeInvestigate,
	"strange": campaign.QuestModeInvestigate, "rumour": campaign.QuestModeInvestigate,
	"rumors": campaign.QuestModeInvestigate, "rumours": campaign.QuestModeInvestigate,
	"sabotage": campaign.QuestModeInvestigate, "smuggled": campaign.QuestModeInvestigate,
	"secret": campaign.QuestModeInvestigate, "secrets": campaign.QuestModeInvestigate,
	"clue": campaign.QuestModeInvestigate, "clues": campaign.QuestModeInvestigate,
	"attack": campaign.QuestModeFight, "attacked": campaign.QuestModeFight,
	"ambush": campaign.QuestModeFight, "ambushed": campaign.QuestModeFight,
	"siege": campaign.QuestModeFight, "besieged": campaign.QuestModeFight,
	"army": campaign.QuestModeFight, "horde": campaign.QuestModeFight,
	"war": campaign.QuestModeFight, "invasion": campaign.QuestModeFight,
	"raid": campaign.QuestModeFight, "raided": campaign.QuestModeFight,
	"defend": campaign.QuestModeFight, "assault": campaign.QuestModeFight,
	"traitor": campaign.QuestModeBetray, "traitors": campaign.QuestModeBetray,
	"betrayal": campaign.QuestModeBetray, "betrayed": campaign.QuestModeBetray,
	"turned": campaign.QuestModeBetray, "spy": campaign.QuestModeBetray,
	"informant": campaign.QuestModeBetray, "mole": campaign.QuestModeBetray,
	"double-cross": campaign.QuestModeBetray, "double agent": campaign.QuestModeBetray,
}

// questGiverWords maps hook words onto the entity kind a giver likely is.
var questGiverWords = map[string]string{
	"innkeeper": campaign.KindNPC, "steward": campaign.KindNPC, "mayor": campaign.KindNPC,
	"duke": campaign.KindNPC, "dutchess": campaign.KindNPC, "baron": campaign.KindNPC,
	"captain": campaign.KindNPC, "guard": campaign.KindNPC, "sheriff": campaign.KindNPC,
	"elder": campaign.KindNPC, "merchant": campaign.KindNPC, "child": campaign.KindNPC,
	"widow": campaign.KindNPC, "survivor": campaign.KindNPC, "survivors": campaign.KindNPC,
	"shepherd": campaign.KindNPC, "priest": campaign.KindNPC, "smith": campaign.KindNPC,
	"guild": campaign.KindOrganization, "guilds": campaign.KindOrganization,
	"order": campaign.KindOrganization, "temple": campaign.KindOrganization,
	"church": campaign.KindOrganization, "council": campaign.KindOrganization,
	"company": campaign.KindOrganization, "cartel": campaign.KindOrganization,
}

// questStopwords carry no cast signal.
var questStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"they": true, "them": true, "their": true, "there": true, "here": true,
	"want": true, "like": true, "some": true, "something": true, "anything": true,
	"quest": true, "quests": true, "hook": true, "party": true, "players": true,
	"player": true, "characters": true, "character": true, "campaign": true,
	"game": true, "session": true, "make": true, "build": true, "give": true,
	"need": true, "would": true, "could": true, "really": true, "very": true,
	"just": true, "maybe": true, "should": true, "about": true, "into": true,
	"from": true, "over": true, "than": true, "then": true, "when": true,
	"were": true, "been": true, "have": true, "has": true, "had": true, "was": true,
	"will": true, "must": true, "back": true, "still": true, "only": true,
}

var questWordRE = regexp.MustCompile(`[a-z][a-z'-]+`)

// ReadQuestHook pulls the deterministic signal out of a free-text hook.
func ReadQuestHook(hook string) QuestHookSignals {
	words := questWordRE.FindAllString(strings.ToLower(hook), -1)
	var s QuestHookSignals
	counts := map[string]int{}
	seenThreat := map[string]bool{}
	seenGiver := map[string]bool{}
	seenTerm := map[string]bool{}
	for _, w := range words {
		singular := strings.TrimSuffix(w, "s")
		if m, ok := questModeWords[w]; ok {
			counts[m]++
		} else if m, ok := questModeWords[singular]; ok {
			counts[m]++
		}
		for _, t := range threatWords[w] {
			if !seenThreat[t] {
				seenThreat[t] = true
				s.Threats = append(s.Threats, t)
			}
		}
		if g, ok := questGiverWords[w]; ok {
			if !seenGiver[g] {
				seenGiver[g] = true
				s.GiverKinds = append(s.GiverKinds, g)
			}
		} else if g, ok := questGiverWords[singular]; ok {
			if !seenGiver[g] {
				seenGiver[g] = true
				s.GiverKinds = append(s.GiverKinds, g)
			}
		}
		if len(w) < 4 || questStopwords[w] || seenTerm[w] {
			continue
		}
		seenTerm[w] = true
		s.Terms = append(s.Terms, w)
	}
	best, bestN := "", 0
	for _, mode := range []string{campaign.QuestModeInvestigate, campaign.QuestModeFight, campaign.QuestModeBetray} {
		if counts[mode] > bestN {
			best, bestN = mode, counts[mode]
		}
	}
	s.Mode = best
	sort.Strings(s.Threats)
	sort.Strings(s.GiverKinds)
	return s
}

/* ---------- the cast, scored out of the campaign ---------- */

// questCastCap is how many candidates each slot offers.
const questCastCap = 8

// questSecretCap is how many existing unreachable secrets the reveal pool
// offers.
const questSecretCap = 4

// questRosterCap is how many of the campaign's entities ride along as
// context.
const questRosterCap = 12

// maxQuestHookLen bounds the hook text.
const maxQuestHookLen = 2000

// questCastCandidate is one scored entity for one cast slot.
type questCastCandidate struct {
	ID     string
	Name   string
	Kind   string
	Score  int
	Reason string
}

// questCastSlot is one role the quest needs filled: which entity kinds fit,
// how candidates were scored, and what a new entity would be.
type questCastSlot struct {
	Role        string
	Label       string
	Kinds       map[string]bool
	NewKind     string
	Candidates  []questCastCandidate
	Description string
}

// scoreCast ranks the campaign's entities for one slot: kind fit first,
// then the hook's terms against name and summary, then the signal words
// (giver kinds for the giver, threat classes for the obstacle).
func scoreCast(entities []campaign.Entity, slot questCastSlot, signals QuestHookSignals) []questCastCandidate {
	var out []questCastCandidate
	for _, e := range entities {
		if e.Status == campaign.StatusDeleted || e.Kind == campaign.KindPC {
			continue
		}
		base := 0
		if slot.Kinds[e.Kind] {
			base = 2
		}
		if base == 0 {
			continue // a giver is not a location; the filter is the point
		}
		score := base
		reason := "kind fits"
		lowName := strings.ToLower(e.Name)
		lowSummary := strings.ToLower(e.Summary)
		for _, t := range signals.Terms {
			if strings.Contains(lowName, t) {
				score += 3
				reason = "named by the hook"
				break
			}
		}
		for _, t := range signals.Terms {
			if strings.Contains(lowSummary, t) {
				score += 1
				if reason == "kind fits" {
					reason = "described by the hook"
				}
				break
			}
		}
		switch slot.Role {
		case "giver":
			for _, g := range signals.GiverKinds {
				if g == e.Kind {
					score += 2
					reason = "the kind of giver the hook names"
				}
			}
		case "obstacle":
			for _, th := range signals.Threats {
				if strings.Contains(lowSummary, strings.Split(th, " ")[0]) || strings.Contains(lowName, strings.Split(th, " ")[0]) {
					score += 2
					reason = "carries the hook's threat"
				}
			}
		}
		out = append(out, questCastCandidate{ID: e.ID, Name: e.Name, Kind: e.Kind, Score: score, Reason: reason})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > questCastCap {
		out = out[:questCastCap]
	}
	return out
}

// questCastSlots builds the three slots the hook's quest needs filled.
func questCastSlots(entities []campaign.Entity, signals QuestHookSignals) map[string]*questCastSlot {
	giver := &questCastSlot{
		Role: "giver", Label: "The giver",
		Kinds:   map[string]bool{campaign.KindNPC: true, campaign.KindOrganization: true},
		NewKind: campaign.KindNPC,
		Description: "Who brings the quest to the party and wants it solved — an NPC or an organization " +
			"the campaign already has, ideally.",
	}
	site := &questCastSlot{
		Role: "site", Label: "The site",
		Kinds:       map[string]bool{campaign.KindLocation: true},
		NewKind:     campaign.KindLocation,
		Description: "Where the quest happens — a place the campaign already has, ideally.",
	}
	obstacle := &questCastSlot{
		Role: "obstacle", Label: "The obstacle",
		Kinds: map[string]bool{campaign.KindNPC: true, campaign.KindFaction: true,
			campaign.KindCreature: true, campaign.KindOrganization: true},
		NewKind:     campaign.KindNPC,
		Description: "What stands in the way — a person, faction or creature the campaign already has, ideally.",
	}
	for _, s := range []*questCastSlot{giver, site, obstacle} {
		s.Candidates = scoreCast(entities, *s, signals)
	}
	return map[string]*questCastSlot{"giver": giver, "site": site, "obstacle": obstacle}
}

/* ---------- the input and the result ---------- */

// QuestDesignInput is one design request: the hook text, an optional shape
// kind, branch count, depth and anchor entity.
type QuestDesignInput struct {
	CampaignID   string
	Hook         string
	Kind         string
	BranchPoints int
	Depth        int
	Anchor       string
	CreatedBy    string
}

// QuestDesignResult is what one generator run produced: the staged batch,
// the shape and topology it filled, the assembled machine (a preview — the
// graph only changes when the batch is accepted), and the entities reused
// rather than duplicated.
type QuestDesignResult struct {
	Batch     *Batch                 `json:"batch,omitempty"`
	Shape     campaign.QuestShape    `json:"shape"`
	Topology  campaign.QuestTopology `json:"topology"`
	Machine   campaign.StateMachine  `json:"machine"`
	Reused    []ReusedEntity         `json:"reused,omitempty"`
	Generated time.Time              `json:"generated_at"`
}

// GenerateQuest runs one design exchange: compute the topology, one
// structured-generation call to fill it, validate the machine and — when
// validation fails — exactly one repair retry with the failures appended,
// then stage the quest, its cast and its secrets as a proposal batch.
// Nothing is written to the graph.
func (s *Store) GenerateQuest(ctx context.Context, in QuestDesignInput) (*QuestDesignResult, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	in.Hook = strings.TrimSpace(in.Hook)
	if in.Hook == "" {
		return nil, fmt.Errorf("%w: a quest needs a hook", ErrInvalid)
	}
	if len([]rune(in.Hook)) > maxQuestHookLen {
		return nil, fmt.Errorf("%w: the hook is longer than %d characters", ErrInvalid, maxQuestHookLen)
	}
	signals := ReadQuestHook(in.Hook)

	kind := in.Kind
	if kind == "" {
		kind = campaign.DefaultQuestKindFor(signals.Mode)
	}
	shape, ok := campaign.QuestShapeFor(kind)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a quest shape — the shapes are investigation, retrieval, escort, siege, mystery and betrayal", ErrInvalid, kind)
	}
	branchPoints := in.BranchPoints
	if branchPoints == 0 {
		branchPoints = 2
	}
	depth := in.Depth
	if depth == 0 {
		depth = 4
	}
	topology, err := campaign.BuildQuestTopology(kind, branchPoints, depth)
	if err != nil {
		return nil, err
	}

	if _, err := s.loadCampaign(ctx, in.CampaignID); err != nil {
		return nil, err
	}
	snap, err := LoadSnapshot(ctx, s.db, in.CampaignID)
	if err != nil {
		return nil, err
	}

	// The anchor, when given, must be a live entity of this campaign; the
	// quest's subject is then anchored to it rather than invented.
	var anchor *campaign.Entity
	if in.Anchor != "" {
		for i := range snap.Entities {
			if snap.Entities[i].ID == in.Anchor && snap.Entities[i].Status != campaign.StatusDeleted {
				anchor = &snap.Entities[i]
				break
			}
		}
		if anchor == nil {
			return nil, fmt.Errorf("%w: anchor entity %s", ErrNotFound, in.Anchor)
		}
	}

	cast := questCastSlots(snap.Entities, signals)

	// The reveal pool: secrets the campaign planted and never led anywhere,
	// by the engine's own unreachable_secret rule. A quest whose states
	// reveal one is what makes it reachable — so the model is offered these
	// first and new statements second.
	revealable := map[string]string{} // statement -> fact id
	var revealOrder []string
	for _, f := range checkUnreachableSecret(snap) {
		for _, fact := range snap.Facts {
			if fact.ID != f.RecordID {
				continue
			}
			stmt := strings.TrimSpace(fact.Statement)
			if stmt == "" || len([]rune(stmt)) > 280 {
				continue
			}
			if _, seen := revealable[stmt]; !seen {
				revealable[stmt] = fact.ID
				revealOrder = append(revealOrder, stmt)
			}
		}
	}
	if len(revealOrder) > questSecretCap {
		revealOrder = revealOrder[:questSecretCap]
	}

	fields, structure := questDesignPrompt(questPromptIn{
		hook: in.Hook, signals: signals, shape: shape, topology: topology,
		cast: cast, anchor: anchor, revealable: revealOrder, entities: snap.Entities,
	})

	note := "Fill every declared field. The structure block is the quest you are designing — its state count, forks and endings are already law; the state keys come from the names you give. " +
		"Reuse the existing entities and the existing secrets where they fit: a quest generated into a campaign attaches to the people and places it already has."

	fill := func(note string) (*Generated, error) {
		return s.Generate(ctx, GenerateInput{
			System: questSystemPrompt, Structure: structure, Fields: fields, Note: note,
		})
	}
	gen, err := fill(note)
	if err != nil {
		return nil, err
	}

	// Assemble and validate; on failure, exactly one repair retry with the
	// failures appended — the generate.go contract, one retry, no second
	// mechanism.
	assembled := assembleQuestDesign(topology, cast, revealable, gen.Values)
	if len(assembled.problems) > 0 {
		problems := assembled.problems
		gen, err = fill(note + "\nYour previous response had these problems:\n- " + strings.Join(problems, "\n- "))
		if err != nil {
			return nil, err
		}
		assembled = assembleQuestDesign(topology, cast, revealable, gen.Values)
		if len(assembled.problems) > 0 {
			return nil, fmt.Errorf("%w: the generated quest failed validation twice: %s",
				ErrInvalid, strings.Join(assembled.problems, "; "))
		}
	}

	// The reuse read-back comes first, before anything is staged: a cast
	// name the model invented that resolves against the campaign graph
	// (through ResolveName, aliases included) is reused, not duplicated —
	// entity_merge_candidate exists because skipping this goes wrong.
	var reused []ReusedEntity
	for _, role := range []string{"giver", "site", "obstacle"} {
		pick := assembled.cast[role]
		if pick.existingID != "" {
			// A pool pick is reuse too: report it so the DM sees which
			// campaign entities the quest attached to.
			for _, c := range cast[role].Candidates {
				if c.ID == pick.existingID {
					reused = append(reused, ReusedEntity{Input: c.Name, ID: c.ID, Name: c.Name, Kind: c.Kind})
					break
				}
			}
			continue
		}
		if pick.name == "" {
			continue
		}
		hits, err := s.campaigns.ResolveName(ctx, campaign.ScopeDM, in.CampaignID, pick.name)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			pick.existingID = hits[0].ID
			assembled.cast[role] = pick
			reused = append(reused, ReusedEntity{Input: pick.name, ID: hits[0].ID, Name: hits[0].Name, Kind: hits[0].Kind})
		}
	}

	// The batch: new cast entities first, then the new secret facts, then
	// the quest itself — depends_on carries the dependency order because
	// the quest references entities and facts this same batch creates.
	var items []BatchItemInput
	var questDeps []string

	entityRef := func(role string) string { // how the quest item references the slot's entity
		pick := assembled.cast[role]
		if pick.existingID != "" {
			return pick.existingID
		}
		if pick.name == "" {
			return ""
		}
		return "cast-" + role
	}
	for _, role := range []string{"giver", "site", "obstacle"} {
		pick := assembled.cast[role]
		if pick.existingID != "" || pick.name == "" {
			continue
		}
		id := "cast-" + role
		items = append(items, BatchItemInput{
			ID: id, Kind: "entity", Subject: pick.name, Summary: pick.summary,
			Payload: map[string]any{
				"local_id": id, "kind": cast[role].NewKind,
				"name": pick.name, "summary": pick.summary,
			},
		})
		questDeps = append(questDeps, id)
	}

	revealSlots := topology.RevealSlots()
	var stateFacts []map[string]any
	for _, slot := range revealSlots {
		pick := assembled.secrets[slot.ID]
		if pick.statement == "" {
			continue
		}
		key := assembled.keyOf(slot.ID)
		if pick.existingID != "" {
			stateFacts = append(stateFacts, map[string]any{
				"state": key, "fact": pick.existingID, "disposition": campaign.QuestFactReveals,
			})
			continue
		}
		id := "secret-" + slot.ID
		items = append(items, BatchItemInput{
			ID: id, Kind: "fact", Subject: "The secret " + assembled.labelOf(slot.ID) + " reveals",
			Summary: pick.statement,
			Payload: map[string]any{
				"local_id": id, "statement": pick.statement,
				"subject": entityRef("obstacle"), "predicate": "hides",
				"object_literal": in.Hook, "visibility": campaign.VisibilitySecret,
			},
			DependsOn: depsForRef(entityRef("obstacle"), questDeps),
		})
		stateFacts = append(stateFacts, map[string]any{
			"state": key, "fact": id, "disposition": campaign.QuestFactReveals,
		})
		questDeps = append(questDeps, id)
	}

	questPayload := map[string]any{
		"name": assembled.name, "summary": assembled.summary,
		"machine": assembled.machine,
		"entities": func() []map[string]any {
			var out []map[string]any
			for _, role := range []string{"giver", "site", "obstacle"} {
				if ref := entityRef(role); ref != "" {
					out = append(out, map[string]any{"entity": ref, "role": role})
				}
			}
			if anchor != nil {
				out = append(out, map[string]any{"entity": anchor.ID, "role": campaign.QuestRoleSubject})
			}
			return out
		}(),
		"state_facts": stateFacts,
	}
	items = append(items, BatchItemInput{
		ID: "quest", Kind: "quest", Subject: assembled.name, Summary: assembled.summary,
		Payload: questPayload, DependsOn: questDeps,
	})

	promptRecord := fmt.Sprintf("hook: %s | kind: %s | branch points: %d, depth: %d | signals: mode %q, threats [%s] | cast: giver %q, site %q, obstacle %q | reveals: %d existing secrets",
		in.Hook, kind, topology.BranchPoints, topology.Depth, signals.Mode,
		strings.Join(signals.Threats, ", "),
		assembled.cast["giver"].name, assembled.cast["site"].name, assembled.cast["obstacle"].name,
		countExistingSecrets(assembled.secrets))
	if anchor != nil {
		promptRecord += " | anchor: " + anchor.Name
	}
	if len(reused) > 0 {
		var names []string
		for _, r := range reused {
			names = append(names, r.Name)
		}
		promptRecord += " | reused: " + strings.Join(names, ", ")
	}

	batch, err := s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceQuest,
		Prompt: promptRecord, CreatedBy: in.CreatedBy, Items: items,
	})
	if err != nil {
		return nil, err
	}
	return &QuestDesignResult{
		Batch: batch, Shape: shape, Topology: topology, Machine: assembled.machine,
		Reused: reused, Generated: s.now(),
	}, nil
}

// depsForRef returns the batch-item dependency for a reference when the
// reference names a sibling this batch creates, nil when it names something
// that already exists.
func depsForRef(ref string, newIDs []string) []string {
	for _, id := range newIDs {
		if ref == id {
			return []string{id}
		}
	}
	return nil
}

func countExistingSecrets(secrets map[string]questSecretPick) int {
	n := 0
	for _, p := range secrets {
		if p.existingID != "" {
			n++
		}
	}
	return n
}

/* ---------- the prompt ---------- */

// questPromptIn carries everything the prompt builder needs.
type questPromptIn struct {
	hook       string
	signals    QuestHookSignals
	shape      campaign.QuestShape
	topology   campaign.QuestTopology
	cast       map[string]*questCastSlot
	anchor     *campaign.Entity
	revealable []string
	entities   []campaign.Entity
}

// questDesignPrompt builds the declared schema and the structure block.
// Field count follows the computed topology — the model cannot return a
// machine of a different shape than the one asked for.
func questDesignPrompt(in questPromptIn) ([]FieldSpec, map[string]any) {
	newLabel := func(role string) string {
		return fmt.Sprintf(`Legal values: an existing candidate's exact name, or "new" when nothing fits — then fill %s_new_name and %s_new_summary.`, role, role)
	}
	fields := []FieldSpec{
		{Key: "quest_name", Required: true, MaxLen: 80,
			Desc: "The quest's name, the way it will read on the quest board."},
		{Key: "quest_summary", Required: true, MaxLen: 400,
			Desc: "Two or three sentences: what the party must do and what it costs them."},
	}
	for _, role := range []string{"giver", "site", "obstacle"} {
		slot := in.cast[role]
		var pool []string
		for _, c := range slot.Candidates {
			pool = append(pool, c.Name)
		}
		pool = append(pool, "new")
		fields = append(fields,
			FieldSpec{Key: role, Required: true, MaxLen: 120, Pool: pool,
				Desc: fmt.Sprintf("%s. %s", slot.Description, newLabel(role))},
			FieldSpec{Key: role + "_new_name", Required: false, MaxLen: 60,
				Desc: fmt.Sprintf("Name for a new %s, only when %s is \"new\".", slot.NewKind, role)},
			FieldSpec{Key: role + "_new_summary", Required: false, MaxLen: 280,
				Desc: fmt.Sprintf("Two sentences on the new %s, only when %s is \"new\".", slot.NewKind, role)},
		)
	}
	for i := range in.topology.States {
		slot := in.topology.States[i]
		nameDesc := fmt.Sprintf("Name of state %q (%s): %s The name becomes the state's key in the machine.", slot.ID, slot.Role, slot.Brief)
		if slot.Role == campaign.QuestSlotEnding {
			nameDesc = fmt.Sprintf("Name of the ending %q: %s", slot.ID, slot.Brief)
		}
		fields = append(fields, FieldSpec{Key: "state_" + slot.ID + "_name", Required: true, MaxLen: 60, Desc: nameDesc})
		if slot.Role != campaign.QuestSlotEnding {
			fields = append(fields, FieldSpec{Key: "state_" + slot.ID + "_detail", Required: true, MaxLen: 400,
				Desc: fmt.Sprintf("What happens at %q, two sentences a DM can run.", slot.ID)})
		}
		if slot.Reveal {
			pool := append([]string{}, in.revealable...)
			pool = append(pool, "new")
			fields = append(fields,
				FieldSpec{Key: "secret_" + slot.ID, Required: true, MaxLen: 300, Pool: pool,
					Desc: fmt.Sprintf("The secret state %q reveals: an existing secret the campaign planted (reuse it, exact statement) or \"new\".", slot.ID)},
				FieldSpec{Key: "secret_" + slot.ID + "_new", Required: false, MaxLen: 300,
					Desc: fmt.Sprintf("The new secret's one-sentence statement, only when secret_%s is \"new\".", slot.ID)},
			)
		}
	}

	structure := map[string]any{
		"hook":    in.hook,
		"signals": in.signals,
		"shape":   in.shape,
		"topology": map[string]any{
			"kind": in.topology.Kind, "initial": in.topology.Initial,
			"branch_points": in.topology.BranchPoints, "depth": in.topology.Depth,
			"states": in.topology.States, "edges": in.topology.Edges,
		},
	}
	var castView []map[string]any
	for _, role := range []string{"giver", "site", "obstacle"} {
		slot := in.cast[role]
		v := map[string]any{"role": role, "label": slot.Label, "description": slot.Description, "new_kind": slot.NewKind}
		var cands []map[string]any
		for _, c := range slot.Candidates {
			cands = append(cands, map[string]any{"name": c.Name, "kind": c.Kind, "why": c.Reason})
		}
		v["candidates"] = cands
		castView = append(castView, v)
	}
	structure["cast"] = castView
	structure["revealable_secrets"] = in.revealable
	if in.anchor != nil {
		structure["anchor"] = map[string]any{"name": in.anchor.Name, "kind": in.anchor.Kind, "summary": in.anchor.Summary}
	}
	var roster []skeletonExisting
	for _, e := range in.entities {
		if e.Status == campaign.StatusDeleted || e.Kind == campaign.KindPC {
			continue
		}
		roster = append(roster, skeletonExisting{Name: e.Name, Kind: e.Kind, Summary: e.Summary})
		if len(roster) >= questRosterCap {
			break
		}
	}
	structure["existing_entities"] = roster
	return fields, structure
}

// questSystemPrompt is the designer's system prompt.
const questSystemPrompt = `You are Grimoire's quest designer. You are handed the complete structure of a quest — computed by the server and authoritative: every state, which states fork, which branches rejoin, the endings, the cast candidates scored out of the campaign, and the secrets the campaign planted but never led anywhere — plus the DM's hook. Your job is to fill that structure with names, prose and specificity, nothing more.

STRICT RULES
1. Every field is a plain string. Fill every one. No lists, no markdown, no commentary.
2. The structure is fixed. Do not add, merge or rename states beyond naming them; a fork has exactly the two arms the topology declares, and the endings are already decided.
3. Each state name becomes a machine key (lowercased, dashed): two states whose names slug to the same key are the same state — name every state distinctly.
4. The cast: pick an existing candidate by its exact name whenever one fits. Only answer "new" when nothing does, and then write the new entity's name and summary. Never invent a second version of someone the campaign already has.
5. The secrets: prefer an existing revealable secret, quoted exactly — a quest that reveals it is the clue path it was waiting for. Answer "new" only when none fits.
6. The branches out of one fork are exclusive outcomes of one choice: name them as the choice each represents ("trust the survivor", "accuse the survivor").
7. Honour the DM's hook: its subject, its tone, its specifics. Where it names people or places, use them.`

/* ---------- assembly: values into a machine ---------- */

// questCastPick is one cast slot after the model filled it: either an
// existing entity id, or the new entity's name and summary.
type questCastPick struct {
	existingID string
	name       string
	summary    string
}

// questSecretPick is one reveal slot after the model filled it: either an
// existing fact id or the new statement.
type questSecretPick struct {
	existingID string
	statement  string
}

// questAssembled is a design after assembly: the machine, the quest's
// headline, the cast and secret picks, the internal-id-to-key map, and the
// problems that were found (empty when the fill is valid).
type questAssembled struct {
	machine   campaign.StateMachine
	name      string
	summary   string
	cast      map[string]questCastPick
	secrets   map[string]questSecretPick
	keyByID   map[string]string
	labelByID map[string]string
	problems  []string
}

func (a *questAssembled) keyOf(slotID string) string   { return a.keyByID[slotID] }
func (a *questAssembled) labelOf(slotID string) string { return a.labelByID[slotID] }

// assembleQuestDesign turns validated values into the machine and the cast
// picks, running every check a staged machine must pass: the key rule
// (every state named, no two slugging to one key), StateMachine.Validate,
// fork exclusivity, and the MAD-369 quest checks over the assembled graph.
func assembleQuestDesign(topology campaign.QuestTopology, cast map[string]*questCastSlot, revealable map[string]string, values map[string]any) *questAssembled {
	a := &questAssembled{
		cast: map[string]questCastPick{}, secrets: map[string]questSecretPick{},
		keyByID: map[string]string{}, labelByID: map[string]string{},
	}
	val := func(key string) string {
		sv, _ := values[key].(string)
		return strings.TrimSpace(sv)
	}
	a.name = val("quest_name")
	a.summary = val("quest_summary")

	// The cast.
	for _, role := range []string{"giver", "site", "obstacle"} {
		pick := val(role)
		byName := ""
		for _, c := range cast[role].Candidates {
			if c.Name == pick {
				byName = c.ID
				break
			}
		}
		switch {
		case byName != "":
			a.cast[role] = questCastPick{existingID: byName, name: pick}
		case pick == "new" || pick == "":
			name, summary := val(role+"_new_name"), val(role+"_new_summary")
			if pick == "new" && name == "" {
				a.problems = append(a.problems, fmt.Sprintf("%s is \"new\" but %s_new_name is missing", role, role))
			}
			a.cast[role] = questCastPick{name: name, summary: summary}
		default:
			// The pool validation should have caught this; fail loudly
			// rather than inventing an entity the model never described.
			a.problems = append(a.problems, fmt.Sprintf("%s %q is neither an existing candidate nor \"new\"", role, pick))
			a.cast[role] = questCastPick{name: pick}
		}
	}

	// The machine: topology over the model's names. The initial state's
	// key is the slug of the name the model gave the initial slot.
	m := campaign.StateMachine{}
	seenKeys := map[string]string{}
	for _, slot := range topology.States {
		label := val("state_" + slot.ID + "_name")
		if label == "" {
			a.problems = append(a.problems, fmt.Sprintf("state_%s_name is missing", slot.ID))
			continue
		}
		key := campaign.QuestStateKey(label)
		if key == "" {
			a.problems = append(a.problems, fmt.Sprintf("state %s's name %q produces an empty key; name it with letters", slot.ID, label))
			continue
		}
		if prev, dup := seenKeys[key]; dup {
			a.problems = append(a.problems, fmt.Sprintf("states %s and %s both produce the key %q; name every state distinctly", prev, slot.ID, key))
			continue
		}
		seenKeys[key] = slot.ID
		a.keyByID[slot.ID] = key
		a.labelByID[slot.ID] = label
		detail := val("state_" + slot.ID + "_detail")
		if detail == "" && slot.Role != campaign.QuestSlotEnding {
			a.problems = append(a.problems, fmt.Sprintf("state_%s_detail is missing", slot.ID))
		}
		m.States = append(m.States, campaign.State{Key: key, Label: label, Detail: detail, Terminal: slot.Terminal})
	}
	for _, e := range topology.Edges {
		from, fromOK := a.keyByID[e.From]
		to, toOK := a.keyByID[e.To]
		if !fromOK || !toOK {
			continue // the missing state already reported a problem
		}
		m.Edges = append(m.Edges, campaign.StateEdge{From: from, To: to, Label: a.labelByID[e.To]})
	}
	if initial, ok := a.keyByID[topology.Initial]; ok {
		m.Initial = initial
	} else {
		a.problems = append(a.problems, "the initial state did not key; it cannot be missing")
	}
	a.machine = m

	// The secrets.
	for _, slot := range topology.RevealSlots() {
		pick := val("secret_" + slot.ID)
		if id, ok := revealable[pick]; ok && pick != "" {
			a.secrets[slot.ID] = questSecretPick{existingID: id, statement: pick}
			continue
		}
		stmt := val("secret_" + slot.ID + "_new")
		if pick == "new" && stmt == "" {
			a.problems = append(a.problems, fmt.Sprintf("secret_%s is \"new\" but secret_%s_new is missing", slot.ID, slot.ID))
		}
		if pick != "" && pick != "new" {
			a.problems = append(a.problems, fmt.Sprintf("secret_%s %q is neither an existing secret nor \"new\"", slot.ID, pick))
		}
		a.secrets[slot.ID] = questSecretPick{statement: stmt}
	}

	// The machine's own checks, only meaningful once every state keyed.
	if len(a.problems) == 0 {
		if err := m.Validate(); err != nil {
			a.problems = append(a.problems, err.Error())
		}
		a.problems = append(a.problems, campaign.ForksExclusive(m)...)
		a.problems = append(a.problems, questMachineFindings(m)...)
	}
	return a
}

// questMachineFindings runs the MAD-369 quest checks over an assembled
// machine the way the engine will once the quest lands.
func questMachineFindings(m campaign.StateMachine) []string {
	snap := campaign.Snapshot{Quests: []campaign.Quest{{
		ID: "quest-preview", Name: "preview", Status: campaign.QuestActive,
		Machine: m, CurrentState: m.Initial,
	}}}
	var out []string
	for _, f := range campaign.Check(&snap) {
		switch f.Check {
		case campaign.CheckQuestStateUnreachable, campaign.CheckQuestDeadEnd, campaign.CheckQuestNoEnding:
			out = append(out, f.Message)
		}
	}
	return out
}

/* ---------- branch this quest ---------- */

// QuestBranchInput is one branch request: the quest, the state the DM wants
// two exclusive outcomes off, and optional notes on the direction.
type QuestBranchInput struct {
	CampaignID string
	QuestID    string
	StateKey   string
	Notes      string
	CreatedBy  string
}

// QuestBranchResult is the branch proposal: the batch carrying the machine
// edit, and the merged machine (a preview — the quest's machine only
// changes when the batch is accepted).
type QuestBranchResult struct {
	Batch     *Batch                `json:"batch,omitempty"`
	Machine   campaign.StateMachine `json:"machine"`
	StateKey  string                `json:"state_key"`
	Generated time.Time             `json:"generated_at"`
}

// GenerateQuestBranch takes an existing state of an existing quest and
// proposes two exclusive outcomes off it: two new branch states, each
// running to its own ending. Everything the machine already declares —
// every state, every edge, every recorded transition — is respected;
// applying the edit goes through UpdateQuest, which refuses to orphan
// history.
func (s *Store) GenerateQuestBranch(ctx context.Context, in QuestBranchInput) (*QuestBranchResult, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	q, err := s.campaigns.GetQuest(ctx, campaign.ScopeDM, in.CampaignID, in.QuestID)
	if err != nil {
		return nil, err
	}
	in.StateKey = strings.TrimSpace(in.StateKey)
	st, ok := q.Machine.State(in.StateKey)
	if !ok {
		return nil, fmt.Errorf("%w: state %q is not a declared state of quest %q", ErrInvalid, in.StateKey, q.Name)
	}
	if st.Terminal != campaign.TerminalNone {
		return nil, fmt.Errorf("%w: state %q is an ending (%s); branch a state the quest can still leave", ErrInvalid, in.StateKey, st.Terminal)
	}
	if len(in.Notes) > maxQuestHookLen {
		return nil, fmt.Errorf("%w: the notes are longer than %d characters", ErrInvalid, maxQuestHookLen)
	}

	fields := []FieldSpec{
		{Key: "branch_a_name", Required: true, MaxLen: 60,
			Desc: "Name of the first outcome off the chosen state — the choice it represents (e.g. \"trust the survivor\"). It becomes a state key; name it distinctly from every existing state."},
		{Key: "branch_a_detail", Required: true, MaxLen: 400,
			Desc: "What happens on the first outcome's branch, two sentences a DM can run."},
		{Key: "branch_a_ending", Required: true, MaxLen: 60,
			Desc: "Name of the first outcome's ending (a success: the quest pays off this way)."},
		{Key: "branch_b_name", Required: true, MaxLen: 60,
			Desc: "Name of the second outcome off the chosen state — exclusive with the first."},
		{Key: "branch_b_detail", Required: true, MaxLen: 400,
			Desc: "What happens on the second outcome's branch, two sentences a DM can run."},
		{Key: "branch_b_ending", Required: true, MaxLen: 60,
			Desc: "Name of the second outcome's ending (a failure: the quest is lost this way)."},
	}
	structure := map[string]any{
		"quest": map[string]any{
			"name": q.Name, "summary": q.Summary, "status": q.Status,
			"current_state": q.CurrentState,
		},
		"chosen_state": st,
		"machine":      q.Machine,
		"dm_notes":     in.Notes,
		"existing_state_keys": func() []string {
			var out []string
			for _, s := range q.Machine.States {
				out = append(out, s.Key)
			}
			return out
		}(),
	}
	note := "Fill every declared field. The two outcomes must be genuinely exclusive — the structure's machine is law; you are adding two branches and two endings off the chosen state, changing nothing else."

	fill := func(note string) (*Generated, error) {
		return s.Generate(ctx, GenerateInput{
			System: questBranchSystemPrompt, Structure: structure, Fields: fields, Note: note,
		})
	}
	gen, err := fill(note)
	if err != nil {
		return nil, err
	}

	oldFindings := questMachineFindingsFull(q.Machine)
	assemble := func(values map[string]any) (campaign.StateMachine, []string) {
		return assembleQuestBranch(q.Machine, in.StateKey, values)
	}
	machine, problems := assemble(gen.Values)
	if len(problems) > 0 {
		gen, err = fill(note + "\nYour previous response had these problems:\n- " + strings.Join(problems, "\n- "))
		if err != nil {
			return nil, err
		}
		machine, problems = assemble(gen.Values)
		if len(problems) > 0 {
			return nil, fmt.Errorf("%w: the branch failed validation twice: %s", ErrInvalid, strings.Join(problems, "; "))
		}
	}
	// The branch only adds; a finding the quest already carried (an
	// unreachable state, a dead end) is the DM's to chase, not this
	// proposal's fault — new findings are.
	for _, f := range questMachineFindings(machine) {
		if !oldFindings[f] {
			return nil, fmt.Errorf("%w: the branched machine introduces a new integrity finding: %s", ErrInvalid, f)
		}
	}

	payload := map[string]any{
		"quest_id": q.ID, "name": q.Name, "summary": q.Summary, "machine": machine,
	}
	promptRecord := fmt.Sprintf("branch quest %q (%s) at state %q", q.Name, q.ID, in.StateKey)
	if in.Notes != "" {
		promptRecord += " | notes: " + in.Notes
	}
	batch, err := s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceQuest,
		Prompt: promptRecord, CreatedBy: in.CreatedBy,
		Items: []BatchItemInput{{
			ID: "branch", Kind: "quest",
			Subject: fmt.Sprintf("Branch %s at %q", q.Name, st.Label),
			Summary: fmt.Sprintf("Two exclusive outcomes off %q, each running to its own ending.", st.Label),
			Payload: payload,
		}},
	})
	if err != nil {
		return nil, err
	}
	return &QuestBranchResult{Batch: batch, Machine: machine, StateKey: in.StateKey, Generated: s.now()}, nil
}

// questMachineFindingsFull is questMachineFindings keyed for comparison:
// the finding messages a machine already carries, so a branch proposal can
// be held to "introduces nothing new".
func questMachineFindingsFull(m campaign.StateMachine) map[string]bool {
	out := map[string]bool{}
	for _, p := range questMachineFindings(m) {
		out[p] = true
	}
	return out
}

// assembleQuestBranch merges the model's two outcomes into the quest's
// machine: the chosen state gains a fork, each arm runs to its own ending.
// Everything else is kept verbatim; the endings' keys come from their
// names, so a collision with an existing key is a problem the model
// repairs.
func assembleQuestBranch(m campaign.StateMachine, stateKey string, values map[string]any) (campaign.StateMachine, []string) {
	var problems []string
	val := func(key string) string {
		sv, _ := values[key].(string)
		return strings.TrimSpace(sv)
	}
	out := m
	keyFor := func(name, what string) (string, bool) {
		key := campaign.QuestStateKey(name)
		if key == "" {
			problems = append(problems, fmt.Sprintf("%s name %q produces an empty key; name it with letters", what, name))
			return "", false
		}
		if _, exists := out.State(key); exists {
			problems = append(problems, fmt.Sprintf("%s name %q slugs to key %q, which the machine already declares; name it distinctly", what, name, key))
			return "", false
		}
		return key, true
	}
	addArm := func(armName, armDetail, endingName, terminal, what string) *campaign.State {
		armKey, ok := keyFor(val(armName), what+" branch")
		if !ok {
			return nil
		}
		endKey, ok := keyFor(val(endingName), what+" ending")
		if !ok {
			return nil
		}
		arm := campaign.State{Key: armKey, Label: val(armName), Detail: val(armDetail)}
		end := campaign.State{Key: endKey, Label: val(endingName), Terminal: terminal}
		out.States = append(out.States, arm, end)
		out.Edges = append(out.Edges,
			campaign.StateEdge{From: stateKey, To: armKey, Label: arm.Label},
			campaign.StateEdge{From: armKey, To: endKey, Label: end.Label},
		)
		return &arm
	}
	var arms []string
	if a := addArm("branch_a_name", "branch_a_detail", "branch_a_ending", campaign.TerminalSuccess, "first"); a != nil {
		arms = append(arms, a.Key)
	}
	if b := addArm("branch_b_name", "branch_b_detail", "branch_b_ending", campaign.TerminalFailure, "second"); b != nil {
		arms = append(arms, b.Key)
	}
	if len(problems) > 0 {
		return m, problems
	}
	if err := out.Validate(); err != nil {
		return m, []string{err.Error()}
	}
	if exclusive := campaign.ForksExclusive(out); len(exclusive) > 0 {
		return m, exclusive
	}
	return out, nil
}

// questBranchSystemPrompt is the branch operation's system prompt.
const questBranchSystemPrompt = `You are Grimoire's quest designer, mid-campaign. The DM picked one state of a live quest and wants two exclusive outcomes off it: two new branch states, each running to its own ending — one a success, one a failure. The machine as it stands is law: every state, edge and recorded move is respected, and you add exactly two branches and two endings, nothing else.

STRICT RULES
1. Every field is a plain string. Fill every one.
2. Each name becomes a machine key (lowercased, dashed): a name that slugs to a key the machine already declares is a collision — name distinctly.
3. The two outcomes must be exclusive — the two honest resolutions of one choice the chosen state poses, not variations of each other.
4. Ground the outcomes in the quest the structure block shows you: its name, its summary, the state's label and detail, where the quest sits now.`
