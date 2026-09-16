package engine

// The hidden_zone_leak check (MAD-337, stage 6 of MAD-321): the
// counterpart of the campaign's spoiler_leak. Any surface rendering a
// zone the requesting seat cannot see is an error-severity finding —
// and like spoiler_leak it is a join, not a model call, so it is free.
//
// The join runs between two sets built from the full event log:
//
//	private[seat]  the card identities only that seat is entitled to —
//	               the deck its library began as (DECK_KNOWN, or the
//	               legacy GAME_STARTED echo) and the cards its draws
//	               and looks named (CARD_KNOWN);
//	public         every identity a public row ever carried — casts,
//	               land drops, reveals, battlefield objects. Once the
//	           table has seen a card, the name is public forever.
//
// Scan then renders the join against one surface for one viewer: it
// walks the rendered value (any Go value — a response struct, an event
// slice, a prompt string) and reports every string leaf that contains
// a name private to a seat the viewer does not hold. Exact entitlement
// is the Viewer's to decide: the owner folds everything, a seat folds
// its own hidden zones, and a prompt assembled from a seat's scoped
// state cannot quote what its fold lacks.
//
// This is the deterministic half of the gate; the hard CI gate is the
// reflection sweep the tests run — every read surface, every seat,
// zero findings, plus a proof the scan fires when a filter goes
// missing (the leak_test.go pattern MAD-304 established).

import (
	"reflect"
	"sort"
	"strings"
)

// CheckHiddenZoneLeak is the finding's check name, spoiler_leak's
// counterpart in the table's vocabulary.
const CheckHiddenZoneLeak = "hidden_zone_leak"

// SeverityError marks a leak finding: a hidden zone that reached an
// unentitled viewer is never a warning — it is the invariant breaking.
const SeverityError = "error"

// LeakFinding is one leak: a seat's hidden identity that appeared on a
// surface rendered for a viewer not entitled to it.
type LeakFinding struct {
	Check    string `json:"check"`
	Severity string `json:"severity"`
	Viewer   string `json:"viewer"`   // who the surface was rendered for
	Seat     int    `json:"seat"`     // whose hidden zone leaked
	Evidence string `json:"evidence"` // the identity that leaked
}

// LeakIndex is the entitlement join precomputed over one game's full
// log. Build it once from the unscoped events — the store's whole-log
// read, exactly what the owner sees — and scan every surface with it.
type LeakIndex struct {
	private map[int]map[string]bool
	public  map[string]bool
	scanned int // string leaves visited, so tests can prove the sweep swept
}

// IndexLeaks builds the join over the full, unscoped log. Ordering
// matters twice: a name first seen privately and later played is public
// (the table learned it), and a name first seen publicly never turns
// private again.
func IndexLeaks(events []Event) *LeakIndex {
	x := &LeakIndex{private: map[int]map[string]bool{}, public: map[string]bool{}}
	for i := range events {
		e := events[i]
		switch e.Kind {
		case EventGameStarted:
			// The legacy echo: rows written before the DECK_KNOWN split
			// carried every deck on the public row. The names are
			// indexed private anyway — a scoped surface that renders
			// them is a leak by this check's definition, which is
			// exactly why legacy games stay owner-only.
			for _, sc := range e.Seats {
				for name := range sc.Deck {
					x.addPrivate(sc.Seat, name)
				}
			}
		case EventDeckKnown:
			for name := range e.Deck {
				x.addPrivate(e.TargetSeat, name)
			}
		case EventCardKnown:
			for _, c := range e.Cards {
				x.addPrivate(e.TargetSeat, c)
			}
		case EventCardRevealed:
			for _, c := range e.Cards {
				x.public[c] = true
			}
		}
		if e.Visibility != VisibilitySeat {
			// Public rows make every name they carry public — the cast
			// a seat announced, the land it dropped, the object the
			// battlefield gained, the card a cause names.
			for _, name := range publicNames(e) {
				x.public[name] = true
			}
		}
	}
	return x
}

