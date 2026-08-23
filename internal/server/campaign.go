package server

// The campaign surface (MAD-305): REST under /api/campaigns/... over the
// campaign graph (internal/campaign) and the knowledge layer
// (internal/knowledge), plus the campaign-bound invites that mint
// campaign_members rows.
//
// The one rule this file exists to enforce: every campaign endpoint resolves a
// knowledge.Scope from the caller's campaign_members row before it touches
// the store. A DM route and a player route are different scopes, not
// different prompts. Player-facing reads go through knowledge.PlayerView, so
// the wide store is not even reachable from them — and writes (entities,
// facts, events, relationships, quests, awareness, members, invites) are
// DM-only, checked before any store call.
//
// Default deny is a missing row (ADR 4): a caller with no membership and no
// keeper flag gets the same 404 a wrong campaign id would produce, never a
// hint that the campaign exists.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

// WithCampaigns wires the campaign graph and the knowledge layer. Separate
// from New so the feature is additive; without it the campaign endpoints
// report unavailable rather than panicking on a nil store.
func (s *Server) WithCampaigns(store *campaign.Store, knowledge *knowledge.Store) *Server {
	s.campaigns = store
	s.knowledge = knowledge
	return s
}

// campaignsEnabled reports whether the campaign surface is wired, writing the
// error response when it is not. Accounts are required too: the whole
// membership model is meaningless without a user to be a member.
func (s *Server) campaignsEnabled(w http.ResponseWriter) bool {
	if s.campaigns == nil || s.knowledge == nil || s.users == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("campaigns are not available"))
		return false
	}
	return true
}

/* ---------- access: the scope resolution every handler runs first ---------- */

// campAccess is the caller's resolved standing in one campaign: which
// campaign, what role, whether the keeper flag applies, and — for non-DM
// callers — the narrow PlayerView their reads are served through.
type campAccess struct {
	campaign *campaign.Campaign
	role     string // dm | player | observer; empty for a keeper with no row
	keeper   bool
	member   *campaign.Member
	view     knowledge.PlayerView // nil exactly when the caller may use the wide store
}

// isDM reports whether the caller holds the campaign's DM perspective: the dm
// membership role, or the keeper (who owns the box the campaign lives on).
func (a *campAccess) isDM() bool { return a.role == campaign.RoleDM || a.keeper }

// resolveCampaignAccess loads the campaign and resolves the caller's scope.
// It writes the HTTP error itself and returns nil when the caller should not
// proceed: 404 for a missing campaign or a caller with no standing (kept
// indistinguishable on purpose), 403 for a player construction that cannot be
// served (e.g. a character binding that points at a deleted entity).
func (s *Server) resolveCampaignAccess(w http.ResponseWriter, r *http.Request, campaignID string) *campAccess {
	if !s.campaignsEnabled(w) {
		return nil
	}
	ctx := r.Context()
	uid := userID(r)
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		writeStoreError(w, err)
		return nil
	}
	role, ok, err := s.campaigns.Role(ctx, campaignID, uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return nil
	}
	if !ok {
		// The keeper may always look: they minted the accounts, and the
		// membership-management rule names them. Anyone else with no row
		// learns nothing — same 404 as a missing campaign.
		isAdmin, err := s.users.IsAdmin(ctx, uid)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return nil
		}
		if !isAdmin {
			writeError(w, http.StatusNotFound, fmt.Errorf("not found"))
			return nil
		}
		return &campAccess{campaign: c, keeper: true}
	}
	a := &campAccess{campaign: c, role: role}
	if role == campaign.RoleDM {
		return a
	}
	members, err := s.campaigns.Members(ctx, campaignID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return nil
	}
	for i := range members {
		if members[i].UserID == uid {
			a.member = &members[i]
			break
		}
	}
	scope := knowledge.ScopeParty
	if role == campaign.RolePlayer && a.member != nil && a.member.CharacterID != "" {
		scope = knowledge.ScopeCharacter(a.member.CharacterID)
	}
	view, err := s.knowledge.PlayerViewOf(scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return nil
	}
	a.view = view
	return a
}

// requireDM writes the response and returns false when the caller did not
// resolve to the DM perspective. Every write route calls it before touching
// the store.
func (a *campAccess) requireDM(w http.ResponseWriter) bool {
	if a == nil || !a.isDM() {
		writeError(w, http.StatusForbidden, fmt.Errorf("the campaign's DM may do that; a player may not"))
		return false
	}
	return true
}

// writeStoreError maps the stores' sentinel errors onto HTTP statuses:
// not-found stays 404, invalid input is 400, a duplicate is 409, and anything
// else surfaces as 500 without its SQL traceback.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, campaign.ErrNotFound), errors.Is(err, knowledge.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, campaign.ErrAlreadyExists):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, campaign.ErrInvalid), errors.Is(err, knowledge.ErrInvalid), errors.Is(err, campaign.ErrScope):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

/* ---------- views ---------- */

