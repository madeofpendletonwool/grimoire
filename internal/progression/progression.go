// Package progression is the 2014 class leveling tables (MAD-424, stage 7
// of MAD-417): per class, what a level grants.
//
// The sheet (internal/sheet) stores what a character IS; the ledger
// (internal/ledger) stores what is LEFT; this package answers what the
// NEXT level GIVES — spell slots per level, features gained, hit dice,
// save proficiencies, and the XP-to-level advancement table. A level-up
// is a proposal through the review gate whose diff is computed from these
// tables; nothing here touches a database.
//
// 5e, deliberately: the 2014 SRD, the same edition the sheet, the ledger
// and the encounter math pin. The values were transcribed from the 2014
// PHB class tables and pinned with unit tests (the xpThresholds precedent
// in internal/encounter/math.go): the tables are data, the tests are the
// oracle, and a wrong number fails the build rather than a character.
package progression

import (
	"fmt"
	"sort"
	"strings"
)

// Casting kinds — how a class contributes to the slot table.
const (
	CastingFull = "full" // bard, cleric, druid, sorcerer, wizard
	CastingHalf = "half" // paladin, ranger
	CastingPact = "pact" // warlock: slots scale separately, recover on a short rest
	CastingNone = ""     // barbarian, fighter, monk, rogue
)

// MaxLevel is the 2014 level cap.
const MaxLevel = 20

// xpToLevel is the XP a character needs to BE at a level (index 1..20):
// cumulative, from the PHB "Character Advancement" table's middle
// (Fast/ Medium/ Slow is a pacing choice; the middle column is the
// standard game and the one every published adventure budgets against).
var xpToLevel = [21]int{
	0,
	0, 300, 900, 2700, 6500, 14000, 23000, 34000, 48000, 64000,
	85000, 100000, 120000, 140000, 165000, 195000, 225000, 265000, 305000, 355000,
}

// XPForLevel returns the cumulative XP a character needs to have reached
// the level. Level 1 costs 0; levels outside 1..20 are an error.
func XPForLevel(level int) (int, error) {
	if level < 1 || level > MaxLevel {
		return 0, fmt.Errorf("level %d is outside 1..%d", level, MaxLevel)
	}
	return xpToLevel[level], nil
}

// LevelForXP returns the level a character with xp total XP has reached.
// XP below the level-2 threshold is level 1; XP at or beyond level 20's
// threshold is level 20.
func LevelForXP(xp int) int {
	if xp < 0 {
		xp = 0
	}
	level := 1
	for l := 2; l <= MaxLevel; l++ {
		if xp >= xpToLevel[l] {
			level = l
		}
	}
	return level
}

// XPToNext returns the XP still needed between the sheet's current total
// level and the next, and whether a next level exists. Level 20 is the
// cap: there is no next.
func XPToNext(totalLevel, xp int) (int, bool) {
	if totalLevel >= MaxLevel {
		return 0, false
	}
	next := xpToLevel[totalLevel+1]
	if xp >= next {
		return 0, true
	}
	return next - xp, true
}

/* ---------- the classes ---------- */

// Feature is one named gain at one class level. Base-class features only:
// the 2014 SRD carries each class's own table; subclass features appear
// as the class's placeholder ("Bard College feature") marking the slot,
// because which feature it is belongs to the subclass, not the class.
type Feature struct {
	Level int    `json:"level"`
	Name  string `json:"name"`
}

// Class is one 2014 class's leveling definition.
type Class struct {
	Name     string    // canonical lower-case name
	HitDie   int       // the die's sides: 6, 8, 10, 12
	Casting  string    // CastingFull | CastingHalf | CastingPact | CastingNone
	Saves    []string  // saving-throw proficiencies, lower case
	Primary  []string  // primary abilities, lower case
	Slots    []SlotRow // the class's OWN single-class slot table (full/half/pact)
	Features []Feature
}

// SlotRow is one class level's slot grant: the slot level and how many of
// them the table carries.
type SlotRow struct {
	Level int // the CLASS level this row is for
	Slots map[int]int
}

