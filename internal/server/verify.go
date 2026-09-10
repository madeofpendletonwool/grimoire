package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/data"
)

// The whole promise of this app is that an answer is made of real rule text
// rather than a model's recollection of it. Grounding, anchoring and
// mid-answer lookups all serve that promise; none of them check it. A number
// can still reach the page because the model half-remembered it, and a
// confidently wrong "608.2c" is indistinguishable, to a reader, from a right
// one — it renders as the same clickable citation.
//
// So the rule numbers in a rendered answer are resolved against the index
// before they are offered as citations. Each comes back as one of:
//
//	indexed  a real rule — the reader gets its title and text on the chip,
//	         which is the check they would otherwise have to click to make
//	unknown  no such rule — the UI marks it as an invention instead of
//	         dressing it up as a source
//
// It runs at render time rather than as part of answering, so a conversation
// reloaded a week later is checked exactly as strictly as a live one.

// maxCitationChecks bounds one verification request. An answer citing more
// than this many distinct rules is not a citation list anyone is reading.
const maxCitationChecks = 60

// citationCheck is the verdict on one cited rule number.
type citationCheck struct {
	Number string `json:"number"`
	Status string `json:"status"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`
}

// Citation statuses.
const (
	citeIndexed = "indexed"
	citeUnknown = "unknown"
)

// citationBody caps the rule text returned for a chip's tooltip.
const citationBody = 400

// handleCitations resolves cited rule numbers against the index. Corpora that
// do not number their rules get an empty result: a number in SRD prose is a
// spell level or a die size, never a citation.
func (s *Server) handleCitations(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Corpus  string   `json:"corpus"`
		Numbers []string `json:"numbers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	corpus := parseCorpus(req.Corpus)
	checks := []citationCheck{}
	if corpus != data.CorpusMTG {
		writeJSON(w, http.StatusOK, map[string]any{"checks": checks})
		return
	}

	seen := map[string]bool{}
	for _, num := range req.Numbers {
		num = strings.TrimSpace(num)
		if num == "" || seen[num] || len(checks) >= maxCitationChecks {
			continue
		}
		seen[num] = true
		rule, err := s.store.Rule(r.Context(), corpus, num)
		if err != nil {
			// A failed lookup must never downgrade a citation to "invented".
			// Omitting it leaves the number rendering as an ordinary
			// reference, which is the honest outcome.
			continue
		}
		if rule == nil {
			checks = append(checks, citationCheck{Number: num, Status: citeUnknown})
			continue
		}
		checks = append(checks, citationCheck{
			Number: num, Status: citeIndexed, Title: rule.Title,
			Body: truncateText(rule.Body, citationBody),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": checks})
}

// truncateText shortens body text for a tooltip without cutting mid-word.
func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return cut + "…"
}
