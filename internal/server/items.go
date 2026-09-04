package server

// The item designer's HTTP surface (MAD-383): the SRD item shelf the
// catalog serves, the design validation that compares and never computes,
// the homebrew shelf it saves onto, and the placement that stages a
// designed item into a campaign through the proposal batch. Nothing here
// needs the model.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/items"
)

// WithItems wires the SRD item catalog and the homebrew item store
// (MAD-383). nil disables both; the endpoints report unavailable.
func (s *Server) WithItems(catalog *items.Catalog, homebrew *items.HomebrewStore) *Server {
	s.itemCatalog = catalog
	s.itemHomebrew = homebrew
	return s
}

// itemsEnabled reports whether the item catalog is wired, writing the
// error response when it is not.
func (s *Server) itemsEnabled(w http.ResponseWriter) bool {
	if s.itemCatalog == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("the item catalog is not available"))
		return false
	}
	return true
}

// homebrewItemsEnabled reports whether the item designer's shelf is
// wired, writing the error response when it is not.
func (s *Server) homebrewItemsEnabled(w http.ResponseWriter) bool {
	if s.itemHomebrew == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("homebrew items are not enabled on this install"))
		return false
	}
	return true
}

// itemHomebrewOverlay loads the caller's homebrew items as a catalog
// overlay. With a campaign named, that campaign's designs ride along.
// Every failure degrades to no overlay — the catalog must never stop
// because the shelf is unreadable.
func (s *Server) itemHomebrewOverlay(r *http.Request, campaignID string) *items.Overlay {
	if s.itemHomebrew == nil {
		return nil
	}
	list, err := s.itemHomebrew.Overlay(r.Context(), userID(r), campaignID)
	if err != nil || len(list) == 0 {
		return nil
	}
	return items.NewOverlay(list)
}

// writeItemError maps the item stores' sentinels, the item surface's own
// rule rather than writeStoreError's campaign set.
func writeItemError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, items.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, items.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeStoreError(w, err)
	}
}

/* ---------- the SRD shelf ---------- */

// handleItems serves the SRD item shelf: a name search for the picker,
// or a filter browse (type, rarity, tag) when no query is given. The
// caller's homebrew leads either way, and an optional campaign scopes
// which designs ride along.
func (s *Server) handleItems(w http.ResponseWriter, r *http.Request) {
	if !s.itemsEnabled(w) {
		return
	}
	overlay := s.itemHomebrewOverlay(r, strings.TrimSpace(r.URL.Query().Get("campaign")))
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q != "" {
		writeJSON(w, http.StatusOK, map[string]any{"items": nonNilItems(s.itemCatalog.Search(q, 0, overlay))})
		return
	}
	f := items.Filter{
		Types:    splitCSV(r.URL.Query().Get("type")),
		Rarities: splitCSV(r.URL.Query().Get("rarity")),
		Tags:     splitCSV(r.URL.Query().Get("tag")),
		Terms:    splitCSV(r.URL.Query().Get("terms")),
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNilItems(s.itemCatalog.Filter(f, overlay))})
}

// nonNilItems keeps an empty shelf a JSON array rather than a null.
func nonNilItems(list []items.Item) []items.Item {
	if list == nil {
		return []items.Item{}
	}
	return list
}

func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

/* ---------- the designer's comparison ---------- */

// handleItemDesign validates one draft. A structurally broken design is
// rejected — 400, with the rules it broke named. A sound one is placed
// against the SRD distribution: the design's metrics, the corpus bands,
// the checkable claims, and the nearest real items. The response carries
// no field that computes or implies a rarity — that absence is the
// feature.
func (s *Server) handleItemDesign(w http.ResponseWriter, r *http.Request) {
	if !s.itemsEnabled(w) {
		return
	}
	var d items.Design
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&d); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	rep := items.Compare(d, s.itemCatalog.All())
	if len(rep.Problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"report": rep})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": rep})
}

/* ---------- the homebrew shelf ---------- */

