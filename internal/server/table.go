package server

// The table screen's surface (MAD-425, stage 8 of MAD-417): the room's
// public page, live over the campaign pub/sub.
//
//	POST   /api/campaigns/{id}/table-screen        mint a screen link (DM)
//	GET    /api/campaigns/{id}/table-screen        the campaign's links (DM)
//	DELETE /api/campaigns/{id}/table-screen/{token} close one (DM)
//	GET    /t/{token}                              the projector page
//	GET    /t/{token}/board                        the public snapshot (JSON)
//	GET    /t/{token}/stream                       the screen, live (SSE)
//
// The public routes live outside /api on purpose — the share page's
// rule: no session is required and none is consulted, the token is the
// whole access model, and EventSource cannot carry headers anyway.
// Everything the screen may read was decided while the snapshot was
// built (internal/table — the observer standing, the public roll
// query, the DM's per-monster reveal); nothing here filters after the
// fact, because there is nothing to filter.
//
// The stream is the board stream and the dice stream braided: an open
// frame paints the whole screen, a wake re-derives the view and sends
// it only when it changed, public rolls ride their own events off the
// seq cursor, and a revocation — checked on the ping cadence — closes
// the screen with a gone event rather than a silent hang. A projector
// that drops and reconnects re-enters through the open frame; a slow
// poll is the safety net for any ping a subscriber misses.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/table"
)

// WithTable wires the table screen (MAD-425). Without it the mint
// endpoints answer 503 and the public routes render their closed page.
func (s *Server) WithTable(store *table.Store) *Server {
	s.table = store
	return s
}

// tableEnabled reports the screen's availability.
func (s *Server) tableEnabled(w http.ResponseWriter) bool {
	if s.table == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the table screen is not configured on this install"))
		return false
	}
	return true
}

/* ---------- minting (the DM's side) ---------- */