// Classes is the twelve 2014 base classes keyed by canonical name.
var Classes = map[string]*Class{
	"barbarian": {
		Name: "barbarian", HitDie: 12, Casting: CastingNone,
		Saves:   []string{"str", "con"},
		Primary: []string{"str"},
		Features: []Feature{
			{1, "Rage"}, {1, "Unarmored Defense"},
			{2, "Reckless Attack"}, {2, "Danger Sense"},
			{3, "Primal Path"},
			{5, "Extra Attack"}, {5, "Fast Movement"},
			{7, "Feral Instinct"},
			{9, "Brutal Critical (1 die)"},
			{10, "Primal Path feature"},
			{11, "Relentless Rage"},
			{13, "Brutal Critical (2 dice)"},
			{14, "Primal Path feature"},
			{15, "Persistent Rage"},
			{17, "Brutal Critical (3 dice)"},
			{18, "Primal Path feature"},
			{20, "Primal Champion"},
		},
	},
	"bard": {
		Name: "bard", HitDie: 8, Casting: CastingFull,
		Saves:   []string{"dex", "cha"},
		Primary: []string{"cha"},
		Slots:   fullCasterSlots,
		Features: []Feature{
			{1, "Spellcasting"}, {1, "Bardic Inspiration (d6)"},
			{2, "Jack of All Trades"}, {2, "Song of Rest (d6)"},
			{3, "Expertise"}, {3, "Bard College"},
			{5, "Bardic Inspiration (d8)"}, {5, "Font of Inspiration"},
			{6, "Countercharm"}, {6, "Bard College feature"},
			{9, "Song of Rest (d8)"},
			{10, "Expertise"}, {10, "Bardic Inspiration (d10)"}, {10, "Magical Secrets"},
			{13, "Song of Rest (d10)"},
			{14, "Magical Secrets"}, {14, "Bard College feature"},
			{15, "Bardic Inspiration (d12)"},
			{17, "Song of Rest (d12)"},
			{18, "Magical Secrets"}, {18, "Bard College feature"},
			{20, "Superior Inspiration"},
		},
	},
	"cleric": {
		Name: "cleric", HitDie: 8, Casting: CastingFull,
		Saves:   []string{"wis", "cha"},
		Primary: []string{"wis"},
		Slots:   fullCasterSlots,
		Features: []Feature{
			{1, "Spellcasting"}, {1, "Divine Domain"},
			{2, "Channel Divinity (1/rest)"}, {2, "Divine Domain feature"},
			{5, "Destroy Undead (CR 1/2)"},
			{6, "Channel Divinity (2/rest)"}, {6, "Divine Domain feature"},
			{8, "Destroy Undead (CR 1)"}, {8, "Divine Domain feature"},
			{10, "Divine Intervention"},
			{11, "Destroy Undead (CR 2)"},
			{14, "Destroy Undead (CR 3)"},
			{17, "Destroy Undead (CR 4)"}, {17, "Divine Domain feature"},
			{18, "Channel Divinity (3/rest)"},
			{20, "Divine Intervention Improvement"},
		},
	},
	"druid": {
		Name: "druid", HitDie: 8, Casting: CastingFull,
		Saves:   []string{"int", "wis"},
		Primary: []string{"wis"},
		Slots:   fullCasterSlots,
		Features: []Feature{
			{1, "Druidic"}, {1, "Spellcasting"},
			{2, "Wild Shape"}, {2, "Druid Circle"},
			{6, "Druid Circle feature"},
			{10, "Druid Circle feature"},
			{14, "Druid Circle feature"},
			{18, "Timeless Body"}, {18, "Beast Spells"},
			{20, "Archdruid"},
		},
	},
	"fighter": {
		Name: "fighter", HitDie: 10, Casting: CastingNone,
		Saves:   []string{"str", "con"},
		Primary: []string{"str", "dex"},
		Features: []Feature{
			{1, "Fighting Style"}, {1, "Second Wind"},
			{2, "Action Surge (1 use)"},
			{3, "Martial Archetype"},
			{5, "Extra Attack"},
			{7, "Martial Archetype feature"},
			{9, "Indomitable (1 use)"},
			{10, "Martial Archetype feature"},
			{11, "Extra Attack (2 attacks)"},
			{13, "Indomitable (2 uses)"},
			{15, "Martial Archetype feature"},
			{17, "Action Surge (2 uses)"}, {17, "Indomitable (3 uses)"},
			{18, "Martial Archetype feature"},
			{20, "Extra Attack (3 attacks)"},
		},
	},
	"monk": {
		Name: "monk", HitDie: 8, Casting: CastingNone,
		Saves:   []string{"str", "dex"},
		Primary: []string{"dex", "wis"},
		Features: []Feature{
			{1, "Unarmored Defense"}, {1, "Martial Arts"},
			{2, "Ki"}, {2, "Unarmored Movement"},
			{3, "Deflect Missiles"}, {3, "Monastic Tradition"},
			{4, "Slow Fall"},
			{5, "Extra Attack"}, {5, "Stunning Strike"},
			{6, "Ki-Empowered Strikes"}, {6, "Monastic Tradition feature"},
			{7, "Evasion"}, {7, "Stillness of Mind"},
			{9, "Unarmored Movement improvement"},
			{10, "Purity of Body"},
			{11, "Monastic Tradition feature"},
			{13, "Tongue of Sun and Moon"},
			{14, "Diamond Soul"},
			{15, "Timeless Body"},
			{17, "Monastic Tradition feature"},
			{18, "Empty Body"},
			{20, "Perfect Self"},
		},
	},
	"paladin": {
		Name: "paladin", HitDie: 10, Casting: CastingHalf,
		Saves:   []string{"wis", "cha"},
		Primary: []string{"str", "cha"},
		Slots:   paladinSlots,
		Features: []Feature{
			{1, "Divine Sense"}, {1, "Lay on Hands"},
			{2, "Fighting Style"}, {2, "Spellcasting"}, {2, "Divine Smite"},
			{3, "Divine Health"}, {3, "Sacred Oath"},
			{5, "Extra Attack"},
			{6, "Aura of Protection"},
			{7, "Sacred Oath feature"},
			{10, "Aura of Courage"},
			{11, "Improved Divine Smite"},
			{14, "Cleansing Touch"},
			{15, "Sacred Oath feature"},
			{18, "Aura Improvements"},
			{20, "Sacred Oath feature"},
		},
	},
	"ranger": {
		Name: "ranger", HitDie: 10, Casting: CastingHalf,
		Saves:   []string{"str", "dex"},
		Primary: []string{"dex", "wis"},
		Slots:   rangerSlots,
		Features: []Feature{
			{1, "Favored Enemy"}, {1, "Natural Explorer"},
			{2, "Fighting Style"}, {2, "Spellcasting"},
			{3, "Primeval Awareness"}, {3, "Ranger Archetype"},
			{5, "Extra Attack"},
			{6, "Favored Enemy and Natural Explorer improvements"},
			{7, "Ranger Archetype feature"},
			{8, "Land's Stride"},
			{10, "Hide in Plain Sight"}, {10, "Natural Explorer improvement"},
			{11, "Ranger Archetype feature"},
			{13, "Favored Enemy improvement"},
			{14, "Vanish"},
			{15, "Ranger Archetype feature"},
			{18, "Feral Senses"},
			{20, "Foe Slayer"},
		},
	},
	"rogue": {
		Name: "rogue", HitDie: 8, Casting: CastingNone,
		Saves:   []string{"dex", "int"},
		Primary: []string{"dex"},
		Features: []Feature{
			{1, "Expertise"}, {1, "Sneak Attack"}, {1, "Thieves' Cant"},
			{2, "Cunning Action"},
			{3, "Roguish Archetype"},
			{5, "Uncanny Dodge"},
			{6, "Expertise"},
			{7, "Evasion"},
			{9, "Roguish Archetype feature"},
			{11, "Reliable Talent"},
			{13, "Roguish Archetype feature"},
			{14, "Blindsense"},
			{15, "Slippery Mind"},
			{17, "Roguish Archetype feature"},
			{18, "Elusive"},
			{20, "Stroke of Luck"},
		},
	},
	"sorcerer": {
		Name: "sorcerer", HitDie: 6, Casting: CastingFull,
		Saves:   []string{"con", "cha"},
		Primary: []string{"cha"},
		Slots:   fullCasterSlots,
		Features: []Feature{
			{1, "Spellcasting"},
			{2, "Font of Magic"},
			{3, "Metamagic (2 options)"}, {3, "Sorcerous Origin"},
			{6, "Sorcerous Origin feature"},
			{10, "Metamagic (2 more options)"},
			{14, "Sorcerous Origin feature"},
			{17, "Metamagic (2 more options)"},
			{18, "Sorcerous Origin feature"},
			{20, "Sorcerous Restoration"},
		},
	},
	"warlock": {
		Name: "warlock", HitDie: 8, Casting: CastingPact,
		Saves:   []string{"wis", "cha"},
		Primary: []string{"cha"},
		Slots:   pactSlots,
		Features: []Feature{
			{1, "Otherworldly Patron"}, {1, "Pact Magic"},
			{2, "Eldritch Invocations"},
			{3, "Pact Boon"},
			{6, "Otherworldly Patron feature"},
			{10, "Otherworldly Patron feature"},
			{11, "Mystic Arcanum (6th level)"},
			{13, "Mystic Arcanum (7th level)"},
			{14, "Otherworldly Patron feature"},
			{15, "Mystic Arcanum (8th level)"},
			{17, "Mystic Arcanum (9th level)"},
			{20, "Eldritch Master"},
		},
	},
	"wizard": {
		Name: "wizard", HitDie: 6, Casting: CastingFull,
		Saves:   []string{"int", "wis"},
		Primary: []string{"int"},
		Slots:   fullCasterSlots,
		Features: []Feature{
			{1, "Spellcasting"}, {1, "Arcane Recovery"},
			{2, "Arcane Tradition"},
			{6, "Arcane Tradition feature"},
			{10, "Arcane Tradition feature"},
			{14, "Arcane Tradition feature"},
			{18, "Spell Mastery"},
			{20, "Signature Spells"},
		},
	},
}

