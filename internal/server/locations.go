package server

// The location surface (MAD-370): the places listing, the scope-resolved
// dossier, and the place block editor.
//
// The dossier is a read, not a write. Present NPCs, child locations, items,
// secrets, events and sited quests are live graph rows — assembled by
// place.Dossier over a snapshot — and the only stored interior is the
// payload's place block (campaign.PlaceOf, under the "place" key). The DM's
// snapshot is campaign.LoadSnapshot; a player's is the scoped snapshot the
// knowledge layer builds, so the forbidden material is absent because the
// rows were never loaded, not because a handler blanked a field. The
// description lives in entities.summary, where the prose index reads it —
// never in the payload.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/place"
)

/* ---------- views ---------- */

// entityChip is one entity the dossier references: a read-only chip that
// links into the entity browser, so the sheet is visibly a view of the
// graph rather than a second place to type things.
type entityChip struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

func toEntityChips(es []campaign.Entity) []entityChip {
	out := make([]entityChip, 0, len(es))
	for i := range es {
		out = append(out, entityChip{ID: es[i].ID, Name: es[i].Name, Kind: es[i].Kind, Status: es[i].Status})
	}
	return out
}

// routeChipView is one route out of the location: the travel block MAD-365
// owns, read where it is there and rendered nowhere when it is not.
type routeChipView struct {
	To      string `json:"to"`
	Days    int64  `json:"days"`
	Terrain string `json:"terrain,omitempty"`
}

// questChipView is one quest whose site this place is.
type questChipView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
}

// locationListItem is one row of the places listing.
type locationListItem struct {
	campaignEntityView
	Scale    string `json:"scale,omitempty"` // the block's scale; empty until authored (DM reads only)
	Children int    `json:"children"`        // locations inside this one, by edge
}

// locationDossierView is the location page's whole payload. The DM shape
// carries the full place block (private truth included) and the routes; a
// player's carries the block's public half and no routes key material —
// the payload never crossed the scope line to be filtered here.
type locationDossierView struct {
	campaignEntityView
	Place    campaign.Place  `json:"place"`
	Present  []entityChip    `json:"present"`
	Children []entityChip    `json:"children"`
	Routes   []routeChipView `json:"routes,omitempty"` // DM reads only, by construction
	Items    []entityChip    `json:"items"`
	Secrets  []factView      `json:"secrets"` // DM reads only, by construction
	Events   []eventView     `json:"events"`
	Quests   []questChipView `json:"quests"`
	Rumours  []string        `json:"rumours"` // empty until MAD-374 lands
}

/* ---------- the listing ---------- */

