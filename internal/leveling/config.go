package leveling

// The campaign's leveling mode (MAD-424): data on the campaign, under the
// settings payload's "leveling" key — the board-config pattern
// (internal/board/config.go), the second typed key the freeform settings
// carry. XP is the rules default; milestone is the DM's toggle. The mode
// is a setting, not a code path: in milestone mode the XP surfaces refuse
// (an award answers "this campaign does not track XP"), while the
// level-up gate proposes exactly the same diffs from the same tables.

import "fmt"

// SettingsKey is the settings payload key the mode is stored under.
const SettingsKey = "leveling"

// Modes.
const (
	ModeXP        = "xp"        // encounter XP, the 2014 default
	ModeMilestone = "milestone" // the DM levels the party by decree
)

// Config is the campaign's leveling choice. Absent settings mean the
// default: XP.
type Config struct {
	Mode string `json:"mode"`
}

// Default is the config a campaign that never chose carries.
func Default() Config { return Config{Mode: ModeXP} }

// Parse validates a raw settings value. nil (the key absent) is the
// default; anything present must spell a legal mode exactly — the house
// rule: errors, never guesses.
func Parse(v any) (Config, error) {
	if v == nil {
		return Default(), nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Default(), fmt.Errorf("leveling config must be an object")
	}
	raw, ok := m["mode"]
	if !ok {
		return Default(), nil
	}
	mode, ok := raw.(string)
	if !ok {
		return Default(), fmt.Errorf("leveling mode must be a string")
	}
	switch mode {
	case ModeXP, ModeMilestone:
		return Config{Mode: mode}, nil
	default:
		return Default(), fmt.Errorf("leveling mode %q is not xp | milestone", mode)
	}
}

// ConfigOf reads the mode off a settings value, failing to the default.
// Reads never 500 on a hand-mangled payload.
func ConfigOf(v any) Config {
	cfg, err := Parse(v)
	if err != nil {
		return Default()
	}
	return cfg
}

// SettingsValue is the raw JSON value to store for a validated config —
// the exact shape Parse reads back.
func (c Config) SettingsValue() map[string]any {
	return map[string]any{"mode": c.Mode}
}