// ASILevels is each class's Ability Score Improvement schedule: every
// class at 4/8/12/16/19, except the fighter (4/6/8/12/14/16/19) and the
// rogue (4/8/10/12/16/19).
var ASILevels = map[string][]int{
	"barbarian": {4, 8, 12, 16, 19},
	"bard":      {4, 8, 12, 16, 19},
	"cleric":    {4, 8, 12, 16, 19},
	"druid":     {4, 8, 12, 16, 19},
	"fighter":   {4, 6, 8, 12, 14, 16, 19},
	"monk":      {4, 8, 12, 16, 19},
	"paladin":   {4, 8, 12, 16, 19},
	"ranger":    {4, 8, 12, 16, 19},
	"rogue":     {4, 8, 10, 12, 16, 19},
	"sorcerer":  {4, 8, 12, 16, 19},
	"warlock":   {4, 8, 12, 16, 19},
	"wizard":    {4, 8, 12, 16, 19},
}

// fullCasterSlots is the 2014 slot table bard, cleric, druid, sorcerer
// and wizard share verbatim.
var fullCasterSlots = []SlotRow{
	{1, slotMap("1:2")},
	{2, slotMap("1:3")},
	{3, slotMap("1:4", "2:2")},
	{4, slotMap("1:4", "2:3")},
	{5, slotMap("1:4", "2:3", "3:2")},
	{6, slotMap("1:4", "2:3", "3:3")},
	{7, slotMap("1:4", "2:3", "3:3", "4:1")},
	{8, slotMap("1:4", "2:3", "3:3", "4:2")},
	{9, slotMap("1:4", "2:3", "3:3", "4:3", "5:1")},
	{10, slotMap("1:4", "2:3", "3:3", "4:3", "5:2")},
	{11, slotMap("1:4", "2:3", "3:3", "4:3", "5:2", "6:1")},
	{12, slotMap("1:4", "2:3", "3:3", "4:3", "5:2", "6:1")},
	{13, slotMap("1:4", "2:3", "3:3", "4:3", "5:2", "6:1", "7:1")},
	{14, slotMap("1:4", "2:3", "3:3", "4:3", "5:2", "6:1", "7:1")},
	{15, slotMap("1:4", "2:3", "3:3", "4:3", "5:2", "6:1", "7:1", "8:1")},
	{16, slotMap("1:4", "2:3", "3:3", "4:3", "5:2", "6:1", "7:1", "8:1")},
	{17, slotMap("1:4", "2:3", "3:3", "4:3", "5:2", "6:1", "7:1", "8:1", "9:1")},
	{18, slotMap("1:4", "2:3", "3:3", "4:3", "5:3", "6:1", "7:1", "8:1", "9:1")},
	{19, slotMap("1:4", "2:3", "3:3", "4:3", "5:3", "6:2", "7:1", "8:1", "9:1")},
	{20, slotMap("1:4", "2:3", "3:3", "4:3", "5:3", "6:2", "7:2", "8:1", "9:1")},
}

