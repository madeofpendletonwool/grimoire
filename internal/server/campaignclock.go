package server

// The campaign clock surface (MAD-365): the calendar, the clock and its
// ledger, the schedule, and travel time. The pure arithmetic is internal/clock
// (no DB, no wall clock, no network); the rows are internal/campaign/clock.go.
//
// Scope rules (ADR 8): players see the current date and public schedule
// entries; secret entries are DM-only and absent — not blanked — from every
// non-DM response. The calendar definition itself is not secret: it is how a
// player reads any date at all. Advancing the clock, editing the calendar,
// editing the schedule and travel are DM-only.
//
// Weather is derived, never stored: GET /clock computes it from the campaign
// seed on every request, deterministically, for zero rows and zero tokens.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/clock"
)

// campaignCalendar loads the campaign's calendar for date formatting. A nil
// return (only a broken stored definition) degrades every date to the bare
// day number rather than failing the read.
func (s *Server) campaignCalendar(ctx context.Context, campaignID string) *clock.Calendar {
	cal, _, err := s.campaigns.GetCalendar(ctx, campaignID)
	if err != nil {
		return nil
	}
	return cal
}

/* ---------- views ---------- */

type calendarView struct {
	Name       string          `json:"name"`
	Months     []clock.Month   `json:"months"`
	Weekdays   []string        `json:"weekdays"`
	Seasons    []clock.Season  `json:"seasons"`
	LeapRule   *clock.LeapRule `json:"leap_rule,omitempty"`
	EpochLabel string          `json:"epoch_label"`
	Seed       string          `json:"seed"`
}

func toCalendarView(cal *clock.Calendar, seed string) calendarView {
	return calendarView{
		Name: cal.Name, Months: cal.Months, Weekdays: cal.Weekdays, Seasons: cal.Seasons,
		LeapRule: cal.LeapRule, EpochLabel: cal.EpochLabel, Seed: seed,
	}
}

type stripDayView struct {
	Day        int64  `json:"day"`
	DayOfMonth int    `json:"day_of_month"`
	Month      string `json:"month"`
	Label      string `json:"label"`       // "15 Thirdmonth 12 CR"
	Weekday    string `json:"weekday"`     // "Thirdday"
	MonthStart bool   `json:"month_start"` // first day of its month
	Season     string `json:"season,omitempty"`
	Today      bool   `json:"today"`
}

type dueView struct {
	EntryID string `json:"entry_id"`
	Name    string `json:"name"`
	Day     int64  `json:"day"`
	Date    string `json:"date,omitempty"`
}

type advanceView struct {
	ID        string `json:"id"`
	FromDay   int64  `json:"from_day"`
	ToDay     int64  `json:"to_day"`
	Reason    string `json:"reason"`
	Note      string `json:"note"`
	SessionID string `json:"session_id,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at"`
}

func toAdvanceView(a *campaign.Advance) advanceView {
	return advanceView{
		ID: a.ID, FromDay: a.FromDay, ToDay: a.ToDay, Reason: a.Reason, Note: a.Note,
		SessionID: a.SessionID, CreatedBy: a.CreatedBy,
		CreatedAt: a.CreatedAt.Format(http.TimeFormat),
	}
}

type scheduleEntryView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Detail     string `json:"detail,omitempty"`
	Day        int64  `json:"day"`
	Date       string `json:"date,omitempty"`
	Recurrence string `json:"recurrence"` // none | yearly | monthly | every_n_days:<N>
	Status     string `json:"status"`
	EntityID   string `json:"entity_id,omitempty"`
	Location   string `json:"location_entity,omitempty"`
	Visibility string `json:"visibility"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

func toScheduleEntryView(e *campaign.ScheduledEvent, cal *clock.Calendar) scheduleEntryView {
	rec := e.Recurrence
	if e.Recurrence == clock.RecurEveryNDays {
		rec = clock.FormatRecurrence(e.Recurrence, e.EveryNDays)
	}
	v := scheduleEntryView{
		ID: e.ID, Name: e.Name, Detail: e.Detail, Day: e.Day,
		Recurrence: rec, Status: e.Status, EntityID: e.EntityID,
		Location: e.LocationEntity, Visibility: e.Visibility,
		CreatedAt: e.CreatedAt.Format(http.TimeFormat), UpdatedAt: e.UpdatedAt.Format(http.TimeFormat),
	}
	if cal != nil {
		v.Date = cal.Format(e.Day)
	}
	return v
}

type clockView struct {
	Day      int64           `json:"day"`
	Date     string          `json:"date"`
	Weekday  string          `json:"weekday"`
	Season   string          `json:"season,omitempty"`
	Climate  string          `json:"climate,omitempty"` // the location tag the weather was drawn for, when asked
	Weather  *clock.Forecast `json:"weather,omitempty"`
	Strip    []stripDayView  `json:"strip,omitempty"`
	Due      []dueView       `json:"due,omitempty"`
	Advances []advanceView   `json:"advances,omitempty"` // DM reads only
}

/* ---------- the calendar ---------- */

func (s *Server) handleCampaignCalendar(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	cal, seed, err := s.campaigns.GetCalendar(r.Context(), a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"calendar": toCalendarView(cal, seed)})
}

func (s *Server) handlePutCampaignCalendar(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Calendar *clock.Calendar `json:"calendar"`
		Seed     string          `json:"seed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Calendar == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: calendar is required"))
		return
	}
	cal, seed, err := s.campaigns.PutCalendar(r.Context(), a.campaign.ID, req.Calendar, req.Seed)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"calendar": toCalendarView(cal, seed)})
}

