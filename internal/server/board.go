package server

// The party board's surface (MAD-423, stage 6 of MAD-417): the table's
// live view of the party's mechanical state, over the campaign pub/sub.
//
//	GET /api/campaigns/{id}/board           the snapshot
//	GET /api/campaigns/{id}/board/stream    the board, live (SSE)
//	PUT /api/campaigns/{id}/board/settings  the visibility config (DM)
//
// Permissions are the member row's, resolved by the same
// resolveCampaignAccess every campaign handler runs: any member reads
// the board, the DM's read adds the monster side, and the visibility
// config is the owner's to set. The config lives on the campaign and
// the snapshot builder enforces it by construction — a hidden field is
// absent from the payload (the leak tests assert the absence on the raw
// body), never merely hidden in the client.
//
// The stream is the dice feed's shape applied to snapshots: an open
// frame paints the board, then a wake re-derives it and sends it only
// when it changed; a ping keeps proxies honest and a slow poll is the
// safety net for a ping any subscriber misses. Presence rides the same
// connection — holding the stream open is being at the table.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/board"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// boardEnabled reports the board's availability.
func (s *Server) boardEnabled(w http.ResponseWriter) bool {
	if s.board == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the party board is not configured on this install"))
		return false
	}
	return true
}

// WithBoard wires the party board (MAD-423). Without it the board
// endpoints answer 503.
func (s *Server) WithBoard(store *board.Store) *Server {
	s.board = store
	return s
}

// boardStanding translates the resolved member row into the builder's
// viewer: the DM perspective, a player bound to their own character, or
// an observer with no self exception to claim.
func boardStanding(a *campAccess) board.Standing {
	if a.isDM() {
		return board.DMStanding()
	}
	if a.view != nil && a.playerScope.Kind() == campaign.ScopeKindCharacter {
		return board.PlayerStanding(a.playerScope.EntityID())
	}
	return board.PlayerStanding()
}

/* ---------- the snapshot ---------- */

func (s *Server) handleBoardSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.boardEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	snap, err := s.board.Snapshot(r.Context(), a.campaign.ID, boardStanding(a))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"board": snap})
}

/* ---------- the live stream ---------- */

func (s *Server) handleBoardStream(w http.ResponseWriter, r *http.Request) {
	if !s.boardEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	campaignID := a.campaign.ID
	standing := boardStanding(a)
	uid := userID(r)

	sse := newSSEWriter(w)
	wake, stop := s.board.SubscribeAs(campaignID, uid)
	// Leaving the table is a board change too: cancel presence, then
	// wake the survivors' streams to re-render without us.
	defer func() {
		stop()
		s.board.Notify(campaignID)
	}()
	s.board.Notify(campaignID)

	// sendAll paints the board; sendIfChanged is the stream's steady
	// state — identical snapshots (the poll's usual finding) send
	// nothing, so the wire stays quiet between real changes.
	last := []byte(nil)
	snapshot := func() ([]byte, *board.Snapshot, error) {
		snap, err := s.board.Snapshot(r.Context(), campaignID, standing)
		if err != nil {
			return nil, nil, err
		}
		body, err := json.Marshal(snap)
		if err != nil {
			return nil, nil, err
		}
		return body, snap, nil
	}
	body, snap, err := snapshot()
	if err != nil {
		return
	}
	last = body
	if err := sse.send("open", map[string]any{"board": snap}); err != nil {
		return
	}

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-poll.C:
		case <-ping.C:
			if err := sse.send("ping", map[string]any{"t": time.Now().UTC().Unix()}); err != nil {
				return
			}
			continue
		}
		body, snap, err := snapshot()
		if err != nil {
			return
		}
		if bytes.Equal(body, last) {
			continue
		}
		last = body
		if err := sse.send("board", map[string]any{"board": snap}); err != nil {
			return
		}
	}
}

/* ---------- the config ---------- */

// handleBoardSettings writes the visibility config: data on the
// campaign under the board settings key, validated strictly (the house
// rule — errors, never guesses), owner-shaped like every settings
// write. The store's own notify wakes every stream, so a table going
// word-mode re-renders live.
func (s *Server) handleBoardSettings(w http.ResponseWriter, r *http.Request) {
	if !s.boardEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.isDM() || (a.campaign.OwnerID != userID(r) && !a.keeper) {
		writeError(w, http.StatusForbidden, fmt.Errorf("only the campaign's owner may set the board's visibility"))
		return
	}
	var req struct {
		HP    string `json:"hp"`
		Slots string `json:"slots"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	// An empty field keeps the current value — a toggle changes what it
	// names, not everything at once.
	current := board.ConfigOf(a.campaign.Settings[board.SettingsKey])
	raw := current.SettingsValue()
	if req.HP != "" {
		raw["hp"] = req.HP
	}
	if req.Slots != "" {
		raw["slots"] = req.Slots
	}
	cfg, err := board.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	settings := a.campaign.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	settings[board.SettingsKey] = cfg.SettingsValue()
	c, err := s.campaigns.UpdateCampaign(r.Context(), a.campaign.OwnerID, a.campaign.ID,
		nil, nil, nil, nil, settings)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"board_settings": board.ConfigOf(c.Settings[board.SettingsKey])})
}
