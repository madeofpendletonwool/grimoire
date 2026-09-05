package campaign

// The party block (MAD-378). Designing an encounter against a campaign needs
// to know who is at the table, and until now the only thing Grimoire read off
// a pc was a bare "level" key — enough to date a difficulty band, far too thin
// to design against.
//
// This formalises what a pc entity's payload may declare, the same way
// MAD-370 formalised the place block and MAD-313 the npc agent block: a typed
// view over the existing entities.payload JSON, no new table. Unlike those two
// the block is NOT nested under its own key — "level" has been read from the
// payload's top level since the campaign core shipped, the seed writes it
// there, and moving it would silently re-band every planned encounter in every
// existing campaign. The block is the payload.
//
// Every key is optional. A campaign that declares nothing degrades to exactly
// today's behaviour, which is the rule the awareness layer already follows.
//
// What is deliberately NOT here: what the character knows (the awareness
// layer's, enforced in SQL at the character:<id> scope) and who they are
// (entities.name and entities.summary, which is what the campaign prose index
// is built from). This block is the mechanical sheet only.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PartyBlock is the declared mechanical sheet of a pc entity. Every field is
// optional; the zero PartyBlock is a pc the DM has not written numbers for.
//
// Level is 0 when the pc declares no usable level. "Usable" is the rule the
// continuity engine has always applied: present, parseable, and between 1 and
// 20. A level outside that window is reported as a problem rather than
// clamped — a DM who typed 50 wants to hear about it, and guessing which
// legal level they meant would be worse than saying nothing.
type PartyBlock struct {
	Level             int            `json:"level,omitempty"`
	Class             string         `json:"class,omitempty"`
	Subclass          string         `json:"subclass,omitempty"`
	AC                int            `json:"ac,omitempty"`
	MaxHP             int            `json:"max_hp,omitempty"`
	PassivePerception int            `json:"passive_perception,omitempty"`
	Saves             map[string]int `json:"saves,omitempty"`
	Resources         PartyResources `json:"resources,omitzero"`
	DamageResistances []string       `json:"damage_resistances,omitempty"`
	Conditions        []string       `json:"conditions,omitempty"`
	Items             []string       `json:"items,omitempty"`
	Notes             string         `json:"notes,omitempty"`
}

// PartyResources is what the character has left: spell slots by level (the
// key is the slot level as a string, because that is how JSON objects key
// numbers) and remaining hit dice.
type PartyResources struct {
	SpellSlots map[string]int `json:"spell_slots,omitempty"`
	HitDice    int            `json:"hit_dice,omitempty"`
}

// IsZero reports an entirely undeclared resource block, so encoding/json can
// drop it via `omitzero` rather than writing "resources":{} onto every pc.
func (r PartyResources) IsZero() bool { return len(r.SpellSlots) == 0 && r.HitDice == 0 }

// HasLevel reports whether the pc declares a level the encounter math may
// use. It is the single predicate Levels() and every caller branch on.
func (b PartyBlock) HasLevel() bool { return b.Level >= 1 && b.Level <= 20 }

// PartyProblem is one key of one pc's block that could not be read. A
// problem is reported, never fatal: a DM who hand-edited a payload into
// nonsense still gets their party, minus the key they broke.
type PartyProblem struct {
	EntityID string `json:"entity_id"`
	Name     string `json:"name"`
	Field    string `json:"field"`
	Detail   string `json:"detail"`
}

// PartyMember is one pc as the party table sees it.
type PartyMember struct {
	EntityID string     `json:"entity_id"`
	Name     string     `json:"name"`
	Status   string     `json:"status"`
	Block    PartyBlock `json:"block"`
}

// PartyTable is a campaign's whole party: its live pcs in name order, plus
// every block key that could not be read. It is DM material — a party sheet
// names what the characters carry and what they can still cast.
type PartyTable struct {
	CampaignID string         `json:"campaign_id"`
	Members    []PartyMember  `json:"members"`
	Problems   []PartyProblem `json:"problems,omitempty"`
}

// Levels is the party's declared character levels, in the members' name
// order. It is bit-for-bit what canon.Snapshot.Party has always computed:
// live pcs only, a level only when it parses to 1..20, name order, and nil
// when nobody declares one — which is what makes party_level_drift skip.
func (t *PartyTable) Levels() []int {
	if t == nil {
		return nil
	}
	var out []int
	for _, m := range t.Members {
		if m.Block.HasLevel() {
			out = append(out, m.Block.Level)
		}
	}
	return out
}

// Size is how many characters declare a level — the party size the DMG
// budget is computed for, which is not always len(Members).
func (t *PartyTable) Size() int { return len(t.Levels()) }

/* ---------- decoding ---------- */

