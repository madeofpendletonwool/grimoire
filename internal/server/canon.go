package server

// The canon engine's deterministic surface (MAD-309): run the consistency
// engine, read the flag ledger, decide a flag. DM-only, like every write and
// every secret-bearing read on the campaign surface — the engine walks every
// fact, secret and proposal included.
//
// The check endpoint needs no model and no key: it is the same offline path
// the `grimoire canon check` subcommand runs, so the consistency engine works
// on a self-hosted box with nothing configured at all.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// WithCanon wires the canon engine's deterministic surface. Separate from New
// so the feature is additive; without it the canon endpoints report
// unavailable rather than panicking on a nil store.
func (s *Server) WithCanon(store *canon.Store) *Server {
	s.canon = store
	return s
}

// canonEnabled reports whether the canon surface is wired, writing the error
// response when it is not.
func (s *Server) canonEnabled(w http.ResponseWriter) bool {
	if s.canon == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the canon engine is not available"))
		return false
	}
	return true
}

// flagView is one ledger row as the API renders it.
type flagView struct {
	ID           string `json:"id"`
	CheckCode    string `json:"check_code"`
	RecordKind   string `json:"record_kind"`
	RecordID     string `json:"record_id"`
	Severity     string `json:"severity"`
	Message      string `json:"message"`
	Status       string `json:"status"`
	FirstSeenAt  int64  `json:"first_seen_at"`
	LastSeenAt   int64  `json:"last_seen_at"`
	ClearedAt    *int64 `json:"cleared_at,omitempty"`
	DecidedBy    string `json:"decided_by,omitempty"`
	DecidedAt    *int64 `json:"decided_at,omitempty"`
	DecisionNote string `json:"decision_note,omitempty"`
}

func toFlagView(f canon.Flag) flagView {
	v := flagView{
		ID: f.ID, CheckCode: f.CheckCode, RecordKind: f.RecordKind, RecordID: f.RecordID,
		Severity: f.Severity, Message: f.Message, Status: f.Status,
		FirstSeenAt: f.FirstSeenAt.UnixMilli(), LastSeenAt: f.LastSeenAt.UnixMilli(),
		DecidedBy: f.DecidedBy, DecisionNote: f.DecisionNote,
	}
	if !f.ClearedAt.IsZero() {
		ms := f.ClearedAt.UnixMilli()
		v.ClearedAt = &ms
	}
	if !f.DecidedAt.IsZero() {
		ms := f.DecidedAt.UnixMilli()
		v.DecidedAt = &ms
	}
	return v
}

// handleCanonCheck runs the deterministic engine over one campaign, refreshes
// the flag ledger, and returns it. Pure offline: no model is consulted.
func (s *Server) handleCanonCheck(w http.ResponseWriter, r *http.Request) {
	if !s.canonEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	flags, err := s.canon.CheckCampaign(r.Context(), a.campaign.ID, canon.DefaultCheckOptions())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]flagView, 0, len(flags))
	for _, f := range flags {
		views = append(views, toFlagView(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"flags": views})
}

// handleCanonFlags lists the campaign's flag ledger, optionally narrowed by
// ?status=open|accepted|dismissed|cleared.
func (s *Server) handleCanonFlags(w http.ResponseWriter, r *http.Request) {
	if !s.canonEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && status != canon.FlagOpen && status != canon.FlagAccepted &&
		status != canon.FlagDismissed && status != canon.FlagCleared {
		writeError(w, http.StatusBadRequest, errors.New("status must be open, accepted, dismissed or cleared"))
		return
	}
	flags, err := s.canon.Flags(r.Context(), a.campaign.ID, status)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]flagView, 0, len(flags))
	for _, f := range flags {
		views = append(views, toFlagView(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"flags": views})
}

// handleCanonFlagDecision records the DM's decision on one open flag:
// accepted or dismissed, with an optional note. A decided flag keeps its
// decision forever; the review queue (MAD-310) builds its engine_flag items
// on this ledger.
func (s *Server) handleCanonFlagDecision(w http.ResponseWriter, r *http.Request) {
	if !s.canonEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		CheckCode  string `json:"check_code"`
		RecordKind string `json:"record_kind"`
		RecordID   string `json:"record_id"`
		Decision   string `json:"decision"`
		Note       string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.CheckCode == "" || req.RecordKind == "" || req.RecordID == "" {
		writeError(w, http.StatusBadRequest, errors.New("check_code, record_kind and record_id are required"))
		return
	}
	err := s.canon.DecideFlag(r.Context(), a.campaign.ID, req.CheckCode, req.RecordKind, req.RecordID,
		req.Decision, req.Note, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	flags, err := s.canon.Flags(r.Context(), a.campaign.ID, "")
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]flagView, 0, len(flags))
	for _, f := range flags {
		views = append(views, toFlagView(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"flags": views})
}
