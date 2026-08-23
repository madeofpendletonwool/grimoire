// The campaign session API: campaigns (list/create, the minimum the
// Sessions view needs for a context to hang sessions on), sessions, sources,
// addressable spans, the event log, prior-ruling surfacing, and the Markdown
// export.

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
)

// maxSourceBytes caps one ingested source. A four-hour session transcript is
// a few hundred kilobytes of text; twenty megabytes is generous headroom
// while still refusing to store an accidentally-dropped ISO.
const maxSourceBytes = 20 << 20

// WithCampaign wires the campaign graph and the session layer. Without it
// the campaign endpoints report unavailable, the same additive pattern the
// other stores follow.
func (s *Server) WithCampaign(campaigns *campaign.Store, sessions *gamesession.Store) *Server {
	s.campaigns = campaigns
	s.sessions = sessions
	return s
}

// sessionsEnabled reports whether the session layer is wired, writing the
// error response when it is not. The campaign surface at large (knowledge,
// members) has its own check in campaign.go; this one gates the session
// routes, which need the gamesession store.
func (s *Server) sessionsEnabled(w http.ResponseWriter) bool {
	if s.campaigns == nil || s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("sessions are not available"))
		return false
	}
	return true
}

// campaignAccess resolves the caller's standing in a campaign. A caller with
// no membership row gets ok=false — which is the answer "not found", not an
// error: a stranger's campaign and an unknown id are indistinguishable, the
// same rule every other ownership check here follows. It writes the response
// itself and returns false when the request should not proceed.
func (s *Server) campaignAccess(w http.ResponseWriter, r *http.Request, campaignID string) (string, bool) {
	if !s.sessionsEnabled(w) {
		return "", false
	}
	role, ok, err := s.campaigns.Role(r.Context(), campaignID, userID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return "", false
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("campaign %s", campaignID))
		return "", false
	}
	return role, true
}

// requireDM gates a write behind the DM role. It writes the response itself
// and returns false when the request should not proceed.
func (s *Server) requireDM(w http.ResponseWriter, r *http.Request, campaignID string) bool {
	role, ok := s.campaignAccess(w, r, campaignID)
	if !ok {
		return false
	}
	if role != campaign.RoleDM {
		writeError(w, http.StatusForbidden, fmt.Errorf("the DM's seat only"))
		return false
	}
	return true
}

// sessionInCampaign loads a session and confirms it belongs to the campaign
// in the path, so a session id from another campaign is a plain 404. It
// writes the response itself and returns nil when the request should not
// proceed.
func (s *Server) sessionInCampaign(w http.ResponseWriter, r *http.Request, campaignID, sessionID string) (*gamesession.Session, bool) {
	ses, err := s.sessions.GetSession(r.Context(), sessionID)
	if err != nil || ses.Campaign != campaignID {
		writeError(w, http.StatusNotFound, fmt.Errorf("session %s", sessionID))
		return nil, false
	}
	return ses, true
}

/* ---------- campaigns (the minimum the Sessions view needs) ----------
   Listed and created by the handlers in campaign.go, which own the richer
   campaignView the whole campaign surface shares. */

// writeCampaignError maps the store vocabulary onto HTTP statuses.
func writeCampaignError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, campaign.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, campaign.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, gamesession.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, gamesession.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

/* ---------- sessions ---------- */

