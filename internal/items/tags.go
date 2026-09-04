package items

// Derived tags (MAD-383). The vocabulary was not declared up front — it
// was read off the corpus, the way the creature tags were: what an item
// does in play is what a DM picks it for, and every rule below names one
// of those things in the words the printed items actually use. A tag is
// present because the item's category or its own text says so, never
// because a model guessed, so the filter, the rarity bands and the
// nearest-neighbour read all share one explainable vocabulary.

import (
	"regexp"
	"sort"
	"strings"
)

// textTagRules are matched against the item's printed text. They name the
// things a DM actually reaches for an item to do. Most are keyword rules;
// the ones whose printed shapes vary get a regex.
var itemTextTagRules = []tagRule{
	{"offensive", []string{"you deal", "target takes", "creature takes", "the target must succeed"}, nil},
	{"defensive", []string{"resistance to", "immunity to", "armor class"}, nil},
	{"save-boost", nil, regexp.MustCompile(`bonus to [a-z, ]{0,40}saving throws|advantage on [a-z ]{0,30}saving throws|saving throws against (spells|magic)`)},
	{"movement", []string{"flying speed", "swimming speed", "climbing speed",
		"flying equal to your walking speed", "your speed increases", "you are propelled"}, nil},
	{"utility", []string{"you can cast", "you can use an action", "understand", "darkvision",
		"see invisible", "detect", "speak with", "allow you to"}, nil},
	{"damage-rider", []string{"extra ", "one extra"}, nil},
}

// reExtraDamage is the printed shape a damage rider takes: "an extra 2d6
// fire damage", "deals an extra 1d6 damage of the weapon's type". It is
// what separates a rider on a hit from an activation that deals damage.
var reExtraDamage = regexp.MustCompile(`extra (\d+d\d+|\d+|one) [a-z ]{0,24}damage`)

// tagRule is one matching rule over an item's text: keywords, a regex,
// or both.
type tagRule struct {
	tag   string
	words []string
	re    *regexp.Regexp
}

// deriveTags reads an item's category and its own text and reports what
// the item does in play, so the filter, the designer's comparison and the
// prompt can talk about roles rather than raw numbers.
func deriveTags(it Item) []string {
	seen := map[string]bool{}
	var tags []string
	add := func(t string) {
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		tags = append(tags, t)
	}

	// Category: what the shelf itself says the item is.
	cat := strings.ToLower(strings.TrimSpace(it.Type))
	switch {
	case cat == "weapon" || strings.Contains(it.Text, "magic weapon"):
		add("weapon")
	case cat == "armor":
		add("armor")
	case cat == "potion" || cat == "scroll":
		add("consumable")
	}

	// Printed text: the interesting half.
	body := strings.ToLower(it.Name + "\n" + it.Text)
	for _, rule := range itemTextTagRules {
		matched := false
		for _, w := range rule.words {
			if strings.Contains(body, w) {
				matched = true
				break
			}
		}
		if !matched && rule.re != nil {
			matched = rule.re.MatchString(body)
		}
		if matched {
			add(rule.tag)
		}
	}
	// A damage rider is specifically a bonus on a hit, not an activation
	// that deals damage somewhere else — the regex demands the printed
	// "extra ... damage" shape.
	if reExtraDamage.MatchString(body) {
		add("damage-rider")
	}

	sort.Strings(tags)
	return tags
}
