package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/madeofpendletonwool/grimoire/internal/uistate"
)

// WithUIState wires the Campaign OS layout store. Separate from New so it is
// additive: without it the window manager keeps its layouts in the browser and
// these endpoints report unavailable, which is exactly the degradation an
// installation running without accounts wants.
func (s *Server) WithUIState(store *uistate.Store) *Server {
	s.ui = store
	return s
}

func (s *Server) uiEnabled(w http.ResponseWriter) bool {
	if s.ui == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("saved layouts are not available"))
		return false
	}
	return true
}

// uiStatus maps a store error onto a status code. Everything the caller can
// fix is ErrInvalid and answers 400; anything else is ours and answers 500.
func uiStatus(err error) int {
	if errors.Is(err, uistate.ErrInvalid) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// handleUILayouts returns every saved workspace for the caller and one corpus.
//
// A new account has none, and that is a 200 with an empty list rather than a
// 404: the client seeds the presets itself, so "you have not saved anything"
// and "this corpus does not exist" must not look the same.
func (s *Server) handleUILayouts(w http.ResponseWriter, r *http.Request) {
	if !s.uiEnabled(w) {
		return
	}
	corpus := r.URL.Query().Get("corpus")
	layouts, err := s.ui.Layouts(r.Context(), userID(r), corpus)
	if err != nil {
		writeError(w, uiStatus(err), err)
		return
	}
	if layouts == nil {
		layouts = []uistate.Layout{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"layouts": layouts})
}

// handleUISaveLayout writes one workspace slot.
//
// The tree is taken as raw JSON and validated rather than decoded into a Go
// shape and re-encoded: the front end owns the layout's vocabulary, and a
// server that rewrote it on the way through would silently move the user's
// windows every time the two drifted.
func (s *Server) handleUISaveLayout(w http.ResponseWriter, r *http.Request) {
	if !s.uiEnabled(w) {
		return
	}
	var req struct {
		Corpus string          `json:"corpus"`
		Slot   int             `json:"slot"`
		Name   string          `json:"name"`
		Tree   json.RawMessage `json:"tree"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, uistate.MaxTreeSize*2)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}

	l := uistate.Layout{Corpus: req.Corpus, Slot: req.Slot, Name: req.Name, Tree: req.Tree}
	if err := s.ui.SaveLayout(r.Context(), userID(r), l); err != nil {
		writeError(w, uiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUIDeleteLayout clears one slot, which is how "reset to the preset"
// works — the client re-seeds from the preset on its next load.
func (s *Server) handleUIDeleteLayout(w http.ResponseWriter, r *http.Request) {
	if !s.uiEnabled(w) {
		return
	}
	slot, err := strconv.Atoi(r.PathValue("slot"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("slot must be a number"))
		return
	}
	if err := s.ui.DeleteLayout(r.Context(), userID(r), r.URL.Query().Get("corpus"), slot); err != nil {
		writeError(w, uiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUIPrefs returns the caller's interface preferences — the chosen scene,
// the pixel cursor, the corpus for new chats, the active workspace.
func (s *Server) handleUIPrefs(w http.ResponseWriter, r *http.Request) {
	if !s.uiEnabled(w) {
		return
	}
	prefs, err := s.ui.Prefs(r.Context(), userID(r))
	if err != nil {
		writeError(w, uiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prefs": prefs})
}

// handleUISavePrefs merges the posted keys into the caller's preferences.
// Absent keys are left alone so the client can save one setting on its own.
func (s *Server) handleUISavePrefs(w http.ResponseWriter, r *http.Request) {
	if !s.uiEnabled(w) {
		return
	}
	var req struct {
		Prefs map[string]string `json:"prefs"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if err := s.ui.SetPrefs(r.Context(), userID(r), req.Prefs); err != nil {
		writeError(w, uiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
