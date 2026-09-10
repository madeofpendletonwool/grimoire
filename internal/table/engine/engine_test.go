package engine

import (
	"testing"
)

// threeSeats is the standing test pod: Collin on Atraxa, Bob on Krenko,
// Alice on Urza. Library compositions stay absent — unknown counts are a
// representable value and the tests exercise them.
func threeSeats() []SeatConfig {
	return []SeatConfig{
		{Seat: 1, Name: "Collin", Commander: "Atraxa, Praetors' Voice"},
		{Seat: 2, Name: "Bob", Commander: "Krenko, Mob Boss"},
		{Seat: 3, Name: "Alice", Commander: "Urza, Lord High Artificer"},
	}
}

// start applies START_GAME to a fresh state and folds it.
func start(t *testing.T, seats []SeatConfig) *State {
	t.Helper()
	if seats == nil {
		seats = threeSeats()
	}
	evs, err := Apply(NewState(), Action{Kind: ActionStartGame, Seats: seats})
	if err != nil {
		t.Fatalf("start game: %v", err)
	}
	return Fold(evs)
}

// act applies an action that must succeed and folds it in, returning the
// events it produced.
func act(t *testing.T, s *State, a Action) []Event {
	t.Helper()
	evs, err := Apply(s, a)
	if err != nil {
		t.Fatalf("apply %s: %v", a.Kind, err)
	}
	s.FoldInto(evs)
	return cloneEvents(evs)
}

// rejected asserts the action fails and produces zero events — the
// invariant that makes optimistic application safe to build on.
func rejected(t *testing.T, s *State, a Action) {
	t.Helper()
	evs, err := Apply(s, a)
	if err == nil {
		t.Fatalf("%s: expected rejection", a.Kind)
	}
	if len(evs) != 0 {
		t.Fatalf("%s: rejected action produced %d events", a.Kind, len(evs))
	}
}

func cloneEvents(evs []Event) []Event {
	out := make([]Event, len(evs))
	copy(out, evs)
	return out
}

// summon puts a creature token on a seat's battlefield and returns its id —
// the fast path tests use to build boards. Token specs carry base
// characteristics exactly like declared cards do.
func summon(t *testing.T, s *State, seat int, name string, power, toughness int) int64 {
	t.Helper()
	id := s.NextObject
	act(t, s, Action{Kind: ActionCreateToken, Seat: seat, Token: &TokenSpec{
		Name: name, Types: []string{"Creature"}, Power: &power, Toughness: &toughness}})
	return id
}

// land puts a land on the battlefield (playing from an unknown hand keeps
// the count honest: unknown stays unknown).
func land(t *testing.T, s *State, seat int, name string) int64 {
	t.Helper()
	id := s.NextObject
	act(t, s, Action{Kind: ActionPlayLand, Seat: seat, Card: name,
		Base: &BaseChars{Name: name, Types: []string{"Land"}}})
	return id
}

func TestStartGame(t *testing.T) {
	s := start(t, nil)
	if s.Status != StatusActive {
		t.Fatalf("status = %s", s.Status)
	}
	if len(s.Seats) != 3 || len(s.Order) != 3 {
		t.Fatalf("seats = %d order = %v", len(s.Seats), s.Order)
	}
	if s.Order[0] != 1 || s.Order[1] != 2 || s.Order[2] != 3 {
		t.Fatalf("turn order = %v", s.Order)
	}
	for seat, p := range s.Seats {
		if p.Life != 40 || !p.Alive {
			t.Fatalf("seat %d: life %d alive %v", seat, p.Life, p.Alive)
		}
		// Nothing was told about hands or deckless libraries: unknown is
		// the honest value, not zero.
		if p.Hand.Known || p.Library.Known {
			t.Fatalf("seat %d: hand/library should be unknown, got %+v %+v", seat, p.Hand, p.Library)
		}
	}
	// The three commanders are public fact: objects in the command zone.
	if s.NextObject != 4 {
		t.Fatalf("commander objects not minted: next = %d", s.NextObject)
	}
	for id := int64(1); id < 4; id++ {
		if o := s.Objects[id]; o.Zone != ZoneCommand || o.Controller != int(id) {
			t.Fatalf("commander %d: zone %s controller %d", id, o.Zone, o.Controller)
		}
	}
	if s.Turn != 1 || s.TurnSeat != 1 || s.Phase != "beginning" || s.Step != "untap" {
		t.Fatalf("turn = %d/%d phase = %s/%s", s.Turn, s.TurnSeat, s.Phase, s.Step)
	}
	if s.PrioritySeat != 1 {
		t.Fatalf("priority = %d", s.PrioritySeat)
	}
}

