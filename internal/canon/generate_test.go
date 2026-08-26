package canon

// The structured-generation harness (MAD-359): parse, validate against the
// declared pools, and retry exactly once with the problems appended. The
// tests replay a bad-then-good model script to prove the repair path, and
// a bad-then-bad script to prove the give-up path names its problems.

import (
	"context"
	"strings"
	"testing"
)

func genFields() []FieldSpec {
	min, max := 0.0, 1.0
	return []FieldSpec{
		{Key: "name", Desc: "the faction's name", Required: true, MaxLen: 80},
		{Key: "kind", Desc: "the entity kind", Required: true, Enum: EntityKinds()},
		{Key: "rel_type", Desc: "the relationship", Required: true, Pool: []string{"knows", "serves"}},
		{Key: "confidence", Desc: "0..1", Type: FieldNumber, Min: &min, Max: &max},
		{Key: "detail", Desc: "optional extra"},
	}
}

func TestGenerate_ValidatesAndRepairs(t *testing.T) {
	db, _, _ := seeded(t)
	s := newStore(t, db, &fakeModel{responses: []string{
		// First attempt: illegal enum, illegal pool value, an unknown key,
		// and a confidence out of range.
		`{"name":"House Vane","kind":"house","rel_type":"likes","confidence":1.5,"detail":"x","extra":"nope"}`,
		// The repaired attempt, quoting the problems back.
		`{"name":"House Vane","kind":"faction","rel_type":"serves","confidence":0.8,"detail":"x"}`,
	}}, testConfig())

	out, err := s.Generate(context.Background(), GenerateInput{
		System:    "you fill in campaign structure",
		Structure: map[string]any{"factions": 1},
		Fields:    genFields(),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !out.Retried {
		t.Fatal("harness did not retry a failed validation")
	}
	if out.Values["kind"] != "faction" || out.Values["rel_type"] != "serves" || out.Values["confidence"] != 0.8 {
		t.Fatalf("values = %v", out.Values)
	}
	if _, ok := out.Values["extra"]; ok {
		t.Fatalf("undeclared key survived: %v", out.Values)
	}
}

func TestGenerate_RepairPromptNamesTheProblems(t *testing.T) {
	db, _, _ := seeded(t)
	m := &fakeModel{responses: []string{
		`{"name":"","kind":"house","rel_type":"likes","confidence":2}`,
		`{"name":"House Vane","kind":"faction","rel_type":"serves","confidence":0.8}`,
	}}
	s := newStore(t, db, m, testConfig())

	if _, err := s.Generate(context.Background(), GenerateInput{System: "s", Fields: genFields()}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(m.calls) != 2 {
		t.Fatalf("model called %d times, want 2", len(m.calls))
	}
	for _, want := range []string{
		`"kind"`,
		`"rel_type"`,
		`"confidence"`,
		`"name"`,
	} {
		if !strings.Contains(m.calls[1], want) {
			t.Fatalf("repair prompt missing %s: %s", want, m.calls[1])
		}
	}
}

func TestGenerate_GivesUpAfterTwoFailures(t *testing.T) {
	db, _, _ := seeded(t)
	s := newStore(t, db, &fakeModel{responses: []string{
		`{"kind":"house"}`,
		`{"kind":"still-not-a-kind"}`,
	}}, testConfig())

	_, err := s.Generate(context.Background(), GenerateInput{System: "s", Fields: genFields()})
	if err == nil {
		t.Fatal("second failure returned no error")
	}
	if !strings.Contains(err.Error(), "still-not-a-kind") || !strings.Contains(err.Error(), `"name" is required`) {
		t.Fatalf("give-up error does not name the second attempt's problems: %v", err)
	}
}

func TestGenerate_UnparseableResponseRetriesThenFails(t *testing.T) {
	db, _, _ := seeded(t)
	s := newStore(t, db, &fakeModel{responses: []string{
		"I will surely return JSON... any moment now.",
		"```json\n{\"name\":\"V\",\"kind\":\"npc\",\"rel_type\":\"knows\"}\n```",
	}}, testConfig())

	// The retry tolerates the fence the second attempt wrapped its JSON in.
	out, err := s.Generate(context.Background(), GenerateInput{System: "s", Fields: genFields()})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !out.Retried || out.Values["kind"] != "npc" {
		t.Fatalf("values = %v retried = %v", out.Values, out.Retried)
	}

	// A model that never produces JSON gives up with the parse problem.
	s2 := newStore(t, db, &fakeModel{responses: []string{"nope", "still nope"}}, testConfig())
	_, err = s2.Generate(context.Background(), GenerateInput{System: "s", Fields: genFields()})
	if err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("unparseable give-up = %v", err)
	}
}

func TestGenerate_OfflineStoreRefuses(t *testing.T) {
	db, _, _ := seeded(t)
	s, err := NewOffline(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Generate(context.Background(), GenerateInput{System: "s", Fields: genFields()}); err != errOffline {
		t.Fatalf("offline Generate err = %v", err)
	}
}

func TestGenerate_RejectsBadSchemas(t *testing.T) {
	db, _, _ := seeded(t)
	s := newStore(t, db, &fakeModel{responses: []string{`{"a":1}`}}, testConfig())
	ctx := context.Background()
	if _, err := s.Generate(ctx, GenerateInput{System: "s"}); err == nil {
		t.Fatal("empty schema accepted")
	}
	if _, err := s.Generate(ctx, GenerateInput{System: "s",
		Fields: []FieldSpec{{Key: "a"}, {Key: "a"}}}); err == nil {
		t.Fatal("duplicate field accepted")
	}
}

func TestValidateGenerated_FieldRules(t *testing.T) {
	min, max := 0.0, 1.0
	fields := []FieldSpec{
		{Key: "s", Type: FieldString, Required: true, MaxLen: 3},
		{Key: "opt"},
		{Key: "n", Type: FieldNumber, Min: &min, Max: &max},
		{Key: "i", Type: FieldInteger},
		{Key: "e", Enum: []string{"a", "b"}},
	}
	cases := []struct {
		name   string
		values map[string]any
		want   string // "" means valid
	}{
		{"valid", map[string]any{"s": "ok", "n": 0.5, "i": 3.0, "e": "a"}, ""},
		{"missing required", map[string]any{}, `"s" is required`},
		{"empty required", map[string]any{"s": "  "}, `"s" is required`},
		{"too long", map[string]any{"s": "abcd"}, "longer than 3"},
		{"wrong type", map[string]any{"s": 3}, "must be a string"},
		{"number out of range", map[string]any{"s": "x", "n": 1.5}, "above 1"},
		{"not a number", map[string]any{"s": "x", "n": "0.5"}, "must be a JSON number"},
		{"fractional integer", map[string]any{"s": "x", "i": 2.5}, "whole number"},
		{"illegal enum", map[string]any{"s": "x", "e": "c"}, "not one of the legal values"},
		{"undeclared key", map[string]any{"s": "x", "zz": 1}, "not a declared field"},
	}
	for _, tc := range cases {
		problems := validateGenerated(fields, tc.values)
		if tc.want == "" {
			if len(problems) != 0 {
				t.Fatalf("%s: unexpected problems %v", tc.name, problems)
			}
			continue
		}
		found := false
		for _, p := range problems {
			if strings.Contains(p, tc.want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: problems %v do not mention %q", tc.name, problems, tc.want)
		}
	}
}