// paladinSlots is the 2014 paladin table: slots begin at class level 2.
var paladinSlots = []SlotRow{
	{2, slotMap("1:2")},
	{3, slotMap("1:3")},
	{4, slotMap("1:3")},
	{5, slotMap("1:4", "2:2")},
	{6, slotMap("1:4", "2:2")},
	{7, slotMap("1:4", "2:3")},
	{8, slotMap("1:4", "2:3")},
	{9, slotMap("1:4", "2:3", "3:2")},
	{10, slotMap("1:4", "2:3", "3:2")},
	{11, slotMap("1:4", "2:3", "3:3")},
	{12, slotMap("1:4", "2:3", "3:3")},
	{13, slotMap("1:4", "2:3", "3:3", "4:1")},
	{14, slotMap("1:4", "2:3", "3:3", "4:1")},
	{15, slotMap("1:4", "2:3", "3:3", "4:2")},
	{16, slotMap("1:4", "2:3", "3:3", "4:2")},
	{17, slotMap("1:4", "2:3", "3:3", "4:3")},
	{18, slotMap("1:4", "2:3", "3:3", "4:3")},
	{19, slotMap("1:4", "2:3", "3:3", "4:3", "5:1")},
	{20, slotMap("1:4", "2:3", "3:3", "4:3", "5:1")},
}