// handleCampaignLocations lists the campaign's places at the caller's scope,
// each with the block's scale and a live child count read off the edges.
func (s *Server) handleCampaignLocations(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	ctx := r.Context()
	var (
		locations []campaign.Entity
		all       []campaign.Entity
		rels      []campaign.Relationship
	)
	if a.isDM() {
		list, err := s.campaigns.ListEntities(ctx, campaign.ScopeDM, a.campaign.ID, campaign.KindLocation)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		locations = list
		everything, err := s.campaigns.ListEntities(ctx, campaign.ScopeDM, a.campaign.ID, "")
		if err != nil {
			writeStoreError(w, err)
			return
		}
		all = everything
		edges, err := s.campaigns.ListRelationships(ctx, campaign.ScopeDM, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		rels = edges
	} else {
		list, err := a.view.Entities(ctx, a.campaign.ID, campaign.KindLocation)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		locations = list
		everything, err := a.view.Entities(ctx, a.campaign.ID, "")
		if err != nil {
			writeStoreError(w, err)
			return
		}
		all = everything
		edges, err := a.view.Relationships(ctx, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		rels = edges
	}

	// A child is a location placed inside another: the located_in edge
	// points at its parent, the contains edge leaves it. Counted live off
	// the edges — the listing never writes a row.
	isLocation := map[string]bool{}
	for i := range all {
		if all[i].Kind == campaign.KindLocation {
			isLocation[all[i].ID] = true
		}
	}
	children := map[string]int{}
	for _, rel := range rels {
		switch rel.RelType {
		case "located_in":
			if isLocation[rel.FromEntity] {
				children[rel.ToEntity]++
			}
		case "contains":
			if isLocation[rel.ToEntity] {
				children[rel.FromEntity]++
			}
		}
	}

	views := make([]locationListItem, 0, len(locations))
	for i := range locations {
		item := locationListItem{
			campaignEntityView: toCampaignEntityView(&locations[i], false),
			Children:           children[locations[i].ID],
		}
		if a.isDM() {
			item.Scale = campaign.PlaceOf(&locations[i]).Scale
		}
		views = append(views, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"locations": views})
}

/* ---------- the dossier ---------- */

// handleCampaignLocation serves the scope-resolved dossier: the entity with
// its place block, and everything the graph says about the place. The DM
// reads the whole block, the routes and the secrets; a player reads the
// block's public half and none of the rest — because their snapshot never
// loaded it.
func (s *Server) handleCampaignLocation(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	eid := r.PathValue("eid")
	ctx := r.Context()

	var entity *campaign.Entity
	if a.isDM() {
		e, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		entity = e
	} else {
		e, err := a.view.Entity(ctx, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		entity = e
	}
	if entity.Kind != campaign.KindLocation {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s is a %s, not a location", entity.Name, entity.Kind))
		return
	}

	var snap *campaign.Snapshot
	if a.isDM() {
		loaded, err := campaign.LoadSnapshot(ctx, campaign.ScopeDM, s.campaigns.DB(), a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		snap = loaded
	} else {
		loaded, err := a.view.PlaceSnapshot(ctx, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		snap = loaded
	}
	d, ok := place.Dossier(snap, eid)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("location %s", eid))
		return
	}

	cal := s.campaignCalendar(ctx, a.campaign.ID)
	view := locationDossierView{
		campaignEntityView: toCampaignEntityView(&d.Location, a.isDM()),
		Place:              d.Place,
		Present:            toEntityChips(d.Present),
		Children:           toEntityChips(d.Children),
		Items:              toEntityChips(d.Items),
		Rumours:            []string{},
	}
	for _, route := range d.Routes {
		view.Routes = append(view.Routes, routeChipView{To: route.To, Days: route.Days, Terrain: route.Terrain})
	}
	for i := range d.Secrets {
		view.Secrets = append(view.Secrets, toFactView(&d.Secrets[i]))
	}
	for i := range d.Events {
		view.Events = append(view.Events, toEventViewCal(&d.Events[i], false, cal))
	}
	for _, q := range d.Quests {
		view.Quests = append(view.Quests, questChipView{ID: q.ID, Name: q.Name, Summary: q.Summary, Status: q.Status})
	}
	if view.Secrets == nil {
		view.Secrets = []factView{}
	}
	if view.Events == nil {
		view.Events = []eventView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"location": view})
}

/* ---------- the place block editor ---------- */

// handlePutCampaignPlace replaces a location's place block. The body is the
// block itself; every other payload key — the travel block above all —
// survives untouched. The read-aloud description belongs in entities.summary
// (PATCH the entity), where the prose index reads it; a description written
// here would be invisible to campaign search.
func (s *Server) handlePutCampaignPlace(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	ctx := r.Context()
	eid := r.PathValue("eid")
	entity, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, a.campaign.ID, eid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if entity.Kind != campaign.KindLocation {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s is a %s, not a location", entity.Name, entity.Kind))
		return
	}
	var block campaign.Place
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&block); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return
	}
	updated, err := s.campaigns.UpdateEntity(ctx, a.campaign.ID, eid, nil, nil, nil, campaign.WithPlace(entity.Payload, block))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"place": campaign.PlaceOf(updated)})
}
