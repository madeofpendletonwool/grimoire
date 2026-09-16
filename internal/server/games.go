package server

// The Magic table's HTTP surface (MAD-326, stage 3 of MAD-321): the
// engine over REST + SSE, in the dice feed's shape.
//
//	POST /api/games                     create a game
//	GET  /api/games                     list my games
//	GET  /api/games/{id}                one game + its folded state
//	POST /api/games/{id}/seats          seat a player (setup only)
//	POST /api/games/{id}/start          start: seats → GAME_STARTED
//	POST /api/games/{id}/actions        submit an Action; body is the Action
//	GET  /api/games/{id}/events         the log window (?after=, ?limit=)
//	GET  /api/games/{id}/stream         the log, live (SSE, ?after=)
//	POST /api/games/{id}/rewind         truncate at an ordinal and re-fold
//	POST /api/games/{id}/amend          truncate at an entry's batch and apply the corrected action
//
// The multiplayer story is the store's: one writer assigns contiguous
// per-game ordinals, clients receive events by ordinal and fold locally,
// and a reconnect replays from the last ordinal it saw — a dropped
// connection is a cursor, not a resync problem. The stream announces a
// rewind with a `rewind` control frame (model.md invariant 2) so no
// client is left folding a log that no longer exists.
//
// Auth is the session gate's: games are scoped per account through
// owner_id, and not-found and not-yours are the same 404 — the chat
// store's rule. The owner sees everything, the DM analog; per-seat
// hidden-zone scoping is 6a's (MAD-337), not this issue's.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
	"github.com/madeofpendletonwool/grimoire/internal/table/odds"
	"github.com/madeofpendletonwool/grimoire/internal/table/universe"
)

// gamesEnabled reports the Magic engine's availability.
func (s *Server) gamesEnabled(w http.ResponseWriter) bool {
	if s.games == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the magic table is not configured on this install"))
		return false
	}
	return true
}

// WithGames wires the Magic engine's store (MAD-326). Without it the
// game endpoints answer 503.
func (s *Server) WithGames(store *engine.Store) *Server {
	s.games = store
	return s
}

// WithUniverse wires the decklist-scoped resolver (MAD-329): spoken and
// typed card names against the attached decks first, the global index
// last, with the per-game identity cache in front. Without it the
// resolve endpoint answers 503 and the game endpoints work on.
func (s *Server) WithUniverse(store *universe.Store) *Server {
	s.universe = store
	return s
}

// universeEnabled reports the resolver's availability.
func (s *Server) universeEnabled(w http.ResponseWriter) bool {
	if s.universe == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("card identification is not configured on this install"))
		return false
	}
	return true
}