// rangerSlots is the 2014 ranger table: slots from class level 1, one
// level ahead of the paladin early and identical from 5th.
var rangerSlots = []SlotRow{
	{1, slotMap("1:2")},
	{2, slotMap("1:3")},
	{3, slotMap("1:4")},
	{4, slotMap("1:4")},
	{5, slotMap("1:4", "2:2")},
	{6, slotMap("1:4", "2:2")},
	{7, slotMap("1:4", "2:3")},
	{8, slotMap("1:4", "2:3")},
	{9, slotMap("1:4", "2:3", "3:2")},
	{10, slotMap("1:4", "2:3", "3:2")},
	{11, slotMap("1:4", "2:3", "3:3")},
	{12, slotMap("1:4", "2:3", "3:3")},
	{13, slotMap("1:4", "2:3", "3:3", "4:1")},
	{14, slotMap("1:4", "2:3", "3:3", "4:1")},
	{15, slotMap("1:4", "2:3", "3:3", "4:2")},
	{16, slotMap("1:4", "2:3", "3:3", "4:2")},
	{17, slotMap("1:4", "2:3", "3:3", "4:3")},
	{18, slotMap("1:4", "2:3", "3:3", "4:3")},
	{19, slotMap("1:4", "2:3", "3:3", "4:3", "5:1")},
	{20, slotMap("1:4", "2:3", "3:3", "4:3", "5:1")},
}