type sessionView struct {
	ID        string `json:"id"`
	Campaign  string `json:"campaign_id"`
	Ordinal   int64  `json:"ordinal"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
}

func toSessionView(ses gamesession.Session) sessionView {
	v := sessionView{
		ID: ses.ID, Campaign: ses.Campaign, Ordinal: ses.Ordinal,
		Name: ses.Name, Status: ses.Status,
	}
	if !ses.StartedAt.IsZero() {
		v.StartedAt = ses.StartedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if !ses.EndedAt.IsZero() {
		v.EndedAt = ses.EndedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return v
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.campaignAccess(w, r, r.PathValue("cid")); !ok {
		return
	}
	list, err := s.sessions.ListSessions(r.Context(), r.PathValue("cid"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]sessionView, 0, len(list))
	for _, ses := range list {
		views = append(views, toSessionView(ses))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": views})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireDM(w, r, r.PathValue("cid")) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	ses, err := s.sessions.CreateSession(r.Context(), r.PathValue("cid"), req.Name)
	if err != nil {
		writeCampaignError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"session": toSessionView(*ses)})
}

// handleGetSession returns one session with its source and event counts, the
// shape the session detail header wants.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.campaignAccess(w, r, r.PathValue("cid")); !ok {
		return
	}
	ses, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid"))
	if !ok {
		return
	}
	view := toSessionView(*ses)
	sources, err := s.sessions.ListSources(r.Context(), ses.ID, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	events, err := s.sessions.ListEvents(r.Context(), ses.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session": view, "sources": len(sources), "events": len(events),
	})
}

func (s *Server) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireDM(w, r, r.PathValue("cid")) {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	var req struct {
		Name   *string `json:"name"`
		Status *string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	ses, err := s.sessions.UpdateSession(r.Context(), r.PathValue("sid"), req.Name, req.Status)
	if err != nil {
		writeCampaignError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": toSessionView(*ses)})
}

/* ---------- sources ---------- */

type sourceView struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Kind      string `json:"kind"`
	Author    string `json:"author"`
	Title     string `json:"title"`
	Checksum  string `json:"checksum"`
	ByteSize  int64  `json:"byte_size"`
	Timed     bool   `json:"timed"`
	CreatedAt string `json:"created_at"`
	Content   string `json:"content,omitempty"`
}

func toSourceView(src gamesession.Source, withContent bool) sourceView {
	v := sourceView{
		ID: src.ID, SessionID: src.SessionID, Kind: src.Kind, Author: src.Author,
		Title: src.Title, Checksum: src.Checksum, ByteSize: src.ByteSize,
		Timed:     len(src.Timing) > 0,
		CreatedAt: src.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if withContent {
		v.Content = src.Content
	}
	return v
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	role, ok := s.campaignAccess(w, r, r.PathValue("cid"))
	if !ok {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	// DM-only kinds are filtered in SQL for everyone else (ADR 2).
	sources, err := s.sessions.ListSources(r.Context(), r.PathValue("sid"), role == campaign.RoleDM)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]sourceView, 0, len(sources))
	for i := range sources {
		views = append(views, toSourceView(sources[i], false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": views})
}

// handleAddSource ingests a source two ways: a JSON paste (the transcript box
// in the UI) or a multipart upload (.txt / .md / .srt / .vtt, parsed by
// extension). Both land identically: verbatim content, sha256 checksum,
// timing when the format carried it.
func (s *Server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireDM(w, r, r.PathValue("cid")) {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}

	var (
		kind, author, title string
		ing                 gamesession.Ingested
	)
	if mt := r.Header.Get("content-type"); strings.HasPrefix(mt, "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, maxSourceBytes+(1<<20))
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("upload too large (max %d MB)", maxSourceBytes>>20))
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("file field is required"))
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxSourceBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("read upload: %v", err))
			return
		}
		if len(data) > maxSourceBytes {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("source exceeds %d MB", maxSourceBytes>>20))
			return
		}
		kind = r.FormValue("kind")
		author = r.FormValue("author")
		title = r.FormValue("title")
		if header.Filename != "" && title == "" {
			title = header.Filename
		}
		ing, err = gamesession.IngestByFilename(header.Filename, data)
		if err != nil {
			writeCampaignError(w, err)
			return
		}
	} else {
		var req struct {
			Kind    string `json:"kind"`
			Author  string `json:"author"`
			Title   string `json:"title"`
			Content string `json:"content"`
			Format  string `json:"format"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxSourceBytes+(1<<20))
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
			return
		}
		if int64(len(req.Content)) > maxSourceBytes {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("source exceeds %d MB", maxSourceBytes>>20))
			return
		}
		kind, author, title = req.Kind, req.Author, req.Title
		var err error
		ing, err = gamesession.Ingest(req.Format, []byte(req.Content))
		if err != nil {
			writeCampaignError(w, err)
			return
		}
	}

	src, err := s.sessions.AddSource(r.Context(), r.PathValue("sid"), kind, author, title, ing.Text, ing.Timing)
	if err != nil {
		writeCampaignError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"source": toSourceView(*src, false)})
}

