package server

// The typed character sheet surface (MAD-418, stage 1 of MAD-417): three
// routes under the campaign the pc belongs to.
//
//	GET  /api/campaigns/{id}/characters/{eid}/sheet   read (DM, or the player bound to eid)
//	PUT  /api/campaigns/{id}/characters/{eid}/sheet   replace, validated (DM)
//	POST /api/campaigns/{id}/characters/import        create a pc from an export (DM)
//
// The sheet is a payload block, so the entity CRUD above it is unchanged;
// these routes are the typed editor and the import door. Reads carry the
// unstructured marker — a pc whose payload predates the typed sheet reads
// structured:false rather than pretending — and writes refuse anything
// Validate rejects, with every problem named, because a rejected sheet and
// a confused DM is worse than an empty field.
//
// Scoping: the DM reads and writes anything; a player reads exactly their
// own bound character through the player view (the one deliberate widening
// MAD-418 makes, narrow by construction — see internal/knowledge/sheet.go);
// nobody else reads a sheet and nobody but the DM writes one. The sheet is
// the character's definition, edited deliberately — the player-edit
// question is a product decision for the portal stage (MAD-319), not a
// default this issue sets.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

// sheetRead is the GET body: the typed sheet when there is one, the marker
// when there is not. Problems carries payload-block problems the way the
// party table does — reported, never fatal.
type sheetRead struct {
	EntityID   string          `json:"entity_id"`
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	Structured bool            `json:"structured"`
	Sheet      json.RawMessage `json:"sheet,omitempty"`
	Problems   []string        `json:"problems,omitempty"`
}

func (s *Server) handleGetCharacterSheet(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	eid := r.PathValue("eid")
	ctx := r.Context()
	if a.isDM() {
		entity, err := s.campaigns.GetEntity(ctx, campaign.ScopeDM, a.campaign.ID, eid)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if entity.Kind != campaign.KindPC {
			writeError(w, http.StatusBadRequest, fmt.Errorf("%s is a %s, not a pc", entity.Name, entity.Kind))
			return
		}
		writeJSON(w, http.StatusOK, sheetReadOf(entity))
		return
	}
	if a.view == nil || a.playerScope.Kind() != campaign.ScopeKindCharacter || a.playerScope.EntityID() != eid {
		writeError(w, http.StatusForbidden, fmt.Errorf("the sheet belongs to its character and the DM"))
		return
	}
	read, err := a.view.CharacterSheet(ctx, a.campaign.ID, eid)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	body := sheetRead{EntityID: read.EntityID, Name: read.Name, Status: read.Status, Structured: read.Structured}
	if read.Structured {
		if blob, err := json.Marshal(read.Sheet); err == nil {
			body.Sheet = blob
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// sheetReadOf builds the read shape from an entity the DM path already
// loaded. The sheet is re-marshaled from the typed struct — the same bytes
// a PUT wrote, which is what makes the round-trip stable.
func sheetReadOf(e *campaign.Entity) sheetRead {
	body := sheetRead{EntityID: e.ID, Name: e.Name, Status: e.Status}
	s, has, err := campaign.SheetOf(e)
	if err != nil {
		body.Problems = append(body.Problems, err.Error())
		return body
	}
	body.Structured = has
	if has {
		if blob, err := json.Marshal(s); err == nil {
			body.Sheet = blob
		}
	}
	return body
}

func (s *Server) handlePutCharacterSheet(w http.ResponseWriter, r *http.Request) {
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
	if entity.Kind != campaign.KindPC {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s is a %s, not a pc", entity.Name, entity.Kind))
		return
	}
	var body sheet.Sheet
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if problems := sheet.Validate(body); len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "sheet validation failed",
			"problems": problems,
		})
		return
	}
	updated, err := s.campaigns.UpdateEntity(ctx, a.campaign.ID, eid, nil, nil, nil, campaign.WithSheet(entity.Payload, body))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := sheet.SyncEntity(ctx, s.campaigns.DB(), a.campaign.ID, eid); err != nil {
		// The sheet is stored; the projection is a cache that rebuilds at
		// boot. The write succeeded — report it with the cache complaint.
		read := sheetReadOf(updated)
		read.Problems = append(read.Problems, err.Error())
		writeJSON(w, http.StatusOK, read)
		return
	}
	writeJSON(w, http.StatusOK, sheetReadOf(updated))
}

// importRequest is the import body: the format's name (or "auto"), the
// export verbatim, and the name the campaign should call the pc when the
// export does not carry one.
type importRequest struct {
	Name   string          `json:"name"`
	Format string          `json:"format"`
	Data   json.RawMessage `json:"data"`
}

// importResponse is the result: the entity the import created, the typed
// sheet it produced, and the report's honesty about what mapped.
type importResponse struct {
	Entity campaignEntityView `json:"entity"`
	Sheet  sheet.Sheet        `json:"sheet"`
	Report sheet.ImportReport `json:"report"`
}

func (s *Server) handleImportCharacter(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req importRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if len(req.Data) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("data is required: the export to import"))
		return
	}
	// The export travels as JSON when it is JSON and as a string when the
	// caller had to wrap XML — unwrap a bare string so both spellings work.
	data := []byte(req.Data)
	if len(data) > 0 && data[0] == '"' {
		var s string
		if json.Unmarshal(data, &s) == nil {
			data = []byte(s)
		}
	}
	imported, report, err := sheet.Import(req.Format, data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if problems := sheet.Validate(imported); len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "the import produced a sheet that does not validate",
			"problems": problems,
			"report":   report,
		})
		return
	}
	name := req.Name
	if name == "" {
		name = report.Name
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name is required: the export carries none"))
		return
	}
	entity, err := s.campaigns.CreateEntity(r.Context(), a.campaign.ID, campaign.KindPC, name, "", campaign.WithSheet(nil, imported))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := sheet.SyncEntity(r.Context(), s.campaigns.DB(), a.campaign.ID, entity.ID); err != nil {
		_ = err // the projection rebuilds at boot; the import itself is done
	}
	writeJSON(w, http.StatusCreated, importResponse{
		Entity: toCampaignEntityView(entity, true),
		Sheet:  imported,
		Report: report,
	})
}