// itemView is one homebrew item as the wire carries it: the full
// structured design, the rarity the DM asked for — echoed, never judged —
// and the derived tags. The requested rarity is the labelling; there is
// no computed half, by design.
func itemView(m *items.HomebrewItem) map[string]any {
	v := map[string]any{
		"id": m.ID, "name": m.Name, "slug": m.Slug,
		"design":           m.Design,
		"requested_rarity": m.RequestedRarity,
		"tags":             m.Tags,
		"notes":            m.Notes,
		"source":           m.Source,
		"homebrew":         true,
		"created_at":       m.CreatedAt.Format(http.TimeFormat),
		"updated_at":       m.UpdatedAt.Format(http.TimeFormat),
	}
	if m.CampaignID != "" {
		v["campaign_id"] = m.CampaignID
	}
	return v
}

func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request) {
	if !s.homebrewItemsEnabled(w) {
		return
	}
	list, err := s.itemHomebrew.List(r.Context(), userID(r), strings.TrimSpace(r.URL.Query().Get("campaign")))
	if err != nil {
		writeItemError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, itemView(&list[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleCreateItem(w http.ResponseWriter, r *http.Request) {
	if !s.homebrewItemsEnabled(w) {
		return
	}
	var req struct {
		Name       string        `json:"name"`
		CampaignID string        `json:"campaign_id"`
		Notes      string        `json:"notes"`
		Source     string        `json:"source"`
		Design     *items.Design `json:"design"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	// A campaign-scoped save is a write into that campaign's material:
	// the DM gate applies, exactly like every other campaign write.
	if req.CampaignID != "" {
		a := s.resolveCampaignAccess(w, r, req.CampaignID)
		if a == nil || !a.requireDM(w) {
			return
		}
	}
	m, err := s.itemHomebrew.Save(r.Context(), userID(r), "", items.HomebrewInput{
		Name: req.Name, CampaignID: req.CampaignID,
		Design: req.Design, Notes: req.Notes, Source: req.Source,
	})
	if err != nil {
		writeItemError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"item": itemView(m)})
}

func (s *Server) handleGetItem(w http.ResponseWriter, r *http.Request) {
	if !s.homebrewItemsEnabled(w) {
		return
	}
	m, err := s.itemHomebrew.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		writeItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": itemView(m)})
}

func (s *Server) handleDeleteItem(w http.ResponseWriter, r *http.Request) {
	if !s.homebrewItemsEnabled(w) {
		return
	}
	if err := s.itemHomebrew.Delete(r.Context(), userID(r), r.PathValue("id")); err != nil {
		writeItemError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

/* ---------- the placement ---------- */

// handleItemPlace stages the campaign placement as a proposal batch —
// one item entity behind the review gate. DM only.
func (s *Server) handleItemPlace(w http.ResponseWriter, r *http.Request) {
	if !s.canonEnabled(w) {
		return
	}
	if !s.homebrewItemsEnabled(w) {
		return
	}
	var req struct {
		CampaignID string `json:"campaign_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	a := s.resolveCampaignAccess(w, r, req.CampaignID)
	if a == nil || !a.requireDM(w) {
		return
	}
	m, err := s.itemHomebrew.Get(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		writeItemError(w, err)
		return
	}
	summary := itemEntitySummary(m)
	batch, err := s.canon.PlaceItem(r.Context(), canon.ItemPlaceInput{
		CampaignID: req.CampaignID, HomebrewID: m.ID, Name: m.Name,
		Summary: summary, Rarity: m.RequestedRarity, Notes: m.Notes,
		CreatedBy: userID(r),
	})
	if err != nil {
		if canon.IsOffline(err) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"item_id": m.ID,
		"batch":   toBatchView(*batch, true),
	})
}

// itemEntitySummary is the one line the placed item entity carries: the
// notes' first sentence when there are notes, else the rarity-and-type
// line.
func itemEntitySummary(m *items.HomebrewItem) string {
	if notes := strings.TrimSpace(m.Notes); notes != "" {
		if i := strings.IndexAny(notes, ".!?"); i > 0 {
			return notes[:i+1]
		}
		return notes
	}
	return fmt.Sprintf("A homebrew %s (%s).", strings.TrimSpace(m.RequestedRarity+" magic item"), m.Design.Type)
}
