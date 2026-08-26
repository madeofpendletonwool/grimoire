package canon

// The structured-generation harness (MAD-359): the one place a stage-5
// generator talks to a model. A generator computes the structure (the
// vetted shape — how many factions, which hooks exist, what must connect),
// declares a schema for what the model may fill in, and supplies the pool
// of legal values; this harness does everything else. It calls the model,
// parses the JSON object out of the reply, validates every field against
// the declared pools (entity kinds, the controlled relationship types, the
// confidence and visibility vocabularies — whatever the generator
// declares), and retries exactly once with the validation errors appended
// before giving up. No generator re-implements parse-and-validate.
//
// The harness never writes anything. Its output is values; a generator
// turns values into a BatchInput and StageBatch puts them behind the review
// gate, exactly like every other machine proposal.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/* ---------- the schema a generator declares ---------- */

// Field types a generator may declare.
const (
	FieldString  = "string"
	FieldNumber  = "number"
	FieldInteger = "integer"
)

// FieldSpec is one field the model may fill in. Enum is a controlled
// vocabulary (entity kinds, visibility, stance); Pool is a
// campaign-sourced set of legal values (the relationship types, the entity
// references the structure already vetted). Both validate the same way:
// the value must be one of the listed strings.
type FieldSpec struct {
	Key string
	// Desc tells the model what the field is; rendered into the prompt.
	Desc string
	// Type is one of FieldString (the default), FieldNumber, FieldInteger.
	Type string
	// Required refuses an absent field. An optional field may come back
	// missing; the harness simply does not set it.
	Required bool
	// Enum / Pool restrict a string field's legal values.
	Enum []string
	Pool []string
	// MaxLen bounds a string field's length in runes.
	MaxLen int
	// Min / Max bound a numeric field (confidence's 0..1, for instance).
	Min *float64
	Max *float64
}

