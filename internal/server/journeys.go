package server

// The journeys surface (MAD-375): the road between two places, at the
// density the DM asked for. The pure roll and the rows are
// internal/journey; this file is the HTTP shape. DM-only, like every write
// and every secret-bearing read on the campaign surface — a day table
// names what the DM planted along the road.
//
// POST .../journeys plans: the seeded roll runs, the prose pass may write
// one line per chosen day, and only the journey's own rows are written.
// GET/PATCH .../journeys/{jid} reads the table and settles bookkeeping.
// POST .../journeys/{jid}/days/{n}/resolve marks one day happened at the
// table; POST .../journeys/{jid}/resolve stages the whole road as one
// proposal batch, and the decision happens on the ordinary batch view:
// accepting moves the clock by exactly the journey's days, reason
// 'travel', exactly once.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/clock"
	"github.com/madeofpendletonwool/grimoire/internal/journey"
)

// WithJourneys wires the journey store (MAD-375). Without it the journeys
// endpoints answer 503.
func (s *Server) WithJourneys(store *journey.Store) *Server {
	s.journeys = store
	return s
}

// journeysEnabled reports the surface's availability.
func (s *Server) journeysEnabled(w http.ResponseWriter) bool {
	if s.journeys == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("journeys are not configured on this install"))
		return false
	}
	return true
}

/* ---------- views ---------- */

type legView struct {
	To      string `json:"to"`
	ToName  string `json:"to_name,omitempty"`
	Days    int64  `json:"days"`
	Terrain string `json:"terrain,omitempty"`
}

type journeyView struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	FromName  string    `json:"from_name"`
	ToName    string    `json:"to_name"`
	Route     []legView `json:"route"`
	StartDay  int64     `json:"start_day"`
	EndDay    int64     `json:"end_day"`
	StartDate string    `json:"start_date,omitempty"`
	EndDate   string    `json:"end_date,omitempty"`
	Days      int64     `json:"days"`
	Density   string    `json:"density"`
	Pace      string    `json:"pace"`
	Seed      int64     `json:"seed"`
	Status    string    `json:"status"`
	SessionID string    `json:"session_id,omitempty"`
	BatchID   string    `json:"batch_id,omitempty"`
	Line      string    `json:"line"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt int64     `json:"created_at"`
}

type journeyDayView struct {
	Index       int64          `json:"index"`
	ClockDay    int64          `json:"clock_day"`
	Date        string         `json:"date,omitempty"`
	Leg         string         `json:"leg,omitempty"`
	LegName     string         `json:"leg_name,omitempty"`
	Weather     clock.Forecast `json:"weather"`
	EventKind   string         `json:"event_kind"`
	Detail      string         `json:"detail,omitempty"`
	EntityID    string         `json:"entity_id,omitempty"`
	EntityName  string         `json:"entity_name,omitempty"`
	Encounter   string         `json:"encounter_budget,omitempty"`
	EncounterID string         `json:"encounter_id,omitempty"`
	Resolved    bool           `json:"resolved"`
}

// toJourneyView renders the journey row with the calendar's own dates and
// the names the graph holds.
func (s *Server) toJourneyView(j *journey.JourneyRow, cal *clock.Calendar, names map[string]string) journeyView {
	v := journeyView{
		ID: j.ID, From: j.FromEntity, To: j.ToEntity,
		StartDay: j.StartDay, EndDay: j.StartDay + j.Days, Days: j.Days,
		Density: j.Density, Pace: j.Pace, Seed: j.Seed, Status: j.Status,
		SessionID: j.SessionID, BatchID: j.BatchID,
		Line:      fmt.Sprintf("You travel for %d days.", j.Days),
		CreatedBy: j.CreatedBy, CreatedAt: j.CreatedAt.UnixMilli(),
	}
	v.FromName, v.ToName = names[j.FromEntity], names[j.ToEntity]
	if v.FromName != "" && v.ToName != "" {
		v.Line = fmt.Sprintf("You travel from %s to %s: %d days.", v.FromName, v.ToName, j.Days)
	}
	for _, l := range j.Route {
		v.Route = append(v.Route, legView{To: l.To, ToName: names[l.To], Days: l.Days, Terrain: l.Terrain})
	}
	if cal != nil {
		v.StartDate = cal.Format(j.StartDay)
		v.EndDate = cal.Format(j.StartDay + j.Days)
	}
	return v
}

func (s *Server) toDayView(d *journey.DayPlan, names map[string]string, cal *clock.Calendar) journeyDayView {
	v := journeyDayView{
		Index: d.Index, ClockDay: d.ClockDay, Leg: d.Leg, LegName: names[d.Leg],
		Weather: d.Weather, EventKind: d.EventKind, Detail: d.Detail,
		EntityID: d.EntityID, EntityName: names[d.EntityID], Encounter: d.Encounter,
		Resolved: d.Resolved,
	}
	if cal != nil {
		v.Date = cal.Format(d.ClockDay)
	}
	return v
}

/* ---------- handlers ---------- */

// handleCampaignJourneys plans one journey:
// {"from": "<entity-id>", "to": "<entity-id>", "density": "standard",
//
//	"pace": "normal", "days": 12, "seed": 7, "session": "<session-id>"}.
//
// days is the DM's override — the required answer when the map holds no
// route, and the scenic-route answer when it does. The response carries
// the journey's row and the day table (empty at density none: the line is
// that answer).
func (s *Server) handleCampaignJourneys(w http.ResponseWriter, r *http.Request) {
	if !s.journeysEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if r.Method == http.MethodGet {
		rows, err := s.journeys.List(r.Context(), a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		cal, _, cerr := s.campaigns.GetCalendar(r.Context(), a.campaign.ID)
		if cerr != nil {
			cal = nil
		}
		names := s.journeyEntityNames(r.Context(), a.campaign.ID)
		out := make([]journeyView, 0, len(rows))
		for i := range rows {
			out = append(out, s.toJourneyView(&rows[i], cal, names))
		}
		writeJSON(w, http.StatusOK, map[string]any{"journeys": out})
		return
	}
	var req struct {
		From    string `json:"from"`
		To      string `json:"to"`
		Days    *int64 `json:"days"`
		Density string `json:"density"`
		Pace    string `json:"pace"`
		Seed    *int64 `json:"seed"`
		Session string `json:"session"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.From == "" || req.To == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a journey needs \"from\" and \"to\" location entities"))
		return
	}
	row, res, err := s.journeys.Plan(r.Context(), a.campaign.ID, req.From, req.To,
		req.Days, req.Density, req.Pace, req.Seed, req.Session, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	cal, _, cerr := s.campaigns.GetCalendar(r.Context(), a.campaign.ID)
	if cerr != nil {
		cal = nil
	}
	names := s.journeyEntityNames(r.Context(), a.campaign.ID)
	days := make([]journeyDayView, 0, len(res.DayTable))
	for i := range res.DayTable {
		days = append(days, s.toDayView(&res.DayTable[i], names, cal))
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"journey": s.toJourneyView(row, cal, names),
		"days":    days,
		"result":  res,
	})
}