/* ---------- the clock ---------- */

func (s *Server) handleCampaignClock(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	ctx := r.Context()
	cal, seed, err := s.campaigns.GetCalendar(ctx, a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	day := a.campaign.Clock
	view := clockView{
		Day:     day,
		Date:    cal.Format(day),
		Weekday: cal.WeekdayOf(day),
	}
	if season, ok := cal.SeasonOf(day); ok {
		view.Season = season.Name
	}
	// Weather: derived from the seed, the day, the season, and an optional
	// location's climate tag. The location resolves through the wide store:
	// weather is ambient — what the sky is doing is not knowledge a
	// perspective can be missing, and the caller supplied the id.
	climate := ""
	if loc := r.URL.Query().Get("location"); loc != "" {
		if e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, a.campaign.ID, loc); err == nil {
			// The place block's climate tag first, then the bare top-level
			// tag a payload carried before the block existed (MAD-370).
			climate = campaign.ClimateOf(e)
		}
	}
	forecast := clock.Weather(seed, day, view.Season, climate)
	view.Weather = &forecast
	view.Climate = climate

	if v := r.URL.Query().Get("strip"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 60 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("strip must be 1..60 days"))
			return
		}
		view.Strip = s.clockStrip(cal, day, n)
	}
	if v := r.URL.Query().Get("due"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 400 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("due must be 1..400 days"))
			return
		}
		due, err := s.campaigns.ScheduleDue(ctx, a.campaign.ID, a.isDM(), day, day+int64(n))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for _, d := range due {
			view.Due = append(view.Due, dueView{EntryID: d.EntryID, Name: d.Name, Day: d.Day, Date: cal.FormatShort(d.Day)})
		}
	}
	if a.isDM() {
		ledger, err := s.campaigns.ClockLedger(ctx, campaign.ScopeDM, a.campaign.ID, 10)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for i := range ledger {
			view.Advances = append(view.Advances, toAdvanceView(&ledger[i]))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"clock": view})
}

// clockStrip builds the calendar strip: today, then n-1 days forward, each
// labelled through the calendar. Month starts and the season carry so the UI
// can draw the bands without knowing any calendar math.
func (s *Server) clockStrip(cal *clock.Calendar, today int64, n int) []stripDayView {
	strip := make([]stripDayView, 0, n)
	for i := int64(0); i < int64(n); i++ {
		day := today + i
		d := cal.DateOf(day)
		v := stripDayView{
			Day: day, DayOfMonth: d.Day, Month: cal.Months[d.Month-1].Name,
			Label: cal.FormatShort(day), Weekday: cal.WeekdayOf(day),
			MonthStart: d.Day == 1, Today: i == 0,
		}
		if season, ok := cal.SeasonOf(day); ok {
			v.Season = season.Name
		}
		strip = append(strip, v)
	}
	return strip
}

