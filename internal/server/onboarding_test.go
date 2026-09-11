package server

// The onboarding snapshot (the Guide). These are scope tests in the
// internal/server style (ADR 8): the product being pinned is that one
// response shape serves both seats — a player's request answers 200 with
// zeroes in the DM fields rather than 403, and a non-member learns nothing
// a wrong id would not tell them.
//
// The counting itself is asserted through the API rather than the store,
// because the failure worth catching is a count that is never *reached*:
// a DM block computed at a player's standing would leak how much prep the
// DM has done, which is exactly the shape of thing the player portal
// exists to withhold.

import (
	"encoding/json"
	"net/http"
	"testing"
)

func onboarding(t *testing.T, s *Server, target string, cookie *http.Cookie) map[string]any {
	t.Helper()
	rec := hit(t, s, http.MethodGet, target, "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", target, rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v (%s)", target, err, rec.Body)
	}
	return body
}

func num(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	v, ok := body[key].(float64)
	if !ok {
		t.Fatalf("%s missing or not a number: %v", key, body[key])
	}
	return int(v)
}

func TestOnboardingStateWithoutACampaign(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	admin := adminSession(t, s)

	// A brand-new account: the state the fork card is drawn from. Zero
	// campaigns is an answer, not an error — an install whose first keeper
	// has not founded a table yet is the most common first boot there is.
	body := onboarding(t, s, "/api/onboarding/state", admin)
	if got := num(t, body, "campaigns"); got != 0 {
		t.Fatalf("campaigns = %d, want 0", got)
	}
	if role, _ := body["role"].(string); role != "" {
		t.Fatalf("role = %q, want empty with no campaign named", role)
	}
}

func TestOnboardingStateCountsTheDMsMilestones(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	// One seated player, so the party count has something to find.
	addPlayerMember(t, s, f, "mira", true)
	dm := dmSession(t, s)

	body := onboarding(t, s, "/api/onboarding/state?campaign="+f.campaignID, dm)

	if got := num(t, body, "campaigns"); got != 1 {
		t.Fatalf("campaigns = %d, want 1", got)
	}
	if role, _ := body["role"].(string); role != "dm" {
		t.Fatalf("role = %q, want dm", role)
	}
	// The party, not the table: the DM holds a member row of their own
	// campaign, and counting it would report "you have invited someone"
	// to a DM who has invited nobody.
	if got := num(t, body, "members"); got != 1 {
		t.Fatalf("members = %d, want 1 (the player, never the DM)", got)
	}
	if got := num(t, body, "invites"); got != 1 {
		t.Fatalf("invites = %d, want 1", got)
	}
	// Nothing in the fixture has touched the spine or the builder, so these
	// must read as outstanding rather than as done-by-default.
	if body["has_spine"] != false {
		t.Fatalf("has_spine = %v, want false on a campaign with no acts", body["has_spine"])
	}
	for _, key := range []string{"encounters", "sessions", "decided"} {
		if got := num(t, body, key); got != 0 {
			t.Fatalf("%s = %d, want 0", key, got)
		}
	}
}

func TestOnboardingStateAtAPlayerSeatHidesTheDMsPrep(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	player := addPlayerMember(t, s, f, "mira", true)

	// The whole point: a player asks the same route and is answered, not
	// refused. A 403 here would force the client to read "you may not see
	// this" as "this has not happened yet" — two very different facts.
	body := onboarding(t, s, "/api/onboarding/state?campaign="+f.campaignID, player)

	if role, _ := body["role"].(string); role != "player" {
		t.Fatalf("role = %q, want player", role)
	}
	if body["has_character"] != true {
		t.Fatalf("has_character = %v, want true for a bound player", body["has_character"])
	}
	// How much prep the DM has done is not the party's business. These stay
	// zero at a player's standing whatever the campaign actually holds.
	for _, key := range []string{"decided", "pending", "encounters", "members", "invites"} {
		if got := num(t, body, key); got != 0 {
			t.Fatalf("%s = %d at a player seat, want 0 — the DM's prep leaked", key, got)
		}
	}
	if body["has_spine"] != false {
		t.Fatalf("has_spine = %v at a player seat, want false", body["has_spine"])
	}
}

func TestOnboardingStateUnboundPlayerHasNoCharacter(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	// An observer-shaped member: joined, never bound. Nothing
	// character-shaped may reach them (ADR 22), so the Guide must not
	// offer them a step that can never tick over.
	watcher := addPlayerMember(t, s, f, "wren", false)

	body := onboarding(t, s, "/api/onboarding/state?campaign="+f.campaignID, watcher)
	if body["has_character"] != false {
		t.Fatalf("has_character = %v, want false for an unbound member", body["has_character"])
	}
	if got := num(t, body, "journal_entries"); got != 0 {
		t.Fatalf("journal_entries = %d, want 0 for an unbound member", got)
	}
}

func TestOnboardingStateHidesACampaignFromANonMember(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)

	// A real account with no standing in this campaign. Default deny is a
	// missing row (ADR 4): the same 404 a wrong id gives, never a hint that
	// the campaign exists.
	admin := dmSession(t, s)
	inv := createInvite(t, s, admin, "a stranger")
	code, _ := inv["code"].(string)
	reg := hit(t, s, http.MethodPost, "/api/auth/register", registerJSON("stranger", "a-fine-passphrase", code))
	if reg.Code != http.StatusCreated {
		t.Fatalf("register stranger: status %d, body %s", reg.Code, reg.Body)
	}
	stranger := sessionFrom(t, reg)

	rec := hit(t, s, http.MethodGet, "/api/onboarding/state?campaign="+f.campaignID, "", stranger)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-member status = %d, want 404", rec.Code)
	}

	// And a campaign that does not exist answers identically, which is the
	// property that makes the 404 above carry no information.
	missing := hit(t, s, http.MethodGet, "/api/onboarding/state?campaign=no-such-campaign", "", stranger)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing campaign status = %d, want 404", missing.Code)
	}
}
