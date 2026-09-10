package engine

// Computed characteristics: base + modifiers + counters, never stored
// flattened. This walk is what makes "why is this creature 7/7?" a render
// of rows the log already holds — the PTTrace below is the SELECT the
// model doc promises, with no model call anywhere in the path.

// Characteristics is an object's current computed shape.
type Characteristics struct {
	Name       string   `json:"name,omitempty"`
	Types      []string `json:"types,omitempty"`
	Colors     []string `json:"colors,omitempty"`
	Keywords   []string `json:"keywords,omitempty"`
	Power      *int     `json:"power,omitempty"`
	Toughness  *int     `json:"toughness,omitempty"`
	Loyalty    *int     `json:"loyalty,omitempty"`
	Controller int      `json:"controller,omitempty"`
}

// Characteristics computes an object's current characteristics by walking
// CR 613's layers in order, timestamp (arrival) order within a layer;
// counters apply after pt_modify and before pt_switch, exactly where the
// trace renders them. Unknown characteristics stay unknown: a modifier on
// an object whose base P/T was never declared does not invent one — the
// honest answer to "what is it?" remains "we don't know".
func (s *State) Characteristics(id int64) Characteristics {
	o, ok := s.Objects[id]
	if !ok {
		return Characteristics{}
	}
	c := Characteristics{Name: o.Base.Name, Controller: o.Controller}
	if o.Identity.Token != nil && o.Identity.Token.Name != "" {
		c.Name = o.Identity.Token.Name
	}
	if o.Identity.Card != "" {
		c.Name = o.Identity.Card
	}
	types := append([]string{}, o.Base.Types...)
	colors := append([]string{}, o.Base.Colors...)
	keywords := append([]string{}, o.Base.Keywords...)
	var power, toughness *int
	if o.Base.Power != nil {
		p := *o.Base.Power
		power = &p
	}
	if o.Base.Toughness != nil {
		t := *o.Base.Toughness
		toughness = &t
	}

	// The seven layers through pt_modify, in order. pt_switch waits: the
	// switch is the last thing that happens to P/T, after counters.
	for _, layer := range layerOrder[:len(layerOrder)-1] {
		for _, mod := range o.Modifiers {
			if mod.Layer != layer {
				continue
			}
			switch layer {
			case LayerCopy:
				// A copy layer takes the source's base characteristics,
				// one level deep. Copy chains (a copy of a copy) are
				// beyond what declared data represents honestly; the
				// trace still shows the edge.
				if src, ok := s.Objects[mod.Delta.CopyOf]; ok {
					types = append([]string{}, src.Base.Types...)
					colors = append([]string{}, src.Base.Colors...)
					keywords = append([]string{}, src.Base.Keywords...)
					power = cloneInt(src.Base.Power)
					toughness = cloneInt(src.Base.Toughness)
					switch {
					case src.Identity.Card != "":
						c.Name = src.Identity.Card
					case src.Identity.Token != nil && src.Identity.Token.Name != "":
						c.Name = src.Identity.Token.Name
					}
				}
			case LayerControl:
				if mod.Delta.Controller != nil {
					c.Controller = *mod.Delta.Controller
				}
			case LayerType:
				types = removeEach(types, mod.Delta.RemoveTypes)
				types = addMissing(types, mod.Delta.AddTypes)
			case LayerColor:
				colors = removeEach(colors, mod.Delta.RemoveColors)
				colors = addMissing(colors, mod.Delta.AddColors)
			case LayerAbility:
				keywords = removeEach(keywords, mod.Delta.RemoveKeywords)
				keywords = addMissing(keywords, mod.Delta.AddKeywords)
			case LayerPTSet:
				if mod.Delta.SetPower != nil {
					power = cloneInt(mod.Delta.SetPower)
				}
				if mod.Delta.SetToughness != nil {
					toughness = cloneInt(mod.Delta.SetToughness)
				}
			case LayerPTModify:
				if mod.Delta.Power != nil {
					power = addTo(power, *mod.Delta.Power)
				}
				if mod.Delta.Toughness != nil {
					toughness = addTo(toughness, *mod.Delta.Toughness)
				}
			}
		}
	}

	// Counters: the two the engine understands structurally are the P/T
	// counters; every other name rides in its map until a rule needs it.
	if n := o.Counters["+1/+1"]; n != 0 {
		power = addTo(power, n)
		toughness = addTo(toughness, n)
	}
	if n := o.Counters["-1/-1"]; n != 0 {
		power = addTo(power, -n)
		toughness = addTo(toughness, -n)
	}

	// The switch, last of all.
	for _, mod := range o.Modifiers {
		if mod.Layer == LayerPTSwitch && mod.Delta.Swap {
			power, toughness = toughness, power
			break
		}
	}

	c.Types, c.Colors, c.Keywords = types, colors, keywords
	c.Power, c.Toughness, c.Loyalty = power, toughness, o.Base.Loyalty
	return c
}