// legalValues returns the field's combined legal values (Enum first, then
// Pool), deduplicated in declaration order — nil when the value is free.
func (f FieldSpec) legalValues() []string {
	if len(f.Enum) == 0 && len(f.Pool) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, v := range append(append([]string{}, f.Enum...), f.Pool...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// GenerateInput is one structured-generation call: the computed structure
// the generator vetted, and the fields the model may fill in.
type GenerateInput struct {
	// System is the generator's system prompt.
	System string
	// Structure is the vetted shape, rendered into the prompt as JSON so
	// the model sees exactly what it is filling into.
	Structure any
	// Fields is the fillable schema; every key the model returns must be
	// declared here.
	Fields []FieldSpec
	// Note carries extra instructions rendered after the schema.
	Note string
}

// Generated is the harness's output: the values that passed validation,
// whether it took the repair retry, and the token accounting.
type Generated struct {
	Values       map[string]any
	Retried      bool
	InputTokens  int
	OutputTokens int
}

/* ---------- vocabulary helpers for the pools ---------- */

// entityKindValues is the entities CHECK constraint's kind list — the one
// definition validEntityKind (extraction's drop rule) and EntityKinds (a
// generator's pool) both read from.
var entityKindValues = []string{
	"pc", "npc", "faction", "location", "item", "deity", "organization", "creature", "concept",
}

// EntityKinds is the pool of legal entity kinds a generator may declare.
func EntityKinds() []string {
	out := make([]string, len(entityKindValues))
	copy(out, entityKindValues)
	return out
}

// VisibilityValues is the fact visibility vocabulary.
func VisibilityValues() []string {
	return []string{campaign.VisibilityPublic, campaign.VisibilitySecret}
}

// RelationshipTypes loads the campaign's controlled relationship
// vocabulary — the pool a generator declares for rel_type fields.
func (s *Store) RelationshipTypes(ctx context.Context, campaignID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM relationship_types ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("load relationship types: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

/* ---------- validation ---------- */

// validateGenerated checks a parsed response against the declared schema.
// Every problem is a string the repair retry quotes back to the model.
func validateGenerated(fields []FieldSpec, values map[string]any) []string {
	declared := map[string]FieldSpec{}
	for _, f := range fields {
		declared[f.Key] = f
	}
	var problems []string
	for key := range values {
		if _, ok := declared[key]; !ok {
			problems = append(problems, fmt.Sprintf("%q is not a declared field; return only the declared fields", key))
		}
	}
	sort.Strings(problems) // stable order: unknown keys first, then per-field
	for _, f := range fields {
		v, ok := values[f.Key]
		if !ok || v == nil {
			if f.Required {
				problems = append(problems, fmt.Sprintf("%q is required and was missing", f.Key))
			}
			continue
		}
		typ := f.Type
		if typ == "" {
			typ = FieldString
		}
		switch typ {
		case FieldString:
			s, isStr := v.(string)
			if !isStr {
				problems = append(problems, fmt.Sprintf("%q must be a string", f.Key))
				continue
			}
			s = strings.TrimSpace(s)
			if s == "" && f.Required {
				problems = append(problems, fmt.Sprintf("%q is required and was empty", f.Key))
				continue
			}
			if legal := f.legalValues(); legal != nil && s != "" {
				okLegal := false
				for _, cand := range legal {
					if s == cand {
						okLegal = true
						break
					}
				}
				if !okLegal {
					problems = append(problems, fmt.Sprintf("%q: %q is not one of the legal values [%s]",
						f.Key, s, strings.Join(legal, ", ")))
				}
			}
			if f.MaxLen > 0 && utf8.RuneCountInString(s) > f.MaxLen {
				problems = append(problems, fmt.Sprintf("%q is longer than %d characters", f.Key, f.MaxLen))
			}
		case FieldNumber, FieldInteger:
			n, isNum := v.(float64)
			if !isNum {
				problems = append(problems, fmt.Sprintf("%q must be a JSON number", f.Key))
				continue
			}
			if typ == FieldInteger && n != float64(int64(n)) {
				problems = append(problems, fmt.Sprintf("%q must be a whole number", f.Key))
			}
			if f.Min != nil && n < *f.Min {
				problems = append(problems, fmt.Sprintf("%q is below %v", f.Key, *f.Min))
			}
			if f.Max != nil && n > *f.Max {
				problems = append(problems, fmt.Sprintf("%q is above %v", f.Key, *f.Max))
			}
		default:
			problems = append(problems, fmt.Sprintf("%q declares unknown type %q", f.Key, f.Type))
		}
	}
	return problems
}

/* ---------- the harness ---------- */

// Generate runs one structured-generation exchange: model call, parse,
// validate, and — when validation failed — one retry with the problems
// appended, so the model repairs its own output. A response that still
// fails validation after the retry gives up with the problems listed; a
// model or parse error propagates immediately.
func (s *Store) Generate(ctx context.Context, in GenerateInput) (*Generated, error) {
	if s.model == nil {
		return nil, errOffline
	}
	if len(in.Fields) == 0 {
		return nil, fmt.Errorf("%w: a generation call needs at least one field", ErrInvalid)
	}
	seenKey := map[string]bool{}
	for _, f := range in.Fields {
		if strings.TrimSpace(f.Key) == "" {
			return nil, fmt.Errorf("%w: a field has no key", ErrInvalid)
		}
		if seenKey[f.Key] {
			return nil, fmt.Errorf("%w: field %q declared twice", ErrInvalid, f.Key)
		}
		seenKey[f.Key] = true
	}

	out := &Generated{Values: map[string]any{}}
	user := generatePrompt(in, nil)
	for attempt := 0; attempt < 2; attempt++ {
		compl, err := s.model.Complete(ctx, in.System, user)
		if err != nil {
			return nil, fmt.Errorf("model call failed: %w", err)
		}
		out.InputTokens += compl.InputTokens
		out.OutputTokens += compl.OutputTokens

		block, err := jsonBlock(compl.Text)
		if err != nil {
			if attempt == 0 {
				out.Retried = true
				user = generatePrompt(in, []string{err.Error()})
				continue
			}
			return nil, fmt.Errorf("%w: %s", ErrInvalid, err.Error())
		}
		var values map[string]any
		if err := json.Unmarshal([]byte(block), &values); err != nil {
			problem := fmt.Sprintf("response is not a JSON object: %v", err)
			if attempt == 0 {
				out.Retried = true
				user = generatePrompt(in, []string{problem})
				continue
			}
			return nil, fmt.Errorf("%w: %s", ErrInvalid, problem)
		}
		problems := validateGenerated(in.Fields, values)
		if len(problems) == 0 {
			for k, v := range values {
				out.Values[k] = v
			}
			return out, nil
		}
		if attempt == 0 {
			out.Retried = true
			user = generatePrompt(in, problems)
			continue
		}
		return nil, fmt.Errorf("%w: generation failed validation twice: %s", ErrInvalid, strings.Join(problems, "; "))
	}
	return nil, fmt.Errorf("%w: generation did not complete", ErrInvalid)
}

// generatePrompt renders the user prompt: the schema with its pools, the
// computed structure as JSON context, and — on the repair retry — the
// previous attempt's problems.
func generatePrompt(in GenerateInput, problems []string) string {
	var b strings.Builder
	b.WriteString("Return ONLY a JSON object with exactly these keys, no prose, no markdown fences:\n")
	for _, f := range in.Fields {
		fmt.Fprintf(&b, "\n- %q", f.Key)
		typ := f.Type
		if typ == "" {
			typ = FieldString
		}
		switch typ {
		case FieldInteger:
			b.WriteString(" (whole number")
			if !f.Required {
				b.WriteString(", omit if not applicable")
			}
			b.WriteString(")")
		case FieldNumber:
			b.WriteString(" (number")
			if !f.Required {
				b.WriteString(", omit if not applicable")
			}
			b.WriteString(")")
		default:
			b.WriteString(" (string")
			if !f.Required {
				b.WriteString(", omit if not applicable")
			}
			b.WriteString(")")
		}
		if f.Desc != "" {
			fmt.Fprintf(&b, ": %s", f.Desc)
		}
		if legal := f.legalValues(); legal != nil {
			fmt.Fprintf(&b, " Legal values: %s.", strings.Join(legal, ", "))
		}
		if f.MaxLen > 0 {
			fmt.Fprintf(&b, " At most %d characters.", f.MaxLen)
		}
		if f.Min != nil && f.Max != nil {
			fmt.Fprintf(&b, " Between %v and %v.", *f.Min, *f.Max)
		}
	}
	if in.Structure != nil {
		structure, err := json.Marshal(in.Structure)
		if err == nil {
			fmt.Fprintf(&b, "\n\nThe structure you are filling in, for context:\n%s\n", structure)
		}
	}
	if in.Note != "" {
		fmt.Fprintf(&b, "\n%s\n", in.Note)
	}
	if len(problems) > 0 {
		b.WriteString("\nYour previous response had these problems:")
		for _, p := range problems {
			fmt.Fprintf(&b, "\n- %s", p)
		}
		b.WriteString("\nReturn the corrected JSON object only.")
	}
	return b.String()
}