func (s *Server) handleCampaignClockAdvance(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		To        *int64 `json:"to"`
		By        *int64 `json:"by"`
		Reason    string `json:"reason"`
		Note      string `json:"note"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.Reason == "" {
		req.Reason = campaign.AdvanceManual
	}
	if req.To == nil && req.By == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("advance needs either \"to\" (absolute day) or \"by\" (day count)"))
		return
	}
	var (
		adv *campaign.Advance
		c   *campaign.Campaign
		err error
	)
	if req.By != nil {
		adv, c, err = s.campaigns.AdvanceClockBy(r.Context(), a.campaign.ID, *req.By, req.Reason, req.Note, req.SessionID, userID(r))
	} else {
		adv, c, err = s.campaigns.AdvanceClock(r.Context(), a.campaign.ID, *req.To, req.Reason, req.Note, req.SessionID, userID(r))
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	a.campaign = c
	cal, _, cerr := s.campaigns.GetCalendar(r.Context(), a.campaign.ID)
	if cerr != nil {
		writeStoreError(w, cerr)
		return
	}
	view := clockView{Day: adv.ToDay, Date: cal.Format(adv.ToDay), Weekday: cal.WeekdayOf(adv.ToDay)}
	if season, ok := cal.SeasonOf(adv.ToDay); ok {
		view.Season = season.Name
	}
	writeJSON(w, http.StatusOK, map[string]any{"advance": toAdvanceView(adv), "clock": view})
}

/* ---------- the schedule ---------- */

func (s *Server) handleCampaignSchedule(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	ctx := r.Context()
	cal, _, err := s.campaigns.GetCalendar(ctx, a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	entries, err := s.campaigns.ListScheduledEvents(ctx, a.campaign.ID, a.isDM())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]scheduleEntryView, 0, len(entries))
	for i := range entries {
		views = append(views, toScheduleEntryView(&entries[i], cal))
	}
	out := map[string]any{"entries": views}
	if v := r.URL.Query().Get("due"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 400 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("due must be 1..400 days"))
			return
		}
		due, err := s.campaigns.ScheduleDue(ctx, a.campaign.ID, a.isDM(), a.campaign.Clock, a.campaign.Clock+int64(n))
		if err != nil {
			writeStoreError(w, err)
			return
		}
		var dueViews []dueView
		for _, d := range due {
			dueViews = append(dueViews, dueView{EntryID: d.EntryID, Name: d.Name, Day: d.Day, Date: cal.FormatShort(d.Day)})
		}
		out["due"] = dueViews
	}
	writeJSON(w, http.StatusOK, out)
}

// scheduleInputFromRequest decodes the shared create/patch body.
func scheduleInputFromRequest(r *http.Request) (campaign.ScheduleInput, error) {
	var req struct {
		Name       *string `json:"name"`
		Detail     *string `json:"detail"`
		Day        *int64  `json:"day"`
		Recurrence *string `json:"recurrence"`
		Status     *string `json:"status"`
		EntityID   *string `json:"entity_id"`
		Location   *string `json:"location_entity"`
		Visibility *string `json:"visibility"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return campaign.ScheduleInput{}, fmt.Errorf("invalid request body")
	}
	return campaign.ScheduleInput{
		Name: req.Name, Detail: req.Detail, Day: req.Day, Recurrence: req.Recurrence,
		Status: req.Status, EntityID: req.EntityID, LocationEntity: req.Location,
		Visibility: req.Visibility,
	}, nil
}

func (s *Server) handleCreateCampaignSchedule(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	in, err := scheduleInputFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	e, err := s.campaigns.CreateScheduledEvent(r.Context(), a.campaign.ID, in)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	cal, _, cerr := s.campaigns.GetCalendar(r.Context(), a.campaign.ID)
	if cerr != nil {
		writeStoreError(w, cerr)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"entry": toScheduleEntryView(e, cal)})
}

func (s *Server) handleUpdateCampaignSchedule(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	in, err := scheduleInputFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	e, err := s.campaigns.UpdateScheduledEvent(r.Context(), a.campaign.ID, r.PathValue("sid"), in)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	cal, _, cerr := s.campaigns.GetCalendar(r.Context(), a.campaign.ID)
	if cerr != nil {
		writeStoreError(w, cerr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": toScheduleEntryView(e, cal)})
}

func (s *Server) handleDeleteCampaignSchedule(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if err := s.campaigns.DeleteScheduledEvent(r.Context(), a.campaign.ID, r.PathValue("sid")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- travel ---------- */

func (s *Server) handleCampaignTravel(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
		Days *int64 `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.From == "" || req.To == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("travel needs \"from\" and \"to\" location entities"))
		return
	}
	res, err := s.campaigns.Travel(r.Context(), a.campaign.ID, req.From, req.To, req.Days, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	cal, _, cerr := s.campaigns.GetCalendar(r.Context(), a.campaign.ID)
	if cerr != nil {
		writeStoreError(w, cerr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"travel": map[string]any{
		"days":  res.Days,
		"path":  res.Path,
		"names": res.Names,
		"clock": res.Clock,
		"date":  cal.Format(res.Clock),
	}})
}
