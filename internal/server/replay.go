package server

// The session replay's surface (MAD-426, stage 9 of MAD-417): the
// mechanical event log, played back.
//
//	GET /api/campaigns/{id}/combats/{cid}/replay        one battle's journal, final frame and checksum (DM)
//	GET /api/campaigns/{id}/combats/{cid}/replay/state  the derived state at ?at=SEQ — the scrub (DM)
//	GET /api/campaigns/{cid}/sessions/{sid}/replay      the session's whole replayable timeline (DM)
//
// The replay is the DM's screen, like the tracker it reads: it spells
// exact monster numbers and secret rolls' context, so every route is
// the DM's the way the combat routes are. Reads only — replay derives,
// it never writes, so there is nothing to ping the broker about. A
// scrub is one in-memory fold over the loaded journal: every position
// costs the same as the last, which is what makes it feel live.
// A journal that drifts from its own grammar is a 422, not a guess.

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/madeofpendletonwool/grimoire/internal/replay"
)

// WithReplay wires the session replay (MAD-426). Without it the replay
// endpoints answer 503.
func (s *Server) WithReplay(store *replay.Store) *Server {
	s.replay = store
	return s
}

// replayEnabled reports the replay's availability.
func (s *Server) replayEnabled(w http.ResponseWriter) bool {
	if s.replay == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the session replay is not configured on this install"))
		return false
	}
	return true
}

// journalEntry is one row of the battle's journal as the scrub bar
// reads it: position, kind, the round it belongs to, who it names, and
// the table's own one-line account.
type journalEntry struct {
	Seq       int64  `json:"seq"`
	Kind      string `json:"kind"`
	Round     int    `json:"round"`
	Combatant string `json:"combatant,omitempty"`
	Amount    int    `json:"amount,omitempty"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at,omitempty"`
}

// handleCombatReplay answers one battle's whole replayable shape: the
// journal to scrub along, the final frame the fold derives, and the
// checksum assertion against the recorded rows.
func (s *Server) handleCombatReplay(w http.ResponseWriter, r *http.Request) {
	if !s.replayEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	b, err := s.replay.Battle(r.Context(), a.campaign.ID, r.PathValue("cid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	journal := make([]journalEntry, 0, len(b.Events))
	for _, ev := range b.Events {
		journal = append(journal, journalEntry{
			Seq: ev.Seq, Kind: ev.Kind, Round: ev.Round,
			Combatant: b.JournalName(ev.CombatantID), Amount: ev.Amount,
			Note: ev.Note, CreatedAt: ev.CreatedAt,
		})
	}
	final, err := b.Frame(b.MaxSeq())
	if err != nil {
		s.writeReplayFoldError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"replay": map[string]any{
			"combat":  b.Combat,
			"journal": journal,
			"final":   final,
			"verify":  b.Verify(),
		},
	})
}

// handleCombatReplayState answers the scrub: the derived frame at
// ?at=SEQ (omitted or past-the-end is the battle as it ended, 0 the
// opening lineup).
func (s *Server) handleCombatReplayState(w http.ResponseWriter, r *http.Request) {
	if !s.replayEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	at := int64(0)
	if v := r.URL.Query().Get("at"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("at must be a journal position (a non-negative integer)"))
			return
		}
		at = parsed
	}
	b, err := s.replay.Battle(r.Context(), a.campaign.ID, r.PathValue("cid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	frame, err := b.Frame(at)
	if err != nil {
		s.writeReplayFoldError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"frame": frame})
}

// handleSessionReplay answers a session's whole replayable picture:
// the log as it was written, with the fights indexed out of it — each
// one a combat whose journal the battle endpoints scrub.
func (s *Server) handleSessionReplay(w http.ResponseWriter, r *http.Request) {
	if !s.replayEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	tl, err := s.replay.Session(r.Context(), a.campaign.ID, r.PathValue("sid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"replay": tl})
}

// writeReplayFoldError answers a fold that refused to derive: a
// journal inconsistent with the grammar it records is unprocessable,
// and the message says exactly which row drifted.
func (s *Server) writeReplayFoldError(w http.ResponseWriter, err error) {
	if d, ok := err.(replay.Drift); ok {
		writeError(w, http.StatusUnprocessableEntity, d)
		return
	}
	writeStoreError(w, err)
}
