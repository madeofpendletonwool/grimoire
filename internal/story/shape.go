package story

// Shape: the legal act structures — three-act, four-act, and five-act with a
// mid turn — and what each act is for. The direct analogue of
// encounter.Shape: the deterministic answer to "what can I even build", so
// the DM picks a structure the way they pick an encounter's build and the
// generators (stage 2) inherit a shape that means something.

// ActRole is one act's place in a structure: its key, its label, and what it
// is for.
type ActRole struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Purpose string `json:"purpose"`
}

// ActShape is one legal structure for a whole campaign.
type ActShape struct {
	Key   string    `json:"key"`
	Label string    `json:"label"`
	Acts  []ActRole `json:"acts"`
}

// shapes is the whole catalogue. Five acts is the ceiling a D&D campaign
// sustains — past that the acts are scenes wearing coats.
var shapes = []ActShape{
	{
		Key:   "three_act",
		Label: "Three acts",
		Acts: []ActRole{
			{Key: "setup", Label: "Setup", Purpose: "Introduce the world, the party and the problem; end on the first real turn."},
			{Key: "confrontation", Label: "Confrontation", Purpose: "Escalate through reversals the party answers with harder choices."},
			{Key: "resolution", Label: "Resolution", Purpose: "Pay everything off; the consequences of the choices land."},
		},
	},
	{
		Key:   "four_act",
		Label: "Four acts",
		Acts: []ActRole{
			{Key: "setup", Label: "Setup", Purpose: "Introduce the world, the party and the problem; end on the first real turn."},
			{Key: "complication", Label: "Complication", Purpose: "The problem turns out to be bigger, older or closer than it looked."},
			{Key: "climax", Label: "Climax", Purpose: "Drive everything to the confrontation the campaign has been walking toward."},
			{Key: "resolution", Label: "Resolution", Purpose: "Pay everything off; the consequences of the choices land."},
		},
	},
	{
		Key:   "five_act",
		Label: "Five acts with a mid turn",
		Acts: []ActRole{
			{Key: "exposition", Label: "Exposition", Purpose: "Introduce the world, the party and the problem."},
			{Key: "rising", Label: "Rising action", Purpose: "Escalate the stakes and gather the pieces the ending will need."},
			{Key: "mid_turn", Label: "The mid turn", Purpose: "The reversal that reframes what the party thought they were doing."},
			{Key: "falling", Label: "Falling action", Purpose: "The pieces move toward the collision; choices narrow."},
			{Key: "catastrophe", Label: "Catastrophe and denouement", Purpose: "The collision itself, then the world after it."},
		},
	},
}

// Shapes lists every legal act structure, fewest acts first. The acts are
// copied, so a caller mutating the catalogue cannot corrupt it.
func Shapes() []ActShape {
	out := make([]ActShape, len(shapes))
	for i, sh := range shapes {
		out[i] = ActShape{Key: sh.Key, Label: sh.Label, Acts: append([]ActRole(nil), sh.Acts...)}
	}
	return out
}

// Shape returns the legal structure for an act count. The second return is
// false when the count has no named structure — anything but three, four or
// five — which is the planner's cue to say so rather than invent one.
func Shape(actCount int) (ActShape, bool) {
	for _, sh := range shapes {
		if len(sh.Acts) == actCount {
			return sh, true
		}
	}
	return ActShape{}, false
}
