package resolver

import (
	"regexp"
	"strings"
)

// Input is one resolver request: the battlefield, the proposed spell/ability
// sequence, and any free-text clarifications.
type Input struct {
	Board    Board    `json:"board"`
	Sequence Sequence `json:"sequence"`
	Note     string   `json:"note,omitempty"`
}

// Board is the set of permanents on the battlefield.
type Board struct {
	Permanents []Permanent `json:"permanents"`
}

// Permanent is one card on the board with its controller and any relevant
// state. Counters/Note are free-form so the reader can convey anything the
// rules care about (a +1/+1 count, an aura, a choice made as it entered).
type Permanent struct {
	Name       string `json:"name"`
	Controller string `json:"controller,omitempty"`
	Tapped     bool   `json:"tapped,omitempty"`
	Counters   string `json:"counters,omitempty"`
	Note       string `json:"note,omitempty"`
}

// Sequence is the proposed order of spells and abilities to resolve.
type Sequence struct {
	Steps []Step `json:"steps"`
}

// Step is one spell or activated/triggered ability, in the order the reader
// proposes to play it.
type Step struct {
	Text       string `json:"text"`
	Controller string `json:"controller,omitempty"`
}

// ParseInput turns the line-oriented board and sequence text the UI sends into
// a structured Input. The grammar is intentionally forgiving so a player at the
// table can type it quickly:
//
//	board      = { board-line }
//	board-line = [ controller ":" ] card-name [ "#" state ]
//	sequence   = { step-line }
//	step-line  = [ number "." | number ")" ] [ controller ":" ] action
//
// A controller is a short token before the first colon ("Me", "Opp", "P2").
// State after "#" carries annotations like "tapped" or "+1/+1: 2". Blank lines
// and lines starting with "#" or "//" are ignored in either block.
func ParseInput(board, sequence, note string) Input {
	return Input{
		Board:    Board{Permanents: parseBoard(board)},
		Sequence: Sequence{Steps: parseSequence(sequence)},
		Note:     strings.TrimSpace(note),
	}
}

func parseBoard(text string) []Permanent {
	var out []Permanent
	for _, line := range splitLines(text) {
		p, ok := parsePermanent(line)
		if !ok {
			continue
		}
		out = append(out, p)
	}
	return out
}

func parseSequence(text string) []Step {
	var out []Step
	for _, line := range splitLines(text) {
		s, ok := parseStep(line)
		if !ok {
			continue
		}
		out = append(out, s)
	}
	return out
}

// leadingNumberRe strips "1. " / "2) " step numbering at the start of a line.
var leadingNumberRe = regexp.MustCompile(`^\s*\d+[.)]\s+`)

// annotationRe pulls a trailing " # ..." state annotation off a board line.
var annotationRe = regexp.MustCompile(`\s+#\s+.*$`)

// markerTappedRe detects an inline [tapped] or (tapped) marker on a board line.
var markerTappedRe = regexp.MustCompile(`(?i)\[(?:tapped|t)\]|\(tapped\)`)

func parsePermanent(line string) (Permanent, bool) {
	line = strings.TrimSpace(line)
	if isSkipLine(line) {
		return Permanent{}, false
	}
	controller, rest := splitController(line)
	note := ""
	if m := annotationRe.FindString(rest); m != "" {
		note = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m), "#"))
		rest = annotationRe.ReplaceAllString(rest, "")
	}
	tapped := strings.Contains(strings.ToLower(note), "tapped") || markerTappedRe.MatchString(rest)
	name := strings.TrimSpace(markerTappedRe.ReplaceAllString(rest, ""))
	name = strings.Trim(name, " []()")
	counters := extractCounters(note)
	if name == "" {
		return Permanent{}, false
	}
	return Permanent{
		Name:       name,
		Controller: controllerName(controller),
		Tapped:     tapped,
		Counters:   counters,
		Note:       note,
	}, true
}

func parseStep(line string) (Step, bool) {
	line = strings.TrimSpace(line)
	if isSkipLine(line) {
		return Step{}, false
	}
	line = leadingNumberRe.ReplaceAllString(line, "")
	controller, rest := splitController(line)
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return Step{}, false
	}
	// The actor usually lives in the prose ("Opp casts …"), so a missing
	// controller is left empty rather than forced to "You" — that keeps the
	// step text truthful and lets the model read who is acting.
	return Step{Text: rest, Controller: rawController(controller)}, true
}

// splitController splits a leading "controller:" prefix off a line. A colon is
// treated as a controller only when the left side is a short, space-free token
// — so a card name that happens to contain a colon is not misread.
func splitController(line string) (controller, rest string) {
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", line
	}
	left := strings.TrimSpace(line[:idx])
	if len(left) > 20 || strings.ContainsAny(left, ".,;") {
		return "", line
	}
	return left, strings.TrimSpace(line[idx+1:])
}

// extractCounters pulls a "+1/+1: N" style counter annotation out of a note,
// so the model gets the count without re-parsing free text.
func extractCounters(note string) string {
	note = strings.ToLower(note)
	idx := strings.Index(note, "counter")
	if idx < 0 {
		return ""
	}
	head := note[:idx]
	head = strings.TrimSpace(head)
	if head == "" {
		return ""
	}
	return head
}

func controllerName(c string) string {
	c = strings.TrimSpace(c)
	if c == "" {
		return "You"
	}
	return c
}

// rawController trims a controller token without defaulting, used for sequence
// steps where the actor may live in the prose instead.
func rawController(c string) string {
	return strings.TrimSpace(c)
}

func isSkipLine(line string) bool {
	if line == "" {
		return true
	}
	line = strings.TrimSpace(line)
	return line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//")
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Split(text, "\n")
}
