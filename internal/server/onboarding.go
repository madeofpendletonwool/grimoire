package server

// The onboarding snapshot (the Guide, web/static/js/guide.js): which of the
// milestones a track walks has actually happened, as counts.
//
// Why one endpoint rather than the Guide firing the reads it already has
// routes for: the milestones live behind /story, /proposals, /encounters and
// /invites, and every one of those is DM-only. A player-seat Guide asking
// them directly would spray 403s across the console to render an onboarding
// card, and would put DM-shaped routes in a player surface's code path for
// no gain. This resolves the caller's standing once and answers with counts
// narrowed to what that standing may know — a player's response carries
// zeroes in the DM fields rather than an error, so one response shape serves
// both tracks and the client needs no per-field branching.
//
// Nothing here is authorization on its own: every count is over rows the
// caller's own membership already reaches, and a caller with no standing
// never gets past resolveCampaignAccess's 404 (ADR 4).

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// onboardingState is deliberately counts and flags, never rows. The Guide
// paints checkmarks; handing it the campaign's actual proposals or members
// would be a second, unscoped copy of surfaces that already exist.
type onboardingState struct {
	Campaigns int    `json:"campaigns"`
	Role      string `json:"role"` // dm | player | observer | keeper | ""

	// The DM's milestones. Zero at a player seat, always.
	HasSpine   bool `json:"has_spine"`
	Decided    int  `json:"decided"`
	Pending    int  `json:"pending"`
	Encounters int  `json:"encounters"`
	Members    int  `json:"members"`
	Invites    int  `json:"invites"`

	// Shared, and the player's own.
	Sessions       int  `json:"sessions"`
	Rolls          int  `json:"rolls"`
	HasCharacter   bool `json:"has_character"`
	JournalEntries int  `json:"journal_entries"`
}

// handleOnboardingState answers the Guide's "what has actually happened yet"
// question. The campaign is optional: without one the response carries only
// the account-level facts, which is exactly the state a brand-new account is
// in and the one the fork card is drawn from.
func (s *Server) handleOnboardingState(w http.ResponseWriter, r *http.Request) {
	if !s.campaignsEnabled(w) {
		return
	}
	ctx := r.Context()
	uid := userID(r)

	list, err := s.campaigns.ListCampaigns(ctx, uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := onboardingState{Campaigns: len(list)}

	campaignID := r.URL.Query().Get("campaign")
	if campaignID == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}

	// A named campaign goes through the same gate every campaign route does,
	// so a caller with no standing learns nothing a wrong id would not tell
	// them. resolveCampaignAccess writes its own error.
	a := s.resolveCampaignAccess(w, r, campaignID)
	if a == nil {
		return
	}
	out.Role = a.role
	if a.keeper && out.Role == "" {
		out.Role = "keeper"
	}

	db := s.campaigns.DB()
	if a.isDM() {
		out.HasSpine = exists(ctx, db,
			`SELECT 1 FROM acts WHERE campaign_id = ?
			 UNION ALL SELECT 1 FROM session_plans WHERE campaign_id = ? LIMIT 1`,
			campaignID, campaignID)
		out.Decided = count(ctx, db,
			`SELECT COUNT(*) FROM proposal_batches WHERE campaign_id = ? AND status <> 'open'`, campaignID)
		out.Pending = count(ctx, db,
			`SELECT COUNT(*) FROM proposal_batches WHERE campaign_id = ? AND status = 'open'`, campaignID)
		out.Encounters = count(ctx, db,
			`SELECT COUNT(*) FROM encounters WHERE campaign_id = ?`, campaignID)
		// The party, not the table: a DM is a member row of their own
		// campaign, and "you have invited nobody" must not read as one.
		out.Members = count(ctx, db,
			`SELECT COUNT(*) FROM campaign_members WHERE campaign_id = ? AND role <> ?`,
			campaignID, campaign.RoleDM)
		out.Invites = count(ctx, db,
			`SELECT COUNT(*) FROM campaign_invites WHERE campaign_id = ?`, campaignID)
	}

	out.Sessions = count(ctx, db,
		`SELECT COUNT(*) FROM game_sessions WHERE campaign_id = ?`, campaignID)

	// The player's own two. A DM walking their own track has no binding and
	// writes no journal, so both stay honest at either seat: these count the
	// caller's rows, never the table's.
	if a.member != nil && a.member.CharacterID != "" {
		out.HasCharacter = true
		out.Rolls = count(ctx, db,
			`SELECT COUNT(*) FROM dice_rolls WHERE campaign_id = ? AND character_id = ?`,
			campaignID, a.member.CharacterID)
		out.JournalEntries = count(ctx, db,
			`SELECT COUNT(*) FROM session_sources s
			   JOIN game_sessions g ON g.id = s.session_id
			  WHERE g.campaign_id = ? AND s.kind = 'player_journal' AND s.author = ?`,
			campaignID, a.member.CharacterID)
	} else if a.isDM() {
		// An unbound caller has no rolls of their own to count; the DM's
		// "has the table rolled at all" is the table's feed.
		out.Rolls = count(ctx, db, `SELECT COUNT(*) FROM dice_rolls WHERE campaign_id = ?`, campaignID)
	}

	writeJSON(w, http.StatusOK, out)
}

// count runs a scalar COUNT. A failed count is zero, not an error: the Guide
// is a progress display, and refusing to draw the whole card because one
// checkmark could not be computed is the worse outcome by far.
func count(ctx context.Context, db *sql.DB, query string, args ...any) int {
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// exists reports whether the query returns any row, on the same terms.
func exists(ctx context.Context, db *sql.DB, query string, args ...any) bool {
	var one int
	err := db.QueryRowContext(ctx, query, args...).Scan(&one)
	return err == nil
}