// PartyBlockOf decodes one pc's block from its payload, tolerating anything.
// Each key is read on its own so a single broken value costs that key and
// nothing else: a payload whose "ac" is the string "high" still yields the
// level, the class and the saves. Problems name the entity and the key, and
// an entity that is not a live pc yields the zero block and no problems.
//
// Since the typed sheet (MAD-418) the sheet is the definition: a payload
// carrying one is read through it and the legacy keys below are the
// fallback. See internal/campaign/sheet.go for what maps and what
// deliberately does not (state — remaining slots, conditions — is the
// ledger's, not the sheet's).
func PartyBlockOf(e *Entity) (PartyBlock, []PartyProblem) {
	var b PartyBlock
	if e == nil || e.Kind != KindPC {
		return b, nil
	}
	if s, has, err := SheetOf(e); has && err == nil {
		return partyBlockOfSheet(e, s)
	}
	var problems []PartyProblem
	bad := func(field, detail string) {
		problems = append(problems, PartyProblem{
			EntityID: e.ID, Name: e.Name, Field: field, Detail: detail,
		})
	}

	if raw, ok := e.Payload["level"]; ok {
		switch n, num, usable := payloadLevel(raw); {
		case !num:
			bad("level", "level is not a number")
		case !usable:
			bad("level", fmt.Sprintf("level %d is outside 1-20", n))
		default:
			b.Level = n
		}
	}
	b.Class = payloadLine(e.Payload, "class", bad)
	b.Subclass = payloadLine(e.Payload, "subclass", bad)
	b.AC = payloadCount(e.Payload, "ac", bad)
	b.MaxHP = payloadCount(e.Payload, "max_hp", bad)
	b.PassivePerception = payloadCount(e.Payload, "passive_perception", bad)
	b.Saves = payloadIntMap(e.Payload, "saves", bad)
	b.DamageResistances = payloadStrings(e.Payload, "damage_resistances", bad)
	b.Conditions = payloadStrings(e.Payload, "conditions", bad)
	b.Items = payloadStrings(e.Payload, "items", bad)
	b.Notes = payloadLine(e.Payload, "notes", bad)

	if raw, ok := e.Payload["resources"]; ok {
		obj, isObj := raw.(map[string]any)
		if !isObj {
			bad("resources", "resources is not an object")
		} else {
			b.Resources.SpellSlots = payloadIntMap(obj, "spell_slots", bad)
			b.Resources.HitDice = payloadCount(obj, "hit_dice", bad)
		}
	}
	return b, problems
}

// WithPartyBlock returns a copy of payload with the block's keys replaced,
// preserving every other key a DM or another feature wrote there. Fields left
// at their zero value are removed rather than written as zeros, so "no AC
// declared" stays distinguishable from "AC 0".
func WithPartyBlock(payload map[string]any, b PartyBlock) map[string]any {
	out := make(map[string]any, len(payload)+len(partyBlockKeys))
	for k, v := range payload {
		out[k] = v
	}
	b.Class = cleanLine(b.Class)
	b.Subclass = cleanLine(b.Subclass)
	b.Notes = cleanLine(b.Notes)
	b.DamageResistances = cleanList(b.DamageResistances)
	b.Conditions = cleanList(b.Conditions)
	b.Items = cleanList(b.Items)

	// Marshal the typed block and splice its keys in one at a time: the
	// block shares the payload's top level with everything else, so a whole
	// map assignment would erase keys this block does not own.
	blob, err := json.Marshal(b)
	if err != nil {
		return out
	}
	var block map[string]any
	if json.Unmarshal(blob, &block) != nil {
		return out
	}
	for _, k := range partyBlockKeys {
		if v, ok := block[k]; ok {
			out[k] = v
		} else {
			delete(out, k)
		}
	}
	return out
}

// partyBlockKeys is every payload key the block owns. WithPartyBlock rewrites
// exactly these and nothing else.
var partyBlockKeys = []string{
	"level", "class", "subclass", "ac", "max_hp", "passive_perception",
	"saves", "resources", "damage_resistances", "conditions", "items", "notes",
}

/* ---------- the table ---------- */

// PartyTableOf builds the party table from entities already in memory. It is
// pure — no DB, no clock — so the continuity engine can call it on the
// snapshot it has already loaded rather than querying twice.
//
// Deleted pcs are out: they are not at the table. Every other status stays,
// including 'dead' — a dead character's sheet is still what the fight was
// planned against, and the engine has never filtered on anything but deleted.
func PartyTableOf(campaignID string, entities []Entity) *PartyTable {
	t := &PartyTable{CampaignID: campaignID}
	for i := range entities {
		e := &entities[i]
		if e.Kind != KindPC || e.Status == StatusDeleted {
			continue
		}
		block, problems := PartyBlockOf(e)
		t.Members = append(t.Members, PartyMember{
			EntityID: e.ID, Name: e.Name, Status: e.Status, Block: block,
		})
		t.Problems = append(t.Problems, problems...)
	}
	// Name order, stably: the order IS the contract Levels() carries, and
	// two pcs sharing a name must not shuffle between reads.
	sort.SliceStable(t.Members, func(i, j int) bool { return t.Members[i].Name < t.Members[j].Name })
	return t
}