func TestStartGameDefaultsAndEcho(t *testing.T) {
	evs, err := Apply(NewState(), Action{Kind: ActionStartGame,
		Seats: []SeatConfig{{Seat: 2, Name: "Bob"}, {Seat: 1, Name: "Collin", StartingLife: 30}}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if evs[0].Kind != EventGameStarted || evs[0].Format != "commander" || evs[0].StartingLife != 40 {
		t.Fatalf("echo = %+v", evs[0])
	}
	s := Fold(evs)
	if s.Order[0] != 1 {
		t.Fatalf("seats not sorted into turn order: %v", s.Order)
	}
	if s.Seats[1].Life != 30 || s.Seats[2].Life != 40 {
		t.Fatalf("per-seat life override lost: %d %d", s.Seats[1].Life, s.Seats[2].Life)
	}
}

func TestStartGameDeckEchoBecomesLibrary(t *testing.T) {
	s := start(t, []SeatConfig{
		{Seat: 1, Name: "Collin", Commander: "Atraxa, Praetors' Voice", Deck: map[string]int{
			"Forest": 20, "Cultivate": 3, "Avenger of Zendikar": 1}},
		{Seat: 2, Name: "Bob", Commander: "Krenko, Mob Boss"},
	})
	p := s.Seats[1]
	if !p.Library.Known || p.Library.N != 24 {
		t.Fatalf("library = %+v", p.Library)
	}
	if !p.LibraryExact || p.LibraryComp["Forest"] != 20 {
		t.Fatalf("composition = %v exact %v", p.LibraryComp, p.LibraryExact)
	}
	if q := s.Seats[2]; q.Library.Known && q.Library.N != 0 || len(q.LibraryComp) != 0 {
		t.Fatalf("deckless library should be empty-known, got %+v", q.Library)
	}
}

func TestStartGameRejections(t *testing.T) {
	rejected(t, NewState(), Action{Kind: ActionStartGame})                                 // no seats
	rejected(t, NewState(), Action{Kind: ActionStartGame, Seats: []SeatConfig{{Seat: 0}}}) // non-positive
	rejected(t, NewState(), Action{Kind: ActionStartGame, Seats: []SeatConfig{{Seat: 1}, {Seat: 1}}})
	s := start(t, nil)
	rejected(t, s, Action{Kind: ActionStartGame, Seats: threeSeats()}) // already active
	rejected(t, NewState(), Action{Kind: ActionAdvance})               // not started
	rejected(t, NewState(), Action{Kind: "NOPE"})                      // unknown kind
}

func TestEndGame(t *testing.T) {
	s := start(t, nil)
	evs := act(t, s, Action{Kind: ActionEndGame, Seat: 1, Reason: "concession"})
	if evs[0].Kind != EventGameEnded || evs[0].Reason != "concession" {
		t.Fatalf("events = %+v", evs)
	}
	if s.Status != StatusFinished {
		t.Fatalf("status = %s", s.Status)
	}
	rejected(t, s, Action{Kind: ActionEndGame})
	rejected(t, s, Action{Kind: ActionDraw, Seat: 1})
	// A setup game can be abandoned without starting.
	fresh, _ := Apply(NewState(), Action{Kind: ActionEndGame, Reason: "scrubbed"})
	if len(fresh) != 1 {
		t.Fatalf("abandon produced %d events", len(fresh))
	}
}

func TestZoneTrackingLevels(t *testing.T) {
	if ZoneBattlefield.Tracking() != TrackTracked || ZoneStack.Tracking() != TrackTracked ||
		ZoneGraveyard.Tracking() != TrackTracked || ZoneExile.Tracking() != TrackTracked ||
		ZoneCommand.Tracking() != TrackTracked {
		t.Fatal("public zones must be tracked")
	}
	if ZoneHand.Tracking() != TrackCountOnly {
		t.Fatal("hand must be count only")
	}
	if ZoneLibrary.Tracking() != TrackComposition {
		t.Fatal("library must be composition")
	}
	// Unknown is a real value, not zero-in-disguise.
	u := UnknownCount()
	if u.Known || u.N != 0 {
		t.Fatalf("unknown = %+v", u)
	}
}

func TestPlayLand(t *testing.T) {
	s := start(t, nil)
	id := land(t, s, 1, "Forest")
	if o := s.Objects[id]; o.Zone != ZoneBattlefield || o.Controller != 1 || o.Identity.Card != "Forest" {
		t.Fatalf("land = %+v", o)
	}
	if s.Seats[1].LandsThisTurn != 1 {
		t.Fatalf("lands this turn = %d", s.Seats[1].LandsThisTurn)
	}
	rejected(t, s, Action{Kind: ActionPlayLand, Seat: 1, Card: "Second Forest"})
	// Seat 2 can still drop theirs.
	land(t, s, 2, "Mountain")
	// A known-empty hand vetoes the drop.
	s2 := start(t, nil)
	act(t, s2, Action{Kind: ActionSetZoneCount, Seat: 1, Zone: ZoneHand, To: intPtr(0)})
	rejected(t, s2, Action{Kind: ActionPlayLand, Seat: 1, Card: "Forest"})
}

func intPtr(n int) *int { return &n }

func TestCastAndResolve(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionSetZoneCount, Seat: 1, Zone: ZoneHand, To: intPtr(7)})
	id := s.NextObject
	act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Grizzly Bears",
		Base:    &BaseChars{Types: []string{"Creature"}, Power: intPtr(2), Toughness: intPtr(2)},
		Targets: []Target{{Seat: 2}}})
	if s.Seats[1].Hand.N != 6 {
		t.Fatalf("hand = %d", s.Seats[1].Hand.N)
	}
	if len(s.Stack) != 1 || s.Stack[0].Object != id || s.Stack[0].Mode != "cast" {
		t.Fatalf("stack = %+v", s.Stack)
	}
	if o := s.Objects[id]; o.Zone != ZoneStack {
		t.Fatalf("spell zone = %s", o.Zone)
	}
	// All three seats pass: the bear resolves to the battlefield and its
	// ETB is the structural event the trigger registry will fire on.
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	if len(s.Stack) != 0 {
		t.Fatalf("stack did not resolve: %+v", s.Stack)
	}
	if o := s.Objects[id]; o.Zone != ZoneBattlefield {
		t.Fatalf("resolved zone = %s", o.Zone)
	}
	if !s.IsCreature(id) {
		t.Fatal("bear is not a creature")
	}
}

