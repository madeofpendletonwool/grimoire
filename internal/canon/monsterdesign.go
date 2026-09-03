package canon

/*
The monster designer's generator (MAD-382, stage 5.2 of MAD-317).

The loop is the feature, and the loop runs in this order:

  1. The server derives the target envelope from the requested CR before a
     model is asked anything — statblock.EnvelopeFor reads the same DMG
     tables ComputeCR prices against, so the draft is designed inside the
     numbers it will be checked by, not near them.
  2. The model designs inside the envelope through the structured-generation
     harness: flat declared fields, action prose in the exact SRD shapes the
     deterministic parser reads, legendary actions only within the budget
     the calculator prices (three points a round).
  3. ComputeCR runs on the assembled draft. Shortfall names the specific
     miss — which half is off, by how much, what raw number closes it — and
     one revision pass carries that wording back to the model. After that
     one retry the draft stands: surfaced with its disagreement shown,
     never silently retuned, never rejected for the miss the DM can see.

With a campaign named, the structure carries the campaign's own material —
factions, NPCs, places — so a brief like "a faction's soldiers" or "the cult
from the rumours" is written against the graph rather than from nothing.

Placement is a separate, model-free operation (PlaceMonster): the saved
monster becomes a creature entity through the proposal batch, and nothing
touches the graph until the batch is decided.
*/

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

/* ---------- the input and the result ---------- */

// MonsterDesignInput is one design request: the brief, the CR the DM asked
// for (printed form — "7", "1/4"), and whether the creature may spend the
// legendary-action budget. CampaignID is optional; a named campaign lends
// the brief its own material.
type MonsterDesignInput struct {
	CampaignID string
	Brief      string
	CR         string
	Legendary  bool
	CreatedBy  string
}

