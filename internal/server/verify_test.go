package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The point of the app is that an answer is made of real rule text. Nothing
// enforced that on the numbers in the prose: a half-remembered "608.2c"
// rendered as the same clickable citation as a real one. Verification resolves
// each number against the index so an invented one is marked, not offered.
func TestCitationsFlagRulesThatDoNotExist(t *testing.T) {
	s, store := newAskServer(t, stubAsk("unused", nil))
	indexDataset(t, store, "702.6", "Keyword Abilities", "Equip")

	body, _ := json.Marshal(map[string]any{
		"corpus":  "mtg",
		"numbers": []string{"702.6", "999.9z", "702.6"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/citations", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Checks []citationCheck `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The repeat is collapsed; each distinct number is checked once.
	if len(out.Checks) != 2 {
		t.Fatalf("checks = %+v, want 2", out.Checks)
	}
	if out.Checks[0].Number != "702.6" || out.Checks[0].Status != citeIndexed {
		t.Errorf("real rule = %+v, want indexed", out.Checks[0])
	}
	// The rule's own text rides along, so the chip can show what it says
	// instead of asking the reader to take the citation on trust.
	if !strings.Contains(out.Checks[0].Body, "Equip") {
		t.Errorf("indexed check carried no rule text: %+v", out.Checks[0])
	}
	if out.Checks[1].Number != "999.9z" || out.Checks[1].Status != citeUnknown {
		t.Errorf("invented rule = %+v, want unknown", out.Checks[1])
	}
}

// D&D cites section titles; a number in SRD prose is a spell level or a die
// size. Checking those would mark ordinary prose as a broken citation.
func TestCitationsSkipUnnumberedCorpora(t *testing.T) {
	s, _ := newAskServer(t, stubAsk("unused", nil))
	body, _ := json.Marshal(map[string]any{"corpus": "dnd", "numbers": []string{"5.1"}})
	req := httptest.NewRequest(http.MethodPost, "/api/citations", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var out struct {
		Checks []citationCheck `json:"checks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Checks) != 0 {
		t.Errorf("checks = %+v, want none", out.Checks)
	}
}
