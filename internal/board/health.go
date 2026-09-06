package board

// The health vocabulary (MAD-423). In word mode the board speaks the
// table's own words instead of numbers: bloodied is the 2014 rules' own
// term (half max or less), and the words around it are the ones every
// table already says out loud. The set is declared here — a strip never
// renders free text as health.

// HealthWord derives the word for a character's state. Dead outranks
// everything; with no known max there is no word to say; downed and
// zero are down; bloodied is hp at or below half max; hurt is anything
// less than full.
func HealthWord(hp, max int, downed, dead bool) string {
	switch {
	case dead:
		return "dead"
	case max <= 0:
		return ""
	case downed || hp <= 0:
		return "down"
	case hp == max:
		return "unhurt"
	case hp*2 <= max:
		return "bloodied"
	default:
		return "hurt"
	}
}

// HealthWords is the declared vocabulary, most dire first — surfaces
// order by it, tests enumerate it.
var HealthWords = []string{"unhurt", "hurt", "bloodied", "down", "dead"}
