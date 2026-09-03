package statblock

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// ParseAttack reads one action's prose and returns the structured attack it
// describes, or ok=false when the prose is not something the parser can read
// in full. It is deterministic: the same prose always parses to the same
// Attack, with no model, no network and no state.
//
// It handles the two shapes the mirrored SRD actually prints — the 2014
// forms ("Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 5
// (1d6+2) slashing damage." / "DC 15 Constitution saving throw ... taking
// 42 (12d6) poison damage on a failed save") and the 2024 forms ("Melee
// Attack Roll: +14, reach 10 ft. 13 (1d10 + 8) Slashing damage plus 5
// (2d4) Fire damage." / "Dexterity Saving Throw: DC 21, each creature in a
// 60-foot Cone. Failure: 59 (17d6) Fire damage.") — plus the Multiattack
// prose of both. Anything else (control effects, spellcasting lists,
// movement) is returned unparsed so the caller records it rather than
// guessing at it.
func ParseAttack(name, desc string) (Attack, bool) {
	norm := normalize(desc)

	if strings.Contains(strings.ToLower(name), "multiattack") {
		return parseMultiattack(norm)
	}

	atk := Attack{Name: strings.TrimSpace(name)}

	// Delivery: an attack roll (2014 "Melee Weapon Attack: ... to hit",
	// 2024 "Melee Attack Roll: ..."), or a saving throw.
	melee := containsAny(norm, "melee weapon attack:", "melee spell attack:", "melee attack roll:")
	ranged := containsAny(norm, "ranged weapon attack:", "ranged spell attack:", "ranged attack roll:")
	switch {
	case melee && ranged:
		atk.Kind = KindMeleeOrRanged
	case melee:
		atk.Kind = KindMelee
	case ranged:
		atk.Kind = KindRanged
	}
	if m := reToHit2014.FindStringSubmatchIndex(norm); m != nil {
		atk.ToHit = atoi(norm[m[2]:m[3]])
	} else if m := reToHit2024.FindStringSubmatchIndex(norm); m != nil {
		atk.ToHit = atoi(norm[m[2]:m[3]])
	}
	if m := reReach.FindStringSubmatchIndex(norm); m != nil {
		atk.Reach = atoi(norm[m[2]:m[3]])
	}
	if m := reRange.FindStringSubmatchIndex(norm); m != nil {
		atk.Range = atoi(norm[m[2]:m[3]])
	}

	// Saving-throw delivery, in either printed order.
	if m := reSave2014.FindStringSubmatchIndex(norm); m != nil {
		atk.Kind = KindSave
		atk.SaveDC = atoi(norm[m[2]:m[3]])
		atk.SaveAbility = abilityShort(norm[m[4]:m[5]])
	} else if m := reSave2024.FindStringSubmatchIndex(norm); m != nil {
		atk.Kind = KindSave
		atk.SaveAbility = abilityShort(norm[m[2]:m[3]])
		atk.SaveDC = atoi(norm[m[4]:m[5]])
	}

	if atk.Kind == "" {
		return Attack{}, false
	}

	// Who it hits, and whether the effect is an area.
	if m := reTargets.FindStringSubmatchIndex(norm); m != nil {
		atk.Targets = trimTargets(norm[m[2]:m[3]])
	}
	if containsAny(norm, " cone", " cube", " sphere", " radius", "-foot line", " in a line") {
		atk.Area = true
	}

	// Damage: the first printed average-and-dice clause is the attack's
	// damage; an immediately following "plus N (dice) type damage" clause is
	// a second component. Anything else after that (condition riders,
	// secondary saves) is prose, kept in Rider.
	d := reDamage.FindStringSubmatchIndex(norm)
	if d != nil {
		primary := Damage{Avg: atoi(norm[d[2]:d[3]]), Dice: squashDice(norm[d[4]:d[5]]), Type: damageType(norm, d)}
		rest := norm[d[1]:]
		if p := rePlusDamage.FindStringSubmatchIndex(rest); p != nil && p[0] < 48 {
			atk.Damage = []Damage{
				primary,
				{Avg: atoi(rest[p[2]:p[3]]), Dice: squashDice(rest[p[4]:p[5]]), Type: damageType(rest, p)},
			}
		} else {
			atk.Damage = []Damage{primary}
		}
		atk.Rider = rider(norm, d[1])
	} else if atk.SaveDC > 0 {
		// A save with no damage ("...or be charmed") is a complete parse of
		// a control effect; the rider is the effect itself.
		atk.Rider = rider(norm, 0)
	} else {
		// Neither attack damage nor a save: the prose names no mechanic the
		// parser can price, so it stays unparsed rather than half-read.
		return Attack{}, false
	}
	return atk, true
}