type campaignView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	System    string `json:"system"`
	Premise   string `json:"premise"`
	Clock     int64  `json:"clock"`
	MyRole    string `json:"my_role"` // dm | player | observer | keeper
	OwnerID   string `json:"owner_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (a *campAccess) toCampaignView() campaignView {
	role := a.role
	if a.keeper && role == "" {
		role = "keeper"
	}
	c := a.campaign
	return campaignView{
		ID: c.ID, Name: c.Name, System: c.System, Premise: c.Premise, Clock: c.Clock,
		MyRole: role, OwnerID: c.OwnerID,
		CreatedAt: c.CreatedAt.Format(http.TimeFormat), UpdatedAt: c.UpdatedAt.Format(http.TimeFormat),
	}
}

type memberView struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	CharacterID string `json:"character_id,omitempty"`
	JoinedAt    string `json:"joined_at"`
}

type campaignEntityView struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Summary   string         `json:"summary"`
	Status    string         `json:"status"`
	Payload   map[string]any `json:"payload,omitempty"` // DM reads only
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

func toCampaignEntityView(e *campaign.Entity, keepPayload bool) campaignEntityView {
	v := campaignEntityView{
		ID: e.ID, Kind: e.Kind, Name: e.Name, Summary: e.Summary, Status: e.Status,
		CreatedAt: e.CreatedAt.Format(http.TimeFormat), UpdatedAt: e.UpdatedAt.Format(http.TimeFormat),
	}
	if keepPayload && len(e.Payload) > 0 {
		v.Payload = e.Payload
	}
	return v
}

type entityNameView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`
}

type factView struct {
	ID            string `json:"id"`
	Subject       string `json:"subject"`
	Predicate     string `json:"predicate"`
	ObjectEntity  string `json:"object_entity,omitempty"`
	ObjectLiteral string `json:"object_literal,omitempty"`
	Statement     string `json:"statement"`
	Confidence    string `json:"confidence"`
	Visibility    string `json:"visibility"`
	CreatedBy     string `json:"created_by"`
	SupersededBy  string `json:"superseded_by,omitempty"`
	CreatedAt     string `json:"created_at"`
}

func toFactView(f *campaign.Fact) factView {
	return factView{
		ID: f.ID, Subject: f.SubjectEntity, Predicate: f.Predicate,
		ObjectEntity: f.ObjectEntity, ObjectLiteral: f.ObjectLiteral,
		Statement: f.Statement, Confidence: f.Confidence, Visibility: f.Visibility,
		CreatedBy: f.CreatedBy, SupersededBy: f.SupersededBy,
		CreatedAt: f.CreatedAt.Format(http.TimeFormat),
	}
}

type provenanceView struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id,omitempty"`
	SourceID  string `json:"source_id,omitempty"`
	SpanStart int64  `json:"span_start,omitempty"`
	SpanEnd   int64  `json:"span_end,omitempty"`
	Quote     string `json:"quote"`
	Method    string `json:"method"`
	CreatedAt string `json:"created_at"`
}

func toProvenanceView(p *campaign.Provenance) provenanceView {
	return provenanceView{
		ID: p.ID, SessionID: p.SessionID, SourceID: p.SourceID,
		SpanStart: p.SpanStart, SpanEnd: p.SpanEnd, Quote: p.Quote, Method: p.Method,
		CreatedAt: p.CreatedAt.Format(http.TimeFormat),
	}
}

type awarenessView struct {
	Knower     string  `json:"knower"`
	FactID     string  `json:"fact_id"`
	Stance     string  `json:"stance"`
	Confidence float64 `json:"confidence"`
	SinceEvent string  `json:"since_event,omitempty"`
	Discovery  string  `json:"discovery_id,omitempty"`
	UpdatedAt  string  `json:"updated_at"`
}

func toAwarenessView(a *knowledge.Awareness) awarenessView {
	return awarenessView{
		Knower: a.Knower, FactID: a.FactID, Stance: a.Stance, Confidence: a.Confidence,
		SinceEvent: a.SinceEvent, Discovery: a.DiscoveryID,
		UpdatedAt: a.UpdatedAt.Format(http.TimeFormat),
	}
}

type eventParticipantView struct {
	EntityID string `json:"entity_id"`
	Role     string `json:"role"`
}

type eventLinkView struct {
	From string `json:"from"`
	To   string `json:"to"`
	Link string `json:"link"`
}

type eventView struct {
	ID           string                 `json:"id"`
	Summary      string                 `json:"summary"`
	ClockAt      *int64                 `json:"clock_at,omitempty"`
	RealOrdinal  int64                  `json:"real_ordinal"`
	Location     string                 `json:"location_entity,omitempty"`
	SessionID    string                 `json:"session_id,omitempty"`
	Participants []eventParticipantView `json:"participants"`
	Links        []eventLinkView        `json:"links,omitempty"` // DM reads only
	CreatedAt    string                 `json:"created_at"`
}

