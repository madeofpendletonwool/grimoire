package server

// Campaign clock surface tests (MAD-365). As with the campaign surface, the
// load-bearing ones assert on HTTP responses: a player cannot read a secret
// schedule entry — the body must not contain it — the clock read serves the
// current date to everyone, and only the DM moves time.

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// clockFixture is a campaign with two locations joined by a road, a public
// and a secret schedule entry, and the clock at day 10.
type clockFixture struct {
	fixture
	ashfordID string
	isleID    string
	publicID  string
	secretID  string
}

func buildClockFixture(t *testing.T, s *Server) clockFixture {
	t.Helper()
	f := buildFixture(t, s)
	dm := dmSession(t, s)

	mkLoc := func(name, payload string) string {
		body := `{"kind":"location","name":` + quote(name) + `,"payload":` + payload + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create location %s: status %d, body %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entity")
	}
	ashford := mkLoc("Ashford", `{}`)
	isle := mkLoc("The Isle", `{"climate":"arctic"}`)

	// Road: Ashford -> Ashford-crossroads -> The Isle would be cheaper; keep
	// it simple — a direct 4-day road is fine for the endpoint test.
	body := `{"payload":{"travel":{"routes":[{"to":` + quote(isle) + `,"days":4,"terrain":"road"}]}}}`
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/entities/"+ashford, body, dm); r.Code != http.StatusOK {
		t.Fatalf("add route: status %d, body %s", r.Code, r.Body)
	}

	mkEntry := func(name string, day int, recurrence, visibility string) string {
		body := `{"name":` + quote(name) + `,"day":` + strconv.Itoa(day) +
			`,"recurrence":` + quote(recurrence) + `,"visibility":` + quote(visibility) + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/schedule", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create schedule entry %s: status %d, body %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entry")
	}
	public := mkEntry("The Lamplight Festival", 100, "yearly", "public")
	secret := mkEntry("The Duke's hidden council", 20, "none", "secret")

	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/clock/advance",
		`{"to":10,"reason":"session","note":"session one"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("advance clock: status %d, body %s", r.Code, r.Body)
	}
	return clockFixture{fixture: f, ashfordID: ashford, isleID: isle, publicID: public, secretID: secret}
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestPlayerCannotReadSecretScheduleEntry is the ADR 8 acceptance test: the
// player's schedule list and due list must not carry the secret entry — not
// its name, not its id — and no schedule write is reachable for them.
func TestPlayerCannotReadSecretScheduleEntry(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildClockFixture(t, s)
	player := addPlayerMember(t, s, f.fixture, "clockplayer", true)

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/schedule", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player schedule: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "hidden council") || strings.Contains(rec.Body.String(), f.secretID) {
		t.Fatalf("secret schedule entry leaked to the player: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Lamplight Festival") {
		t.Fatalf("public entry missing from the player's schedule: %s", rec.Body)
	}

	// The due list filters the same way: the secret lands on day 20, inside
	// a 30-day window, and must not appear.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/schedule?due=30", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player schedule due: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "hidden council") {
		t.Fatalf("secret entry leaked through the due list: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Lamplight Festival") {
		t.Fatalf("public yearly entry missing from the due list: %s", rec.Body)
	}

	// The DM sees both.
	dm := dmSession(t, s)
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/schedule", "", dm)
	if !strings.Contains(rec.Body.String(), "hidden council") {
		t.Fatalf("DM must see the secret entry: %s", rec.Body)
	}

	// Writes are the DM's alone.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/schedule",
		`{"name":"sneak","day":1}`, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player schedule create must be 403, got %d", rec.Code)
	}
	rec = hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/schedule/"+f.secretID,
		`{"status":"fired"}`, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player schedule patch must be 403, got %d", rec.Code)
	}
}

// TestClockReadServesDateAndWeather: every member reads the current date;
// the ledger rides along for the DM only. The date is the default calendar's
// — day 10 is the 11th of Firstmonth, year 1.
func TestClockReadServesDateAndWeather(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildClockFixture(t, s)
	player := addPlayerMember(t, s, f.fixture, "clockreader", true)

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock?strip=5&due=30&location="+f.isleID, "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player clock: status %d, body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"day":10`) {
		t.Fatalf("clock day missing: %s", body)
	}
	for _, want := range []string{`"date":"`, "Firstmonth", `"weather":`, `"climate":"arctic"`, `"strip":[`} {
		if !strings.Contains(body, want) {
			t.Fatalf("clock response missing %q: %s", want, body)
		}
	}
	// The secret entry lands on day 20 — inside this 30-day window — and
	// must not appear anywhere in the player's response.
	if strings.Contains(body, "hidden council") {
		t.Fatalf("secret entry leaked through the clock due list: %s", body)
	}
	if strings.Contains(body, `"advances"`) {
		t.Fatalf("the ledger must not ride along for a player: %s", body)
	}

	dm := dmSession(t, s)
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock", "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"advances":[`) {
		t.Fatalf("DM clock must carry the ledger: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "session one") {
		t.Fatalf("DM ledger note missing: %s", rec.Body)
	}

	// Players never move time.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/clock/advance", `{"by":7}`, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player advance must be 403, got %d", rec.Code)
	}
}

// TestCalendarPutAndGet: the DM authors the calendar; a player reads it (it
// is how any date is legible) but cannot change it. The custom calendar
// round-trips through GET.
func TestCalendarPutAndGet(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildClockFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, f.fixture, "calreader", true)

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/calendar", "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Common Reckoning") {
		t.Fatalf("default calendar must be readable by a player: %d %s", rec.Code, rec.Body)
	}

	custom := `{"calendar":{"name":"The Wheel","epoch_label":"W",
		"months":[{"name":"Sun","days":91},{"name":"Rain","days":120},{"name":"Harvest","days":60}],
		"weekdays":["Star","Stone","Storm"],
		"seasons":[{"name":"green","start_day":1,"end_day":91},{"name":"gold","start_day":92,"end_day":271}],
		"leap_rule":{"every":5,"month":2,"days":2}},"seed":"wheel-seed"}`
	rec = hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/calendar", custom, dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "The Wheel") {
		t.Fatalf("put calendar: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/calendar", "", player)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "wheel-seed") {
		t.Fatalf("custom calendar must round-trip: %d %s", rec.Code, rec.Body)
	}
	// The clock now speaks Wheel: day 10 is the 11th of Sun.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock", "", dm)
	if !strings.Contains(rec.Body.String(), "11 Sun 1 W") {
		t.Fatalf("clock must format through the custom calendar: %s", rec.Body)
	}

	rec = hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/calendar", custom, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player calendar put must be 403, got %d", rec.Code)
	}
	broken := `{"calendar":{"name":"bad","months":[],"weekdays":["A"]}}`
	rec = hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/calendar", broken, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty calendar must be 400, got %d", rec.Code)
	}
}

// TestTravelEndpoint: the road is walked, the clock moves by the day cost,
// and an unrouted pair without a day count is a 400 that asks for one.
func TestTravelEndpoint(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildClockFixture(t, s)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/travel",
		`{"from":`+quote(f.ashfordID)+`,"to":`+quote(f.isleID)+`}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("travel: status %d, body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"days":4`, `"clock":14`, "Ashford", "The Isle", `"date":`} {
		if !strings.Contains(body, want) {
			t.Fatalf("travel response missing %q: %s", want, body)
		}
	}

	// Nowhere-to-nowhere: a third location with no routes.
	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities",
		`{"kind":"location","name":"Duskmere"}`, dm)
	duskmere := idFrom(t, r, "entity")
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/travel",
		`{"from":`+quote(f.ashfordID)+`,"to":`+quote(duskmere)+`}`, dm)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no route") {
		t.Fatalf("unrouted travel must 400 asking for days: %d %s", rec.Code, rec.Body)
	}
	// With the DM's day count it is recorded and the clock moves.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/travel",
		`{"from":`+quote(f.ashfordID)+`,"to":`+quote(duskmere)+`,"days":9}`, dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"clock":23`) {
		t.Fatalf("travel with day count: %d %s", rec.Code, rec.Body)
	}

	// The ledger tells the story of both legs.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock", "", dm)
	if !strings.Contains(rec.Body.String(), "Ashford to Duskmere: 9 days") {
		t.Fatalf("travel advance note missing from ledger: %s", rec.Body)
	}

	// A player cannot move the party.
	player := addPlayerMember(t, s, f.fixture, "traveller", true)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/travel",
		`{"from":`+quote(f.ashfordID)+`,"to":`+quote(f.isleID)+`}`, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player travel must be 403, got %d", rec.Code)
	}
}

// TestCampaignPatchClockRecordsAdvance: the old PATCH surface keeps working
// and lands in the ledger with reason 'manual' — including a backwards fix.
func TestCampaignPatchClockRecordsAdvance(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildClockFixture(t, s)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID, `{"clock":25}`, dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"clock":25`) {
		t.Fatalf("patch clock: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock", "", dm)
	if !strings.Contains(rec.Body.String(), `"reason":"manual"`) {
		t.Fatalf("patch must record a manual advance: %s", rec.Body)
	}

	// A backwards move is legal and recorded.
	rec = hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID, `{"clock":12}`, dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"clock":12`) {
		t.Fatalf("patch clock backwards: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/clock", "", dm)
	if !strings.Contains(rec.Body.String(), `"day":12`) {
		t.Fatalf("backwards patch must land: %s", rec.Body)
	}
}