// PartySnapshot reads one campaign's party table from the database. DM scope
// only, like every other read in this package: a party sheet carries the
// items, the remaining slots and the DM's notes on each character, and ADR 6's
// rule is that a narrower perspective gets a different read path, never a
// filtered version of this one.
func PartySnapshot(ctx context.Context, scope Scope, db *sql.DB, campaignID string) (*PartyTable, error) {
	if err := scope.requireDM(); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+entityCols+` FROM entities WHERE campaign_id = ? AND kind = ?`, campaignID, KindPC)
	if err != nil {
		return nil, fmt.Errorf("party snapshot: %w", err)
	}
	defer rows.Close()
	var pcs []Entity
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		pcs = append(pcs, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("party snapshot: %w", err)
	}
	return PartyTableOf(campaignID, pcs), nil
}

/* ---------- tolerant payload readers ---------- */

// payloadLevel reads the "level" key with exactly the semantics the
// continuity engine has applied since the campaign core: a JSON number or its
// string spelling, truncated to an int, usable only when the value itself
// sits in 1..20. The window is checked on the value rather than the truncated
// int on purpose — 20.5 is not level 20, it is a typo — because
// canon.Snapshot.Party is asserted to be unchanged by this block and that is
// the one place the two spellings disagree.
//
// It reports three things: the level, whether it was a number at all, and
// whether that number is usable. A non-number and an out-of-range number are
// different problems and read differently to the DM.
func payloadLevel(v any) (level int, isNumber, usable bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true, n >= 1 && n <= 20
	case int:
		return n, true, n >= 1 && n <= 20
	case int64:
		return int(n), true, n >= 1 && n <= 20
	case json.Number:
		f, err := n.Float64()
		return int(f), err == nil, err == nil && f >= 1 && f <= 20
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil, err == nil && i >= 1 && i <= 20
	default:
		return 0, false, false
	}
}

// payloadInt reads a JSON number or its string spelling. SQLite round-trips
// payloads through encoding/json, so every number arrives as a float64 — but
// a hand-edited payload may spell one as a string, and the level reader has
// accepted both since the campaign core.
func payloadInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	default:
		return 0, false
	}
}

// payloadCount reads a non-negative count key, reporting a wrong type or a
// negative value rather than storing it.
func payloadCount(payload map[string]any, key string, bad func(field, detail string)) int {
	raw, ok := payload[key]
	if !ok {
		return 0
	}
	n, num := payloadInt(raw)
	if !num {
		bad(key, key+" is not a number")
		return 0
	}
	if n < 0 {
		bad(key, fmt.Sprintf("%s %d is negative", key, n))
		return 0
	}
	return n
}

// payloadLine reads a string key, trimmed.
func payloadLine(payload map[string]any, key string, bad func(field, detail string)) string {
	raw, ok := payload[key]
	if !ok {
		return ""
	}
	s, isStr := raw.(string)
	if !isStr {
		bad(key, key+" is not a string")
		return ""
	}
	return cleanLine(s)
}

// payloadStrings reads a list-of-strings key, dropping (and reporting) the
// entries that are not strings rather than the whole list.
func payloadStrings(payload map[string]any, key string, bad func(field, detail string)) []string {
	raw, ok := payload[key]
	if !ok {
		return nil
	}
	list, isList := raw.([]any)
	if !isList {
		bad(key, key+" is not a list")
		return nil
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, isStr := v.(string)
		if !isStr {
			bad(key, key+" holds a value that is not a string")
			continue
		}
		if s = cleanLine(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// payloadIntMap reads an object of numbers — saves and spell slots both have
// that shape. A wrong-typed value costs its own key, not the map.
func payloadIntMap(payload map[string]any, key string, bad func(field, detail string)) map[string]int {
	raw, ok := payload[key]
	if !ok {
		return nil
	}
	obj, isObj := raw.(map[string]any)
	if !isObj {
		bad(key, key+" is not an object")
		return nil
	}
	out := make(map[string]int, len(obj))
	for k, v := range obj {
		n, num := payloadInt(v)
		if !num {
			bad(key, fmt.Sprintf("%s.%s is not a number", key, k))
			continue
		}
		out[k] = n
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
