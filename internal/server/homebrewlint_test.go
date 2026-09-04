package server

// The homebrew linter's handler tests (MAD-385): the reviewer endpoint
// serves findings that all carry bases, the model pass cannot originate
// or alter a finding even through HTTP, a foreign owner's monster is
// invisible, and no response at any layer expresses a legal/illegal
// verdict.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/homebrew"
)

type lintReportWire struct {
	Report struct {
		Kind     string `json:"kind"`
		Name     string `json:"name"`
		Findings []struct {
			Check    string `json:"check"`
			Severity string `json:"severity"`
			Subject  string `json:"subject"`
			Message  string `json:"message"`
			Basis    struct {
				Origin     string `json:"origin"`
				Arithmetic string `json:"arithmetic"`
				Rule       string `json:"rule"`
				Citation   *struct {
					Corpus string `json:"corpus"`
					Number string `json:"number"`
					Title  string `json:"title"`
				} `json:"citation"`
			} `json:"basis"`
		} `json:"findings"`
		Neighbours []struct {
			Title  string `json:"title"`
			Number string `json:"number"`
			Corpus string `json:"corpus"`
		} `json:"neighbours"`
		Notices     []string `json:"notices"`
		WriteUp     string   `json:"write_up"`
		WrittenUp   string   `json:"write_up_state"`
		WriteUpNote string   `json:"write_up_note"`
	} `json:"report"`
}

// decodeLintReport unmarshals a lint response and holds the wire
// contract: every finding carries a basis — computed arithmetic, a
// declared rule, or a corpus citation — and no verdict-shaped key
// appears anywhere in the payload.
func decodeLintReport(t *testing.T, body []byte) lintReportWire {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode lint response: %v (%s)", err, body)
	}
	assertNoVerdictKeys(t, raw)
	var wire lintReportWire
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode lint report: %v (%s)", err, body)
	}
	for _, f := range wire.Report.Findings {
		b := f.Basis
		switch b.Origin {
		case "computed":
			if b.Arithmetic == "" {
				t.Errorf("finding %s claims computed origin with no arithmetic", f.Check)
			}
		case "structural":
			if b.Rule == "" {
				t.Errorf("finding %s claims structural origin with no rule", f.Check)
			}
		case "retrieved":
			if b.Citation == nil || b.Citation.Number == "" {
				t.Errorf("finding %s claims retrieved origin with no citation", f.Check)
			}
		default:
			t.Errorf("finding %s carries no basis origin", f.Check)
		}
		switch f.Severity {
		case "error", "warning", "note":
		default:
			t.Errorf("finding %s carries severity %q", f.Check, f.Severity)
		}
	}
	return wire
}

// assertNoVerdictKeys walks a decoded JSON payload and fails if any key
// could carry a legal/illegal verdict.
func assertNoVerdictKeys(t *testing.T, v any) {
	t.Helper()
	switch tv := v.(type) {
	case map[string]any:
		for k, sub := range tv {
			low := strings.ToLower(k)
			if strings.Contains(low, "legal") || strings.Contains(low, "verdict") {
				t.Errorf("response carries a verdict-shaped key: %s", k)
			}
			assertNoVerdictKeys(t, sub)
		}
	case []any:
		for _, sub := range tv {
			assertNoVerdictKeys(t, sub)
		}
	}
}

