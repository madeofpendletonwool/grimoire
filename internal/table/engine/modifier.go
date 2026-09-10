package engine

// Modifiers: the ordered effect stack per object. A modifier carries source,
// layer, duration and delta; characteristics are computed as base +
// modifiers (+ counters between pt_modify and pt_switch), never stored
// flattened. This is what makes "why is this creature 7/7?" a fold-render
// of rows the log already holds — a SELECT instead of a model call.

// Layer follows CR 613's layer system, flattened to the sublayers the
// engine's computed characteristics actually read: copy (613.1a), control
// (613.1b), text (613.1c — carried, not computed), type (613.2), color
// (613.3), ability (613.4 — keywords here are the computed ones combat and
// tapping read, e.g. vigilance), pt_set (613.7a switching base P/T),
// pt_modify (613.7b–c) and pt_switch (613.7e). Counters are not modifiers:
// they apply after pt_modify and before pt_switch, which is exactly where
// the trace renders them.
type Layer string

const (
	LayerCopy     Layer = "copy"
	LayerControl  Layer = "control"
	LayerText     Layer = "text"
	LayerType     Layer = "type"
	LayerColor    Layer = "color"
	LayerAbility  Layer = "ability"
	LayerPTSet    Layer = "pt_set"
	LayerPTModify Layer = "pt_modify"
	LayerPTSwitch Layer = "pt_switch"
)

// layerOrder is CR 613's order. Characteristics walks it; within a layer,
// list position is timestamp order.
var layerOrder = []Layer{
	LayerCopy, LayerControl, LayerText, LayerType, LayerColor,
	LayerAbility, LayerPTSet, LayerPTModify, LayerPTSwitch,
}

// Valid reports whether the layer is in the vocabulary. The vocabulary
// lives in the Go type system per the model doc — unvalidated layers would
// silently compute nothing.
func (l Layer) Valid() bool {
	for _, known := range layerOrder {
		if known == l {
			return true
		}
	}
	return false
}

// Duration is how long a modifier lasts. until_end_of_turn expires at
// cleanup — computed by the fold from TURN_ENDED anchors, no expiry events,
// because derived data stored twice is a drift bug. while_source_present
// ends when the engine asserts MODIFIER_REMOVED as the source leaves; that
// one is not cheaply derivable, so it is an event and the log shows it.
// permanent lasts until removed.
type Duration string

const (
	UntilEOT           Duration = "until_end_of_turn"
	WhileSourcePresent Duration = "while_source_present"
	PermanentDuration  Duration = "permanent"
)

// Valid reports whether the duration is in the vocabulary.
func (d Duration) Valid() bool {
	switch d {
	case UntilEOT, WhileSourcePresent, PermanentDuration:
		return true
	}
	return false
}

// Modifier is one effect on an object's computed characteristics. ID is
// minted by the engine (monotonic per game) so REMOVE_MODIFIER can name it.
// Source is the object or card the effect came from: SourceObj for
// continuous effects from a permanent on the battlefield (Glorious Anthem),
// SourceCard for effects from a resolved spell that has moved on (Giant
// Growth) — the trace needs to name either.
type Modifier struct {
	ID         int64    `json:"id"`
	SourceObj  int64    `json:"source_obj,omitempty"`
	SourceCard string   `json:"source_card,omitempty"`
	Layer      Layer    `json:"layer"`
	Duration   Duration `json:"duration"`
	Delta      Delta    `json:"delta"`
}

// Delta is what the modifier does, per layer. The fields a layer reads are
// documented on each; an empty delta is rejected at the reducer so no
// silent no-op modifiers enter the log.
type Delta struct {
	// pt_modify: power/toughness contributions.
	Power     *int `json:"power,omitempty"`
	Toughness *int `json:"toughness,omitempty"`
	// pt_set: the new base power/toughness (Turn to Frog sets 1/1).
	SetPower     *int `json:"set_power,omitempty"`
	SetToughness *int `json:"set_toughness,omitempty"`
	// pt_switch: swap the final power and toughness (about face).
	Swap bool `json:"swap,omitempty"`
	// control: the new controller seat.
	Controller *int `json:"controller,omitempty"`
	// copy: the object id whose base characteristics are copied.
	CopyOf int64 `json:"copy_of,omitempty"`
	// type: added and removed types.
	AddTypes    []string `json:"add_types,omitempty"`
	RemoveTypes []string `json:"remove_types,omitempty"`
	// color: added and removed colors.
	AddColors    []string `json:"add_colors,omitempty"`
	RemoveColors []string `json:"remove_colors,omitempty"`
	// ability: added and removed keywords.
	AddKeywords    []string `json:"add_keywords,omitempty"`
	RemoveKeywords []string `json:"remove_keywords,omitempty"`
}

// empty reports whether the delta would change nothing the engine computes.
// Text-layer prose rides in EFFECT_DECLARED, not here, so an empty delta is
// a mistake, not a note.
func (d Delta) empty() bool {
	return d.Power == nil && d.Toughness == nil &&
		d.SetPower == nil && d.SetToughness == nil &&
		!d.Swap && d.Controller == nil && d.CopyOf == 0 &&
		len(d.AddTypes) == 0 && len(d.RemoveTypes) == 0 &&
		len(d.AddColors) == 0 && len(d.RemoveColors) == 0 &&
		len(d.AddKeywords) == 0 && len(d.RemoveKeywords) == 0
}

// label names the modifier's source for traces and log rendering: the card
// name when there is one, the source object's identity otherwise.
func (m Modifier) label(s *State) string {
	if m.SourceCard != "" {
		return m.SourceCard
	}
	if o, ok := s.Objects[m.SourceObj]; ok && o.Identity.Card != "" {
		return o.Identity.Card
	}
	if o, ok := s.Objects[m.SourceObj]; ok && o.Identity.Token != nil {
		return o.Identity.Token.Name
	}
	return "declared effect"
}
