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
	"io"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
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

/* ---------- the review queue (MAD-310) ---------- */

// reviewView is one queue item as the API renders it. The rendering material
// (payload, span context, adversarial verdict) rides along so the review
// screen draws a whole item from one response.
type reviewView struct {
	ID            string         `json:"id"`
	Kind          string         `json:"kind"`
	Status        string         `json:"status"`
	CandidateID   string         `json:"candidate_id,omitempty"`
	FlagID        string         `json:"flag_id,omitempty"`
	BatchID       string         `json:"batch_id,omitempty"`
	DependsOn     []string       `json:"depends_on,omitempty"`
	Subject       string         `json:"subject"`
	Summary       string         `json:"summary"`
	Detail        string         `json:"detail,omitempty"`
	ResultRef     string         `json:"result_ref,omitempty"`
	DecisionNote  string         `json:"decision_note,omitempty"`
	DecidedBy     string         `json:"decided_by,omitempty"`
	DecidedAt     *int64         `json:"decided_at,omitempty"`
	CreatedAt     int64          `json:"created_at"`
	Payload       map[string]any `json:"payload,omitempty"`
	Quote         string         `json:"quote,omitempty"`
	SpanStart     int64          `json:"span_start,omitempty"`
	SpanEnd       int64          `json:"span_end,omitempty"`
	SourceKind    string         `json:"source_kind,omitempty"`
	SourceAuthor  string         `json:"source_author,omitempty"`
	SourceTitle   string         `json:"source_title,omitempty"`
	ContextBefore string         `json:"context_before,omitempty"`
	ContextAfter  string         `json:"context_after,omitempty"`
	Agreement     float64        `json:"agreement,omitempty"`
	Rationale     string         `json:"rationale,omitempty"`
	Verdict       string         `json:"verdict,omitempty"`
	Confidence    float64        `json:"confidence,omitempty"`
}

func toReviewView(r canon.Review) reviewView {
	v := reviewView{
		ID: r.ID, Kind: r.Kind, Status: r.Status, CandidateID: r.CandidateID,
		FlagID: r.FlagID, BatchID: r.BatchID, DependsOn: r.DependsOn,
		Subject: r.Subject, Summary: r.Summary, Detail: r.Detail,
		ResultRef: r.ResultRef, DecisionNote: r.DecisionNote, DecidedBy: r.DecidedBy,
		CreatedAt: r.CreatedAt.UnixMilli(), Payload: r.Payload, Quote: r.Quote,
		SpanStart: r.SpanStart, SpanEnd: r.SpanEnd, SourceKind: r.SourceKind,
		SourceAuthor: r.SourceAuthor, SourceTitle: r.SourceTitle,
		ContextBefore: r.ContextBefore, ContextAfter: r.ContextAfter,
		Agreement: r.Agreement, Rationale: r.Rationale, Verdict: r.Verdict,
		Confidence: r.Confidence,
	}
	if !r.DecidedAt.IsZero() {
		ms := r.DecidedAt.UnixMilli()
		v.DecidedAt = &ms
	}
	return v
}

