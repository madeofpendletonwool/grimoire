package homebrew

// The model pass (MAD-385, check 3's prose half): the model writes the
// comparison up from the retrieved passages and the computed findings. It
// may not originate a finding, and it may not assert a rules claim
// without a citation or a computed basis — the same constraint the
// encounter builder puts on it, and the reason its output is trustworthy
// there.
//
// Two mechanisms hold that line:
//
//   - Findings are computed before the model runs and are never parsed
//     out of its response. There is no shape by which its output could
//     become a finding; the write-up is prose, or nothing.
//   - The prose gate (CheckWriteUp) reads what it did write: any number
//     that traces to nothing in the engine's own output, or a
//     legal/illegal verdict, rejects the whole write-up. The gate fails
//     closed — a rejected write-up is not shown, and the findings stand
//     unchanged.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Completion is one model response with its token accounting. The shape
// mirrors the canon engine's, so the same adapters serve both.
type Completion struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// ModelClient is the slice of the LLM surface the write-up needs: one
// non-streaming prompt exchange. The canon engine's model adapters
// satisfy this interface as-is.
type ModelClient interface {
	ModelName() string
	Complete(ctx context.Context, system, user string) (Completion, error)
}

// Write-up states.
const (
	WriteUpWritten     = "written"
	WriteUpRejected    = "rejected"
	WriteUpUnavailable = "unavailable"
)

const writeUpSystemPrompt = `You are writing a DM's comparison note for a homebrew linter. You are a reviewer, not a referee.

Rules you may not break:
- You may not originate a finding. The findings are given to you; write from them.
- You may not assert a number that is not in the material you were handed. Every figure you write must appear in the findings, the neighbours, or the numbers block.
- You may not state or imply that anything is legal, illegal, allowed, or banned. There is no such verdict here, ever.
- Cite the retrieved neighbours by their titles when you compare.
- Write at most 180 words of Markdown prose. No headings, no tables — a short, plain comparison the DM reads at the table.`

// writeUp asks the model for the comparison prose and gates it. On any
// failure the report records why and the findings stand untouched.
func (e *Engine) writeUp(ctx context.Context, rep *Report, requested string, numbers any) {
	if e.Model == nil {
		rep.WrittenUp = WriteUpUnavailable
		rep.WriteUpNote = "no model configured — the structural and computed checks ran without one, and that is enough to review"
		return
	}
	// The gate's allowlist is exactly the context the model was shown:
	// marshal once, use twice.
	engineJSON := contextJSON(rep, requested, numbers)
	if engineJSON == nil {
		rep.WrittenUp = WriteUpUnavailable
		rep.WriteUpNote = "the engine context could not be marshalled — the write-up was skipped rather than ungated"
		return
	}
	user := fmt.Sprintf(
		"Homebrew under review (%s): %s\n\nEverything the engine computed — findings, retrieved neighbours, and the numbers. Write the comparison from this and nothing else:\n\n%s",
		rep.Kind, rep.Name, engineJSON)
	comp, err := e.Model.Complete(ctx, writeUpSystemPrompt, user)
	if err != nil {
		rep.WrittenUp = WriteUpUnavailable
		rep.WriteUpNote = fmt.Sprintf("the model pass failed (%v) — the findings stand on their own", err)
		return
	}
	prose := strings.TrimSpace(comp.Text)
	if violations := CheckWriteUp(engineJSON, prose); len(violations) > 0 {
		rep.WrittenUp = WriteUpRejected
		var details []string
		for _, v := range violations {
			details = append(details, v.Detail)
		}
		rep.WriteUpNote = "the write-up was rejected by the prose gate and is not shown: " +
			strings.Join(details, "; ")
		return
	}
	rep.WrittenUp = WriteUpWritten
	rep.WriteUp = prose
}

/* ---------- the prose gate ---------- */

// WriteUpViolation is one thing in the model's prose that traces to
// nothing the engine produced.
type WriteUpViolation struct {
	Token  string `json:"token"`
	Detail string `json:"detail"`
}

var (
	// diceTokenRE lifts dice grammar out before number-scanning: "2d6"
	// is two tokens that usually appear verbatim in the engine context.
	diceTokenRE = regexp.MustCompile(`(?i)\b\d{1,3}\s*d\s*\d{1,3}\b([+-]\s*\d{1,3})?`)
	// numberTokenRE matches numeric tokens, not ones inside words.
	numberTokenRE = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)
	// verdictTokenRE matches the verdict the surface may never express —
	// including inside hyphenated compounds ("game-legal").
	verdictTokenRE = regexp.MustCompile(`(?i)\b(legal|illegal)\b`)
)

// CheckWriteUp reads the model's write-up and returns every number in it
// that traces to nothing in the engine's output, plus any legal/illegal
// verdict. An empty result means the prose may be shown. This is the pass
// a fake model cannot talk its way past: any figure it invents has no
// derivation in the engine context, and no derivation, no number.
func CheckWriteUp(engineJSON []byte, prose string) []WriteUpViolation {
	var out []WriteUpViolation
	prose = strings.TrimSpace(prose)
	if prose == "" {
		return append(out, WriteUpViolation{
			Token:  "",
			Detail: "the model returned no prose",
		})
	}
	context := string(engineJSON)
	if m := verdictTokenRE.FindAllStringSubmatch(prose, -1); len(m) > 0 {
		seen := map[string]bool{}
		var words []string
		for _, v := range m {
			w := strings.ToLower(v[1])
			if !seen[w] {
				seen[w] = true
				words = append(words, w)
			}
		}
		out = append(out, WriteUpViolation{
			Token: strings.Join(words, ", "),
			Detail: fmt.Sprintf(
				"verdict language (%s) — the linter is a reviewer, not a referee, and never expresses a legal or illegal verdict",
				strings.Join(words, ", ")),
		})
	}
	cleaned := diceTokenRE.ReplaceAllString(prose, " ")
	seen := map[string]bool{}
	for _, tok := range numberTokenRE.FindAllString(cleaned, -1) {
		tok = strings.ReplaceAll(tok, ",", "")
		// Whole and decimal forms both check as substrings: the context
		// is the engine's own output, so any figure it produced appears
		// in it verbatim somewhere.
		if strings.Contains(context, tok) {
			continue
		}
		if _, err := strconv.ParseFloat(tok, 64); err != nil {
			continue
		}
		if seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, WriteUpViolation{
			Token:  tok,
			Detail: fmt.Sprintf("%s traces to nothing the engine computed — the write-up may not assert a number", tok),
		})
	}
	return out
}