// handleCampaignJourney reads one journey with its day table.
func (s *Server) handleCampaignJourney(w http.ResponseWriter, r *http.Request) {
	if !s.journeysEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	row, days, err := s.journeys.Get(r.Context(), a.campaign.ID, r.PathValue("jid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	cal, _, cerr := s.campaigns.GetCalendar(r.Context(), a.campaign.ID)
	if cerr != nil {
		cal = nil
	}
	names := s.journeyEntityNames(r.Context(), a.campaign.ID)
	views := make([]journeyDayView, 0, len(days))
	for i := range days {
		views = append(views, s.toDayView(&days[i], names, cal))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"journey": s.toJourneyView(row, cal, names),
		"days":    views,
	})
}

// handleCampaignJourneyPatch settles a journey's bookkeeping:
// {"status": "abandoned", "session": "<session-id>"}. Status moves to
// abandoned only — done is the finalizer's verdict alone.
func (s *Server) handleCampaignJourneyPatch(w http.ResponseWriter, r *http.Request) {
	if !s.journeysEnabled(w) {
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
		Status  *string `json:"status"`
		Session *string `json:"session"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	row, err := s.journeys.Patch(r.Context(), a.campaign.ID, r.PathValue("jid"), req.Status, req.Session)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	cal, _, cerr := s.campaigns.GetCalendar(r.Context(), a.campaign.ID)
	if cerr != nil {
		cal = nil
	}
	names := s.journeyEntityNames(r.Context(), a.campaign.ID)
	writeJSON(w, http.StatusOK, map[string]any{"journey": s.toJourneyView(row, cal, names)})
}

// handleCampaignJourneyDayResolve marks one day happened at the table:
// {"detail": "...", "encounter": "<encounter-id>"} — the DM's own account
// of what the day became, and the encounter that was actually run.
func (s *Server) handleCampaignJourneyDayResolve(w http.ResponseWriter, r *http.Request) {
	if !s.journeysEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	day, err := strconv.ParseInt(r.PathValue("n"), 10, 64)
	if err != nil || day < 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the day index must be a whole number"))
		return
	}
	var req struct {
		Detail    *string `json:"detail"`
		Encounter *string `json:"encounter"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	row, d, err := s.journeys.ResolveDay(r.Context(), a.campaign.ID, r.PathValue("jid"), day, req.Detail, req.Encounter)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	cal, _, cerr := s.campaigns.GetCalendar(r.Context(), a.campaign.ID)
	if cerr != nil {
		cal = nil
	}
	names := s.journeyEntityNames(r.Context(), a.campaign.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"journey": s.toJourneyView(row, cal, names),
		"day":     s.toDayView(d, names, cal),
	})
}

// handleCampaignJourneyResolve turns the whole road into one proposal
// batch. The batch renders and decides on the ordinary review surface —
// accepting it is what writes the events and moves the clock.
func (s *Server) handleCampaignJourneyResolve(w http.ResponseWriter, r *http.Request) {
	if !s.journeysEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	batch, err := s.journeys.Resolve(r.Context(), a.campaign.ID, r.PathValue("jid"), userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"batch": toBatchView(*batch, true),
		"journey": journeyView{
			ID: r.PathValue("jid"), Status: journey.StatusUnderway, BatchID: batch.ID,
		},
	})
}

// entityNames loads the campaign's id -> name join the journey views
// render against. A failure degrades to ids, never fails the read.
func (s *Server) journeyEntityNames(ctx context.Context, campaignID string) map[string]string {
	out := map[string]string{}
	entities, err := s.campaigns.ListEntities(ctx, campaign.ScopeDM, campaignID, "")
	if err != nil {
		return out
	}
	for _, e := range entities {
		out[e.ID] = e.Name
	}
	return out
}
