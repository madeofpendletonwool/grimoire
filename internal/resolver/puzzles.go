package resolver

// Puzzle is a canned Commander interaction used to exercise and demonstrate the
// resolver. Expected lists the rule numbers and outcomes a correct trace should
// mention; it documents the manual-verification bar for the acceptance criteria
// and doubles as a fixture for the grounding and prompt tests.
type Puzzle struct {
	Name        string
	Description string
	Board       string
	Sequence    string
	Note        string
	Expected    []string
}

// Puzzles is the canned set. Each is a classic Commander interaction that is
// hard to resolve correctly from memory and hard to google: they exercise
// APNAP trigger ordering, dies-trigger counting, and a replacement effect
// preempting a dies trigger.
//
// These are the puzzles the acceptance criteria call for; run them in the UI
// (Resolve mode) against a configured model and check the trace against
// Expected.
var Puzzles = []Puzzle{
	{
		Name:        "Blood Artist and Zulaport Cutthroat vs. a board wipe",
		Description: "Several dies triggers from one controller fire off a single Wrath of God; the question is how many trigger and the order they resolve.",
		Board: "You: Blood Artist\n" +
			"You: Zulaport Cutthroat\n" +
			"You: Doomed Traveler\n",
		Sequence: "1. Opp casts Wrath of God\n",
		Note:     "It is the opponent's turn.",
		Expected: []string{
			"603.2",  // a trigger fires each time its event happens — once per creature that dies
			"603.3b", // one controller, so that controller stacks their triggers in any order
			"117",    // the stack resolves top-down
			"each creature that dies triggers Blood Artist and Zulaport Cutthroat once",
		},
	},
	{
		Name:        "Fleshbag Marauder across two players (true APNAP)",
		Description: "A single enters-the-battlefield trigger makes both players sacrifice, so dies triggers from two different controllers compete for the stack — the textbook APNAP ordering puzzle.",
		Board: "You: Blood Artist\n" +
			"Opp: Grim Haruspex\n",
		Sequence: "1. You cast Fleshbag Marauder\n" +
			"2. Its enters-the-battlefield trigger resolves (each player sacrifices a creature)\n",
		Note: "It is your turn (you are the active player).",
		Expected: []string{
			"603.3b", // APNAP: active player puts triggers on first, so the non-active player's resolves first
			"opponent's Grim Haruspex trigger resolves before your Blood Artist trigger",
			"117",
		},
	},
	{
		Name:        "Rest in Peace stops dies triggers (replacement vs. trigger)",
		Description: "A replacement effect exiles a creature instead of putting it in the graveyard, so an ability that triggers on a creature dying never sees the event — the famously counterintuitive replacement-vs-trigger interaction.",
		Board: "You: Rest in Peace\n" +
			"Opp: Blood Artist\n",
		Sequence: "1. Opp casts Wrath of God\n",
		Note:     "Rest in Peace reads: if a card or token would be put into a graveyard from anywhere, exile it instead.",
		Expected: []string{
			"616.1", // replacement effects apply as the event would happen, before it
			"dies means moving from the battlefield to the graveyard",
			"Blood Artist does not trigger because the creature is exiled, not put into the graveyard",
		},
	},
}

// CannedInputs parses Puzzles into structured Input values, one per puzzle.
func CannedInputs() []Input {
	out := make([]Input, 0, len(Puzzles))
	for _, p := range Puzzles {
		out = append(out, ParseInput(p.Board, p.Sequence, p.Note))
	}
	return out
}
