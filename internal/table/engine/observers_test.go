package engine

// Judges, spectators and the ruling log (MAD-338): an observer joins a
// live game through the same code a seat is, reads the public stream
// only — the same WHERE clause, spelled PublicViewer — and a ruling is
// a row anchored to an ordinal that survives the rewind it may itself
// have ordered.

import (
	"strings"
	"testing"
	"time"
)

func seedJudge(t *testing.T, s *Store, id, username string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO users (id, username, password_hash, is_admin, created_at)
		VALUES (?, ?, 'x', 0, ?)`, id, username, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestObserveGameJoinsLiveGame: the judge's front door is the one join
// that stays open while play runs — a dispute is exactly when the judge
// arrives. Seats close; observers do not.
func TestObserveGameJoinsLiveGame(t *testing.T) {
	s, _, ctx, game := newPodGame(t)
	seedJudge(t, s, "j1", "mercer")
	code := mustCode(t, s, ctx, game)

	// The pod game is active: a seat is refused, a judge is not.
	if _, _, err := s.JoinGame(ctx, code, "j1", "Mercer"); err == nil {
		t.Fatal("a seat joined a live game")
	}
	g, err := s.ObserveGame(ctx, code, "j1", "Mercer", RoleJudge)
	if err != nil {
		t.Fatalf("judge join: %v", err)
	}
	if g.ID != game {
		t.Fatalf("judge joined game %s", g.ID)
	}
	role, err := s.ObserverRole(ctx, game, "j1")
	if err != nil || role != RoleJudge {
		t.Fatalf("role = %q err %v", role, err)
	}
	// Idempotent: redeeming again keeps the role, mints no second row.
	if _, err := s.ObserveGame(ctx, code, "j1", "Mercer", RoleJudge); err != nil {
		t.Fatalf("re-observe: %v", err)
	}
	obs, err := s.Observers(ctx, game)
	if err != nil || len(obs) != 1 || obs[0].UserID != "j1" || obs[0].Role != RoleJudge {
		t.Fatalf("observers = %+v err %v", obs, err)
	}
	// A spectator joins the same live game; a later redeem can switch
	// the role — the row is the member's place at the table.
	seedJudge(t, s, "s1", "onlooker")
	if _, err := s.ObserveGame(ctx, code, "s1", "Onlooker", RoleSpectator); err != nil {
		t.Fatalf("spectator join: %v", err)
	}
	if _, err := s.ObserveGame(ctx, code, "j1", "Mercer", RoleSpectator); err != nil {
		t.Fatalf("role switch: %v", err)
	}
	if role, _ := s.ObserverRole(ctx, game, "j1"); role != RoleSpectator {
		t.Fatalf("switched role = %q", role)
	}
	// A seated account is never an observer: the seat already carries
	// more than the public stream.
	if _, err := s.ObserveGame(ctx, code, "u2", "Bob", RoleJudge); err == nil {
		t.Fatal("a seated account observed")
	}
	// The owner's observe is the owner's no-op: the game, no row.
	if _, err := s.ObserveGame(ctx, code, "u1", "Collin", RoleJudge); err != nil {
		t.Fatalf("owner observe: %v", err)
	}
	if role, _ := s.ObserverRole(ctx, game, "u1"); role != "" {
		t.Fatalf("owner grew a role: %q", role)
	}
	// The gates: a bad role, a bad code.
	if _, err := s.ObserveGame(ctx, code, "j1", "M", "referee"); err == nil {
		t.Fatal("invented role accepted")
	}
	if _, err := s.ObserveGame(ctx, "NOPE01", "j1", "M", RoleJudge); err == nil {
		t.Fatal("bad code observed")
	}
	// No role at all is the spectator's mistake to make explicitly.
	if _, err := s.ObserveGame(ctx, code, "j1", "M", ""); err == nil {
		t.Fatal("empty role accepted")
	}
}

// TestObserverReadsArePublicOnly: the judge's and the spectator's read
// is the public stream — seat-visible rows are absent, not filtered,
// and the state folds deckless for every seat but their own (they hold
// none).
func TestObserverReadsArePublicOnly(t *testing.T) {
	s, _, ctx, game := newPodGame(t)
	seedJudge(t, s, "j1", "mercer")
	code := mustCode(t, s, ctx, game)
	if _, err := s.ObserveGame(ctx, code, "j1", "Mercer", RoleJudge); err != nil {
		t.Fatal(err)
	}
	v := PublicViewer()
	evs, err := s.EventsFor(ctx, game, v, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	full, err := s.Events(ctx, game, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	private := 0
	for _, e := range full {
		if e.Visibility == VisibilitySeat {
			private++
		}
	}
	if private == 0 {
		t.Fatal("fixture planted no seat-visible rows")
	}
	if len(evs) != len(full)-private {
		t.Fatalf("observer read %d rows, want the %d public ones", len(evs), len(full)-private)
	}
	for _, e := range evs {
		if e.Visibility == VisibilitySeat {
			t.Fatalf("observer read seat-visible row %d", e.Ord)
		}
	}
	// The fold of the public stream carries nobody's deck composition.
	st, err := s.StateFor(ctx, game, v)
	if err != nil {
		t.Fatal(err)
	}
	for seat, p := range st.Seats {
		if p == nil {
			continue
		}
		if p.Deck != nil || p.LibraryExact || p.HandKnown != nil {
			t.Fatalf("observer folded seat %d's hidden zones: deck=%v exact=%v hand=%v",
				seat, p.Deck != nil, p.LibraryExact, p.HandKnown != nil)
		}
	}
}

// TestRulingAnchorsToAnOrdinal: the ruling log's writes — anchored to a
// real ordinal, capped, attributed, listed oldest first.
func TestRulingAnchorsToAnOrdinal(t *testing.T) {
	s, _, ctx, game := newPodGame(t)
	seedJudge(t, s, "j1", "mercer")
	head, err := s.LatestOrd(ctx, game)
	if err != nil || head < 1 {
		t.Fatalf("head = %d err %v", head, err)
	}
	r, err := s.RecordRuling(ctx, game, head, "  the trigger resolves anyway  ", "j1")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if r.Ord != head || r.Note != "the trigger resolves anyway" {
		t.Fatalf("ruling = %+v", r)
	}
	// The ruler's name resolves from the users row, not the id.
	if r.Ruler != "mercer" {
		t.Fatalf("ruler = %q", r.Ruler)
	}
	// Anchors must exist: zero and past-the-head are refused.
	if _, err := s.RecordRuling(ctx, game, 0, "x", "j1"); err == nil {
		t.Fatal("ordinal 0 accepted")
	}
	if _, err := s.RecordRuling(ctx, game, head+1, "x", "j1"); err == nil {
		t.Fatal("past-head ordinal accepted")
	}
	// Empty notes and oversized ones are refused.
	if _, err := s.RecordRuling(ctx, game, head, "   ", "j1"); err == nil {
		t.Fatal("empty note accepted")
	}
	if _, err := s.RecordRuling(ctx, game, head, strings.Repeat("x", rulingCap+1), "j1"); err == nil {
		t.Fatal("oversized note accepted")
	}
	// The list is oldest first and carries the anchor.
	list, err := s.Rulings(ctx, game)
	if err != nil || len(list) != 1 || list[0].ID != r.ID {
		t.Fatalf("rulings = %+v err %v", list, err)
	}
}

// TestRulingsSurviveRewind: the judge's record is never clobbered by a
// truncate — a rewind to before the anchor leaves the ruling standing,
// including one that ordered the rewind.
func TestRulingsSurviveRewind(t *testing.T) {
	s, _, ctx, game := newPodGame(t)
	seedJudge(t, s, "j1", "mercer")
	head, err := s.LatestOrd(ctx, game)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.RecordRuling(ctx, game, head, "rewind to here — the counter was never counted", "j1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RewindTo(ctx, game, head-1); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	list, err := s.Rulings(ctx, game)
	if err != nil || len(list) != 1 || list[0].ID != r.ID || list[0].Ord != head {
		t.Fatalf("rulings after rewind = %+v err %v", list, err)
	}
}

// TestJoinSeatDropsObserverRow: a spectator who takes a chair stops
// being an observer — the roster counts them once, in the seat.
func TestJoinSeatDropsObserverRow(t *testing.T) {
	s, _, ctx, _ := newPodGame(t)
	fresh, err := s.CreateGame(ctx, "u1", "Fifth Table", "commander", 40)
	if err != nil {
		t.Fatal(err)
	}
	seedJudge(t, s, "j1", "mercer")
	if _, err := s.ObserveGame(ctx, fresh.JoinCode, "j1", "Mercer", RoleSpectator); err != nil {
		t.Fatal(err)
	}
	if role, _ := s.ObserverRole(ctx, fresh.ID, "j1"); role != RoleSpectator {
		t.Fatalf("role = %q", role)
	}
	if _, seat, err := s.JoinGame(ctx, fresh.JoinCode, "j1", "Mercer"); err != nil || seat != 1 {
		t.Fatalf("seat join → %d err %v", seat, err)
	}
	if role, _ := s.ObserverRole(ctx, fresh.ID, "j1"); role != "" {
		t.Fatalf("observer row survived the seat: %q", role)
	}
	if obs, _ := s.Observers(ctx, fresh.ID); len(obs) != 0 {
		t.Fatalf("roster = %+v", obs)
	}
}