// writeGameError maps the engine's sentinels onto HTTP statuses: a
// missing game is 404, a rejected action is 400 (it wrote nothing), a
// scope violation inside a visible game is 403, the odds layer's
// missing card index is 503, and anything else surfaces as 500 without
// its SQL traceback.
func writeGameError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, engine.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, engine.ErrNotEntitled):
		writeError(w, http.StatusForbidden, err)
	case errors.Is(err, engine.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, universe.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, odds.ErrNoCardData):
		writeError(w, http.StatusServiceUnavailable, err)
	case errors.Is(err, odds.ErrOrderDependent),
		errors.Is(err, odds.ErrNoParse):
		// The order refusal is the product talking: 400 so the client
		// renders the engine's own words verbatim.
		writeError(w, http.StatusBadRequest, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

// resolveGame loads the game in the path and enforces the host's
// scope: the owner only. Setup, start, rewind, amend and settings are
// the host's controls — the pod's participants play; the host runs the
// room. Another owner's game answers exactly like a missing one.
func (s *Server) resolveGame(w http.ResponseWriter, r *http.Request) *engine.Game {
	g, err := s.games.GetGame(r.Context(), r.PathValue("id"))
	if err != nil {
		writeGameError(w, err)
		return nil
	}
	if g.OwnerID != userID(r) {
		writeError(w, http.StatusNotFound, engine.ErrNotFound)
		return nil
	}
	return g
}

// gameViewer is the read-side entitlement (MAD-337): the owner sees
// everything (the DM analog), an account holding a seat sees the public
// stream plus that seat's rows, and everyone else gets the same 404 a
// missing game answers — not-found and not-yours are the same answer.
type gameViewer struct {
	Owner bool  `json:"owner"`
	Seats []int `json:"seats,omitempty"`
}

// resolveGameAny loads the game for any entitled reader and returns
// the viewer their reads are scoped to. Every game read a participant
// may reach goes through here; the host-only controls above stay on
// resolveGame.
func (s *Server) resolveGameAny(w http.ResponseWriter, r *http.Request) (*engine.Game, engine.Viewer) {
	g, err := s.games.GetGame(r.Context(), r.PathValue("id"))
	if err != nil {
		writeGameError(w, err)
		return nil, engine.Viewer{}
	}
	if g.OwnerID == userID(r) {
		return g, engine.OwnerViewer()
	}
	seats, err := s.games.SeatsForUser(r.Context(), g.ID, userID(r))
	if err != nil {
		writeGameError(w, err)
		return nil, engine.Viewer{}
	}
	if len(seats) == 0 {
		writeError(w, http.StatusNotFound, engine.ErrNotFound)
		return nil, engine.Viewer{}
	}
	return g, engine.SeatViewer(seats...)
}

// viewerView shapes the entitlement for a response body — the client
// learns which seat is its own, and that is all it learns.
func viewerView(v engine.Viewer) gameViewer {
	out := gameViewer{Owner: v.Owner}
	out.Seats = append(out.Seats, v.Seats...)
	return out
}

// seatGuard enforces that a request's seat parameter is a seat the
// viewer holds: a pod participant acts and asks as themselves. The
// owner passes every seat through.
func seatGuard(v engine.Viewer, seat int) error {
	if v.SeesSeat(seat) {
		return nil
	}
	return fmt.Errorf("%w: seat %d is not yours to act as", engine.ErrNotEntitled, seat)
}

/* ---------- views ---------- */

type gameView struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Format       string         `json:"format"`
	StartingLife int            `json:"starting_life"`
	Status       string         `json:"status"`
	LatestOrd    int64          `json:"latest_ord"`
	Settings     map[string]any `json:"settings,omitempty"`
	// JoinCode is the pod's share code — owner-only in any response
	// (MAD-337): a participant already joined, and a code in a seated
	// client's hands is an invite the host never handed out.
	JoinCode  string  `json:"join_code,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	StartedAt *string `json:"started_at,omitempty"`
	EndedAt   *string `json:"ended_at,omitempty"`
	// Seats is the setup-pane read: the mtg_seats rows while the game is
	// still setup, so a reloaded client can rebuild an unfinished table.
	// Once play begins the fold's GAME_STARTED echo is the seating and
	// this stays nil — the board pane reads the folded state.
	Seats *[]engine.SeatConfig `json:"seats,omitempty"`
}

func toGameView(g *engine.Game, latest int64) gameView {
	v := gameView{
		ID: g.ID, Name: g.Name, Format: g.Format, StartingLife: g.StartingLife,
		Status: string(g.Status), LatestOrd: latest, Settings: g.Settings,
		CreatedAt: g.CreatedAt.Format(http.TimeFormat),
		UpdatedAt: g.UpdatedAt.Format(http.TimeFormat),
	}
	if g.StartedAt != nil {
		t := g.StartedAt.Format(http.TimeFormat)
		v.StartedAt = &t
	}
	if g.EndedAt != nil {
		t := g.EndedAt.Format(http.TimeFormat)
		v.EndedAt = &t
	}
	return v
}

// toGameViewFor shapes the game for one viewer: the owner's copy carries
// the join code, a participant's does not.
func toGameViewFor(g *engine.Game, latest int64, v engine.Viewer) gameView {
	view := toGameView(g, latest)
	if v.Owner {
		view.JoinCode = g.JoinCode
	}
	return view
}

// gameViewFor folds the head ordinal in for a fresh row, viewer-shaped.
func (s *Server) gameViewFor(ctx context.Context, g *engine.Game, v engine.Viewer) gameView {
	latest, err := s.games.LatestOrd(ctx, g.ID)
	if err != nil {
		latest = 0
	}
	return toGameViewFor(g, latest, v)
}

// gameViewWithSeats is the setup pane's read: the game plus the seat
// rows, so a reloaded client can rebuild a table that has not started.
// After start the fold owns the seating and the seats field stays unset.
func (s *Server) gameViewWithSeats(ctx context.Context, g *engine.Game, v engine.Viewer) gameView {
	view := s.gameViewFor(ctx, g, v)
	if g.Status != engine.StatusSetup {
		return view
	}
	seats, err := s.games.Seats(ctx, g.ID)
	if err != nil {
		return view // the pane renders what it can; the error path is the list's
	}
	if len(seats) > 0 {
		view.Seats = &seats
	}
	return view
}

/* ---------- lifecycle ---------- */

// handleCreateGame writes a setup game. Format and life default in the
// store (commander, 40) — the engine is format-agnostic, the defaults
// are the product's first table.
func (s *Server) handleCreateGame(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	var req struct {
		Name         string `json:"name"`
		Format       string `json:"format"`
		StartingLife int    `json:"starting_life"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	g, err := s.games.CreateGame(r.Context(), userID(r), req.Name, req.Format, req.StartingLife)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"game": toGameViewFor(g, 0, engine.OwnerViewer())})
}