// handleCanonReviewsBuild rebuilds the queue from the three upstream passes
// and returns it. Idempotent: a re-run mints items only for findings that
// have never been queued.
func (s *Server) handleCanonReviewsBuild(w http.ResponseWriter, r *http.Request) {
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
	reviews, err := s.canon.BuildQueue(r.Context(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeReviews(w, reviews)
}

// handleCanonReviews lists the queue, optionally narrowed by ?status=.
func (s *Server) handleCanonReviews(w http.ResponseWriter, r *http.Request) {
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
	if status != "" && status != canon.ReviewOpen && status != canon.ReviewAccepted &&
		status != canon.ReviewModified && status != canon.ReviewDismissed {
		writeError(w, http.StatusBadRequest, errors.New("status must be open, accepted, modified or dismissed"))
		return
	}
	reviews, err := s.canon.Reviews(r.Context(), a.campaign.ID, status)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeReviews(w, reviews)
}

// handleCanonReviewDecision records the DM's decision on one open item:
// accept, modify then accept (with a replacement payload), or dismiss. The
// accept path is the only thing that writes canon.
func (s *Server) handleCanonReviewDecision(w http.ResponseWriter, r *http.Request) {
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
		Decision string          `json:"decision"`
		Note     string          `json:"note"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rev, err := s.canon.DecideReview(r.Context(), a.campaign.ID, r.PathValue("rid"),
		req.Decision, req.Note, userID(r), req.Payload)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review": toReviewView(*rev)})
}

// handleCanonReviewsExport renders a session's applied changes as Markdown
// (default) or JSON (?format=json) — the DM's record of what the machine
// changed. ?session_id= narrows to one session; without it every applied
// change in the campaign is exported.
func (s *Server) handleCanonReviewsExport(w http.ResponseWriter, r *http.Request) {
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
	sessionID := r.URL.Query().Get("session_id")
	if r.URL.Query().Get("format") == "json" {
		reviews, err := s.canon.AppliedReviews(r.Context(), a.campaign.ID, sessionID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeReviews(w, reviews)
		return
	}
	md, err := s.canon.ExportApplied(r.Context(), a.campaign.ID, sessionID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("content-type", "text/markdown; charset=utf-8")
	w.Header().Set("content-disposition", "attachment; filename=\"canon-changes.md\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(md))
}

// handleCanonReviewsAcceptAgree batch-accepts every open proposed_* item
// whose adversarial verdict is 'agree' at or above the requested threshold —
// the queue's one batch affordance. low_agreement, contradiction and
// engine_flag items always need an individual decision, and "accept
// everything" is deliberately not offered.
func (s *Server) handleCanonReviewsAcceptAgree(w http.ResponseWriter, r *http.Request) {
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
	req := struct {
		MinAgreement float64 `json:"min_agreement"`
	}{MinAgreement: 0.8}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.canon.AcceptAgreement(r.Context(), a.campaign.ID, req.MinAgreement, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func writeReviews(w http.ResponseWriter, reviews []canon.Review) {
	views := make([]reviewView, 0, len(reviews))
	for _, r := range reviews {
		views = append(views, toReviewView(r))
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": views})
}

/* ---------- continuity, entailment, and health (MAD-312) ---------- */

// findingView is one continuity finding as the API renders it.
type findingView struct {
	Check      string `json:"check"`
	Severity   string `json:"severity"`
	RecordKind string `json:"record_kind"`
	RecordID   string `json:"record_id"`
	Message    string `json:"message"`
}

func toFindingView(f campaign.Finding) findingView {
	return findingView{
		Check: f.Check, Severity: string(f.Severity), RecordKind: f.RecordKind,
		RecordID: f.RecordID, Message: f.Message,
	}
}

// handleCanonContinuity runs the pre-session continuity check over the DM's
// prep: the deterministic joins always, the model residue pass only when a
// model client is wired. DM-only, and never mutates anything.
func (s *Server) handleCanonContinuity(w http.ResponseWriter, r *http.Request) {
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
	var prep canon.Prep
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&prep); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rep, err := s.canon.CheckContinuity(r.Context(), a.campaign.ID, &prep)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	findings := make([]findingView, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		findings = append(findings, toFindingView(f))
	}
	modelFindings := make([]findingView, 0, len(rep.ModelFindings))
	for _, f := range rep.ModelFindings {
		modelFindings = append(modelFindings, toFindingView(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"campaign_id":    rep.CampaignID,
		"offline":        rep.Offline,
		"prompt_version": rep.PromptVersion,
		"findings":       findings,
		"model_findings": modelFindings,
		"names":          rep.Names,
		"problems":       rep.Problems,
		"input_tokens":   rep.InputTokens,
		"output_tokens":  rep.OutputTokens,
		"cost_usd":       rep.CostUSD,
	})
}

// handleCanonEntail runs the entailment pass over one piece of prose before
// the DM sees it: the deterministic name sweep always, the model checker only
// when a model client is wired. Advisory — no mutation, no ledger writes.
func (s *Server) handleCanonEntail(w http.ResponseWriter, r *http.Request) {
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
		Prose    string   `json:"prose"`
		FactIDs  []string `json:"fact_ids"`
		EventIDs []string `json:"event_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Prose) == "" {
		writeError(w, http.StatusBadRequest, errors.New("prose is required"))
		return
	}
	rep, err := s.canon.CheckEntailment(r.Context(), a.campaign.ID, canon.EntailInput{
		Prose: req.Prose, FactIDs: req.FactIDs, EventIDs: req.EventIDs,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	findings := make([]findingView, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		findings = append(findings, toFindingView(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"campaign_id":    rep.CampaignID,
		"offline":        rep.Offline,
		"prompt_version": rep.PromptVersion,
		"records":        rep.Records,
		"findings":       findings,
		"claims":         rep.Claims,
		"problems":       rep.Problems,
		"input_tokens":   rep.InputTokens,
		"output_tokens":  rep.OutputTokens,
		"cost_usd":       rep.CostUSD,
	})
}

// handleCanonHealth is the "what did we forget?" button: the deterministic
// engine over the whole campaign, its ledger refreshed, the sections
// assembled, and a narrative summary when a model client is wired.
func (s *Server) handleCanonHealth(w http.ResponseWriter, r *http.Request) {
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
	rep, err := s.canon.HealthReport(r.Context(), a.campaign.ID, canon.DefaultHealthOptions())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"campaign_id":              rep.CampaignID,
		"campaign_name":            rep.CampaignName,
		"generated_at":             rep.GeneratedAtMS,
		"offline":                  rep.Offline,
		"prompt_version":           rep.PromptVersion,
		"threads":                  rep.Threads,
		"clues":                    rep.Clues,
		"unused_npcs":              rep.UnusedNPCs,
		"dormant_regions":          rep.DormantRegions,
		"unresolved_relationships": rep.Unresolved,
		"pacing":                   rep.Pacing,
		"open_flags":               rep.OpenFlagCount,
		"narrative":                rep.Narrative,
		"problems":                 rep.Problems,
		"input_tokens":             rep.InputTokens,
		"output_tokens":            rep.OutputTokens,
		"cost_usd":                 rep.CostUSD,
	})
}
