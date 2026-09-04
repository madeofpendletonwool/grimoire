package campaign

// The possession vocabulary, shared by every reader of "who holds what"
// (MAD-384). The canon engine's continuity checks have read possession
// facts through a closed predicate set since MAD-309; the loot surface
// needs exactly the same set to date and count hand-outs, and two copies
// of the vocabulary would drift the moment one side learned a predicate
// the other did not. It lives here — the lowest layer both read — and
// both read through it.

import "strings"

// PossessionPredicates are the fact predicates read as "this entity is
// somewhere or with someone": held_by, carried_by, possessed_by and their
// kin. The closed set is a heuristic, deliberately: predicates are
// free-form verb phrases, so the join trades recall for never guessing
// wrong. A hand-out recorded some other way is invisible to the fold
// rather than mis-attributed.
var PossessionPredicates = map[string]bool{
	"held_by": true, "carried_by": true, "possessed_by": true, "bears": true,
	"holds": true, "has": true, "keeps": true, "kept_in": true,
	"stored_in": true, "stashed_in": true, "hidden_in": true, "located_in": true,
}

// NormalizePredicate lowercases and trims a predicate so the closed
// vocabularies match what free-form authoring actually produces.
func NormalizePredicate(p string) string {
	return strings.ToLower(strings.TrimSpace(p))
}