// handleListGames answers the caller's games — owned or seated in —
// most recently updated first. Another account's games are absent, not
// hidden, and each row is viewer-shaped: only games the caller owns
// carry a join code.
func (s *Server) handleListGames(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	uid := userID(r)
	games, err := s.games.ListGames(r.Context(), uid)
	if err != nil {
		writeGameError(w, err)
		return
	}
	views := make([]gameView, 0, len(games))
	for _, g := range games {
		v := engine.OwnerViewer()
		if g.OwnerID != uid {
			if seats, err := s.games.SeatsForUser(r.Context(), g.ID, uid); err == nil {
				v = engine.SeatViewer(seats...)
			}
		}
		views = append(views, s.gameViewFor(r.Context(), g, v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"games": views})
}

// handleJoinGame redeems a join code and binds the caller to a seat
// (MAD-337): the pod's front door. The reply carries the game and the
// seat the account now holds, so the client opens the table already
// knowing which chair is its own. Redeeming a code you already redeemed
// answers the same seat — a reloaded tab is a rejoin, not a new seat.
func (s *Server) handleJoinGame(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		if u, ok := userFrom(r.Context()); ok {
			name = u.Username
		}
	}
	g, seat, err := s.games.JoinGame(r.Context(), req.Code, userID(r), name)
	if err != nil {
		writeGameError(w, err)
		return
	}
	v := engine.OwnerViewer()
	if g.OwnerID != userID(r) {
		if seats, err := s.games.SeatsForUser(r.Context(), g.ID, userID(r)); err == nil {
			v = engine.SeatViewer(seats...)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"game":  s.gameViewWithSeats(r.Context(), g, v),
		"seat":  seat,
		"owner": g.OwnerID == userID(r),
	})
}

// handleGetGame answers one game with the viewer's folded state — the
// board's whole payload, scoped the way the store scopes it: the owner
// folds everything, a seated account folds the public stream plus its
// own seat's rows (ADR 13). The viewer block tells the client which
// chair is its own.
func (s *Server) handleGetGame(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	state, err := s.games.StateFor(r.Context(), g.ID, viewer)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"game":   s.gameViewWithSeats(r.Context(), g, viewer),
		"state":  state,
		"viewer": viewerView(viewer),
	})
}

