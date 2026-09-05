package server

// The resource ledger surface (MAD-419, stage 2 of MAD-417): the sheet's
// state as an append-only transaction log.
//
//	GET    /api/campaigns/{id}/characters/{eid}/resources                   balances (+ history)
//	POST   /api/campaigns/{id}/characters/{eid}/resources                   register a pool (DM)
//	PATCH  /api/campaigns/{id}/characters/{eid}/resources/{pid}             edit a registered pool (DM)
//	DELETE /api/campaigns/{id}/characters/{eid}/resources/{pid}             remove a registered pool (DM)
//	POST   /api/campaigns/{id}/characters/{eid}/resources/{pid}/transactions spend / regain / set
//	POST   /api/campaigns/{id}/rests                                        the live rest button (DM)
//	POST   /api/campaigns/{id}/rests/propose                                a rest through the review gate
//
// Permissions are the issue's one rule: players spend their own — a
// character-scoped player may read and move exactly their bound character's
// pools, and only spend and regain; the DM may adjust anyone, and the
// adjustment is a visible transaction, never a silent edit. A 'set' is a DM
// correction and refused to players on the same rule.
//
// The rest is the one write that moves the clock: the live button executes
// immediately (a DM pressing it is the confirmation), a model's proposal
// stages behind the review gate like every other machine proposal, and a
// long rest advances the campaign clock one day — the schedule, the faction
// plans and the sim all react to the new day exactly as they do to any
// other advance.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
)

// ledgerEnabled reports the resource surface's availability.
func (s *Server) ledgerEnabled(w http.ResponseWriter) bool {
	if s.ledgers == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the resource ledger is not configured on this install"))
		return false
	}
	return true
}

// WithLedger wires the resource ledger (MAD-419). Without it the resource
// endpoints answer 503. Wiring also re-derives every pc's sheet pools — the
// same boot-time rebuild contract the sheet projection holds, so a dropped
// or stale pool row never outlives a restart.
func (s *Server) WithLedger(store *ledger.Store) *Server {
	s.ledgers = store
	if store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = store.SyncAll(ctx)
	}
	return s
}

// requireOwnCharacter checks a non-DM caller's standing on one character:
// exactly the pc their membership row binds. It writes the response itself
// and returns false when the caller may not proceed.
func (s *Server) requireOwnCharacter(w http.ResponseWriter, a *campAccess, eid string) bool {
	if a.isDM() {
		return true
	}
	if a.view == nil || a.playerScope.Kind() != campaign.ScopeKindCharacter || a.playerScope.EntityID() != eid {
		writeError(w, http.StatusForbidden, fmt.Errorf("those resources belong to their character and the DM"))
		return false
	}
	return true
}

/* ---------- views ---------- */

type balanceView struct {
	Pool         poolView `json:"pool"`
	Current      int      `json:"current"`
	Spent        int      `json:"spent"`
	Transactions int      `json:"transactions"`
}

type poolView struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	Size        int    `json:"size"`
	Recovery    string `json:"recovery"`
	Granularity int    `json:"granularity,omitempty"`
	Source      string `json:"source"`
	Bounded     bool   `json:"bounded"`
}

func toPoolView(p ledger.Pool) poolView {
	return poolView{
		ID: p.ID, Kind: p.Kind, Name: p.Name, Label: p.Label, Size: p.Size,
		Recovery: p.Recovery, Granularity: p.Granularity, Source: p.Source,
		Bounded: p.Bounded(),
	}
}

func toBalanceView(b ledger.Balance) balanceView {
	return balanceView{
		Pool: toPoolView(b.Pool), Current: b.Current, Spent: b.Spent,
		Transactions: b.TxnCount,
	}
}

type txnView struct {
	ID        string `json:"id"`
	Pool      string `json:"pool"`
	Kind      string `json:"kind"`
	Amount    int    `json:"amount"`
	RestID    string `json:"rest_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	EventID   string `json:"session_event_id,omitempty"`
	Actor     string `json:"actor,omitempty"`
	Note      string `json:"note,omitempty"`
	Day       int64  `json:"day"`
	CreatedAt string `json:"created_at"`
}

func toTxnView(t ledger.TxnRow) txnView {
	return txnView{
		ID: t.ID, Pool: t.Pool, Kind: t.Kind, Amount: t.Amount, RestID: t.RestID,
		SessionID: t.SessionID, EventID: t.EventID, Actor: t.Actor, Note: t.Note,
		Day: t.Day, CreatedAt: t.CreatedAt.Format(http.TimeFormat),
	}
}

type plannedTxnView struct {
	Pool   string `json:"pool"`
	Kind   string `json:"kind"`
	Amount int    `json:"amount"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	Reason string `json:"reason"`
}

type restView struct {
	ID        string                      `json:"id"`
	Kind      string                      `json:"kind"`
	Source    string                      `json:"source"`
	Status    string                      `json:"status"`
	BatchID   string                      `json:"batch_id,omitempty"`
	Actor     string                      `json:"actor,omitempty"`
	SessionID string                      `json:"session_id,omitempty"`
	AdvanceID string                      `json:"advance_id,omitempty"`
	ClockFrom int64                       `json:"clock_from,omitempty"`
	ClockTo   int64                       `json:"clock_to,omitempty"`
	Note      string                      `json:"note,omitempty"`
	CreatedAt string                      `json:"created_at"`
	Plan      map[string][]plannedTxnView `json:"plan,omitempty"`
}

