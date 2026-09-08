package server

// The dice surface (MAD-420, stage 3 of MAD-417): one write and two
// reads.
//
//	POST /api/campaigns/{id}/rolls          roll (any member)
//	GET  /api/campaigns/{id}/rolls          the feed window (?after=, ?limit=)
//	GET  /api/campaigns/{id}/rolls/stream   the feed, live (SSE)
//
// Permissions are the issue's one rule: a player rolls public, as their
// bound character, and reads exactly the public feed — the filter is in
// the query, so a secret roll is absent from a player-scoped read, not
// hidden after the fact (the leak test asserts the absence). The DM may
// roll secret as anyone. `/r 3d6+2` in the composer and the roll bar in
// the dice window both land here.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
)

// diceEnabled reports the roller's availability.
func (s *Server) diceEnabled(w http.ResponseWriter) bool {
	if s.dice == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the dice roller is not configured on this install"))
		return false
	}
	return true
}

// WithDice wires the dice engine's store (MAD-420). Without it the roll
// endpoints answer 503.
func (s *Server) WithDice(store *dice.Store) *Server {
	s.dice = store
	return s
}

/* ---------- views ---------- */

type rollView struct {
	ID             string          `json:"id"`
	Seq            int64           `json:"seq"`
	SessionID      string          `json:"session_id,omitempty"`
	SessionEventID string          `json:"session_event_id,omitempty"`
	ActorID        string          `json:"actor_id"`
	ActorName      string          `json:"actor_name"`
	CharacterID    string          `json:"character_id,omitempty"`
	CharacterName  string          `json:"character_name,omitempty"`
	ContextKind    string          `json:"context"`
	Detail         string          `json:"detail,omitempty"`
	TargetID       string          `json:"target_id,omitempty"`
	TargetName     string          `json:"target_name,omitempty"`
	Formula        string          `json:"formula"`
	Mode           string          `json:"mode,omitempty"`
	Inspiration    bool            `json:"inspiration,omitempty"` // the roll that spent it (MAD-428)
	Notation       string          `json:"notation"`
	Dice           json.RawMessage `json:"dice,omitempty"`
	Modifier       int             `json:"modifier"`
	Total          int             `json:"total"`
	Natural20      bool            `json:"natural_20,omitempty"`
	Natural1       bool            `json:"natural_1,omitempty"`
	Visibility     string          `json:"visibility"`
	CreatedAt      string          `json:"created_at"`
}

func toRollView(r dice.RollRow) rollView {
	v := rollView{
		ID: r.ID, Seq: r.Seq, SessionID: r.SessionID, SessionEventID: r.SessionEventID,
		ActorID: r.Actor, ActorName: r.ActorName,
		CharacterID: r.CharacterID, CharacterName: r.CharacterName,
		ContextKind: r.ContextKind, Detail: r.Detail,
		TargetID: r.TargetID, TargetName: r.TargetName,
		Formula: r.Formula, Mode: r.Mode, Notation: r.Notation(),
		Inspiration: r.Inspiration,
		Modifier:    r.Result.Modifier, Total: r.Result.Total,
		Natural20: r.Result.Natural20, Natural1: r.Result.Natural1,
		Visibility: r.Visibility,
		CreatedAt:  r.CreatedAt.Format(http.TimeFormat),
	}
	if len(r.Result.Terms) > 0 {
		if b, err := json.Marshal(r.Result.Terms); err == nil {
			v.Dice = b
		}
	}
	return v
}

/* ---------- rolling ---------- */

// rollRequest is the composer's and the roll bar's shared body.
type rollRequest struct {
	Formula     string `json:"formula"`
	Mode        string `json:"mode"`
	Visibility  string `json:"visibility"`
	ContextKind string `json:"context"`
	Detail      string `json:"detail"`
	TargetID    string `json:"target_id"`
	CharacterID string `json:"character_id"`
	SessionID   string `json:"session_id"`
	// SpendInspiration spends the character's inspiration on this roll
	// (MAD-428): advantage on one attack roll, saving throw or ability
	// check — the 2014 rule, enforced as the rule reads.
	SpendInspiration bool `json:"spend_inspiration"`
}