// handleSeatPlayer writes one seat: position in turn order, a display
// name, optionally a bound user, an attached deck and the commander.
// Setup only — once the log begins, seats are what GAME_STARTED echoed.
func (s *Server) handleSeatPlayer(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	var req struct {
		Position     *int   `json:"position"`
		Name         string `json:"name"`
		UserID       string `json:"user_id"`
		DeckID       string `json:"deck_id"`
		Commander    string `json:"commander"`
		StartingLife *int   `json:"starting_life"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	position := 0
	if req.Position != nil {
		position = *req.Position
	}
	if err := s.games.SeatPlayer(r.Context(), g.ID, position, req.Name, req.UserID, req.DeckID, req.Commander, req.StartingLife); err != nil {
		writeGameError(w, err)
		return
	}
	fresh, err := s.games.GetGame(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"game": s.gameViewWithSeats(r.Context(), fresh, engine.OwnerViewer())})
}

// handleStartGame loads the seat table and submits START_GAME: the
// GAME_STARTED echo makes the log self-describing — seats, life,
// commanders, and what every library held when play began.
func (s *Server) handleStartGame(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	evs, state, err := s.games.StartGame(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	fresh, err := s.games.GetGame(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"game": s.gameViewFor(r.Context(), fresh, engine.OwnerViewer()), "state": state, "events": evs})
}

// handleResolveName is the identification half of the intent pipeline
// (MAD-329): a spoken or typed name for one seat resolves against the
// game's known-card universe — the attached decks first, the rest of the
// table next, the global index last, the per-game cache in front of it
// all. The answer carries its method and confidence, which is what the
// confirmation ladder (4c) keys on. Resolution never writes game state;
// an unresolved name says so rather than guessing.
func (s *Server) handleResolveName(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) || !s.universeEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	var req struct {
		Seat   *int   `json:"seat"`
		Spoken string `json:"spoken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	seat := 0
	if req.Seat != nil {
		seat = *req.Seat
	}
	// A pod participant resolves as a seat they hold: the tiers then
	// walk their own deck and the index, and the shared cache serves
	// them public-tier rows only — another seat's library composition
	// cannot ride a resolution (ADR 13).
	if err := seatGuard(viewer, seat); err != nil {
		writeGameError(w, err)
		return
	}
	state, err := s.games.StateFor(r.Context(), g.ID, viewer)
	if err != nil {
		writeGameError(w, err)
		return
	}
	res, err := s.universe.ResolveGameFor(r.Context(), state, g.ID, seat, req.Spoken, !viewer.Owner)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolution": res})
}

/* ---------- the log ---------- */

// handleSubmitAction is the writer's front door: the request body is the
// Action JSON — the same shape the event row's cause column stores, so
// amend prefills from the log itself. A rejected action answers 400 and
// writes nothing, by the reducer's construction. A pod participant may
// act only as a seat they hold (MAD-337); the host acts for anyone, as
// the solo tracker always did. The response is viewer-shaped: events
// the caller may not see never ride their own action's reply.
func (s *Server) handleSubmitAction(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	var action engine.Action
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&action); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid action body: %v", err))
		return
	}
	if action.Kind == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("an action needs a kind"))
		return
	}
	if err := seatGuard(viewer, action.Seat); err != nil {
		writeGameError(w, err)
		return
	}
	if action.Source == "" {
		action.Source = "manual" // API entry is a hand on the tracker
	}
	evs, state, err := s.games.Submit(r.Context(), g.ID, action)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events": engine.FilterEvents(viewer, evs),
		"state":  engine.RedactFor(state, viewer),
	})
}

// handleGameEvents answers the log window past an ordinal, oldest
// first, with the head ordinal so an empty window still carries the
// cursor's horizon — the REST replay the stream resumes from. The
// window is the viewer's: selected in SQL, public rows plus the seats
// they hold (ADR 13).
func (s *Server) handleGameEvents(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	evs, err := s.games.EventsFor(r.Context(), g.ID, viewer, after, limit)
	if err != nil {
		writeGameError(w, err)
		return
	}
	latest, err := s.games.LatestOrd(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "latest": latest})
}

// handleRewindGame truncates the log past an ordinal and answers the
// refolded state: undo as truncate-and-refold, the whole correction
// story. Attached streams wake and announce the rewind as a control
// frame, so every client re-folds instead of holding rows that no
// longer exist.
func (s *Server) handleRewindGame(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	var req struct {
		To *int64 `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if req.To == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a rewind needs the ordinal to rewind to"))
		return
	}
	state, err := s.games.RewindTo(r.Context(), g.ID, *req.To)
	if err != nil {
		writeGameError(w, err)
		return
	}
	// A question about an entry the rewind removed is not waiting on
	// anything: the tray closes it (mtg_pending's contract).
	s.dismissRewound(r, g.ID, state)
	fresh, err := s.games.GetGame(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	head := int64(0)
	if state != nil {
		head = state.LastOrd
	}
	writeJSON(w, http.StatusOK, map[string]any{"game": s.gameViewFor(r.Context(), fresh, engine.OwnerViewer()), "state": state, "head": head})
}

/* ---------- the live stream ---------- */

// handleAmendGame is the correction path the log pane owns (MAD-328):
// truncate at the entry's batch and apply the corrected action in one
// store transaction, so no attached client ever observes the rewound
// intermediate and a rejected correction (400) leaves the log exactly
// as it was. `at` is any ordinal of the entry being corrected; `action`
// is the corrected Action — the shape the entry's own cause column
// stores, which is where the client prefills from.
func (s *Server) handleAmendGame(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g := s.resolveGame(w, r)
	if g == nil {
		return
	}
	var req struct {
		At     *int64         `json:"at"`
		Action *engine.Action `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if req.At == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("an amend needs the ordinal it corrects"))
		return
	}
	if req.Action == nil || req.Action.Kind == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("an amend needs the corrected action"))
		return
	}
	if req.Action.Source == "" {
		req.Action.Source = "manual" // API entry is a hand on the tracker
	}
	evs, state, err := s.games.AmendAt(r.Context(), g.ID, *req.At, *req.Action)
	if err != nil {
		writeGameError(w, err)
		return
	}
	// Amend is a rewind with a correction riding: the tray's contract
	// applies exactly as it does behind a bare rewind.
	s.dismissRewound(r, g.ID, state)
	fresh, err := s.games.GetGame(r.Context(), g.ID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"game": s.gameViewFor(r.Context(), fresh, engine.OwnerViewer()), "events": evs, "state": state})
}