func toRestView(r *ledger.RestRow, plans map[string][]ledger.PlannedTxn) restView {
	v := restView{
		ID: r.ID, Kind: r.Kind, Source: r.Source, Status: r.Status, BatchID: r.BatchID,
		Actor: r.Actor, SessionID: r.SessionID, AdvanceID: r.AdvanceID,
		ClockFrom: r.ClockFrom, ClockTo: r.ClockTo, Note: r.Note,
		CreatedAt: r.CreatedAt.Format(http.TimeFormat),
	}
	if len(plans) > 0 {
		v.Plan = make(map[string][]plannedTxnView, len(plans))
		for eid, plan := range plans {
			row := make([]plannedTxnView, 0, len(plan))
			for _, t := range plan {
				row = append(row, plannedTxnView{Pool: t.Pool, Kind: t.Kind, Amount: t.Amount,
					From: t.From, To: t.To, Reason: t.Reason})
			}
			v.Plan[eid] = row
		}
	}
	return v
}

/* ---------- balances and history ---------- */

func (s *Server) handleCharacterResources(w http.ResponseWriter, r *http.Request) {
	if !s.ledgerEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	eid := r.PathValue("eid")
	if !s.requireOwnCharacter(w, a, eid) {
		return
	}
	ctx := r.Context()
	balances, err := s.ledgers.Balances(ctx, a.campaign.ID, eid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]balanceView, 0, len(balances))
	for _, b := range balances {
		views = append(views, toBalanceView(b))
	}
	out := map[string]any{"balances": views}
	if v := r.URL.Query().Get("history"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("history must be 1..500 transactions"))
			return
		}
		history, err := s.ledgers.History(ctx, a.campaign.ID, eid, n)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		hv := make([]txnView, 0, len(history))
		for _, t := range history {
			hv = append(hv, toTxnView(t))
		}
		out["history"] = hv
	}
	writeJSON(w, http.StatusOK, out)
}

/* ---------- transactions ---------- */

func (s *Server) handleResourceTransaction(w http.ResponseWriter, r *http.Request) {
	if !s.ledgerEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	eid := r.PathValue("eid")
	if !s.requireOwnCharacter(w, a, eid) {
		return
	}
	var req ledger.TxnInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	// The permission line: players spend and regain their own; only the DM
	// corrects to an absolute value.
	if !a.isDM() && req.Kind == ledger.TxnSet {
		writeError(w, http.StatusForbidden, fmt.Errorf("a set is the DM's correction; spend and regain are yours"))
		return
	}
	txn, balances, err := s.ledgers.Apply(r.Context(), a.campaign.ID, eid, r.PathValue("pid"), req, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]balanceView, 0, len(balances))
	for _, b := range balances {
		views = append(views, toBalanceView(b))
	}
	writeJSON(w, http.StatusCreated, map[string]any{"transaction": toTxnView(*txn), "balances": views})
}

/* ---------- pool registration ---------- */

func (s *Server) handleCreateResourcePool(w http.ResponseWriter, r *http.Request) {
	if !s.ledgerEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var p ledger.Pool
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	created, err := s.ledgers.CreatePool(r.Context(), a.campaign.ID, r.PathValue("eid"), p)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pool": toPoolView(*created)})
}

func (s *Server) handleUpdateResourcePool(w http.ResponseWriter, r *http.Request) {
	if !s.ledgerEnabled(w) {
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
		Label       *string `json:"label"`
		Size        *int    `json:"size"`
		Recovery    *string `json:"recovery"`
		Granularity *int    `json:"granularity"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	p, err := s.ledgers.UpdatePool(r.Context(), a.campaign.ID, r.PathValue("eid"), r.PathValue("pid"),
		req.Label, req.Size, req.Recovery, req.Granularity)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pool": toPoolView(*p)})
}

func (s *Server) handleDeleteResourcePool(w http.ResponseWriter, r *http.Request) {
	if !s.ledgerEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if err := s.ledgers.DeletePool(r.Context(), a.campaign.ID, r.PathValue("eid"), r.PathValue("pid")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

/* ---------- rests ---------- */

// restRequest is the shared body of the live button and the proposal.
type restRequest struct {
	Kind       string   `json:"kind"`
	Characters []string `json:"characters"`
	SessionID  string   `json:"session_id"`
	Note       string   `json:"note"`
}

func (s *Server) handleCampaignRest(w http.ResponseWriter, r *http.Request) {
	if !s.ledgerEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req restRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	rest, plans, err := s.ledgers.Rest(r.Context(), a.campaign.ID, req.Characters, req.Kind,
		req.SessionID, req.Note, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"rest": toRestView(rest, plans)})
}

func (s *Server) handleCampaignRestPropose(w http.ResponseWriter, r *http.Request) {
	if !s.ledgerEnabled(w) {
		return
	}
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req restRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	rest, batch, plans, err := s.ledgers.StageRest(r.Context(), a.campaign.ID, req.Characters, req.Kind,
		req.SessionID, req.Note, userID(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"rest":  toRestView(rest, plans),
		"batch": toBatchView(*batch, true),
	})
}