func toEventView(e *campaign.Event, keepLinks bool) eventView {
	v := eventView{
		ID: e.ID, Summary: e.Summary, ClockAt: e.ClockAt, RealOrdinal: e.RealOrdinal,
		Location: e.LocationEntity, SessionID: e.SessionID,
		Participants: make([]eventParticipantView, 0, len(e.Participants)),
		CreatedAt:    e.CreatedAt.Format(http.TimeFormat),
	}
	for _, p := range e.Participants {
		v.Participants = append(v.Participants, eventParticipantView{EntityID: p.EntityID, Role: p.Role})
	}
	if keepLinks {
		for _, l := range e.Links {
			v.Links = append(v.Links, eventLinkView{From: l.FromEvent, To: l.ToEvent, Link: l.Link})
		}
	}
	return v
}

type relationshipView struct {
	ID              string `json:"id"`
	From            string `json:"from"`
	RelType         string `json:"rel_type"`
	To              string `json:"to"`
	Strength        int64  `json:"strength"`
	JustifiedByFact string `json:"justified_by_fact,omitempty"`
	SinceEvent      string `json:"since_event,omitempty"`
	CreatedAt       string `json:"created_at"`
}

func toRelationshipView(r *campaign.Relationship) relationshipView {
	return relationshipView{
		ID: r.ID, From: r.FromEntity, RelType: r.RelType, To: r.ToEntity, Strength: r.Strength,
		JustifiedByFact: r.JustifiedByFact, SinceEvent: r.SinceEvent,
		CreatedAt: r.CreatedAt.Format(http.TimeFormat),
	}
}

type questView struct {
	ID           string                `json:"id"`
	Name         string                `json:"name"`
	Machine      campaign.StateMachine `json:"state_machine"`
	CurrentState string                `json:"current_state"`
	CreatedAt    string                `json:"created_at"`
	UpdatedAt    string                `json:"updated_at"`
}

func toQuestView(q *campaign.Quest) questView {
	return questView{
		ID: q.ID, Name: q.Name, Machine: q.Machine, CurrentState: q.CurrentState,
		CreatedAt: q.CreatedAt.Format(http.TimeFormat), UpdatedAt: q.UpdatedAt.Format(http.TimeFormat),
	}
}

type questTransitionView struct {
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	EventID   string `json:"event_id,omitempty"`
	CreatedAt string `json:"created_at"`
}

// toCampaignInviteView renders a bound invite for the DM's manager; it is the
// keeper's inviteView with the campaign fields filled (the same shape, so one
// component can render both lists).
func toCampaignInviteView(r *http.Request, inv auth.Invite) inviteView {
	return toInviteView(r, inv)
}

/* ---------- campaigns ---------- */

