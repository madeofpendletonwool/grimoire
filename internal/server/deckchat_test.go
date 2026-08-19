package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The reported bug end to end: a decklist exported with set codes resolved as
// a fraction of its cards, and the ones that "matched" matched the wrong card.
func TestDeckAnalyzeStripsPrintingAnnotations(t *testing.T) {
	s := newDeckServer(t, nil)
	list := strings.Join([]string{
		"Commander (1)",
		"1 Kaalia of the Vast [40K]",
		"",
		"Creatures (1)",
		"1 Angel of Serenity (C15) 12",
		"",
		"Artifacts (1)",
		"1 Sol Ring (LTC) 123 *F*",
		"",
		"Lands (36)",
		"36 Mountain [10E]",
	}, "\n")

	rec, body := postJSON(t, s, "/api/deck/analyze", fmt.Sprintf(`{"decklist":%q}`, list))
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze: %d %s", rec.Code, rec.Body)
	}
	if unres, _ := body["unresolved"].([]any); len(unres) != 0 {
		t.Fatalf("unresolved = %v, want none", unres)
	}
	analysis, _ := body["analysis"].(map[string]any)
	if analysis["identity"] != "BRW" {
		t.Fatalf("identity = %v — the commander line did not resolve", analysis["identity"])
	}
	if got := analysis["total_main"]; got != float64(38) {
		t.Fatalf("total_main = %v, want 38", got)
	}
	// The commander is listed in the command zone, not twice.
	commanderRows := 0
	for _, e := range body["deck"].([]any) {
		row := e.(map[string]any)
		if row["name"] == "Kaalia of the Vast" {
			commanderRows++
			if row["board"] != "commander" {
				t.Errorf("commander sits on board %v", row["board"])
			}
		}
	}
	if commanderRows != 1 {
		t.Errorf("commander appears %d times in the list", commanderRows)
	}
	// Every entry kept its real name, and none picked up a wrong-card match.
	names := map[string]bool{}
	for _, e := range body["deck"].([]any) {
		names[e.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"Angel of Serenity", "Sol Ring", "Mountain"} {
		if !names[want] {
			t.Errorf("missing %q from the resolved deck: %v", want, names)
		}
	}
}

// A name that is genuinely not a card is reported, not swapped for the nearest
// text-search hit — the behaviour that turned "Command Tower" into "Command
// Power Plant".
func TestDeckAnalyzeReportsRatherThanGuesses(t *testing.T) {
	s := newDeckServer(t, nil)
	rec, body := postJSON(t, s, "/api/deck/analyze", `{"decklist":"1 Command Tower\n1 Sol Ring"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze: %d %s", rec.Code, rec.Body)
	}
	unres, _ := body["unresolved"].([]any)
	if len(unres) != 1 {
		t.Fatalf("unresolved = %v, want just Command Tower", unres)
	}
	if got := unres[0].(map[string]any)["name"]; got != "Command Tower" {
		t.Fatalf("unresolved name = %v", got)
	}
	for _, e := range body["deck"].([]any) {
		if name := e.(map[string]any)["name"]; name != "Sol Ring" {
			t.Fatalf("unexpected resolved card %v", name)
		}
	}
}

// Repeated lines for one card fold into a single entry rather than listing the
// same card twice.
func TestDeckAnalyzeMergesDuplicateLines(t *testing.T) {
	s := newDeckServer(t, nil)
	_, body := postJSON(t, s, "/api/deck/analyze", `{"decklist":"1 Mountain\n3 Mountain [10E]\n1 mountain"}`)
	entries, _ := body["deck"].([]any)
	if len(entries) != 1 {
		t.Fatalf("deck = %v, want one merged entry", entries)
	}
	if got := entries[0].(map[string]any)["count"]; got != float64(5) {
		t.Fatalf("count = %v, want 5", got)
	}
}

func TestDeckChatAnswersOverTheList(t *testing.T) {
	var seen struct {
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	s := newDeckServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		sseStub("Cut Counterspell — it is outside Kaalia's identity.")(w, r)
	})

	body := `{"commander":"Kaalia of the Vast",
	          "cards":[{"name":"Sol Ring","count":1},{"name":"Angel of Serenity","count":1},{"name":"Mountain","count":36}],
	          "question":"What should I cut for more ramp?",
	          "history":[{"role":"user","content":"is this deck fast?"},{"role":"assistant","content":"not very"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/deck/chat", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	for _, want := range []string{"event: delta", "event: done", "Cut Counterspell"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %q in stream:\n%s", want, raw)
		}
	}

	// The earlier turns were replayed, and the final turn carries the real
	// list and the computed analysis rather than the question alone.
	if len(seen.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (history + question): %+v", len(seen.Messages), seen.Messages)
	}
	if seen.Messages[0].Role != "user" || !strings.Contains(seen.Messages[0].Content, "is this deck fast?") {
		t.Fatalf("history not replayed: %+v", seen.Messages[0])
	}
	last := seen.Messages[2].Content
	for _, want := range []string{
		"Kaalia of the Vast",
		"Oracle text:",
		"Angel of Serenity",
		"Analysis (authoritative",
		"What should I cut for more ramp?",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("grounding is missing %q:\n%s", want, last)
		}
	}
}

func TestDeckChatValidates(t *testing.T) {
	s := newDeckServer(t, sseStub("hello"))
	if rec, _ := postJSON(t, s, "/api/deck/chat", `{"commander":"Kaalia of the Vast"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty question: %d", rec.Code)
	}
	// Without a model the surface says so plainly rather than streaming an
	// empty answer.
	unconfigured := newDeckServer(t, nil)
	rec, _ := postJSON(t, unconfigured, "/api/deck/chat", `{"question":"how is my curve?"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d %s", rec.Code, rec.Body)
	}
}