/* ---------- the ordinal stream ---------- */

// handleGameStream is the game's ordinal feed: every event as it lands,
// pushed the moment the writer commits. A client holding a cursor
// resumes with ?after=<last ordinal seen> and receives exactly the rows
// it missed, in order — the reconnect contract. Rewind detection is the
// sentinel: the row at the cursor must still be the row this stream
// last saw; if it is gone or replaced, the log was truncated (and
// possibly re-appended) past the client's position, and the stream says
// so with a `rewind` frame carrying the current head. A client that
// receives it re-folds — re-requesting the window from an ordinal it
// trusts — before applying anything that follows.
func (s *Server) handleGameStream(w http.ResponseWriter, r *http.Request) {
	if !s.gamesEnabled(w) {
		return
	}
	g, viewer := s.resolveGameAny(w, r)
	if g == nil {
		return
	}
	gameID := g.ID

	var after int64
	// Seed the cursor honestly: no cursor means the client paints the
	// window through the REST feed and the stream starts at the present;
	// a stale or runaway cursor clamps to the head. Either way, remember
	// which row the cursor points at so a later rewind is detectable by
	// identity, not just by absence.
	head, err := s.games.LatestOrd(r.Context(), gameID)
	if err != nil {
		writeGameError(w, err)
		return
	}
	if v := r.URL.Query().Get("after"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
		if after < 0 || after > head {
			after = head
		}
	} else {
		after = head
	}
	lastID := ""
	if after > 0 {
		if id, err := s.games.EventIDAt(r.Context(), gameID, after); err == nil {
			lastID = id
		}
	}

	sse := newSSEWriter(w)
	wake, stop := s.games.Subscribe(gameID)
	defer stop()

	sendNew := func() bool {
		// The rewind sentinel: the row at the cursor must still exist
		// and still be the one this stream last saw there.
		if after > 0 {
			id, err := s.games.EventIDAt(r.Context(), gameID, after)
			if err != nil && !errors.Is(err, engine.ErrNotFound) {
				return false
			}
			if err != nil || id != lastID {
				head, err := s.games.LatestOrd(r.Context(), gameID)
				if err != nil {
					return false
				}
				if err := sse.send("rewind", map[string]any{"head": head}); err != nil {
					return false
				}
				after, lastID = head, ""
				if after > 0 {
					if id, err := s.games.EventIDAt(r.Context(), gameID, after); err == nil {
						lastID = id
					}
				}
				return true // resynced; new events stream on the next pass
			}
		}
		evs, err := s.games.EventsFor(r.Context(), gameID, viewer, after, 200)
		if err != nil {
			return false
		}
		for i := range evs {
			if err := sse.send("event", map[string]any{"event": evs[i]}); err != nil {
				return false
			}
			after, lastID = evs[i].Ord, evs[i].ID
		}
		return true
	}
	_ = sse.send("open", map[string]any{"after": after})

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	poll := time.NewTicker(2 * time.Second) // safety net for a missed wake
	defer poll.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
			if !sendNew() {
				return
			}
		case <-poll.C:
			if !sendNew() {
				return
			}
		case <-ping.C:
			if err := sse.send("ping", map[string]any{"t": time.Now().UTC().Unix()}); err != nil {
				return
			}
		}
	}
}