func (s *Server) handleListCampaigns(w http.ResponseWriter, r *http.Request) {
	if !s.campaignsEnabled(w) {
		return
	}
	list, err := s.campaigns.ListCampaigns(r.Context(), userID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]campaignView, 0, len(list))
	for i := range list {
		role := ""
		if list[i].OwnerID == userID(r) {
			role = campaign.RoleDM
		} else if memberRole, ok, err := s.campaigns.Role(r.Context(), list[i].ID, userID(r)); err == nil && ok {
			role = memberRole
		}
		c := list[i]
		views = append(views, campaignView{
			ID: c.ID, Name: c.Name, System: c.System, Premise: c.Premise, Clock: c.Clock,
			MyRole: role, OwnerID: c.OwnerID,
			CreatedAt: c.CreatedAt.Format(http.TimeFormat), UpdatedAt: c.UpdatedAt.Format(http.TimeFormat),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaigns": views})
}

func (s *Server) handleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.campaignsEnabled(w) {
		return
	}
	var req struct {
		Name    string `json:"name"`
		System  string `json:"system"`
		Premise string `json:"premise"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	c, err := s.campaigns.CreateCampaign(r.Context(), userID(r), req.Name, req.System, req.Premise)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"campaign": campaignView{
		ID: c.ID, Name: c.Name, System: c.System, Premise: c.Premise, Clock: c.Clock,
		MyRole: campaign.RoleDM, OwnerID: c.OwnerID,
		CreatedAt: c.CreatedAt.Format(http.TimeFormat), UpdatedAt: c.UpdatedAt.Format(http.TimeFormat),
	}})
}

func (s *Server) handleGetCampaign(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaign": a.toCampaignView()})
}

func (s *Server) handleUpdateCampaign(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	// Settings and deletion are owner-shaped actions; the DM role that grants
	// them here is the owner's own row, or the keeper standing in for it.
	if !a.isDM() || (a.campaign.OwnerID != userID(r) && !a.keeper) {
		writeError(w, http.StatusForbidden, fmt.Errorf("only the campaign's owner may change its settings"))
		return
	}
	var req struct {
		Name    *string `json:"name"`
		System  *string `json:"system"`
		Premise *string `json:"premise"`
		Clock   *int64  `json:"clock"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	c, err := s.campaigns.UpdateCampaign(r.Context(), a.campaign.OwnerID, a.campaign.ID,
		req.Name, req.System, req.Premise, req.Clock, nil)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	a.campaign = c
	writeJSON(w, http.StatusOK, map[string]any{"campaign": a.toCampaignView()})
}

func (s *Server) handleDeleteCampaign(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.isDM() || (a.campaign.OwnerID != userID(r) && !a.keeper) {
		writeError(w, http.StatusForbidden, fmt.Errorf("only the campaign's owner may delete it"))
		return
	}
	if err := s.campaigns.DeleteCampaign(r.Context(), a.campaign.OwnerID, a.campaign.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- joining: the campaign invite redeem paths ---------- */

// handleJoinCampaign redeems a campaign invite from an account that already
// exists. The signed-out friend follows the same link to /api/auth/register,
// which writes the account and the membership in one transaction.
func (s *Server) handleJoinCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.campaignsEnabled(w) {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	campaignID, role, err := s.users.JoinWithInvite(r.Context(), req.Code, userID(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.campaigns.AddMember(r.Context(), campaignID, userID(r), role, ""); err != nil {
		if errors.Is(err, campaign.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, fmt.Errorf("you are already a member of that campaign"))
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"joined": campaignID, "role": role})
}

/* ---------- members ---------- */

func (s *Server) handleCampaignMembers(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	members, err := s.campaigns.Members(r.Context(), a.campaign.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.UserID)
	}
	names, err := s.users.Usernames(r.Context(), ids)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]memberView, 0, len(members))
	for _, m := range members {
		views = append(views, memberView{
			UserID: m.UserID, Username: names[m.UserID], Role: m.Role,
			CharacterID: m.CharacterID, JoinedAt: m.JoinedAt.Format(http.TimeFormat),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": views})
}

func (s *Server) handleUpdateCampaignMember(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	uid := r.PathValue("uid")
	var req struct {
		Role        *string `json:"role"`
		CharacterID *string `json:"character_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	ctx := r.Context()
	if req.Role != nil {
		if err := s.campaigns.SetMemberRole(ctx, a.campaign.ID, uid, *req.Role); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if req.CharacterID != nil {
		if err := s.campaigns.SetMemberCharacter(ctx, a.campaign.ID, uid, *req.CharacterID); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	members, err := s.campaigns.Members(ctx, a.campaign.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, m := range members {
		if m.UserID == uid {
			writeJSON(w, http.StatusOK, map[string]any{"member": memberView{
				UserID: m.UserID, Role: m.Role, CharacterID: m.CharacterID,
				JoinedAt: m.JoinedAt.Format(http.TimeFormat),
			}})
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("member %s", uid))
}

func (s *Server) handleRemoveCampaignMember(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if err := s.campaigns.RemoveMember(r.Context(), a.campaign.ID, r.PathValue("uid")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- campaign invites ---------- */

func (s *Server) handleListCampaignInvites(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	invites, err := s.users.ListCampaignInvites(r.Context(), a.campaign.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	views := make([]inviteView, 0, len(invites))
	for _, inv := range invites {
		views = append(views, toCampaignInviteView(r, inv))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": views})
}

func (s *Server) handleCreateCampaignInvite(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Note string `json:"note"`
		Role string `json:"role"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // both fields optional
	if req.Role == "" {
		req.Role = campaign.RolePlayer
	}
	inv, err := s.users.CreateCampaignInvite(r.Context(), userID(r), req.Note, a.campaign.ID, req.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"invite": toCampaignInviteView(r, *inv)})
}

func (s *Server) handleRevokeCampaignInvite(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	// Scoped through the campaign's own invite list so a DM can only revoke
	// what they minted, never the keeper's account invites.
	invites, err := s.users.ListCampaignInvites(r.Context(), a.campaign.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, inv := range invites {
		if inv.ID == r.PathValue("iid") {
			if err := s.users.RevokeInvite(r.Context(), inv.ID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("invite %s", r.PathValue("iid")))
}

/* ---------- entities ---------- */

func (s *Server) handleCampaignEntities(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	ctx := r.Context()
	var entities []campaign.Entity
	if a.isDM() {
		list, err := s.campaigns.ListEntities(ctx, campaign.ScopeDM, a.campaign.ID, kind)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		entities = list
	} else {
		list, err := a.view.Entities(ctx, a.campaign.ID, kind)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		entities = list
	}
	views := make([]campaignEntityView, 0, len(entities))
	for i := range entities {
		views = append(views, toCampaignEntityView(&entities[i], a.isDM()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"entities": views})
}

func (s *Server) handleCreateCampaignEntity(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Kind    string         `json:"kind"`
		Name    string         `json:"name"`
		Summary string         `json:"summary"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	e, err := s.campaigns.CreateEntity(r.Context(), a.campaign.ID, req.Kind, req.Name, req.Summary, req.Payload)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"entity": toCampaignEntityView(e, true)})
}

// entityDetail is the entity browser's detail payload: the entity itself plus
// everything attached to it the caller's scope may see — names, facts,
// relationships and timeline appearances. Payloads, provenance and event
// links are DM structure and appear only at the DM scope.
type entityDetail struct {
	campaignEntityView
	Names         []entityNameView   `json:"names"`
	Facts         []factView         `json:"facts"`
	Relationships []relationshipView `json:"relationships"`
	Events        []eventView        `json:"events"`
}

func (s *Server) handleCampaignEntity(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	eid := r.PathValue("eid")
	ctx := r.Context()
	var detail entityDetail
	if a.isDM() {
		e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		detail.campaignEntityView = toCampaignEntityView(e, true)
		names, err := s.campaigns.EntityNames(ctx, campaign.ScopeDM, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for _, n := range names {
			detail.Names = append(detail.Names, entityNameView{
				ID: n.ID, Name: n.Name, Kind: n.Kind, CreatedAt: n.CreatedAt.Format(http.TimeFormat),
			})
		}
		subject, err := s.campaigns.ListFacts(ctx, campaign.ScopeDM, a.campaign.ID, campaign.FactFilter{SubjectEntity: eid})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		// campaign.FactFilter has no object filter; the knowledge store's
		// scoped read does, and serves the DM scope just as wide.
		object, err := s.knowledge.Facts(ctx, campaign.ScopeDM, a.campaign.ID, knowledge.FactFilter{ObjectEntity: eid})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		seen := map[string]bool{}
		for i := range subject {
			if !seen[subject[i].ID] {
				seen[subject[i].ID] = true
				detail.Facts = append(detail.Facts, toFactView(&subject[i]))
			}
		}
		for i := range object {
			if !seen[object[i].ID] {
				seen[object[i].ID] = true
				detail.Facts = append(detail.Facts, toFactView(&object[i]))
			}
		}
		rels, err := s.campaigns.RelationshipsOf(ctx, campaign.ScopeDM, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for i := range rels {
			detail.Relationships = append(detail.Relationships, toRelationshipView(&rels[i]))
		}
		events, err := s.campaigns.ListEvents(ctx, campaign.ScopeDM, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for i := range events {
			if eventTouches(&events[i], eid) {
				detail.Events = append(detail.Events, toEventView(&events[i], false))
			}
		}
	} else {
		e, err := a.view.Entity(ctx, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		detail.campaignEntityView = toCampaignEntityView(e, false)
		subject, err := a.view.Facts(ctx, a.campaign.ID, knowledge.FactFilter{SubjectEntity: eid})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		object, err := a.view.Facts(ctx, a.campaign.ID, knowledge.FactFilter{ObjectEntity: eid})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		seen := map[string]bool{}
		for i := range subject {
			if !seen[subject[i].ID] {
				seen[subject[i].ID] = true
				detail.Facts = append(detail.Facts, toFactView(&subject[i]))
			}
		}
		for i := range object {
			if !seen[object[i].ID] {
				seen[object[i].ID] = true
				detail.Facts = append(detail.Facts, toFactView(&object[i]))
			}
		}
		rels, err := a.view.Relationships(ctx, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for i := range rels {
			if rels[i].FromEntity == eid || rels[i].ToEntity == eid {
				detail.Relationships = append(detail.Relationships, toRelationshipView(&rels[i]))
			}
		}
		events, err := a.view.Timeline(ctx, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for i := range events {
			if eventTouches(&events[i], eid) {
				detail.Events = append(detail.Events, toEventView(&events[i], false))
			}
		}
	}
	if detail.Names == nil {
		detail.Names = []entityNameView{}
	}
	if detail.Facts == nil {
		detail.Facts = []factView{}
	}
	if detail.Relationships == nil {
		detail.Relationships = []relationshipView{}
	}
	if detail.Events == nil {
		detail.Events = []eventView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entity": detail})
}

// eventTouches reports whether the entity took part in or hosted the event.
func eventTouches(e *campaign.Event, eid string) bool {
	if e.LocationEntity == eid {
		return true
	}
	for _, p := range e.Participants {
		if p.EntityID == eid {
			return true
		}
	}
	return false
}

func (s *Server) handleUpdateCampaignEntity(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Name    *string        `json:"name"`
		Summary *string        `json:"summary"`
		Status  *string        `json:"status"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	e, err := s.campaigns.UpdateEntity(r.Context(), a.campaign.ID, r.PathValue("eid"),
		req.Name, req.Summary, req.Status, req.Payload)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entity": toCampaignEntityView(e, true)})
}

func (s *Server) handleDeleteCampaignEntity(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if err := s.campaigns.DeleteEntity(r.Context(), a.campaign.ID, r.PathValue("eid")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAddEntityName(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if req.Kind == "" {
		req.Kind = campaign.NameAlias
	}
	n, err := s.campaigns.AddEntityName(r.Context(), a.campaign.ID, r.PathValue("eid"), req.Name, req.Kind)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": entityNameView{
		ID: n.ID, Name: n.Name, Kind: n.Kind, CreatedAt: n.CreatedAt.Format(http.TimeFormat),
	}})
}

/* ---------- facts ---------- */

func (s *Server) handleCampaignFacts(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	q := r.URL.Query()
	subject := q.Get("subject")
	ctx := r.Context()
	var facts []campaign.Fact
	if a.isDM() {
		filter := campaign.FactFilter{
			SubjectEntity: subject,
			Predicate:     q.Get("predicate"),
			Confidence:    q.Get("confidence"),
			Visibility:    q.Get("visibility"),
			NotSuperseded: q.Get("superseded") != "1", // retconned history on request
		}
		list, err := s.campaigns.ListFacts(ctx, campaign.ScopeDM, a.campaign.ID, filter)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		facts = list
	} else {
		list, err := a.view.Facts(ctx, a.campaign.ID, knowledge.FactFilter{
			SubjectEntity: subject,
			Predicate:     q.Get("predicate"),
		})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		facts = list
	}
	views := make([]factView, 0, len(facts))
	for i := range facts {
		views = append(views, toFactView(&facts[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"facts": views})
}

// factRequest is the writable half of a fact. Object is an entity or a
// literal, never both — the store enforces it, the editor reflects it.
// Provenance method is dm_authored: everything written through the API is a
// human decision, which lands as canon or derived (never proposed — that is
// the machine staging path, and it does not exist here).
type factRequest struct {
	Subject       string `json:"subject"`
	Predicate     string `json:"predicate"`
	ObjectEntity  string `json:"object_entity"`
	ObjectLiteral string `json:"object_literal"`
	Statement     string `json:"statement"`
	Confidence    string `json:"confidence"`
	Visibility    string `json:"visibility"`
}

func (req *factRequest) defaults() {
	if req.Confidence == "" {
		req.Confidence = campaign.ConfidenceCanon
	}
	if req.Visibility == "" {
		req.Visibility = campaign.VisibilityPublic
	}
}

func (s *Server) createFactFromRequest(w http.ResponseWriter, r *http.Request, a *campAccess, req factRequest) (*campaign.Fact, bool) {
	req.defaults()
	f, err := s.campaigns.CreateFact(r.Context(), a.campaign.ID, req.Subject, req.Predicate,
		req.ObjectEntity, req.ObjectLiteral, req.Statement, req.Confidence, req.Visibility,
		userID(r), []campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored}})
	if err != nil {
		writeStoreError(w, err)
		return nil, false
	}
	return f, true
}

func (s *Server) handleCreateCampaignFact(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req factRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	f, ok := s.createFactFromRequest(w, r, a, req)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"fact": toFactView(f)})
}

// factDetail is one fact plus its audit trail: where it came from (DM only)
// and who holds it (DM only). Those are the "why do we believe this?" answers.
type factDetail struct {
	factView
	Provenance []provenanceView `json:"provenance,omitempty"`
	Awareness  []awarenessView  `json:"awareness,omitempty"`
}

func (s *Server) handleCampaignFact(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	fid := r.PathValue("fid")
	ctx := r.Context()
	var detail factDetail
	if a.isDM() {
		f, err := s.campaigns.GetFact(ctx, campaign.ScopeDM, a.campaign.ID, fid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		detail.factView = toFactView(f)
		prov, err := s.campaigns.FactProvenance(ctx, campaign.ScopeDM, a.campaign.ID, fid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for i := range prov {
			detail.Provenance = append(detail.Provenance, toProvenanceView(&prov[i]))
		}
		aware, err := s.knowledge.Awareness(ctx, campaign.ScopeDM, a.campaign.ID, "", fid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for i := range aware {
			detail.Awareness = append(detail.Awareness, toAwarenessView(&aware[i]))
		}
	} else {
		f, err := a.view.Fact(ctx, a.campaign.ID, fid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		detail.factView = toFactView(f)
	}
	writeJSON(w, http.StatusOK, map[string]any{"fact": detail})
}

func (s *Server) handleSupersedeFact(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req factRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	f, ok := s.createFactFromRequest(w, r, a, req)
	if !ok {
		return
	}
	if err := s.campaigns.SupersedeFact(r.Context(), a.campaign.ID, r.PathValue("fid"), f.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"fact": toFactView(f)})
}

/* ---------- events + timeline ---------- */

func (s *Server) handleCampaignTimeline(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	ctx := r.Context()
	var events []campaign.Event
	if a.isDM() {
		list, err := s.campaigns.ListEvents(ctx, campaign.ScopeDM, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		events = list
	} else {
		list, err := a.view.Timeline(ctx, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		events = list
	}
	views := make([]eventView, 0, len(events))
	for i := range events {
		views = append(views, toEventView(&events[i], a.isDM()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": views})
}

func (s *Server) handleCreateCampaignEvent(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Summary      string `json:"summary"`
		ClockAt      *int64 `json:"clock_at"`
		SessionID    string `json:"session_id"`
		Location     string `json:"location_entity"`
		Participants []struct {
			EntityID string `json:"entity_id"`
			Role     string `json:"role"`
		} `json:"participants"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	e, err := s.campaigns.CreateEvent(r.Context(), a.campaign.ID, req.SessionID, req.Summary, req.ClockAt, req.Location)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	for _, p := range req.Participants {
		if err := s.campaigns.AddParticipant(r.Context(), a.campaign.ID, e.ID, p.EntityID, p.Role); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if e2, err := s.campaigns.GetEvent(r.Context(), campaign.ScopeDM, a.campaign.ID, e.ID); err == nil {
		e = e2
	}
	writeJSON(w, http.StatusCreated, map[string]any{"event": toEventView(e, true)})
}

func (s *Server) handleAddEventParticipant(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		EntityID string `json:"entity_id"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if err := s.campaigns.AddParticipant(r.Context(), a.campaign.ID, r.PathValue("eid"), req.EntityID, req.Role); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLinkCampaignEvents(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		To   string `json:"to"`
		Link string `json:"link"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if err := s.campaigns.LinkEvents(r.Context(), a.campaign.ID, r.PathValue("eid"), req.To, req.Link); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- relationships ---------- */

func (s *Server) handleCampaignRelationships(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	ctx := r.Context()
	var rels []campaign.Relationship
	if a.isDM() {
		var err error
		if eid := r.URL.Query().Get("entity"); eid != "" {
			rels, err = s.campaigns.RelationshipsOf(ctx, campaign.ScopeDM, a.campaign.ID, eid)
		} else {
			rels, err = s.campaigns.ListRelationships(ctx, campaign.ScopeDM, a.campaign.ID)
		}
		if err != nil {
			writeStoreError(w, err)
			return
		}
	} else {
		list, err := a.view.Relationships(ctx, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if eid := r.URL.Query().Get("entity"); eid != "" {
			for i := range list {
				if list[i].FromEntity == eid || list[i].ToEntity == eid {
					rels = append(rels, list[i])
				}
			}
		} else {
			rels = list
		}
	}
	views := make([]relationshipView, 0, len(rels))
	for i := range rels {
		views = append(views, toRelationshipView(&rels[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"relationships": views})
}

func (s *Server) handleCreateCampaignRelationship(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		From            string `json:"from"`
		RelType         string `json:"rel_type"`
		To              string `json:"to"`
		Strength        int64  `json:"strength"`
		JustifiedByFact string `json:"justified_by_fact"`
		SinceEvent      string `json:"since_event"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	rel, err := s.campaigns.CreateRelationship(r.Context(), a.campaign.ID, req.From, req.RelType, req.To,
		req.Strength, req.JustifiedByFact, req.SinceEvent)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"relationship": toRelationshipView(rel)})
}

func (s *Server) handleDeleteCampaignRelationship(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if err := s.campaigns.DeleteRelationship(r.Context(), a.campaign.ID, r.PathValue("rid")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- awareness ---------- */

// Awareness read and manual set are DM surfaces: awareness is the epistemic
// ledger itself, and a player reading who-knows-what would be a seat at the
// DM screen. The player portal (Stage 6) gets Discoveries and Summaries, not
// raw awareness rows.
func (s *Server) handleCampaignAwareness(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	aware, err := s.knowledge.Awareness(r.Context(), campaign.ScopeDM, a.campaign.ID,
		r.URL.Query().Get("knower"), r.URL.Query().Get("fact"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]awarenessView, 0, len(aware))
	for i := range aware {
		views = append(views, toAwarenessView(&aware[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"awareness": views})
}

func (s *Server) handleSetCampaignAwareness(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Knower     string  `json:"knower"`
		FactID     string  `json:"fact_id"`
		Stance     string  `json:"stance"`
		Confidence float64 `json:"confidence"`
		SinceEvent string  `json:"since_event"`
		Discovery  string  `json:"discovery_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	// A manual set is the DM asserting a fact of the table: full confidence
	// unless the DM says otherwise.
	confidence := req.Confidence
	if confidence == 0 {
		confidence = 1
	}
	aw, err := s.knowledge.SetAwareness(r.Context(), a.campaign.ID, req.Knower, req.FactID, req.Stance,
		confidence, req.SinceEvent, req.Discovery)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"awareness": toAwarenessView(aw)})
}

/* ---------- quests ---------- */

func (s *Server) handleCampaignQuests(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	quests, err := s.campaigns.ListQuests(r.Context(), campaign.ScopeDM, a.campaign.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]questView, 0, len(quests))
	for i := range quests {
		views = append(views, toQuestView(&quests[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"quests": views})
}

func (s *Server) handleCreateCampaignQuest(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Name    string                `json:"name"`
		Machine campaign.StateMachine `json:"state_machine"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	if err := req.Machine.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	q, err := s.campaigns.CreateQuest(r.Context(), a.campaign.ID, req.Name, req.Machine)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"quest": toQuestView(q)})
}

func (s *Server) handleCampaignQuest(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	q, err := s.campaigns.GetQuest(r.Context(), campaign.ScopeDM, a.campaign.ID, r.PathValue("qid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	transitions, err := s.campaigns.QuestTransitions(r.Context(), campaign.ScopeDM, a.campaign.ID, q.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]questTransitionView, 0, len(transitions))
	for _, t := range transitions {
		views = append(views, questTransitionView{
			FromState: t.FromState, ToState: t.ToState, EventID: t.EventID,
			CreatedAt: t.CreatedAt.Format(http.TimeFormat),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"quest": toQuestView(q), "transitions": views})
}

func (s *Server) handleQuestTransition(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		To      string `json:"to"`
		EventID string `json:"event_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	q, err := s.campaigns.TransitionQuest(r.Context(), a.campaign.ID, r.PathValue("qid"), req.To, req.EventID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quest": toQuestView(q)})
}

/* ---------- graph view v0 + prose search ---------- */

// graphNode is one entity in the neighbourhood; graphEdge is one relationship.
// The layout is the client's; the scope enforcement is entirely here — the
// edge list is read through the same scoped path as the browser, so a
// player's graph cannot contain an edge or node their perspective cannot see.
type graphNode struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Hops    int    `json:"hops"` // distance from the center, 0 for the center
}

type graphEdge struct {
	From    string `json:"from"`
	RelType string `json:"rel_type"`
	To      string `json:"to"`
}

// maxGraphNodes caps graph view v0. One or two hops around a hub NPC can
// reach a whole campaign; past this the view stops being a neighbourhood and
// becomes a rendering problem (whole-campaign graphing is explicitly out).
const maxGraphNodes = 80

func (s *Server) handleCampaignGraph(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	center := strings.TrimSpace(r.URL.Query().Get("center"))
	if center == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("center (entity id) is required"))
		return
	}
	hops := 1
	if v := r.URL.Query().Get("hops"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && (n == 1 || n == 2) {
			hops = n
		}
	}
	ctx := r.Context()

	// The center must itself be visible at this scope before anything else.
	if a.isDM() {
		if _, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, a.campaign.ID, center); err != nil {
			writeStoreError(w, err)
			return
		}
	} else {
		if _, err := a.view.Entity(ctx, a.campaign.ID, center); err != nil {
			writeStoreError(w, err)
			return
		}
	}

	var rels []campaign.Relationship
	if a.isDM() {
		list, err := s.campaigns.ListRelationships(ctx, campaign.ScopeDM, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		rels = list
	} else {
		list, err := a.view.Relationships(ctx, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		rels = list
	}

	// Breadth-first over the scoped edge list, both directions.
	adj := map[string][]campaign.Relationship{}
	for _, rel := range rels {
		adj[rel.FromEntity] = append(adj[rel.FromEntity], rel)
		adj[rel.ToEntity] = append(adj[rel.ToEntity], rel)
	}
	dist := map[string]int{center: 0}
	frontier := []string{center}
	for d := 0; d < hops && len(frontier) > 0; d++ {
		var next []string
		for _, id := range frontier {
			for _, rel := range adj[id] {
				for _, end := range []string{rel.FromEntity, rel.ToEntity} {
					if _, seen := dist[end]; !seen {
						dist[end] = d + 1
						next = append(next, end)
					}
				}
			}
		}
		frontier = next
	}
	if len(dist) > maxGraphNodes {
		// Keep the center and the nearest nodes; a truncated ring beats a
		// wedged render, and the cap is visible in the response.
		type kv struct {
			id string
			d  int
		}
		all := make([]kv, 0, len(dist))
		for id, d := range dist {
			all = append(all, kv{id, d})
		}
		for i := 1; i < len(all); i++ {
			for j := i; j > 0 && all[j].d < all[j-1].d; j-- {
				all[j], all[j-1] = all[j-1], all[j]
			}
		}
		dist = map[string]int{}
		for _, pair := range all[:maxGraphNodes] {
			dist[pair.id] = pair.d
		}
	}

	// Nodes: fetch each visible entity through the scoped read path.
	nodes := make([]graphNode, 0, len(dist))
	for id, d := range dist {
		if a.isDM() {
			e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, a.campaign.ID, id)
			if err != nil {
				continue // soft-deleted mid-flight; skip rather than fail the view
			}
			nodes = append(nodes, graphNode{ID: e.ID, Kind: e.Kind, Name: e.Name, Summary: e.Summary, Hops: d})
		} else {
			e, err := a.view.Entity(ctx, a.campaign.ID, id)
			if err != nil {
				continue
			}
			nodes = append(nodes, graphNode{ID: e.ID, Kind: e.Kind, Name: e.Name, Summary: e.Summary, Hops: d})
		}
	}
	// Edges between included nodes only.
	included := map[string]bool{}
	for _, n := range nodes {
		included[n.ID] = true
	}
	edges := make([]graphEdge, 0)
	seen := map[string]bool{}
	for _, rel := range rels {
		if included[rel.FromEntity] && included[rel.ToEntity] && !seen[rel.ID] {
			seen[rel.ID] = true
			edges = append(edges, graphEdge{From: rel.FromEntity, RelType: rel.RelType, To: rel.ToEntity})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"center": center, "hops": hops, "nodes": nodes, "edges": edges})
}

// handleCampaignSearch is scoped prose search over the campaign — entity
// names and summaries, fact statements, event summaries. The authorization
// rides the same CTEs as the structured reads, in the same SQL statement as
// the MATCH: a secret fact's text is indexed, but a scope that cannot read
// the fact cannot surface its snippet.
func (s *Server) handleCampaignSearch(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"hits": []knowledge.ProseHit{}})
		return
	}
	var hits []knowledge.ProseHit
	var err error
	if a.isDM() {
		hits, err = s.knowledge.SearchProse(r.Context(), campaign.ScopeDM, a.campaign.ID, q, 20)
	} else {
		hits, err = a.view.SearchProse(r.Context(), a.campaign.ID, q, 20)
	}
	if err != nil {
		if errors.Is(err, knowledge.ErrEmptyQuery) {
			writeJSON(w, http.StatusOK, map[string]any{"hits": []knowledge.ProseHit{}})
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
}