// addPrivate records a seat-private identity.
func (x *LeakIndex) addPrivate(seat int, name string) {
	if seat <= 0 || strings.TrimSpace(name) == "" {
		return
	}
	if x.private[seat] == nil {
		x.private[seat] = map[string]bool{}
	}
	x.private[seat][name] = true
}

// publicNames lists the identity-bearing names one public row carries.
func publicNames(e Event) []string {
	var out []string
	if e.Card != "" {
		out = append(out, e.Card)
	}
	if e.Identity.Card != "" {
		out = append(out, e.Identity.Card)
	}
	if e.SourceCard != "" {
		out = append(out, e.SourceCard)
	}
	out = append(out, e.Cards...)
	for _, t := range e.Targets {
		if t.Card != "" {
			out = append(out, t.Card)
		}
	}
	return out
}

// PrivateTo lists the names private to one seat (public ones excluded),
// sorted for stable test output.
func (x *LeakIndex) PrivateTo(seat int) []string {
	var out []string
	for name := range x.private[seat] {
		if !x.public[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Seats lists the seats holding private names.
func (x *LeakIndex) Seats() []int {
	var out []int
	for seat := range x.private {
		out = append(out, seat)
	}
	sort.Ints(out)
	return out
}

// Scanned reports how many string leaves the last Scan walked — the
// sweep-must-sweep counter tests assert on.
func (x *LeakIndex) Scanned() int { return x.scanned }

// Scan walks one rendered surface for one viewer and reports every
// hidden-zone identity that reached a viewer not entitled to it. The
// rendered value may be anything — structs, maps, slices, strings; the
// walk reaches every string leaf and matches by containment, because a
// prompt buries a card name inside a sentence. Findings are
// error-severity by definition; zero findings is the pass.
func (x *LeakIndex) Scan(viewer Viewer, viewerLabel string, rendered any) []LeakFinding {
	var findings []LeakFinding
	priv := map[string]int{} // name → the seat it is private to
	for seat := range x.private {
		if viewer.SeesSeat(seat) {
			continue // the viewer holds this seat's hidden zones
		}
		for name := range x.private[seat] {
			if x.public[name] {
				continue // the table learned it; the name is free
			}
			if _, taken := priv[name]; !taken {
				priv[name] = seat
			}
		}
	}
	if len(priv) == 0 {
		return nil
	}
	names := make([]string, 0, len(priv))
	for name := range priv {
		names = append(names, name)
	}
	x.walk(reflect.ValueOf(rendered), func(leaf string) {
		for _, name := range names {
			if strings.Contains(leaf, name) {
				findings = append(findings, LeakFinding{
					Check: CheckHiddenZoneLeak, Severity: SeverityError,
					Viewer: viewerLabel, Seat: priv[name], Evidence: name,
				})
				return
			}
		}
	})
	return findings
}

// walk visits every string leaf in the value, depth-capped against
// cycles (surfaces are trees, but a runaway reflection must never hang
// a test) and skipping unexported fields (they cannot ride a JSON
// response; a leak must be through a surface, not through memory).
func (x *LeakIndex) walk(v reflect.Value, fn func(string)) {
	x.walkDepth(v, fn, 0)
}

const walkDepthCap = 12

func (x *LeakIndex) walkDepth(v reflect.Value, fn func(string), depth int) {
	if depth > walkDepthCap || !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		x.scanned++
		fn(v.String())
	case reflect.Interface, reflect.Ptr:
		if !v.IsNil() {
			x.walkDepth(v.Elem(), fn, depth+1)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			x.walkDepth(v.Index(i), fn, depth+1)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			x.walkDepth(iter.Key(), fn, depth+1)
			x.walkDepth(iter.Value(), fn, depth+1)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if t.Field(i).PkgPath != "" {
				continue // unexported
			}
			x.walkDepth(v.Field(i), fn, depth+1)
		}
	}
}
