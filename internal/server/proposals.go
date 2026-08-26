package server

// The proposal-batch surface (MAD-359): the stage-5 generators' multi-object
// proposals, staged and decided as one unit behind the same human gate as
// every other machine proposal. DM-only, like every write and every
// secret-bearing read on the campaign surface. A batch can be staged by
// hand through POST — the generators that will call StageBatch from their
// own endpoints are later stage-5 issues; the decision path ships now.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// batchView is one proposal batch as the API renders it. The list read
// carries counts; the single-batch read carries its items.
type batchView struct {
	ID        string               `json:"id"`
	Source    string               `json:"source"`
	Prompt    string               `json:"prompt,omitempty"`
	Status    string               `json:"status"`
	CreatedBy string               `json:"created_by,omitempty"`
	CreatedAt int64                `json:"created_at"`
	DecidedAt *int64               `json:"decided_at,omitempty"`
	ItemCount int                  `json:"item_count"`
	OpenCount int                  `json:"open_count"`
	Items     []reviewView         `json:"items,omitempty"`
	Skipped   []canon.BatchSkipped `json:"skipped,omitempty"`
}

func toBatchView(b canon.Batch, withItems bool) batchView {
	v := batchView{
		ID: b.ID, Source: b.Source, Prompt: b.Prompt, Status: b.Status,
		CreatedBy: b.CreatedBy, CreatedAt: b.CreatedAt.UnixMilli(),
		ItemCount: b.ItemCount, OpenCount: b.OpenCount,
	}
	if !b.DecidedAt.IsZero() {
		ms := b.DecidedAt.UnixMilli()
		v.DecidedAt = &ms
	}
	if withItems {
		v.Items = make([]reviewView, 0, len(b.Items))
		for _, r := range b.Items {
			v.Items = append(v.Items, toReviewView(r))
		}
	}
	v.Skipped = b.Skipped
	return v
}

// handleProposalsStage stages one batch by hand: the generator's source,
// the prompt it ran from, and the items (each an id, a proposed_* kind, a
// payload, and the sibling ids it depends on). The depends_on graph is
// validated before anything is written.
func (s *Server) handleProposalsStage(w http.ResponseWriter, r *http.Request) {
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
		Source string `json:"source"`
		Prompt string `json:"prompt"`
		Items  []struct {
			ID        string         `json:"id"`
			Kind      string         `json:"kind"`
			Subject   string         `json:"subject"`
			Summary   string         `json:"summary"`
			Payload   map[string]any `json:"payload"`
			DependsOn []string       `json:"depends_on"`
		} `json:"items"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in := canon.BatchInput{
		CampaignID: a.campaign.ID, Source: req.Source, Prompt: req.Prompt,
		CreatedBy: userID(r),
	}
	for _, it := range req.Items {
		in.Items = append(in.Items, canon.BatchItemInput{
			ID: it.ID, Kind: it.Kind, Subject: it.Subject, Summary: it.Summary,
			Payload: it.Payload, DependsOn: it.DependsOn,
		})
	}
	batch, err := s.canon.StageBatch(r.Context(), in)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"batch": toBatchView(*batch, true)})
}

// handleProposalsList lists the campaign's batches, optionally narrowed by
// ?status=open|accepted|partially_accepted|dismissed.
func (s *Server) handleProposalsList(w http.ResponseWriter, r *http.Request) {
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
	if status != "" && status != canon.BatchOpen && status != canon.BatchAccepted &&
		status != canon.BatchPartiallyAccepted && status != canon.BatchDismissed {
		writeError(w, http.StatusBadRequest,
			errors.New("status must be open, accepted, partially_accepted or dismissed"))
		return
	}
	batches, err := s.canon.Batches(r.Context(), a.campaign.ID, status)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]batchView, 0, len(batches))
	for _, b := range batches {
		views = append(views, toBatchView(b, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": views})
}

// handleProposalGet returns one batch with its items, enriched the way the
// review queue enriches them.
func (s *Server) handleProposalGet(w http.ResponseWriter, r *http.Request) {
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
	batch, err := s.canon.GetBatch(r.Context(), a.campaign.ID, r.PathValue("bid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": toBatchView(*batch, true)})
}

// handleProposalDecision records the DM's decision on one open batch:
// accept (apply every open item in dependency order) or dismiss them all,
// with optional per-item overrides — modify with a replacement payload, or
// dismiss, which drops that item and everything depending on it. Deciding
// a decided batch is a no-op that returns its current state.
func (s *Server) handleProposalDecision(w http.ResponseWriter, r *http.Request) {
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
		Decision string `json:"decision"`
		Items    []struct {
			ItemID   string          `json:"item_id"`
			Decision string          `json:"decision"`
			Payload  json.RawMessage `json:"payload"`
			Note     string          `json:"note"`
		} `json:"items"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	items := make([]canon.ItemDecision, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, canon.ItemDecision{
			ItemID: it.ItemID, Decision: it.Decision, Payload: it.Payload, Note: it.Note,
		})
	}
	res, err := s.canon.DecideBatch(r.Context(), a.campaign.ID, r.PathValue("bid"),
		req.Decision, items, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch": toBatchView(*res.Batch, true),
		"items": res.Items,
	})
}
