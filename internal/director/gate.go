package director

// The citation gate (MAD-427): the pass a fake model cannot talk its
// way past. Three rules, all fail-closed — a suggestion that breaks
// any one is dropped, never repaired:
//
//  1. no suggestion survives without at least one valid citation —
//     unknown ids do not count;
//  2. no number the model asserts survives without appearing in the
//     text of a cited basis line — the per-suggestion spelling of the
//     constraint that makes the builder's output trustworthy;
//  3. no suggestion survives with empty action text.
//
// The gate is pure: a grounding and a raw model reply go in, gated
// suggestions and a drop count come out. Identical input, identical
// verdict, forever.

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// maxSuggestions bounds one advisory pass; the prompt asks for two to
// four and the gate keeps at most this many.
const maxSuggestions = 8

var (
	// diceTokenRE matches dice grammar ("2d6", "1d20"); dice
	// expressions are statblock vocabulary, not invented figures, so
	// they are lifted out before number checking on both sides.
	diceTokenRE = regexp.MustCompile(`(?i)\b\d{1,2}\s*d\s*(4|6|8|10|12|20|100)\b`)
	// numberTokenRE matches numeric tokens but not ones inside words.
	numberTokenRE = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)
)

// Gate reads the model's reply and returns the suggestions that
// earned passage. raw may carry prose around the fenced block; only
// the final fenced block counts, and a reply whose block will not
// parse is an error — the caller surfaces it rather than guessing.
func Gate(g *Grounding, raw string) ([]Suggestion, int, error) {
	block, ok := finalJSONBlock(raw)
	if !ok {
		return nil, 0, fmt.Errorf("the director's reply carried no json block")
	}
	var decoded struct {
		Suggestions []Suggestion `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(block), &decoded); err != nil {
		return nil, 0, fmt.Errorf("the director's reply did not parse: %w", err)
	}

	byID := make(map[string]Basis, len(g.Basis))
	for _, b := range g.Basis {
		byID[b.ID] = b
	}

	var out []Suggestion
	dropped := 0
	for _, s := range decoded.Suggestions {
		s.Action = strings.TrimSpace(s.Action)
		s.Actor = strings.TrimSpace(s.Actor)
		s.Reasoning = strings.TrimSpace(s.Reasoning)
		if len(out) >= maxSuggestions {
			dropped++
			continue
		}
		if s.Action == "" {
			dropped++
			continue
		}
		// Rule 1: at least one citation that resolves.
		var cited []Basis
		var ids []string
		seenID := map[string]bool{}
		for _, id := range s.BasisIDs {
			id = strings.TrimSpace(id)
			if seenID[id] {
				continue
			}
			seenID[id] = true
			b, ok := byID[id]
			if !ok {
				continue
			}
			cited = append(cited, b)
			ids = append(ids, id)
		}
		if len(cited) == 0 {
			dropped++
			continue
		}
		// Rule 2: every number in the suggestion's action and
		// reasoning appears in the text of a cited line.
		if inventedNumbers(s.Action+" "+s.Reasoning, cited) {
			dropped++
			continue
		}
		s.BasisIDs = ids
		out = append(out, s)
	}
	return out, dropped, nil
}

// inventedNumbers reports whether the suggestion text carries a number
// that none of its cited basis lines contains. Dice grammar is
// stripped from both sides first: "1d6+2" is statblock vocabulary, not
// a figure. Number words (two goblins) are prose, not figures, and
// ride ungated.
func inventedNumbers(text string, cited []Basis) bool {
	allowed := map[int]bool{}
	for _, b := range cited {
		clean := diceTokenRE.ReplaceAllString(b.Text, " ")
		for _, tok := range numberTokenRE.FindAllString(clean, -1) {
			addNumber(allowed, tok)
		}
	}
	clean := diceTokenRE.ReplaceAllString(text, " ")
	for _, tok := range numberTokenRE.FindAllString(clean, -1) {
		v, ok := numberValue(tok)
		if !ok {
			continue
		}
		if !allowed[v] {
			return true
		}
	}
	return false
}

// addNumber folds one token's value into the allowed set at its whole
// and rounded forms, the same double-count the tactics gate allows.
func addNumber(set map[int]bool, tok string) {
	if v, ok := numberValue(tok); ok {
		set[v] = true
	}
}

// numberValue parses one numeric token at its whole value, commas
// stripped; decimals round. A token that will not parse is ignored
// upstream, never guessed.
func numberValue(tok string) (int, bool) {
	v, err := strconv.ParseFloat(strings.ReplaceAll(tok, ",", ""), 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false
	}
	return int(v), true
}

// finalJSONBlock lifts the last fenced code block in the reply, the
// contract's required closing shape. An info string (```json) is
// dropped; anything after the closing fence disqualifies the block —
// it is table talk, not the contract.
func finalJSONBlock(text string) (string, bool) {
	close := strings.LastIndex(text, "```")
	if close < 0 {
		return "", false
	}
	if strings.TrimSpace(text[close+3:]) != "" {
		return "", false // the last fence is not the reply's tail
	}
	open := strings.LastIndex(text[:close], "```")
	if open < 0 {
		return "", false
	}
	block := text[open+3 : close]
	if nl := strings.IndexByte(block, '\n'); nl >= 0 && !strings.Contains(block[:nl], "{") {
		block = block[nl+1:] // drop the fence's info string
	}
	return strings.TrimSpace(block), true
}
