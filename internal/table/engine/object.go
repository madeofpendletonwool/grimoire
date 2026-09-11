package engine

// Objects: anything the log tracks by identity — a resolved card on the
// stack or battlefield, a token, a copy, a commander in the command zone.
// Ids are minted by the engine, monotonic per game, and stable across zone
// moves: object 88 on the stack is object 88 when it resolves.

// TokenSpec is a token's declared characteristics. Tokens are never card
// references — a 1/1 Soldier is data on the event that created it, which is
// what lets CREATE_TOKEN work with no card database in the engine at all.
type TokenSpec struct {
	Name      string   `json:"name,omitempty"`
	Types     []string `json:"types,omitempty"`
	Colors    []string `json:"colors,omitempty"`
	Power     *int     `json:"power,omitempty"`
	Toughness *int     `json:"toughness,omitempty"`
	Loyalty   *int     `json:"loyalty,omitempty"`
}

// Identity is what an object is: a card name resolved against the
// known-card universe, or a token spec, or neither. Neither is a real state,
// not a fallback: "a card, we don't know which" enters the log honestly —
// milled face-down, exiled while unknown — instead of collapsing into a
// name nobody actually confirmed.
type Identity struct {
	Card  string     `json:"card,omitempty"`
	Token *TokenSpec `json:"token,omitempty"`
}

// BaseChars are the characteristics an object enters with, declared by the
// caller from carddb or the token spec. The engine never reads oracle text
// (ADR 11): whatever is declared here is the base everything else modifies.
// Empty fields mean unknown, and unknown stays unknown.
type BaseChars struct {
	Name      string   `json:"name,omitempty"`
	Types     []string `json:"types,omitempty"`
	Colors    []string `json:"colors,omitempty"`
	Keywords  []string `json:"keywords,omitempty"`
	Power     *int     `json:"power,omitempty"`
	Toughness *int     `json:"toughness,omitempty"`
	Loyalty   *int     `json:"loyalty,omitempty"`
}

// Object is one tracked thing. Controller is the base controller; control
// changes are layer-2 modifiers layered on top at read time, never writes to
// this field. Counters and modifiers are per object; damage is marked
// damage, cleared at TURN_ENDED exactly like until-end-of-turn modifiers.
type Object struct {
	ID         int64          `json:"id"`
	Identity   Identity       `json:"identity"`
	Base       BaseChars      `json:"base"`
	Owner      int            `json:"owner"`
	Controller int            `json:"controller"`
	Zone       Zone           `json:"zone"`
	Tapped     bool           `json:"tapped,omitempty"`
	Phased     bool           `json:"phased,omitempty"`
	Counters   map[string]int `json:"counters,omitempty"`
	// Modifiers are the ordered effect stack — order is timestamp order,
	// which for this stage is arrival order (see Modifier). Computed
	// characteristics are base + these + counters, never stored flattened.
	Modifiers []Modifier `json:"modifiers,omitempty"`
	// AttachedTo is the object this one is attached to (aura, equipment);
	// Attachments is everything attached to this one. ATTACHED / UNATTACHED
	// events make and break the edge, and the engine detaches both
	// directions when either end leaves the battlefield.
	AttachedTo  int64   `json:"attached_to,omitempty"`
	Attachments []int64 `json:"attachments,omitempty"`
	// Damage is marked damage on a creature. It persists until cleanup,
	// so it clears on TURN_ENDED; lethal-damage deaths are state-based
	// actions, asserted as DIED rows by the sweep.
	Damage int `json:"damage,omitempty"`
}

// IsCreature reports whether the object's computed type line contains
// Creature — the check CREATURE_ETB and combat arithmetic key on.
func (s *State) IsCreature(id int64) bool {
	c := s.Characteristics(id)
	return hasString(c.Types, "Creature")
}

// isCommander reports whether the object is the commander card of its
// owner's seat. Commander damage derivation needs exactly this predicate
// over DAMAGE_DEALT rows; partner pairs match per name.
func (s *State) isCommander(o *Object) bool {
	if o.Identity.Card == "" {
		return false
	}
	p, ok := s.Seats[o.Owner]
	return ok && p.Commander == o.Identity.Card
}

// hasString is a case-insensitive membership check — type lines and
// keywords are stored in whatever case the declarer used.
func hasString(list []string, want string) bool {
	for _, s := range list {
		if equalFold(s, want) {
			return true
		}
	}
	return false
}

// equalFold is strings.EqualFold without the import; ASCII is plenty for
// Magic's vocabulary.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
