package engine

// The hidden_zone_leak gate (MAD-337): the deterministic check from
// leak.go, proven three ways, the way internal/knowledge's MAD-304
// gate is proven —
//
//   - the scoped surfaces are clean: every Viewer-taking store read,
//     enumerated by reflection so a method added later joins the sweep
//     automatically, scanned for the planted markers a non-entitled
//     viewer must never see;
//   - the check has teeth: the unscoped read scanned at a seat's scope
//     fires, so a missing filter cannot pass silently;
//   - publicization is honest: a card the table has seen stops being a
//     leak even when it started private.
//
// CI runs this with every other test (`go test ./...`); that is the
// gate the issue's acceptance names.

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// markerNames are the planted hidden identities: one deck marker per
// seat plus the two cards seat 2 drew. None is a substring of another,
// so the scan's containment match cannot cross-fire.
func markerNames() []string {
	return []string{
		"Seat One Secret", "Seat Two Secret", "Seat Three Secret", "Seat Four Secret",
		"Hand Two A", "Hand Two B",
	}
}

// newMarkerGame is newPodGame plus a second, unidentified draw by seat
// 2 (markers Hand Two A/B) and a public play of one of seat 1's deck
// markers, so publicization is exercised on real rows.
func newMarkerGame(t *testing.T) (*Store, context.Context, string) {
	t.Helper()
	s, _, ctx, game := newPodGame(t)
	if _, _, err := s.Submit(ctx, game, Action{Kind: ActionDraw, Seat: 2, Count: 2,
		Cards: []string{"Hand Two A", "Hand Two B"}}); err != nil {
		t.Fatalf("second draw: %v", err)
	}
	if _, _, err := s.Submit(ctx, game, Action{Kind: ActionCast, Seat: 1, Card: "Seat One Secret"}); err != nil {
		t.Fatalf("cast: %v", err)
	}
	return s, ctx, game
}