func TestMonsterLint_ServesGroundedFindings(t *testing.T) {
	s, f, _ := newMonsterServer(t, nil)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/monsters", handSaveBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: status %d, body %s", rec.Code, rec.Body)
	}
	var saved struct {
		Monster struct {
			ID string `json:"id"`
		} `json:"monster"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}

	rec = hit(t, s, http.MethodPost, "/api/monsters/"+saved.Monster.ID+"/lint", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("lint: status %d, body %s", rec.Code, rec.Body)
	}
	wire := decodeLintReport(t, rec.Body.Bytes())
	if wire.Report.Kind != "monster" || wire.Report.Name == "" {
		t.Fatalf("report shape wrong: %+v", wire.Report)
	}
	// No model is configured, and the report says so instead of hiding
	// the pass.
	if wire.Report.WrittenUp != "unavailable" {
		t.Errorf("write-up state %q, want unavailable", wire.Report.WrittenUp)
	}
	if len(wire.Report.Findings) != 0 {
		t.Errorf("the clean hand-built monster raised %+v", wire.Report.Findings)
	}

	// A foreign owner's monster is indistinguishable from a missing one.
	other := otherUserSession(t, s, f, "other-player")
	rec = hit(t, s, http.MethodPost, "/api/monsters/"+saved.Monster.ID+"/lint", "", other)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign lint: status %d, want 404", rec.Code)
	}
}

// lintCyclicBody is a hand-entered monster with a real at-will cycle and
// a creature type the game does not have.
const lintCyclicBody = `{
	"name": "The Endless Well",
	"source": "hand",
	"requested_cr": "9",
	"statblock": {
		"name": "The Endless Well", "size": "Huge", "type": "abomination",
		"ac": 16, "hp": 210,
		"abilities": {"str": 26, "dex": 10, "con": 22, "int": 6, "wis": 12, "cha": 14},
		"actions": [
			{"name": "Slam", "kind": "ACTION",
			 "desc": "Melee Weapon Attack: +11 to hit, reach 10 ft., one target. Hit: 40 (6d10 + 7) bludgeoning damage."},
			{"name": "Well of Magic", "kind": "ACTION", "usage": "at will",
			 "desc": "The Endless Well regains one expended spell slot."}
		]
	}
}`

func TestMonsterLint_FindsTheCycle(t *testing.T) {
	s, _, _ := newMonsterServer(t, nil)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/monsters", lintCyclicBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: status %d, body %s", rec.Code, rec.Body)
	}
	var saved struct {
		Monster struct {
			ID         string `json:"id"`
			ComputedCR string `json:"computed_cr"`
		} `json:"monster"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}

	rec = hit(t, s, http.MethodPost, "/api/monsters/"+saved.Monster.ID+"/lint", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("lint: status %d, body %s", rec.Code, rec.Body)
	}
	wire := decodeLintReport(t, rec.Body.Bytes())

	var cycle, cr *struct {
		Check    string
		Severity string
		Message  string
	}
	for _, f := range wire.Report.Findings {
		switch f.Check {
		case "monster.structure.recharge_cycle":
			cycle = &struct {
				Check    string
				Severity string
				Message  string
			}{f.Check, f.Severity, f.Message}
		case "monster.cr_disagrees":
			cr = &struct {
				Check    string
				Severity string
				Message  string
			}{f.Check, f.Severity, f.Message}
		}
	}
	if cycle == nil || cycle.Severity != "error" {
		t.Fatalf("the at-will cycle did not raise an error finding: %+v", wire.Report.Findings)
	}
	if !strings.Contains(cycle.Message, "spell slots") {
		t.Errorf("cycle message does not name the resource: %s", cycle.Message)
	}
	if cr == nil || cr.Severity != "warning" {
		t.Fatalf("the CR disagreement did not raise a warning: %+v", wire.Report.Findings)
	}
	if !strings.Contains(cr.Message, "short") && !strings.Contains(cr.Message, "past the band") {
		t.Errorf("CR finding does not name the shortfall: %s", cr.Message)
	}
}

// TestMonsterLint_ModelCannotAlterFindings drives the endpoint with a
// scripted model that answers with a findings array, a verdict, and an
// invented figure. The findings the DM sees are still the engine's, and
// the write-up is rejected in full.
// lintScripted bridges the canon harness's scripted model onto the
// linter's completion shape, so both fake at their own seams.
type lintScripted struct{ inner *scriptedCanonModel }

func (l lintScripted) ModelName() string { return l.inner.ModelName() }

func (l lintScripted) Complete(ctx context.Context, system, user string) (homebrew.Completion, error) {
	c, err := l.inner.Complete(ctx, system, user)
	return homebrew.Completion(c), err
}

