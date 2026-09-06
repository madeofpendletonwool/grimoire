package board

// The board's visibility config (MAD-423): data on the campaign, under
// the settings payload's "board" key — the first typed key the freeform
// settings carry. The DM writes it through the board settings endpoint;
// the board reads it on every snapshot. Validation is strict (the house
// rule: errors, never guesses), and an unreadable stored value fails
// closed to the most restrictive shape — a config nobody can parse
// hides numbers rather than showing them.

import "fmt"

// HP modes.
const (
	HPExact = "exact" // hit points as numbers
	HPWord  = "word"  // hit points as the table's word ("bloodied")
)

// Slot modes.
const (
	SlotsVisible = "visible" // everyone's slot summary on every strip
	SlotsPrivate = "private" // each player's slots on their own strip only
)

// Config is the table's visibility choice. Absent settings mean the
// classic default: exact HP, visible slots.
type Config struct {
	HP    string `json:"hp"`
	Slots string `json:"slots"`
}

// Default is the config a campaign that never chose carries.
func Default() Config { return Config{HP: HPExact, Slots: SlotsVisible} }

// Closed is the most restrictive config — what an unparseable stored
// value falls back to, and what a first-time table sees until the DM
// decides otherwise.
func Closed() Config { return Config{HP: HPWord, Slots: SlotsPrivate} }

// Parse validates a raw settings value. nil (the key absent) is the
// default; anything present must spell a legal config exactly.
func Parse(v any) (Config, error) {
	if v == nil {
		return Default(), nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Closed(), fmt.Errorf("board config must be an object")
	}
	cfg := Closed()
	hp, err := field(m, "hp", HPExact, HPWord)
	if err != nil {
		return Closed(), err
	}
	cfg.HP = hp
	slots, err := field(m, "slots", SlotsVisible, SlotsPrivate)
	if err != nil {
		return Closed(), err
	}
	cfg.Slots = slots
	return cfg, nil
}

// ConfigOf reads the config off a settings value, failing closed. Reads
// never 500 on a hand-mangled payload — they hide instead.
func ConfigOf(v any) Config {
	cfg, err := Parse(v)
	if err != nil {
		return Closed()
	}
	return cfg
}

// SettingsValue is the raw JSON value to store for a validated config —
// the exact shape Parse reads back.
func (c Config) SettingsValue() map[string]any {
	return map[string]any{"hp": c.HP, "slots": c.Slots}
}

func field(m map[string]any, key string, options ...string) (string, error) {
	raw, ok := m[key]
	if !ok {
		return options[0], nil // an absent field is its default
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("board %s must be one of %s", key, orList(options))
	}
	for _, o := range options {
		if s == o {
			return s, nil
		}
	}
	return "", fmt.Errorf("board %s %q is not one of %s", key, s, orList(options))
}

func orList(options []string) string {
	out := ""
	for i, o := range options {
		if i > 0 {
			out += " | "
		}
		out += o
	}
	return out
}