func TestLeakIndexPrivateAndPublicSets(t *testing.T) {
	s, ctx, game := newMarkerGame(t)
	evs, err := s.Events(ctx, game, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	x := IndexLeaks(evs)

	// Each seat's deck markers are private to it…
	for seat, marker := range map[int]string{1: "Seat One Secret", 2: "Seat Two Secret", 3: "Seat Three Secret", 4: "Seat Four Secret"} {
		if seat == 1 {
			continue // …except seat 1's, whose marker was cast publicly
		}
		found := false
		for _, name := range x.PrivateTo(seat) {
			if name == marker {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is not private to seat %d", marker, seat)
		}
	}
	// …and a publicly cast deck marker stops being private.
	for _, name := range x.PrivateTo(1) {
		if name == "Seat One Secret" {
			t.Fatal("a publicly cast card is still private")
		}
	}
	// The drawn hand markers are private to seat 2.
	for _, want := range []string{"Hand Two A", "Hand Two B"} {
		hit := false
		for _, name := range x.PrivateTo(2) {
			if name == want {
				hit = true
			}
		}
		if !hit {
			t.Fatalf("%s is not private to seat 2", want)
		}
	}
}

func TestLeakScanFindsTheMissingFilter(t *testing.T) {
	s, ctx, game := newMarkerGame(t)
	evs, err := s.Events(ctx, game, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	x := IndexLeaks(evs)
	if x.Scanned() != 0 {
		t.Fatal("index scanned before any Scan")
	}
	// The unscoped read — the whole log — is exactly the surface a
	// missing WHERE clause would hand a seat. The check must fire, and
	// name the seat and the evidence.
	findings := x.Scan(SeatViewer(3), "seat3", evs)
	if len(findings) == 0 {
		t.Fatal("the scan did not detect an unscoped read")
	}
	var sawDeck, sawHand bool
	for _, f := range findings {
		if f.Check != CheckHiddenZoneLeak || f.Severity != SeverityError {
			t.Fatalf("finding %+v is not an error-severity hidden_zone_leak", f)
		}
		switch f.Evidence {
		case "Seat One Secret":
			t.Fatal("a publicized card flagged as a leak")
		case "Seat Two Secret", "Seat Three Secret", "Seat Four Secret":
			sawDeck = true
		case "Hand Two A", "Hand Two B":
			sawHand = true
		}
	}
	if !sawDeck || !sawHand {
		t.Fatalf("teeth incomplete: deck %v hand %v", sawDeck, sawHand)
	}
	if x.Scanned() == 0 {
		t.Fatal("the sweep swept nothing")
	}
	// The owner is entitled to everything: the same surface is clean.
	if got := x.Scan(OwnerViewer(), "owner", evs); len(got) != 0 {
		t.Fatalf("owner scan fired: %+v", got)
	}
	// A prompt-shaped surface is caught too: the marker buried in a
	// sentence is still a leak.
	prompt := "The table:\n  Bob's hand contains Hand Two A, they say.\nGood luck."
	if got := x.Scan(SeatViewer(4), "seat4", prompt); len(got) == 0 {
		t.Fatal("a prompt leak was not caught")
	}
}

// TestViewerReadsSweepClean is the hard gate: every exported Store
// method that takes a Viewer — enumerated by reflection, so a scoped
// read added later joins automatically — fired at every seat's scope
// with the fixture aimed at its hidden zones, must answer with zero
// hidden-zone identities of a seat that viewer does not hold. The
// sweep must really sweep: rows scanned and methods called, or it
// fails.
func TestViewerReadsSweepClean(t *testing.T) {
	s, ctx, game := newMarkerGame(t)
	evs, err := s.Events(ctx, game, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	x := IndexLeaks(evs)

	storeType := reflect.TypeOf(s)
	called := 0
	for seat := 1; seat <= 4; seat++ {
		viewer := SeatViewer(seat)
		for i := 0; i < storeType.NumMethod(); i++ {
			m := storeType.Method(i)
			if !takesViewer(m.Type) {
				continue
			}
			for _, extra := range viewerExtras(m.Type, ctx, game, viewer) {
				out := m.Func.Call(append([]reflect.Value{reflect.ValueOf(s)}, extra...))
				called++
				for _, res := range out {
					if !res.CanInterface() {
						continue
					}
					findings := x.Scan(viewer, "seat", res.Interface())
					if len(findings) > 0 {
						t.Fatalf("%s leaked at seat %d's scope: %+v", m.Name, seat, findings)
					}
				}
			}
		}
	}
	if called == 0 {
		t.Fatal("the sweep called no viewer reads — it is vacuous")
	}
}

// takesViewer reports whether a method's parameters include the Viewer
// type — the structural selection the sweep keys on.
func takesViewer(t reflect.Type) bool {
	for i := 0; i < t.NumIn(); i++ {
		if t.In(i) == reflect.TypeOf(Viewer{}) {
			return true
		}
	}
	return false
}

// viewerExtras builds the argument sets for one viewer read: the
// method's own parameters filled around the Viewer position — the
// cursor/limit shapes EventsFor takes get the whole-log and windowed
// variants, everything else a single pass. Context and the game id
// carry the fixture's real values, so the reads really read.
func viewerExtras(t reflect.Type, ctx context.Context, game string, v Viewer) [][]reflect.Value {
	cv := reflect.ValueOf(ctx)
	gv := reflect.ValueOf(game)
	vv := reflect.ValueOf(v)
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	fill := func(after, limit reflect.Value) []reflect.Value {
		var out []reflect.Value
		for i := 1; i < t.NumIn(); i++ {
			switch {
			case t.In(i) == ctxType:
				out = append(out, cv)
			case t.In(i) == reflect.TypeOf(""):
				out = append(out, gv)
			case t.In(i) == reflect.TypeOf(Viewer{}):
				out = append(out, vv)
			case t.In(i) == reflect.TypeOf(int64(0)) && after.IsValid():
				out = append(out, after)
				after = reflect.Value{}
			case t.In(i) == reflect.TypeOf(int(0)) && limit.IsValid():
				out = append(out, limit)
				limit = reflect.Value{}
			default:
				out = append(out, reflect.Zero(t.In(i)))
			}
		}
		return out
	}
	return [][]reflect.Value{
		fill(reflect.Value{}, reflect.Value{}),
		fill(reflect.ValueOf(int64(0)), reflect.ValueOf(0)),
		fill(reflect.ValueOf(int64(1)), reflect.ValueOf(5)),
		fill(reflect.ValueOf(int64(2)), reflect.ValueOf(0)),
	}
}

// TestScanWalksNestedSurfaces keeps the walker honest: a marker inside
// a map key, a slice of pointers and a nested struct is found, and an
// unexported field is not the walk's business.
func TestScanWalksNestedSurfaces(t *testing.T) {
	s, ctx, game := newMarkerGame(t)
	evs, err := s.Events(ctx, game, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	x := IndexLeaks(evs)
	type inner struct{ Note string }
	type outer struct {
		Notes  map[string]int
		Items  []*inner
		Public string
		hidden string
	}
	surface := outer{
		Notes:  map[string]int{"Seat Two Secret": 2},
		Items:  []*inner{{Note: "watch for Hand Two A"}},
		Public: "Forest",
		hidden: "Seat Three Secret",
	}
	findings := x.Scan(SeatViewer(1), "seat1", surface)
	var evidence []string
	for _, f := range findings {
		evidence = append(evidence, f.Evidence)
	}
	joined := strings.Join(evidence, ",")
	if !strings.Contains(joined, "Seat Two Secret") || !strings.Contains(joined, "Hand Two A") {
		t.Fatalf("nested markers missed: %v", evidence)
	}
	if strings.Contains(joined, "Seat Three Secret") {
		t.Fatalf("unexported field scanned: %v", evidence)
	}
	if strings.Contains(joined, "Forest") {
		t.Fatalf("public name flagged: %v", evidence)
	}
}
