package server

// The command interface's HTTP surface (MAD-363): one POST — text in, a
// proposal batch, a clarifying question with its candidates, a plain
// refusal, or a spine write out — plus the transcript read that makes the
// referent stack inspectable. DM-only, like every canon write; the model
// key gates the model-driven verbs (undo and the refusals are
// deterministic and still work).
//
// The campaign chat's DM surface routes here: a message that opens with
// "/" is a command, not a question (campaignchat.go). The command bar in
// the campaign view is the second surface onto this same endpoint — there
// is one command engine, not two.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// handleCampaignCommand runs one natural-language command.
func (s *Server) handleCampaignCommand(w http.ResponseWriter, r *http.Request) {
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
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !canon.IsUndo(req.Text) && !s.llm.Configured() {
		writeError(w, http.StatusServiceUnavailable, errors.New(
			"the command line needs the model. Set ANTHROPIC_API_KEY to enable it (undo still works)"))
		return
	}
	res, err := s.canon.RunCommand(r.Context(), canon.CommandInput{
		CampaignID: a.campaign.ID, Text: req.Text, CreatedBy: userID(r),
	})
	if err != nil {
		if canon.IsOffline(err) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": res})
}

// handleCampaignCommandLog reads the command transcript, newest first.
func (s *Server) handleCampaignCommandLog(w http.ResponseWriter, r *http.Request) {
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
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	log, err := s.canon.CommandLog(r.Context(), a.campaign.ID, limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if log == nil {
		log = []canon.CommandLogRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": log})
}

// runCampaignCommand is the chat's routing arm: the DM Grimoire hands a
// slash-prefixed message to the command engine instead of answering it in
// prose. It returns the SSE frames' payload and the persisted message
// bodies — the caller owns the conversation.
func (s *Server) runCampaignCommand(r *http.Request, a *campAccess, text string) *canon.CommandResult {
	res, err := s.canon.RunCommand(r.Context(), canon.CommandInput{
		CampaignID: a.campaign.ID, Text: text, CreatedBy: userID(r),
	})
	if err != nil {
		if canon.IsOffline(err) {
			return &canon.CommandResult{Kind: canon.CommandResultUnsupported,
				Message: "The command line is not configured. Set ANTHROPIC_API_KEY to enable it (undo still works)."}
		}
		return &canon.CommandResult{Kind: canon.CommandResultUnsupported,
			Message: "The command failed: " + err.Error()}
	}
	return res
}

// isChatCommand reports whether a campaign-chat message is a command:
// the DM's, and opening with "/". A player's slash stays an ordinary
// question — players cannot command the database.
func isChatCommand(a *campAccess, question string) bool {
	return a.isDM() && strings.HasPrefix(strings.TrimSpace(question), "/")
}