// pactSlots is the 2014 warlock table: few slots, all one level, back on
// a short rest. The map's single key is the level the slots cast at.
var pactSlots = []SlotRow{
	{1, slotMap("1:1")},
	{2, slotMap("1:2")},
	{3, slotMap("2:2")},
	{4, slotMap("2:2")},
	{5, slotMap("3:2")},
	{6, slotMap("3:2")},
	{7, slotMap("4:2")},
	{8, slotMap("4:2")},
	{9, slotMap("5:2")},
	{10, slotMap("5:2")},
	{11, slotMap("5:3")},
	{12, slotMap("5:3")},
	{13, slotMap("5:3")},
	{14, slotMap("5:3")},
	{15, slotMap("5:3")},
	{16, slotMap("5:3")},
	{17, slotMap("5:4")},
	{18, slotMap("5:4")},
	{19, slotMap("5:4")},
	{20, slotMap("5:4")},
}

// slotMap builds one row from "level:count" pairs.
func slotMap(pairs ...string) map[int]int {
	out := make(map[int]int, len(pairs))
	for _, p := range pairs {
		var lvl, n int
		if _, err := fmt.Sscanf(p, "%d:%d", &lvl, &n); err == nil && lvl >= 1 && lvl <= 9 && n > 0 {
			out[lvl] = n
		}
	}
	return out
}

/* ---------- lookups ---------- */

// Canonical resolves a class name to its canonical lower-case spelling —
// case- and space-insensitive ("Wizard", " FIGHTER ", "ranger" all
// resolve). An unknown class is an error, never a guess: homebrew classes
// are the sheet's business, not the leveling tables'.
func Canonical(name string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return "", fmt.Errorf("class name is empty")
	}
	if _, ok := Classes[key]; ok {
		return key, nil
	}
	return "", fmt.Errorf("class %q is not one of the twelve 2014 classes", name)
}

// ClassNames lists the canonical class names in order.
func ClassNames() []string {
	out := make([]string, 0, len(Classes))
	for name := range Classes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SlotsAt returns the class's single-class slot table at one class level.
// The tables carry a row exactly where the class has slots — every level
// for a full caster, from 2nd for a paladin, from 1st for a ranger — so
// a class level with no row (level 1 paladin) carries no slots, and a
// non-caster carries none at every level.
func SlotsAt(className string, classLevel int) map[int]int {
	c, ok := Classes[className]
	if !ok || classLevel < 1 || classLevel > MaxLevel {
		return nil
	}
	for _, r := range c.Slots {
		if r.Level == classLevel {
			out := make(map[int]int, len(r.Slots))
			for lvl, n := range r.Slots {
				out[lvl] = n
			}
			return out
		}
	}
	return nil
}

// PactSlotsAt returns the warlock pact table at one warlock level: how
// many slots and the level they cast at. Non-warlocks return 0, 0.
func PactSlotsAt(className string, classLevel int) (count, slotLevel int) {
	if className != "warlock" || classLevel < 1 || classLevel > MaxLevel {
		return 0, 0
	}
	row := SlotsAt("warlock", classLevel)
	if len(row) == 0 {
		return 0, 0
	}
	// One key by construction; report it deterministically.
	levels := make([]int, 0, len(row))
	for lvl := range row {
		levels = append(levels, lvl)
	}
	sort.Ints(levels)
	lvl := levels[len(levels)-1]
	return row[lvl], lvl
}

// FeaturesAt returns the base-class feature names gained at one class
// level, including the Ability Score Improvement when the class's
// schedule has one there.
func FeaturesAt(className string, classLevel int) []string {
	c, ok := Classes[className]
	if !ok || classLevel < 1 || classLevel > MaxLevel {
		return nil
	}
	var out []string
	for _, f := range c.Features {
		if f.Level == classLevel {
			out = append(out, f.Name)
		}
	}
	for _, l := range ASILevels[className] {
		if l == classLevel {
			out = append(out, "Ability Score Improvement")
		}
	}
	sort.Strings(out)
	return out
}

// HitPointsGained is the 2014 fixed average: the hit die's average
// rounded up plus the CON modifier — the number the leveling engine
// proposes and a table can modify. A d6 yields 4, a d8 5, a d10 6, a
// d12 7 before the modifier.
func HitPointsGained(hitDie, conMod int) int {
	if hitDie < 1 {
		return conMod
	}
	return hitDie/2 + 1 + conMod
}