// MonsterDesignResult is the draft the DM is shown: the statblock as
// assembled, the designer's prose, and — always — the calculator's verdict.
// Shortfall empty means the maths agrees with the ask; non-empty means the
// disagreement is shown wherever the draft appears.
type MonsterDesignResult struct {
	Statblock statblock.Statblock `json:"statblock"`
	Tactics   string              `json:"tactics,omitempty"`
	Lore      string              `json:"lore,omitempty"`
	Role      string              `json:"encounter_role,omitempty"`

	// Envelope is the target band the draft was designed inside.
	Envelope statblock.Envelope `json:"envelope"`
	// Rating is ComputeCR's whole verdict on the assembled draft.
	Rating statblock.Rating `json:"computed_detail"`
	// Shortfall is the specific disagreement with the requested CR, in the
	// calculator's words. Empty means none.
	Shortfall []string `json:"shortfall,omitempty"`
	// Revised reports that the CR revision pass ran (whatever it landed on).
	Revised bool `json:"revised"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

/* ---------- the vocabularies the schema declares ---------- */

// The SRD's own words, the only ones a designed creature may carry.
var monsterSizes = []string{"Tiny", "Small", "Medium", "Large", "Huge", "Gargantuan"}

var monsterTypes = []string{
	"aberration", "beast", "celestial", "construct", "dragon", "elemental",
	"fey", "fiend", "giant", "humanoid", "monstrosity", "ooze", "plant", "undead",
}

// designerSlots is how many of each prose slot the schema declares. The
// first action is required (a creature with no way to hurt anyone has no
// CR to check); the rest are the model's choice to fill or omit.
const (
	designerTraits    = 3
	designerActions   = 4
	designerLegendary = 3
)

// ptrf is the schema's float-pointer shorthand for numeric bounds.
func ptrf(v float64) *float64 { return &v }

/* ---------- the generator ---------- */

// GenerateMonster designs one creature from a brief. It does not save
// anything: the draft (with its computed CR and the calculator's
// reasoning) is the caller's to keep or discard, and the homebrew store
// recomputes the CR on save regardless — the label is always this
// server's arithmetic.
func (s *Store) GenerateMonster(ctx context.Context, in MonsterDesignInput) (*MonsterDesignResult, error) {
	if s.model == nil {
		return nil, errOffline
	}
	brief := strings.TrimSpace(in.Brief)
	if brief == "" {
		return nil, fmt.Errorf("%w: a monster needs a brief", ErrInvalid)
	}
	cr, ok := statblock.ParseLabel(in.CR)
	if !ok {
		return nil, fmt.Errorf("%w: requested CR %q does not read as one the tables carry (try \"7\" or \"1/4\")", ErrInvalid, in.CR)
	}

	// The campaign's own material, when there is a campaign: the names a
	// brief may lean on, with a line each. Only factions, NPCs and places —
	// the things a monster's lore attaches to.
	var material []map[string]any
	if in.CampaignID != "" {
		if _, err := s.loadCampaign(ctx, in.CampaignID); err != nil {
			return nil, err
		}
		snap, err := LoadSnapshot(ctx, s.db, in.CampaignID)
		if err != nil {
			return nil, err
		}
		for _, e := range snap.Entities {
			if e.Status == "deleted" {
				continue
			}
			switch e.Kind {
			case campaign.KindFaction, campaign.KindNPC, campaign.KindLocation:
				m := map[string]any{"name": e.Name, "kind": e.Kind}
				if e.Summary != "" {
					m["summary"] = e.Summary
				}
				material = append(material, m)
			}
		}
		sort.Slice(material, func(i, j int) bool {
			return material[i]["name"].(string) < material[j]["name"].(string)
		})
		if len(material) > 12 {
			material = material[:12]
		}
	}

	envelope := statblock.EnvelopeFor(cr)

	/* ---------- the schema ---------- */

	abMin, abMax := 1.0, 30.0
	fields := []FieldSpec{
		{Key: "name", Desc: "the creature's name, evocative and four words or fewer", Required: true, MaxLen: 60},
		{Key: "size", Desc: "the creature's size", Required: true, Enum: monsterSizes},
		{Key: "type", Desc: "the creature's type", Required: true, Enum: monsterTypes},
		{Key: "ac", Desc: "armor class, inside the envelope's AC band", Type: FieldInteger, Required: true, Min: ptrf(5.0), Max: ptrf(30.0)},
		{Key: "hp", Desc: "hit points, a plain total inside the envelope's effective-hit-point band", Type: FieldInteger, Required: true, Min: ptrf(1.0), Max: ptrf(850.0)},
		{Key: "hit_dice", Desc: "the hit-dice expression that averages the hit points, like \"16d8+48\"", MaxLen: 24},
		{Key: "speed", Desc: "speeds as printed, like \"30 ft., fly 60 ft.\"", Required: true, MaxLen: 60},
		{Key: "str", Desc: "Strength score", Type: FieldInteger, Required: true, Min: &abMin, Max: &abMax},
		{Key: "dex", Desc: "Dexterity score", Type: FieldInteger, Required: true, Min: &abMin, Max: &abMax},
		{Key: "con", Desc: "Constitution score", Type: FieldInteger, Required: true, Min: &abMin, Max: &abMax},
		{Key: "int", Desc: "Intelligence score", Type: FieldInteger, Required: true, Min: &abMin, Max: &abMax},
		{Key: "wis", Desc: "Wisdom score", Type: FieldInteger, Required: true, Min: &abMin, Max: &abMax},
		{Key: "cha", Desc: "Charisma score", Type: FieldInteger, Required: true, Min: &abMin, Max: &abMax},
		{Key: "resist", Desc: "damage resistances as printed, comma-separated (\"bludgeoning, piercing, and slashing from nonmagical attacks\") — omit when none", MaxLen: 120},
		{Key: "immune", Desc: "damage immunities as printed, comma-separated — omit when none", MaxLen: 120},
	}
	for i := 1; i <= designerTraits; i++ {
		fields = append(fields,
			FieldSpec{Key: fmt.Sprintf("trait%d_name", i), Desc: "a trait's name (the italic bold lead-in a statblock prints)", MaxLen: 48},
			FieldSpec{Key: fmt.Sprintf("trait%d_desc", i), Desc: "that trait's text, one or two sentences", MaxLen: 400},
		)
	}
	for i := 1; i <= designerActions; i++ {
		req := i == 1
		what := "an attack action"
		if i == 2 {
			what = "a second action — the Multiattack line when the creature fights with more than one attack, else another action or recharge power"
		} else if i > 2 {
			what = "another action, or omit"
		}
		fields = append(fields,
			FieldSpec{Key: fmt.Sprintf("action%d_name", i), Desc: what + " — its name", Required: req, MaxLen: 48},
			FieldSpec{Key: fmt.Sprintf("action%d_desc", i), Desc: what + " — its full text in the printed SRD format the server parses", Required: req, MaxLen: 500},
		)
	}
	if in.Legendary {
		for i := 1; i <= designerLegendary; i++ {
			req := i == 1
			fields = append(fields,
				FieldSpec{Key: fmt.Sprintf("legend%d_name", i), Desc: "a legendary action's name", Required: req, MaxLen: 48},
				FieldSpec{Key: fmt.Sprintf("legend%d_desc", i), Desc: "that legendary action's text in the printed SRD format", Required: req, MaxLen: 400},
				FieldSpec{Key: fmt.Sprintf("legend%d_cost", i), Desc: "its legendary action cost in points", Type: FieldInteger, Min: ptrf(1.0), Max: ptrf(3.0)},
			)
		}
	}
	fields = append(fields,
		FieldSpec{Key: "tactics", Desc: "how the creature actually fights: opening move, what it does when hurt, when it flees — three or four plain lines, no numbers the statblock does not carry", Required: true, MaxLen: 900},
		FieldSpec{Key: "lore", Desc: "where it comes from and why it exists — two or three sentences, written against the campaign material when the structure carries any", Required: true, MaxLen: 900},
		FieldSpec{Key: "role", Desc: "the niche it fills in a fight", Required: true, Enum: encounter.EncounterRoles},
	)

	structure := map[string]any{
		"brief":     brief,
		"cr":        envelope.Label,
		"envelope":  envelope,
		"legendary": in.Legendary,
	}
	if len(material) > 0 {
		structure["campaign_material"] = material
	}
	note := monsterDesignNote(envelope, in.Legendary)

	fill := func(note string) (*Generated, error) {
		return s.Generate(ctx, GenerateInput{
			System: monsterSystemPrompt, Structure: structure, Fields: fields, Note: note,
		})
	}
	gen, err := fill(note)
	if err != nil {
		return nil, err
	}

	/* ---------- assemble, check, and revise once ---------- */

	assemble := func(values map[string]any) (*MonsterDesignResult, []string) {
		res, problems := assembleMonster(envelope, in.Legendary, values)
		if len(problems) > 0 {
			return res, problems
		}
		res.Rating = statblock.ComputeCR(res.Statblock)
		res.Shortfall = statblock.Shortfall(cr, res.Rating)
		return res, nil
	}
	res, problems := assemble(gen.Values)

	if len(problems) == 0 && len(res.Shortfall) == 0 {
		res.InputTokens, res.OutputTokens = gen.InputTokens, gen.OutputTokens
		return res, nil
	}

	// The revision pass: the specific misses go back — parse problems and
	// the calculator's own wording — and the model revises the halves that
	// are off. One pass, then the draft stands with whatever disagreement
	// remains, shown.
	var revision []string
	revision = append(revision, problems...)
	revision = append(revision, res.Shortfall...)
	revNote := note + "\nYour previous draft had these problems:\n- " +
		strings.Join(revision, "\n- ") +
		"\nRevise the draft: keep the identity, lore and tactics you wrote, change the numbers and the action text that the problems name. Every band in the envelope is law."
	gen2, err := fill(revNote)
	if err != nil {
		return nil, err
	}
	res2, problems2 := assemble(gen2.Values)
	res2.Revised = true
	res2.InputTokens = gen.InputTokens + gen2.InputTokens
	res2.OutputTokens = gen.OutputTokens + gen2.OutputTokens
	if len(problems2) > 0 {
		return nil, fmt.Errorf("%w: the designed monster failed validation twice: %s", ErrInvalid, strings.Join(problems2, "; "))
	}
	return res2, nil
}

// assembleMonster builds the structured statblock out of validated values,
// deterministically. Anything the parser cannot read is a problem the
// revision pass quotes — an action the calculator is blind to is the one
// thing this generator must never ship quietly.
func assembleMonster(envelope statblock.Envelope, legendary bool, values map[string]any) (*MonsterDesignResult, []string) {
	res := &MonsterDesignResult{Envelope: envelope}
	var problems []string

	str := func(key string) string {
		v, _ := values[key].(string)
		return strings.TrimSpace(v)
	}
	intOf := func(key string) int {
		if v, ok := values[key].(float64); ok {
			return int(v)
		}
		return 0
	}

	name := str("name")
	if name == "" {
		problems = append(problems, `"name" came back empty`)
	}
	sb := statblock.Statblock{
		Name:    name,
		Size:    str("size"),
		Type:    str("type"),
		AC:      intOf("ac"),
		HP:      intOf("hp"),
		HitDice: str("hit_dice"),
		Abilities: statblock.Abilities{
			Str: intOf("str"), Dex: intOf("dex"), Con: intOf("con"),
			Int: intOf("int"), Wis: intOf("wis"), Cha: intOf("cha"),
		},
		ProfBonus: envelope.ProfBonus,
	}
	sb.Speeds = parseSpeeds(str("speed"))
	if len(sb.Speeds) == 0 {
		problems = append(problems, `"speed`+"` did not read as speeds like \"30 ft., fly 60 ft.\"")
	}
	sb.Resist = splitList(str("resist"))
	sb.Immune = splitList(str("immune"))

	for i := 1; i <= designerTraits; i++ {
		n, d := str(fmt.Sprintf("trait%d_name", i)), str(fmt.Sprintf("trait%d_desc", i))
		if n == "" && d == "" {
			continue
		}
		if n == "" || d == "" {
			problems = append(problems, fmt.Sprintf("trait %d has only half its pair (name and text must arrive together)", i))
			continue
		}
		sb.Traits = append(sb.Traits, statblock.Action{Name: n, Desc: d})
	}

	for i := 1; i <= designerActions; i++ {
		n, d := str(fmt.Sprintf("action%d_name", i)), str(fmt.Sprintf("action%d_desc", i))
		if n == "" && d == "" {
			continue
		}
		if n == "" || d == "" {
			problems = append(problems, fmt.Sprintf("action %d has only half its pair (name and text must arrive together)", i))
			continue
		}
		a := statblock.Action{Name: n, Desc: d, Kind: "ACTION"}
		if atk, ok := statblock.ParseAttack(n, d); ok {
			a.Parsed, a.Attack = true, atk
		} else if i <= 2 {
			// The load-bearing actions must parse: the CR loop is blind to
			// prose it cannot read. Later optional slots may be flavour
			// (teleport, summon) and stand unparsed, the never-half-parsed
			// contract kept.
			a.Unparse = "not parseable into a structured attack; revise the text into the printed SRD format"
			problems = append(problems, fmt.Sprintf(
				"action %d (%q) is not in a format the server can read — write attacks as \"Melee Weapon Attack: +7 to hit, reach 5 ft., one target. Hit: 13 (2d8 + 4) slashing damage.\" and saves as \"Each creature in a 15-foot cone must make a DC 15 Dexterity saving throw, taking 24 (7d6) fire damage on a failed save.\"", i, n))
			sb.Actions = append(sb.Actions, a)
			continue
		} else {
			a.Unparse = "not parseable into a structured attack; prose kept verbatim"
		}
		sb.Actions = append(sb.Actions, a)
	}

	if legendary {
		for i := 1; i <= designerLegendary; i++ {
			n, d := str(fmt.Sprintf("legend%d_name", i)), str(fmt.Sprintf("legend%d_desc", i))
			if n == "" && d == "" {
				continue
			}
			if n == "" || d == "" {
				problems = append(problems, fmt.Sprintf("legendary action %d has only half its pair", i))
				continue
			}
			cost := intOf(fmt.Sprintf("legend%d_cost", i))
			if cost <= 0 {
				cost = 1
			}
			a := statblock.Action{Name: n, Desc: d, Kind: "LEGENDARY_ACTION", Cost: cost}
			if atk, ok := statblock.ParseAttack(n, d); ok {
				a.Parsed, a.Attack = true, atk
			} else {
				// Legendary actions that move or sense without attacking
				// are ordinary; only damage-bearing ones must parse.
				a.Unparse = "not parseable into a structured attack; prose kept verbatim"
			}
			sb.Actions = append(sb.Actions, a)
			sb.Legendary = true
		}
		if !sb.Legendary {
			problems = append(problems, "a legendary design needs at least one legendary action")
		}
	}

	// The legendary flag is derived from the actions whatever the draft
	// claimed — a creature with legendary actions is legendary.
	for _, a := range sb.Actions {
		if a.Legendary() {
			sb.Legendary = true
		}
	}

	res.Statblock = sb
	res.Tactics = str("tactics")
	res.Lore = str("lore")
	res.Role = str("role")
	return res, problems
}

// parseSpeeds reads a printed speed line — "30 ft., fly 60 ft., swim 30 ft."
// — into the map the statblock carries. A bare number is the walk.
func parseSpeeds(s string) map[string]int {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "" {
			continue
		}
		kind := "walk"
		if i := strings.Index(part, " "); i > 0 && !isDigit(part[0]) {
			kind = strings.TrimSpace(part[:i])
			part = strings.TrimSpace(part[i:])
			kind = strings.TrimSuffix(kind, " ")
		}
		j := 0
		for j < len(part) && isDigit(part[j]) {
			j++
		}
		if j == 0 {
			continue
		}
		n, err := strconv.Atoi(part[:j])
		if err != nil || n <= 0 {
			continue
		}
		out[kind] = n
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// splitList reads a comma-separated printed list into the statblock's
// slices, trimming and dropping empties.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// monsterDesignNote is the envelope as law: the numbers the draft is
// designed inside, the format the parser reads, and the legendary budget.
func monsterDesignNote(envelope statblock.Envelope, legendary bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The envelope in the structure is law. The requested CR is %s, so the arithmetic that will judge this creature expects: ", envelope.Label)
	fmt.Fprintf(&b, "effective hit points %d–%d (design near %d), armor class %d–%d, damage per round %d–%d (design near %d), delivered at attack bonus +%d–+%d or save DC %d–%d. ",
		envelope.HP.Min, envelope.HP.Max, envelope.HP.Assumed,
		envelope.AC.Min, envelope.AC.Max,
		envelope.DPR.Min, envelope.DPR.Max, envelope.DPR.Assumed,
		envelope.AttackBonus.Min, envelope.AttackBonus.Max,
		envelope.SaveDC.Min, envelope.SaveDC.Max)
	fmt.Fprintf(&b, "The proficiency bonus is +%d — it is already set, do not account for it in attack bonuses beyond the ability modifier and proficiency. ", envelope.ProfBonus)
	b.WriteString("Resistance to bludgeoning, piercing and slashing multiplies effective hit points (×1.5 at CR 0–4, ×2 above), so price such a resistance by lowering the raw hit points into the band.\n")
	b.WriteString("Write every attack action's text exactly in a printed SRD shape the server parses: \"Melee Weapon Attack: +7 to hit, reach 5 ft., one target. Hit: 13 (2d8 + 4) slashing damage.\", or \"Ranged Weapon Attack: +6 to hit, range 150/600 ft., one target. Hit: 8 (1d8 + 4) piercing damage.\", or \"Each creature in a 15-foot cone must make a DC 15 Dexterity saving throw, taking 24 (7d6) fire damage on a failed save.\" A \"Multiattack\" action is one action whose text counts the attacks: \"The creature makes two Claw attacks.\" Damage always prints the average with the dice beside it, like 13 (2d8 + 4).\n")
	if legendary {
		b.WriteString("This creature is legendary: it spends exactly three legendary-action points per round — declare each legendary action's cost (1 to 3 points; the three together should not exceed three points of attacks per round, because the calculator prices three points of the strongest options into every round).")
	} else {
		b.WriteString("This creature is not legendary: no legendary actions.")
	}
	return b.String()
}

// monsterSystemPrompt is the one system line the exchange runs under.
const monsterSystemPrompt = `You are Grimoire's monster designer. You are handed a DM's brief, a target challenge rating and the exact numeric envelope the DMG's own tables assign that CR — expected hit points, armor class, damage per round, attack bonus or save DC, proficiency bonus, and the legendary-action budget when the creature is legendary. Your job is the creature: a complete, playable statblock whose numbers land inside the envelope, traits and actions that make it distinct, tactics that make it run at the table, and lore written against the campaign's own material when any is handed. The server will compute the creature's challenge rating from what you wrote and check it against the ask; design so the maths agrees. Write like the Monster Manual writes — concrete, specific, no filler.`

/* ---------- the placement ---------- */

// MonsterPlaceInput is one placement request: a saved homebrew monster,
// staged into the campaign as a creature entity.
type MonsterPlaceInput struct {
	CampaignID string
	HomebrewID string
	Name       string
	// Summary is the entity's one-line description — the lore's first
	// sentence, or the role line when there is no lore.
	Summary string
	// CRLabel is the calculator's computed label, for the batch's prompt
	// record.
	CRLabel string
	// Lore rides the batch prompt as evidence.
	Lore      string
	CreatedBy string
}

// PlaceMonster stages the monster's placement: one creature entity, behind
// the review gate like every other generated write. Nothing is written to
// the graph here; the entity exists when the batch is accepted.
func (s *Store) PlaceMonster(ctx context.Context, in MonsterPlaceInput) (*Batch, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: a placement needs the monster's name", ErrInvalid)
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		summary = fmt.Sprintf("A homebrew creature of computed CR %s.", in.CRLabel)
	}
	prompt := fmt.Sprintf("the homebrew monster %s (computed CR %s) placed as a creature", name, in.CRLabel)
	if in.Lore != "" {
		prompt += " — lore: " + in.Lore
	}
	return s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceMonster,
		Prompt: prompt, CreatedBy: in.CreatedBy,
		Items: []BatchItemInput{{
			ID: "monster", Kind: "entity", Subject: name, Summary: summary,
			Payload: map[string]any{
				"local_id": "monster", "homebrew_id": in.HomebrewID,
				"kind": campaign.KindCreature, "name": name, "summary": summary,
			},
		}},
	})
}
