package sheet

// Import (MAD-418): turning what a table already has — a sheet exported from
// another tool — into the typed sheet. Import beats build for v1: a full
// character builder is a designer tool for a later stage, and almost nobody
// hand-types a level-8 sheet when their table's app can export one.
//
// Two contracts live here:
//
//   - The Grimoire sheet JSON is the native format and round-trips
//     losslessly: Import of a GET sheet's body is the identity.
//   - Foreign formats are best-effort by declaration: every field the
//     importer maps is a field the export verifiably carries, everything it
//     could not map is named in the report, and nothing is invented. An
//     importer that guessed a field would be worse than one that admits it
//     left the field empty, because the sheet is trusted.
//
// Formats today: "grimoire" (native JSON), "roll20" (the OGL character
// sheet export: a character object whose attribs bag carries the printed
// sheet), "fc5" (the Fight Club 5 / Game Master 5 XML character schema).
// "auto" sniffs the payload when the caller does not know.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ImportReport is the importer's honesty: what format ran, which sheet
// fields it filled, and what it saw but could not map. Unmapped is not a
// failure — a foreign export carries tool-specific state with no sheet
// equivalent — but it is always visible.
type ImportReport struct {
	Format   string   `json:"format"`
	Name     string   `json:"name,omitempty"`
	Mapped   []string `json:"mapped,omitempty"`
	Unmapped []string `json:"unmapped,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

// Import parses data in the named format into a sheet. format may be
// "auto". The returned sheet is normalized but NOT validated — the caller
// validates before storing, the same gate every write passes.
func Import(format string, data []byte) (Sheet, ImportReport, error) {
	var rep ImportReport
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" || format == "auto" {
		format = sniff(data)
	}
	switch format {
	case "grimoire":
		return importGrimoire(data)
	case "roll20":
		return importRoll20(data)
	case "fc5":
		return importFC5(data)
	case "":
		return Sheet{}, rep, fmt.Errorf("could not tell what format the sheet is; name it explicitly (grimoire, roll20, fc5)")
	default:
		return Sheet{}, rep, fmt.Errorf("unknown sheet format %q; supported: grimoire, roll20, fc5", format)
	}
}

// sniff guesses the format from the payload's shape: XML serves fc5, a
// character object with an attribs bag (top-level or under "char") serves
// roll20, everything else JSON is tried as the native format.
func sniff(data []byte) string {
	trimmed := strings.TrimLeft(string(data), " \t\r\n\ufeff")
	if strings.HasPrefix(trimmed, "<") {
		return "fc5"
	}
	var probe struct {
		Attribs *[]any `json:"attribs"`
		Char    *struct {
			Attribs *[]any `json:"attribs"`
		} `json:"char"`
	}
	if json.Unmarshal(data, &probe) == nil {
		if probe.Attribs != nil || probe.Char != nil && probe.Char.Attribs != nil {
			return "roll20"
		}
	}
	return "grimoire"
}

// importGrimoire is the identity import: the native sheet JSON, decoded and
// normalized. This is the path that makes GET -> Import a no-op, and the
// documented interchange format for anything that wants to write Grimoire
// sheets without the API.
func importGrimoire(data []byte) (Sheet, ImportReport, error) {
	var rep ImportReport
	var s Sheet
	if err := json.Unmarshal(data, &s); err != nil {
		return s, rep, fmt.Errorf("not a Grimoire sheet: %v", err)
	}
	if s.Version != 0 && s.Version != Version {
		return Sheet{}, rep, fmt.Errorf("sheet declares version %d; this build reads version %d", s.Version, Version)
	}
	// The native import also accepts the full GET envelope {"sheet": {...}}
	// so a round-tripped export re-imports unchanged.
	var envelope struct {
		Sheet *Sheet `json:"sheet"`
	}
	if s.isZeroAfterNormalize() && json.Unmarshal(data, &envelope) == nil && envelope.Sheet != nil {
		s = *envelope.Sheet
	}
	rep = ImportReport{Format: "grimoire", Mapped: mappedFields(s)}
	s.normalize()
	return s, rep, nil
}

// mappedFields lists the top-level fields a sheet actually carries, the
// report's "what did the import fill" answer.
func mappedFields(s Sheet) []string {
	var fields []string
	add := func(f string) { fields = append(fields, f) }
	if !s.Abilities.IsZero() {
		add("abilities")
	}
	if s.AC != 0 {
		add("ac")
	}
	if s.Alignment != "" {
		add("alignment")
	}
	if s.Background != "" {
		add("background")
	}
	if len(s.Classes) > 0 {
		add("classes")
	}
	if !s.Currency.IsZero() {
		add("currency")
	}
	if len(s.Features) > 0 {
		add("features")
	}
	if s.MaxHP != 0 {
		add("max_hp")
	}
	if !s.Proficiencies.IsZero() {
		add("proficiencies")
	}
	if s.Race != "" {
		add("race")
	}
	if len(s.Resistances) > 0 || len(s.Immunities) > 0 || len(s.Vulnerabilities) > 0 {
		add("resistances")
	}
	if s.Spellcasting != nil {
		add("spellcasting")
	}
	if len(s.Speeds) > 0 {
		add("speeds")
	}
	if len(s.Traits) > 0 {
		add("traits")
	}
	if len(s.Inventory) > 0 {
		add("inventory")
	}
	if s.XP != 0 {
		add("xp")
	}
	return fields
}