func TestMonsterLint_ModelCannotAlterFindings(t *testing.T) {
	model := &scriptedCanonModel{responses: []string{
		`Here are my findings: [{"check":"monster.cr_disagrees","severity":"error","message":"I invented this"}]. This creature is clearly illegal — its 9999 damage per round breaks the game.`,
	}}
	s, _, _ := newMonsterServer(t, model)
	s = s.WithLintModel(lintScripted{model})
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/monsters", lintCyclicBody, dm)
	var saved struct {
		Monster struct {
			ID string `json:"id"`
		} `json:"monster"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}

	rec = hit(t, s, http.MethodPost, "/api/monsters/"+saved.Monster.ID+"/lint", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("lint: status %d, body %s", rec.Code, rec.Body)
	}
	wire := decodeLintReport(t, rec.Body.Bytes())

	if wire.Report.WrittenUp != "rejected" {
		t.Fatalf("write-up state %q, want rejected", wire.Report.WrittenUp)
	}
	if wire.Report.WriteUp != "" {
		t.Fatalf("a rejected write-up must not be shown: %q", wire.Report.WriteUp)
	}
	if !strings.Contains(wire.Report.WriteUpNote, "9999") || !strings.Contains(wire.Report.WriteUpNote, "illegal") {
		t.Errorf("rejection note does not name the violations: %s", wire.Report.WriteUpNote)
	}
	// The findings are the engine's: the cycle error and the CR warning,
	// nothing from the model's JSON.
	for _, f := range wire.Report.Findings {
		if f.Message == "I invented this" {
			t.Fatal("the model's invented finding reached the report")
		}
	}
	if len(model.calls) != 1 {
		t.Fatalf("expected exactly one model call, got %d", len(model.calls))
	}
	// The model was handed the engine's own context to write from.
	if !strings.Contains(model.calls[0], `"findings":[`) {
		t.Errorf("the model was not given the engine context: %s", model.calls[0][:200])
	}
}

func TestItemLint_ComparesAgainstTheShelf(t *testing.T) {
	s, _, _ := newItemServer(t)
	dm := dmSession(t, s)

	// A sound design: saved, and linted without a rarity complaint.
	rec := hit(t, s, http.MethodPost, "/api/items/homebrew", emberbrandBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save: status %d, body %s", rec.Code, rec.Body)
	}
	var saved struct {
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	rec = hit(t, s, http.MethodPost, "/api/items/homebrew/"+saved.Item.ID+"/lint", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("lint: status %d, body %s", rec.Code, rec.Body)
	}
	wire := decodeLintReport(t, rec.Body.Bytes())
	if wire.Report.Kind != "item" {
		t.Fatalf("report kind %q, want item", wire.Report.Kind)
	}
	for _, f := range wire.Report.Findings {
		if f.Check == "item.rarity_disagrees" {
			t.Errorf("a on-shelf design tripped the rarity check: %s", f.Message)
		}
	}

	// A +3 at Uncommon: structurally saveable, but the shelf disagrees.
	rec = hit(t, s, http.MethodPost, "/api/items/homebrew", `{
		"name": "The Overshard",
		"design": {
			"name": "The Overshard", "type": "weapon", "base": "dagger", "bonus": 3,
			"rarity": "Uncommon",
			"effects": [{"text": "Sharp beyond reason.", "damage": "1d4 piercing"}]
		}
	}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save overshard: status %d, body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	rec = hit(t, s, http.MethodPost, "/api/items/homebrew/"+saved.Item.ID+"/lint", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("lint overshard: status %d, body %s", rec.Code, rec.Body)
	}
	wire = decodeLintReport(t, rec.Body.Bytes())
	found := false
	for _, f := range wire.Report.Findings {
		if f.Check == "item.rarity_disagrees" && f.Severity == "warning" {
			found = true
			if !strings.Contains(f.Basis.Arithmetic, "of 1 SRD items") {
				t.Errorf("arithmetic does not state its counts: %s", f.Basis.Arithmetic)
			}
		}
	}
	if !found {
		t.Fatalf("the +3 at Uncommon raised no rarity disagreement: %+v", wire.Report.Findings)
	}
}

// otherUserSession registers a second user through the campaign invite
// flow — the exact path a real player takes — for the ownership tests.
func otherUserSession(t *testing.T, s *Server, f *fixture, name string) *http.Cookie {
	t.Helper()
	dm := dmSession(t, s)
	inv := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/invites", `{"role":"player"}`, dm)
	if inv.Code != http.StatusCreated {
		t.Fatalf("mint campaign invite: status %d, body %s", inv.Code, inv.Body)
	}
	code, _ := inviteCodeFrom(t, inv)
	reg := hit(t, s, http.MethodPost, "/api/auth/register",
		`{"username":"`+name+`","password":"a-fine-passphrase","invite":"`+code+`"}`)
	if reg.Code != http.StatusCreated {
		t.Fatalf("register %s: status %d, body %s", name, reg.Code, reg.Body)
	}
	return sessionFrom(t, reg)
}