// tableView is one link in the DM's list. URL is absolute so it can be
// copied straight onto the TV.
type tableView struct {
	Token     string `json:"token"`
	URL       string `json:"url"`
	CreatedAt string `json:"created_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

func toTableView(r *http.Request, sc table.Screen) tableView {
	return tableView{
		Token: sc.Token, URL: tableURL(r, sc.Token),
		CreatedAt: sc.CreatedAt.Format(time.RFC3339),
		RevokedAt: timeFmt(sc.RevokedAt),
		LastSeen:  timeFmt(sc.LastSeen),
	}
}

// tableURL builds the projector link against the request's host — the
// same behind-any-proxy reasoning as inviteURL and shareURL.
func tableURL(r *http.Request, token string) string {
	scheme := "http"
	if secureCookies(r) {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/t/%s", scheme, r.Host, token)
}

func (s *Server) handleTableScreenList(w http.ResponseWriter, r *http.Request) {
	if !s.tableEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	screens, err := s.table.List(r.Context(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]tableView, 0, len(screens))
	for _, sc := range screens {
		out = append(out, toTableView(r, sc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"table_screens": out})
}

func (s *Server) handleTableScreenMint(w http.ResponseWriter, r *http.Request) {
	if !s.tableEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	token, err := s.table.Mint(r.Context(), a.campaign.ID, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"url": "/t/" + token, "token": token})
}

func (s *Server) handleTableScreenRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.tableEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if err := s.table.Revoke(r.Context(), a.campaign.ID, r.PathValue("token")); err != nil {
		if errors.Is(err, table.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	// Wake the campaign's streams: an open projector re-checks its
	// token and learns it is closed, rather than hanging on it.
	s.table.Notify(a.campaign.ID)
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- the public side ---------- */

// tablePageView is everything the standalone page template renders.
type tablePageView struct {
	State string // "ok" | "gone" | "missing"
	Token string
}

// resolveTable maps a path token to its campaign, answering the public
// error itself. It returns "" when the caller should not proceed.
func (s *Server) resolveTable(w http.ResponseWriter, r *http.Request, token string) string {
	if !validTokenRe.MatchString(token) {
		writeError(w, http.StatusNotFound, fmt.Errorf("not found"))
		return ""
	}
	campaignID, err := s.table.Resolve(r.Context(), token)
	switch {
	case errors.Is(err, table.ErrRevoked):
		writeError(w, http.StatusGone, err)
		return ""
	case errors.Is(err, table.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
		return ""
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return ""
	}
	return campaignID
}

// handleTablePage serves the projector page — no session, no script but
// the painter, the token in the URL the whole access model.
func (s *Server) handleTablePage(w http.ResponseWriter, r *http.Request) {
	v := tablePageView{State: "missing", Token: r.PathValue("token")}
	if s.table == nil {
		s.renderTablePage(w, http.StatusNotFound, v)
		return
	}
	token := r.PathValue("token")
	if validTokenRe.MatchString(token) {
		_, err := s.table.Resolve(r.Context(), token)
		switch {
		case err == nil:
			v.State = "ok"
			v.Token = token
		case errors.Is(err, table.ErrRevoked):
			v.State = "gone"
		case errors.Is(err, table.ErrNotFound):
			// missing — the default
		default:
			v.State = "missing"
		}
	}
	status := http.StatusOK
	if v.State != "ok" {
		status = http.StatusNotFound
		if v.State == "gone" {
			status = http.StatusGone
		}
	}
	s.renderTablePage(w, status, v)
}

func (s *Server) renderTablePage(w http.ResponseWriter, status int, v tablePageView) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, "table.html", v); err != nil {
		log.Printf("render table.html: %v", err)
	}
}

// handleTableBoard serves the public snapshot as JSON — the page's
// first paint and the leak test's read.
func (s *Server) handleTableBoard(w http.ResponseWriter, r *http.Request) {
	if !s.tableEnabled(w) {
		return
	}
	campaignID := s.resolveTable(w, r, r.PathValue("token"))
	if campaignID == "" {
		return
	}
	snap, err := s.table.Snapshot(r.Context(), campaignID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"table": snap})
}

// handleTableStream is the projector's one open connection: the board's
// change detection and the dice feed's cursor, braided.
func (s *Server) handleTableStream(w http.ResponseWriter, r *http.Request) {
	if !s.tableEnabled(w) {
		return
	}
	token := r.PathValue("token")
	campaignID := s.resolveTable(w, r, token)
	if campaignID == "" {
		return
	}

	after := int64(0)
	if v := r.URL.Query().Get("after"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}

	sse := newSSEWriter(w)
	wake, stop := s.table.Subscribe(campaignID)
	defer stop()

	// The open frame paints the whole screen — snapshot plus dice — and
	// sets the cursor the roll events resume from. A client that
	// reconnects with its own cursor keeps it: the follow-up read
	// delivers everything since, in seq order.
	snap, err := s.table.Snapshot(r.Context(), campaignID)
	if err != nil {
		return
	}
	if after == 0 {
		after = snap.Latest
	}
	if err := sse.send("open", map[string]any{"table": snap, "after": after}); err != nil {
		return
	}

	view := func() ([]byte, *table.Snapshot, error) {
		v, err := s.table.View(r.Context(), campaignID)
		if err != nil {
			return nil, nil, err
		}
		body, err := json.Marshal(v)
		if err != nil {
			return nil, nil, err
		}
		return body, v, nil
	}
	last, _, err := view()
	if err != nil {
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
			// The poll is the safety net for a ping any subscriber
			// misses; it re-checks the token too, so a closed screen
			// is gone within moments even without the wake.
			if _, err := s.table.Check(r.Context(), token); err != nil {
				_ = sse.send("gone", map[string]any{"t": time.Now().UTC().Unix()})
				return
			}
		case <-ping.C:
			// The ping doubles as the heartbeat: resolve stamps the
			// screen's last_seen and notices a revocation.
			if _, err := s.table.Resolve(r.Context(), token); err != nil {
				_ = sse.send("gone", map[string]any{"t": time.Now().UTC().Unix()})
				return
			}
			if err := sse.send("ping", map[string]any{"t": time.Now().UTC().Unix()}); err != nil {
				return
			}
			continue
		}
		// A wake (a revoke included) re-checks the token before doing
		// any work — the cheap read that keeps a dead stream short.
		if _, err := s.table.Check(r.Context(), token); err != nil {
			_ = sse.send("gone", map[string]any{"t": time.Now().UTC().Unix()})
			return
		}
		// Dice first — the cursor keeps order no matter what the board
		// read does.
		rolls, _, err := s.table.RollsAfter(r.Context(), campaignID, after, 200)
		if err != nil {
			return
		}
		for _, roll := range rolls {
			if err := sse.send("roll", map[string]any{"roll": roll}); err != nil {
				return
			}
			after = roll.Seq
		}
		body, next, err := view()
		if err != nil {
			return
		}
		if bytes.Equal(body, last) {
			continue
		}
		last = body
		if err := sse.send("board", map[string]any{"table": next}); err != nil {
			return
		}
	}
}