func TestCastSorceryResolvesToGraveyard(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Divination",
		Base: &BaseChars{Types: []string{"Sorcery"}}})
	for seat := 1; seat <= 3; seat++ {
		act(t, s, Action{Kind: ActionPassPriority, Seat: seat})
	}
	if o := s.Objects[s.NextObject-1]; o.Zone != ZoneGraveyard {
		t.Fatalf("sorcery zone = %s", o.Zone)
	}
}

func TestCastCommanderFromCommand(t *testing.T) {
	s := start(t, nil)
	act(t, s, Action{Kind: ActionCast, Seat: 1, FromZone: ZoneCommand,
		Card: "Atraxa, Praetors' Voice",
		Base: &BaseChars{Types: []string{"Creature"}, Power: intPtr(4), Toughness: intPtr(4)}})
	// The commander object moved command → stack: same id, stable across
	// the zone move, exactly as the model doc requires.
	if o := s.Objects[1]; o.Zone != ZoneStack || o.Identity.Card != "Atraxa, Praetors' Voice" {
		t.Fatalf("commander = %+v", o)
	}
	if s.CommanderCasts["Atraxa, Praetors' Voice"] != 1 {
		t.Fatalf("commander casts = %v", s.CommanderCasts)
	}
	if len(s.ZoneObjects(1, ZoneCommand)) != 0 {
		t.Fatal("command zone should be empty while she is on the stack")
	}
}

func TestCastRejections(t *testing.T) {
	s := start(t, nil)
	rejected(t, s, Action{Kind: ActionCast, Seat: 1})                   // no card
	rejected(t, s, Action{Kind: ActionCast, Seat: 9, Card: "Anything"}) // no seat
	act(t, s, Action{Kind: ActionSetZoneCount, Seat: 1, Zone: ZoneHand, To: intPtr(0)})
	rejected(t, s, Action{Kind: ActionCast, Seat: 1, Card: "Grizzly Bears"}) // empty hand
	// Casting from a graveyard that does not hold the card.
	rejected(t, s, Action{Kind: ActionCast, Seat: 1, FromZone: ZoneGraveyard, Card: "Grizzly Bears"})
}

func TestActivate(t *testing.T) {
	s := start(t, nil)
	id := land(t, s, 1, "Sol Ring")
	act(t, s, Action{Kind: ActionActivate, Seat: 1, Object: id,
		Ability: "{T}: Add {C}{C}", Targets: nil})
	if len(s.Stack) != 1 || s.Stack[0].Mode != "activated" || s.Stack[0].Object != id {
		t.Fatalf("stack = %+v", s.Stack)
	}
	rejected(t, s, Action{Kind: ActionActivate, Seat: 1})
	rejected(t, s, Action{Kind: ActionActivate, Seat: 1, Ability: "{T}", Object: 999})
}