/* ---------- prose helpers ---------- */

// normalize folds the typographic noise the SRD prints into one canonical
// form: curly quotes, dashes (en dash in "(Recharge 5–6)"), non-breaking
// spaces, collapsed whitespace — all lower case.
func normalize(s string) string {
	r := strings.NewReplacer(
		"–", "-", "—", "-", "‘", "'", "’", "'", "“", `"`, "”", `"`,
		"\u00a0", " ", "\n", " ", "\r", " ", "\t", " ",
	)
	low := strings.ToLower(r.Replace(s))
	return strings.Join(strings.Fields(low), " ")
}

// squashDice removes the spaces books print inside dice expressions:
// "3d6 + 5" -> "3d6+5".
func squashDice(expr string) string {
	var b strings.Builder
	for _, r := range expr {
		if r != ' ' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// damageType reads the type word captured just before the "damage"
// keyword: ") slashing damage" -> "slashing". An untyped clause ("5 (1d6)
// damage") has no type group and returns "".
func damageType(text string, m []int) string {
	if len(m) >= 8 && m[6] >= 0 {
		return text[m[6]:m[7]]
	}
	return ""
}

// trimTargets keeps the target phrase and drops the clause that follows it:
// 2014 breath prose reads "each creature in that area must succeed on ..."
// and the target is the phrase, not the save.
func trimTargets(s string) string {
	for _, cut := range []string{" must ", " can ", " takes ", " takes a", " succeeds"} {
		if i := strings.Index(s, cut); i > 0 {
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

// rider keeps the prose that follows the damage clause — the on-hit
// condition a DM needs to know about — trimmed to a sentence-ish 240 chars.
func rider(text string, from int) string {
	if from >= len(text) {
		return ""
	}
	rest := strings.TrimSpace(text[from:])
	rest = strings.TrimLeft(rest, ".:;, ")
	if rest == "" {
		return ""
	}
	if i := strings.Index(rest, "success:"); i >= 0 && i < 40 {
		// 2024 breath weapons end with a "Success: Half damage." clause that
		// says nothing a DM needs twice.
		rest = rest[:i]
	}
	rest = strings.TrimSpace(strings.TrimRight(rest, ".,; "))
	if len(rest) > 240 {
		if cut := strings.LastIndexAny(rest[:240], ".;"); cut > 0 {
			rest = rest[:cut]
		} else {
			rest = rest[:240]
		}
		rest = strings.TrimSpace(strings.TrimRight(rest, ".,; "))
	}
	return rest
}

/* ---------- multiattack ---------- */

// numberWords maps the count words a multiattack prints to counts.
var numberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "a": 1, "an": 1,
}

func parseMultiattack(norm string) (Attack, bool) {
	// Only the sentence that does the counting is parsed; later sentences
	// ("It can replace one attack with a use of ...") are alternatives, not
	// parts of the standard round.
	sentence := norm
	if i := strings.Index(norm, "makes"); i >= 0 {
		sentence = sentence[i+len("makes"):]
	}
	if i := strings.IndexAny(sentence, ".!"); i >= 0 {
		sentence = sentence[:i]
	}

	// "makes two attacks, using ..." states the round's total attack count.
	// A number directly before the word "attacks" is that total; component
	// clauses carry their own counts ("three rend attacks" — "rend" is not
	// a number, so the leading three stays with the clause).
	total := 0
	if m := reTotalAttacks.FindStringSubmatchIndex(sentence); m != nil {
		total = numberWords[sentence[m[2]:m[3]]]
		if total == 0 {
			total = atoi(sentence[m[2]:m[3]])
		}
		sentence = sentence[:m[0]] + " " + sentence[m[1]:]
	}

	var comps []Component
	for _, seg := range splitSegments(sentence) {
		comp, ok := parseSegment(seg, total)
		if ok {
			comps = append(comps, comp)
		}
	}
	if len(comps) == 0 {
		return Attack{}, false
	}
	return Attack{Kind: KindMultiattack, Components: comps}, true
}

// splitSegments splits the sentence on the joins that mean "these all
// happen": commas and "and". ("uses Constrict" lands in its own segment.)
func splitSegments(sentence string) []string {
	s := sentence
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		for _, q := range strings.Split(p, " and ") {
			if t := strings.TrimSpace(q); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// parseSegment reads one segment: a single action ("one with its bite",
// "six pact blade attacks") or a choice among actions ("using bladed arm or
// fiery bolt in any combination"). Choice clauses inherit the round's total
// count when they state none of their own.
func parseSegment(seg string, total int) (Component, bool) {
	options := strings.Split(seg, " or ")
	if len(options) == 1 {
		name, count, ok := parseOption(options[0])
		if !ok {
			return Component{}, false
		}
		if count == 0 {
			count = total
		}
		if count == 0 {
			count = 1
		}
		return Component{Count: count, Name: name}, true
	}
	comp := Component{}
	for _, o := range options {
		name, count, ok := parseOption(o)
		if !ok {
			continue
		}
		if comp.Count == 0 {
			comp.Count = count
		}
		comp.Options = append(comp.Options, name)
	}
	if len(comp.Options) == 0 {
		return Component{}, false
	}
	if comp.Count == 0 {
		comp.Count = total
	}
	if comp.Count == 0 {
		comp.Count = 1
	}
	return comp, true
}

// parseOption reads one action reference, stripping the connective noise
// ("with its", "using") and the trailing "attack(s)" — "one with its bite"
// -> "bite", 1; "six pact blade attacks" -> "pact blade", 6.
func parseOption(clause string) (string, int, bool) {
	count := 0
	var kept []string
	for _, w := range strings.Fields(strings.TrimSpace(clause)) {
		if n, ok := numberWords[w]; ok && count == 0 && len(kept) == 0 {
			count = n
			continue
		}
		if n, err := strconv.Atoi(w); err == nil && count == 0 && len(kept) == 0 {
			count = n
			continue
		}
		w = strings.TrimSuffix(w, "'s")
		switch w {
		case "with", "its", "a", "an", "uses", "use", "using", "of", "make", "makes",
			"attack", "attacks", "action", "actions":
			continue
		}
		kept = append(kept, w)
	}
	name := strings.Join(kept, " ")
	for _, suffix := range []string{" in any combination", " in any order", " in any mix"} {
		name = strings.TrimSuffix(name, suffix)
	}
	if name == "" || strings.ContainsAny(name, ":(") {
		return "", 0, false
	}
	return name, count, true
}

// ResolveComponent matches a multiattack component to one of the
// statblock's own actions by squashed, plural-tolerant name, returning the
// action's index or -1.
func ResolveComponent(s Statblock, name string) int {
	want := squash(name)
	if want == "" {
		return -1
	}
	for i, a := range s.Actions {
		if strings.EqualFold(a.Name, "multiattack") {
			continue
		}
		if a.Parsed && squash(a.Name) == want {
			return i
		}
	}
	// Plural forms: the books write "claws" where the action is "Claw".
	for i, a := range s.Actions {
		if a.Parsed && pluralSquash(squash(a.Name)) == pluralSquash(want) {
			return i
		}
	}
	return -1
}

func pluralSquash(s string) string {
	return strings.TrimSuffix(strings.TrimSuffix(s, "es"), "s")
}

func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

/* ---------- compiled patterns ---------- */

var (
	reToHit2014    = regexp.MustCompile(`\+(\d+) to hit`)
	reToHit2024    = regexp.MustCompile(`attack roll: \+(\d+)`)
	reReach        = regexp.MustCompile(`reach (\d+) ft`)
	reRange        = regexp.MustCompile(`range (\d+)(?:/\d+)? ft`)
	reSave2014     = regexp.MustCompile(`dc (\d+) (strength|dexterity|constitution|intelligence|wisdom|charisma) saving throw`)
	reSave2024     = regexp.MustCompile(`(strength|dexterity|constitution|intelligence|wisdom|charisma) saving throw: dc (\d+)`)
	reTargets      = regexp.MustCompile(`(each creature[^.,;]*|every creature[^.,;]*|one target|one creature[^.,;]*|one object[^.,;]*|all creatures[^.,;]*)`)
	reDamage       = regexp.MustCompile(`(\d+) \((\d+d\d+(?: ?[+-] ?\d+)?)\)(?: (\w+))? damage`)
	rePlusDamage   = regexp.MustCompile(`plus (\d+) \((\d+d\d+(?: ?[+-] ?\d+)?)\) (\w+) damage`)
	reTotalAttacks = regexp.MustCompile(`\b(one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|\d+) attacks?\b`)
)

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func abilityShort(word string) string {
	switch word {
	case "strength":
		return "Str"
	case "dexterity":
		return "Dex"
	case "constitution":
		return "Con"
	case "intelligence":
		return "Int"
	case "wisdom":
		return "Wis"
	case "charisma":
		return "Cha"
	}
	return word
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
