package canon

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The wire schema: the JSON contract the model must satisfy, as declared in
// prompts.go's wireContract. Kept separate from the staged-candidate storage
// shape so the prompt contract can evolve (tolerant nulls, stringified
// numbers, missing optionals) without touching the database, exactly the way
// Arda's wire.py sits apart from its record models. Mapping wire records to
// staged candidates happens in extract.go together with the drop rules.

// WireEntity is one new-entity candidate.
type WireEntity struct {
	LocalID    string    `json:"local_id"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Summary    string    `json:"summary"`
	Quote      string    `json:"quote"`
	Confidence flexFloat `json:"confidence"`
}

// WireFact is one candidate fact.
type WireFact struct {
	LocalID       string    `json:"local_id"`
	Statement     string    `json:"statement"`
	Subject       string    `json:"subject"`
	Predicate     string    `json:"predicate"`
	ObjectEntity  string    `json:"object_entity"`
	ObjectLiteral string    `json:"object_literal"`
	Visibility    string    `json:"visibility"`
	Quote         string    `json:"quote"`
	Confidence    flexFloat `json:"confidence"`
}

// WireParticipant is one entity's role in an event.
type WireParticipant struct {
	Entity string `json:"entity"`
	Role   string `json:"role"`
}

// WireEvent is one candidate event.
type WireEvent struct {
	LocalID      string            `json:"local_id"`
	Summary      string            `json:"summary"`
	ClockAt      *flexInt          `json:"clock_at"`
	Location     string            `json:"location"`
	Participants []WireParticipant `json:"participants"`
	Quote        string            `json:"quote"`
	Confidence   flexFloat         `json:"confidence"`
}

// WireDiscovery is one candidate discovery: who learned which fact.
type WireDiscovery struct {
	Fact         string    `json:"fact"`
	DiscoveredBy string    `json:"discovered_by"`
	Stance       string    `json:"stance"`
	Method       string    `json:"method"`
	Quote        string    `json:"quote"`
	Confidence   flexFloat `json:"confidence"`
}

// WireRelationship is one candidate relationship change.
type WireRelationship struct {
	FromEntity string    `json:"from_entity"`
	RelType    string    `json:"rel_type"`
	ToEntity   string    `json:"to_entity"`
	Quote      string    `json:"quote"`
	Confidence flexFloat `json:"confidence"`
}

// WirePayload is the whole model response.
type WirePayload struct {
	NewEntities   []WireEntity       `json:"new_entities"`
	Facts         []WireFact         `json:"facts"`
	Events        []WireEvent        `json:"events"`
	Discoveries   []WireDiscovery    `json:"discoveries"`
	Relationships []WireRelationship `json:"relationships"`
}

/* ---------- tolerant scalars ---------- */

// flexFloat accepts a JSON number or a numeric string, so a model that
// stringify's its 0-1 scores ("0.8") is tolerated rather than dropped.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("%q is not a number", s)
	}
	*f = flexFloat(v)
	return nil
}

// flexInt accepts a JSON number or a numeric string.
type flexInt int64

func (i *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*i = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not an integer", s)
	}
	*i = flexInt(v)
	return nil
}

/* ---------- parsing ---------- */

// jsonBlock best-effort strips prose and markdown fences around a JSON
// object: models wrap payloads in ```json fences and chatty preambles even
// when told not to, and losing every valid record to a fence would be a
// policy error, not a model error. An error means no JSON object at all.
func jsonBlock(text string) (string, error) {
	stripped := strings.TrimSpace(text)
	stripped = strings.TrimPrefix(stripped, "```")
	if i := strings.IndexByte(stripped, '\n'); i >= 0 && !strings.Contains(stripped[:i], "{") {
		// the fence's info string (```json) — drop the first line
		stripped = stripped[i+1:]
	}
	stripped = strings.TrimSuffix(strings.TrimSpace(stripped), "```")
	start := strings.IndexByte(stripped, '{')
	end := strings.LastIndexByte(stripped, '}')
	if start == -1 || end == -1 || end <= start {
		return "", fmt.Errorf("response contains no JSON object")
	}
	return stripped[start : end+1], nil
}

// parseWire parses a model response into a WirePayload.
//
// Each list field is decoded record by record so one malformed record never
// sinks its valid siblings — per-record problems are returned as strings,
// never raised. Absent or null list fields are the empty list. A response
// with no JSON object at all yields an empty payload and one problem entry.
func parseWire(text string) (WirePayload, []string) {
	var problems []string
	block, err := jsonBlock(text)
	if err != nil {
		return WirePayload{}, []string{err.Error()}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(block), &raw); err != nil {
		return WirePayload{}, []string{fmt.Sprintf("invalid JSON: %v", err)}
	}

	var out WirePayload
	out.NewEntities = decodeRecords[WireEntity](raw, "new_entities", &problems)
	out.Facts = decodeRecords[WireFact](raw, "facts", &problems)
	out.Events = decodeRecords[WireEvent](raw, "events", &problems)
	out.Discoveries = decodeRecords[WireDiscovery](raw, "discoveries", &problems)
	out.Relationships = decodeRecords[WireRelationship](raw, "relationships", &problems)
	return out, problems
}

// decodeRecords decodes one list field of the raw payload, salvaging
// record-by-record: valid records are kept, malformed ones are logged to
// problems as "field[index]: reason".
func decodeRecords[T any](raw map[string]json.RawMessage, field string, problems *[]string) []T {
	item, ok := raw[field]
	if !ok || strings.TrimSpace(string(item)) == "null" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(item, &items); err != nil {
		*problems = append(*problems, fmt.Sprintf("%s: not a list; skipped", field))
		return nil
	}
	var out []T
	for i, it := range items {
		var r T
		if err := json.Unmarshal(it, &r); err != nil {
			*problems = append(*problems, fmt.Sprintf("%s[%d]: %v", field, i, err))
			continue
		}
		out = append(out, r)
	}
	return out
}