// Keywords is the computed ability layer — the shortcut combat checks
// like vigilance read through.
func (s *State) Keywords(id int64) []string {
	return s.Characteristics(id).Keywords
}

// PTLine is one row of the "why is this creature 7/7?" trace.
type PTLine struct {
	Kind      string `json:"kind"` // base | modifier | counter | total
	Label     string `json:"label,omitempty"`
	Power     int    `json:"power,omitempty"`
	Toughness int    `json:"toughness,omitempty"`
	Layer     string `json:"layer,omitempty"`
	Duration  string `json:"duration,omitempty"`
	Note      string `json:"note,omitempty"`
}

// PTTrace renders the stack that produces an object's current power and
// toughness: the base, each P/T-relevant modifier in application order,
// the counters, the switch, and the total — the same order
// Characteristics computes in. A SELECT over rows the log already holds,
// which is the whole point of never storing characteristics flattened.
func (s *State) PTTrace(id int64) []PTLine {
	o, ok := s.Objects[id]
	if !ok {
		return nil
	}
	label := "base"
	if o.Identity.Token != nil && o.Identity.Token.Name != "" {
		label = o.Identity.Token.Name
	} else if o.Identity.Card != "" {
		label = o.Identity.Card
	}
	knownPT := o.Base.Power != nil || o.Base.Toughness != nil
	base := PTLine{Kind: "base", Label: label, Power: deref(o.Base.Power), Toughness: deref(o.Base.Toughness)}
	if !knownPT {
		base.Note = "base P/T unknown"
	}
	lines := []PTLine{base}
	power, toughness := deref(o.Base.Power), deref(o.Base.Toughness)

	for _, layer := range layerOrder[:len(layerOrder)-1] {
		for _, mod := range o.Modifiers {
			if mod.Layer != layer {
				continue
			}
			switch layer {
			case LayerPTSet:
				if mod.Delta.SetPower != nil {
					power = *mod.Delta.SetPower
				}
				if mod.Delta.SetToughness != nil {
					toughness = *mod.Delta.SetToughness
				}
				knownPT = true
				lines = append(lines, PTLine{Kind: "modifier", Label: mod.label(s),
					Layer: string(mod.Layer), Duration: string(mod.Duration),
					Power: power, Toughness: toughness, Note: "base becomes"})
			case LayerPTModify:
				if mod.Delta.Power != nil {
					power += *mod.Delta.Power
				}
				if mod.Delta.Toughness != nil {
					toughness += *mod.Delta.Toughness
				}
				lines = append(lines, PTLine{Kind: "modifier", Label: mod.label(s),
					Layer: string(mod.Layer), Duration: string(mod.Duration),
					Power: deref(mod.Delta.Power), Toughness: deref(mod.Delta.Toughness)})
			}
		}
	}
	if n := o.Counters["+1/+1"]; n != 0 {
		power += n
		toughness += n
		lines = append(lines, PTLine{Kind: "counter", Label: "+1/+1 counter", Power: n, Toughness: n})
	}
	if n := o.Counters["-1/-1"]; n != 0 {
		power -= n
		toughness -= n
		lines = append(lines, PTLine{Kind: "counter", Label: "-1/-1 counter", Power: -n, Toughness: -n})
	}
	for _, mod := range o.Modifiers {
		if mod.Layer == LayerPTSwitch && mod.Delta.Swap {
			power, toughness = toughness, power
			lines = append(lines, PTLine{Kind: "modifier", Label: mod.label(s),
				Layer: string(mod.Layer), Duration: string(mod.Duration), Note: "P/T swapped"})
			break
		}
	}
	if knownPT {
		lines = append(lines, PTLine{Kind: "total", Label: "total", Power: power, Toughness: toughness})
	} else {
		lines = append(lines, PTLine{Kind: "total", Label: "total", Note: "P/T unknown"})
	}
	return lines
}

// addTo adds to a maybe-unknown value: unknown stays unknown, because a
// bonus on a stat nobody declared is not a stat.
func addTo(v *int, n int) *int {
	if v == nil {
		return nil
	}
	out := *v + n
	return &out
}

func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func deref(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// removeEach removes every case-insensitive occurrence of the wants.
func removeEach(list, wants []string) []string {
	out := list[:0:0]
	for _, item := range list {
		drop := false
		for _, w := range wants {
			if equalFold(item, w) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, item)
		}
	}
	return out
}

// addMissing appends the wants not already present, case-insensitively.
func addMissing(list, wants []string) []string {
	out := append([]string{}, list...)
	for _, w := range wants {
		if !hasString(out, w) {
			out = append(out, w)
		}
	}
	return out
}