func (s *Server) handleRollDice(w http.ResponseWriter, r *http.Request) {
	if !s.diceEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	var req rollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	// The permission lines. A player rolls public, as their bound
	// character — a secret roll is the DM's alone, and the roller never
	// lets a player claim to be someone else. The DM rolls as anyone,
	// secretly if the table calls for it.
	if !a.isDM() {
		if req.Visibility == dice.VisibilitySecret {
			writeError(w, http.StatusForbidden, fmt.Errorf("a secret roll is the DM's"))
			return
		}
		if req.CharacterID != "" && (a.view == nil ||
			a.playerScope.Kind() != campaign.ScopeKindCharacter ||
			a.playerScope.EntityID() != req.CharacterID) {
			writeError(w, http.StatusForbidden, fmt.Errorf("you roll as your own character"))
			return
		}
		if req.CharacterID == "" && a.view != nil && a.playerScope.Kind() == campaign.ScopeKindCharacter {
			req.CharacterID = a.playerScope.EntityID()
		}
	}
	var spend *ledger.TxnRow
	if req.SpendInspiration {
		txn, err := s.spendInspirationOnRoll(w, r, a, &req)
		if err != nil {
			return // the helper wrote the response
		}
		spend = txn
	}
	roll, err := s.dice.Roll(r.Context(), a.campaign.ID, dice.Input{
		Formula: req.Formula, Mode: req.Mode, Visibility: req.Visibility,
		ContextKind: req.ContextKind, Detail: req.Detail, TargetID: req.TargetID,
		CharacterID: req.CharacterID, SessionID: req.SessionID, Actor: userID(r),
		Inspiration: req.SpendInspiration,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The spend rode ahead of the roll; point it at the event the roll
	// left behind, so the ledger reads the same story the log does.
	if spend != nil {
		_ = s.ledgers.LinkTxnEvent(r.Context(), spend.ID, roll.SessionEventID, roll.SessionID)
	}
	body := map[string]any{"roll": toRollView(*roll)}
	if spend != nil {
		body["inspiration"] = toTxnView(*spend)
	}
	writeJSON(w, http.StatusCreated, body)
}

// spendInspirationOnRoll applies the 2014 rule to one roll request:
// inspiration buys advantage on one attack roll, saving throw or
// ability check — never initiative, never damage, never a roll that
// already carries a mode, and never for a character who does not hold
// it. The spend is validated and written atomically by the ledger
// before the dice move; the caller links it to the roll's event after.
// The roll itself is checked first (dice.Check) so a malformed formula
// cannot eat a character's inspiration. Returns nil only after the
// response has been written.
func (s *Server) spendInspirationOnRoll(w http.ResponseWriter, r *http.Request, a *campAccess, req *rollRequest) (*ledger.TxnRow, error) {
	if s.ledgers == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the resource ledger is not configured on this install"))
		return nil, fmt.Errorf("no ledger")
	}
	if req.CharacterID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("spending inspiration needs a character to spend it"))
		return nil, fmt.Errorf("no character")
	}
	switch req.ContextKind {
	case dice.ContextAttack, dice.ContextSave, dice.ContextCheck:
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"inspiration buys advantage on an attack roll, saving throw or ability check — not %q",
			req.ContextKind))
		return nil, fmt.Errorf("context %q", req.ContextKind)
	}
	if req.Mode != "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the roll already carries %s; inspiration is not spent to stack it", req.Mode))
		return nil, fmt.Errorf("mode %q", req.Mode)
	}
	// The formula must accept advantage before anything is spent —
	// the engine's own rule, checked without writing.
	req.Mode = dice.ModeAdvantage
	if err := dice.Check(dice.Input{
		Formula: req.Formula, Mode: req.Mode, Visibility: req.Visibility,
		ContextKind: req.ContextKind, Detail: req.Detail,
	}); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("inspiration grants advantage: %v", err))
		return nil, err
	}
	txn, err := s.ledgers.TrySpendInspiration(r.Context(), a.campaign.ID, req.CharacterID,
		"advantage on a "+req.ContextKind, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return nil, err
	}
	return txn, nil
}

/* ---------- the feed ---------- */

func (s *Server) handleRollFeed(w http.ResponseWriter, r *http.Request) {
	if !s.diceEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	rolls, err := s.dice.Feed(r.Context(), a.campaign.ID, after, limit, a.isDM())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]rollView, 0, len(rolls))
	var latest int64
	for _, roll := range rolls {
		views = append(views, toRollView(roll))
		if roll.Seq > latest {
			latest = roll.Seq
		}
	}
	if latest == 0 {
		if latest, err = s.dice.LatestSeq(r.Context(), a.campaign.ID, a.isDM()); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	// The caller's standing rides along so a player surface knows whose
	// sheet to load for quick rolls — the same fact their membership row
	// already pins, resolved server-side as always.
	standing := map[string]any{"dm": a.isDM()}
	if !a.isDM() && a.view != nil && a.playerScope.Kind() == campaign.ScopeKindCharacter {
		standing["character_id"] = a.playerScope.EntityID()
	}
	writeJSON(w, http.StatusOK, map[string]any{"rolls": views, "latest": latest, "standing": standing})
}

/* ---------- the live stream ---------- */

// handleRollStream is the party's shared moment: every roll the caller may
// see, pushed as it lands. The cursor resumes exactly where the feed
// window ended; visibility filtering is the same query the feed uses, so
// the stream cannot leak what the feed would not show. A ping keeps
// proxies from timing the connection out; a slow poll is the safety net
// for any ping a subscriber misses — order comes from the seq either way.
func (s *Server) handleRollStream(w http.ResponseWriter, r *http.Request) {
	if !s.diceEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	campaignID := a.campaign.ID
	dm := a.isDM()
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	} else {
		// No cursor: the client paints the window through the REST feed;
		// the stream starts at the present.
		latest, err := s.dice.LatestSeq(r.Context(), campaignID, dm)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		after = latest
	}

	sse := newSSEWriter(w)
	wake, stop := s.dice.Subscribe(campaignID)
	defer stop()

	sendNew := func() bool {
		rolls, err := s.dice.Feed(r.Context(), campaignID, after, 200, dm)
		if err != nil {
			return false
		}
		for _, roll := range rolls {
			if roll.Seq <= after {
				continue
			}
			if err := sse.send("roll", map[string]any{"roll": toRollView(roll)}); err != nil {
				return false
			}
			after = roll.Seq
		}
		return true
	}
	_ = sse.send("open", map[string]any{"after": after})

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	poll := time.NewTicker(2 * time.Second)
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