func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request) {
	role, ok := s.campaignAccess(w, r, r.PathValue("cid"))
	if !ok {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	src, err := s.sessions.GetSource(r.Context(), r.PathValue("srcid"))
	if err != nil || src.SessionID != r.PathValue("sid") {
		writeError(w, http.StatusNotFound, fmt.Errorf("source %s", r.PathValue("srcid")))
		return
	}
	// Same gate as the list: DM-only kinds are simply not there for anyone
	// else — a 404, indistinguishable from a missing row.
	if gamesession.DMOnlySources[src.Kind] && role != campaign.RoleDM {
		writeError(w, http.StatusNotFound, fmt.Errorf("source %s", src.ID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": toSourceView(*src, true),
		"timing": src.Timing,
	})
}

/* ---------- addressable spans ---------- */

// handleResolveSpan is the "actual words someone actually said" endpoint:
// given a source and byte offsets it returns the verbatim quote, and given a
// quote it locates the offsets. Everything downstream that cites a span
// resolves through here.
func (s *Server) handleResolveSpan(w http.ResponseWriter, r *http.Request) {
	role, ok := s.campaignAccess(w, r, r.PathValue("cid"))
	if !ok {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	q := r.URL.Query()
	sourceID := q.Get("source_id")

	// The span endpoint respects the source visibility gate: a quote out of
	// the DM's notes is as secret as the notes.
	src, err := s.sessions.GetSource(r.Context(), sourceID)
	if err != nil || src.SessionID != r.PathValue("sid") {
		writeError(w, http.StatusNotFound, fmt.Errorf("source %s", sourceID))
		return
	}
	if gamesession.DMOnlySources[src.Kind] && role != campaign.RoleDM {
		writeError(w, http.StatusNotFound, fmt.Errorf("source %s", sourceID))
		return
	}

	if quote := q.Get("quote"); quote != "" {
		span, err := s.sessions.LocateSpan(r.Context(), sourceID, quote)
		if err != nil {
			writeCampaignError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"span": span})
		return
	}
	start, err1 := strconv.ParseInt(q.Get("start"), 10, 64)
	end, err2 := strconv.ParseInt(q.Get("end"), 10, 64)
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("start and end byte offsets are required"))
		return
	}
	span, err := s.sessions.ResolveSpan(r.Context(), sourceID, start, end)
	if err != nil {
		writeCampaignError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"span": span})
}

/* ---------- events ---------- */

type sessionEventView struct {
	ID        string         `json:"id"`
	SessionID string         `json:"session_id"`
	Seq       int64          `json:"seq"`
	Kind      string         `json:"kind"`
	Summary   string         `json:"summary"`
	Detail    string         `json:"detail"`
	Payload   map[string]any `json:"payload"`
	CreatedAt string         `json:"created_at"`
}

func toSessionEventView(ev gamesession.Event) sessionEventView {
	return sessionEventView{
		ID: ev.ID, SessionID: ev.SessionID, Seq: ev.Seq, Kind: ev.Kind,
		Summary: ev.Summary, Detail: ev.Detail, Payload: ev.Payload,
		CreatedAt: ev.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// handleAddEvent appends to the log. Recording a ruling or a Q&A also runs
// the prior-ruling surfacing retained from MAD-286: the question is
// FTS-matched against the campaign's past rulings and the top matches ride
// along in the response — "you ruled the other way on this three sessions
// ago."
func (s *Server) handleAddEvent(w http.ResponseWriter, r *http.Request) {
	if !s.requireDM(w, r, r.PathValue("cid")) {
		return
	}
	ses, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid"))
	if !ok {
		return
	}
	var req struct {
		Kind    string         `json:"kind"`
		Summary string         `json:"summary"`
		Detail  string         `json:"detail"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	ev, err := s.sessions.AddEvent(r.Context(), ses.ID, req.Kind, req.Summary, req.Detail, req.Payload)
	if err != nil {
		writeCampaignError(w, err)
		return
	}
	resp := map[string]any{"event": toSessionEventView(*ev)}
	if ev.Kind == gamesession.EventRuling || ev.Kind == gamesession.EventQA {
		matches, err := s.sessions.MatchPriorRulings(r.Context(), ses.Campaign, ev.Summary, ev.ID, 5)
		if err == nil && len(matches) > 0 {
			resp["prior_matches"] = matches
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.campaignAccess(w, r, r.PathValue("cid")); !ok {
		return
	}
	if _, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid")); !ok {
		return
	}
	events, err := s.sessions.ListEvents(r.Context(), r.PathValue("sid"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]sessionEventView, 0, len(events))
	for i := range events {
		views = append(views, toSessionEventView(events[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": views})
}

/* ---------- export ---------- */

// handleExportSession renders the whole session log as Markdown. DM-only:
// the export contains the DM's notes by design — it is the DM's record.
func (s *Server) handleExportSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireDM(w, r, r.PathValue("cid")) {
		return
	}
	ses, ok := s.sessionInCampaign(w, r, r.PathValue("cid"), r.PathValue("sid"))
	if !ok {
		return
	}
	md, err := s.sessions.ExportMarkdown(r.Context(), ses.ID)
	if err != nil {
		writeCampaignError(w, err)
		return
	}
	name := fmt.Sprintf("session-%02d-%s.md", ses.Ordinal, slug(ses.Name))
	w.Header().Set("content-type", "text/markdown; charset=utf-8")
	w.Header().Set("content-disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(md))
}

// slug flattens a session name into a filename-safe fragment.
func slug(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
