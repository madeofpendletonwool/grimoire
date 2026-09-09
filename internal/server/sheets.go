package server

// The typed character sheet surface (MAD-418, stage 1 of MAD-417): three
// routes under the campaign the pc belongs to, plus the player-edit knob
// MAD-488 added.
//
//	GET  /api/campaigns/{id}/characters/{eid}/sheet        read (DM, or the player bound to eid)
//	PUT  /api/campaigns/{id}/characters/{eid}/sheet        replace, validated (DM, or the bound player)
//	POST /api/campaigns/{id}/characters/import             create a pc from an export (DM)
//	PUT  /api/campaigns/{id}/sheet-edit/settings           the player-edit knob (DM, owner-shaped)
//
// The sheet is a payload block, so the entity CRUD above it is unchanged;
// these routes are the typed editor and the import door. Reads carry the
// unstructured marker — a pc whose payload predates the typed sheet reads
// structured:false rather than pretending — and writes refuse anything
// Validate rejects, with every problem named, because a rejected sheet and
// a confused DM is worse than an empty field.
//
// Scoping: the DM reads and writes anything; a player reads exactly their
// own bound character through the player view (see internal/knowledge/
// sheet.go); nobody else reads a sheet. MAD-488 widened the write the same
// way: a player may edit their own character's sheet and nobody else's,
// ever — the write gate mirrors the read's character:<eid> binding. The
// edit itself is split conservatively: inventory (attunement flags
// included) and player notes are player-writable day one; the mechanical
// definition — abilities, classes, proficiencies, and the purse, which
// sizes the ledger's pools — stays DM-writable unless the campaign opts in
// via settings["sheet_edit"].mechanics, default off. A player's write is
// therefore a merge, not a replace: their inventory and notes land on the
// stored sheet, and every other key survives untouched — omission cannot
// zero an ability score, and a sneaky field in the body cannot either.
//
// Provenance: every sheet write records who and when under the payload's
// "sheet_meta" key (campaign.SheetEditMeta) — extending the entity's
// update metadata rather than inventing an audit table — so the DM's read
// can answer "who changed this". Import stays DM-only: creating characters
// is a DM act.
//
// The resource question is already settled elsewhere and stays settled:
// a seated player spends and regains their own pools through the ledger's
// transaction surface (MAD-419 — sets are the DM's correction), and rests,
// live or staged, are DM-actuated because a long rest moves the campaign
// clock. A sheet write never touches balances; it re-derives pool
// definitions exactly like the DM's write does.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
)

// sheetRead is the GET body: the typed sheet when there is one, the marker
// when there is not. Problems carries payload-block problems the way the
// party table does — reported, never fatal. QuickRolls (MAD-420) rides
// along: the roll bar's one-tap formulas, derived server-side so no
// surface re-derives proficiency bonuses of its own. LastEdit is the
// provenance of the most recent write (MAD-488): the DM's read surfaces
// it; the player read path leaves it unset.
type sheetRead struct {
	EntityID   string                  `json:"entity_id"`
	Name       string                  `json:"name"`
	Status     string                  `json:"status"`
	Structured bool                    `json:"structured"`
	Sheet      json.RawMessage         `json:"sheet,omitempty"`
	QuickRolls []sheet.QuickRoll       `json:"quick_rolls,omitempty"`
	Problems   []string                `json:"problems,omitempty"`
	LastEdit   *campaign.SheetEditMeta `json:"last_edit,omitempty"`
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
		body.QuickRolls = sheet.QuickRolls(read.Sheet)
	}
	writeJSON(w, http.StatusOK, body)
}

// sheetReadOf builds the read shape from an entity the DM path already
// loaded. The sheet is re-marshaled from the typed struct — the same bytes
// a PUT wrote, which is what makes the round-trip stable — and the
// last-write provenance rides along so the DM can answer "who changed
// this" (MAD-488).
func sheetReadOf(e *campaign.Entity) sheetRead {
	body := sheetRead{EntityID: e.ID, Name: e.Name, Status: e.Status, LastEdit: campaign.SheetEditMetaOf(e.Payload)}
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
		body.QuickRolls = sheet.QuickRolls(s)
	}
	return body
}

func (s *Server) handlePutCharacterSheet(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	ctx := r.Context()
	eid := r.PathValue("eid")
	// The widened gate (MAD-488), mirroring the read's: the DM writes any
	// sheet; a player writes exactly the pc their membership row binds;
	// an observer, an unbound member or anyone else's character is 403.
	boundPlayer := !a.isDM() && a.view != nil &&
		a.playerScope.Kind() == campaign.ScopeKindCharacter && a.playerScope.EntityID() == eid
	if !a.isDM() && !boundPlayer {
		writeError(w, http.StatusForbidden, fmt.Errorf("the sheet belongs to its character and the DM"))
		return
	}
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
	toStore := body
	if boundPlayer && !campaign.SheetEditConfigOf(a.campaign.Settings[campaign.SheetEditSettingsKey]).Mechanics {
		// The conservative split: the player's write is a merge onto the
		// stored sheet — their inventory and notes land, every other key
		// survives from what the DM wrote. A body cannot widen its own
		// permissions by omission or by smuggling fields.
		stored, has, err := campaign.SheetOf(entity)
		if err != nil && has {
			writeError(w, http.StatusConflict, fmt.Errorf("the stored sheet does not decode; the DM must rewrite it before a player may edit"))
			return
		}
		stored.Inventory = body.Inventory
		stored.Notes = body.Notes
		toStore = stored
	}
	if problems := sheet.Validate(toStore); len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "sheet validation failed",
			"problems": problems,
		})
		return
	}
	payload := campaign.WithSheet(entity.Payload, toStore)
	payload[campaign.SheetMetaKey] = s.sheetEditMeta(ctx, r, a)
	updated, err := s.campaigns.UpdateEntity(ctx, a.campaign.ID, eid, nil, nil, nil, payload)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	problems := s.syncSheetDerivations(ctx, a.campaign.ID, eid)
	read := sheetReadOf(updated)
	read.Problems = append(read.Problems, problems...)
	if len(problems) > 0 {
		// The sheet is stored; the caches are a convenience that rebuilds
		// at boot. The write succeeded — report it with the complaint.
		writeJSON(w, http.StatusOK, read)
		return
	}
	writeJSON(w, http.StatusOK, read)
}

// sheetEditMeta is the provenance record a sheet write stamps: who, as the
// table knows them, through which perspective, and when. The user id is
// the durable key; the username is display sugar that may be empty when
// the lookup fails.
func (s *Server) sheetEditMeta(ctx context.Context, r *http.Request, a *campAccess) campaign.SheetEditMeta {
	role := campaign.RoleDM
	if !a.isDM() {
		role = campaign.RolePlayer
	}
	uid := userID(r)
	name := ""
	if s.users != nil {
		if names, err := s.users.Usernames(ctx, []string{uid}); err == nil {
			name = names[uid]
		}
	}
	return campaign.SheetEditMeta{By: uid, Name: name, Role: role, At: time.Now().UTC().Format(time.RFC3339)}
}

// handleSheetEditSettings writes the player-edit knob (MAD-488):
// whether a seated player may edit their sheet's mechanical definition,
// data on the campaign under the sheet_edit settings key, validated
// strictly, owner-shaped like every settings write — the board-settings
// pattern verbatim.
func (s *Server) handleSheetEditSettings(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("id"))
	if a == nil {
		return
	}
	if !a.isDM() || (a.campaign.OwnerID != userID(r) && !a.keeper) {
		writeError(w, http.StatusForbidden, fmt.Errorf("only the campaign's owner may set the sheet-edit policy"))
		return
	}
	var req struct {
		Mechanics *bool `json:"mechanics"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err))
		return
	}
	if req.Mechanics == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("mechanics is required: true | false"))
		return
	}
	cfg := campaign.SheetEditConfig{Mechanics: *req.Mechanics}
	settings := a.campaign.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	settings[campaign.SheetEditSettingsKey] = cfg.SettingsValue()
	if _, err := s.campaigns.UpdateCampaign(r.Context(), a.campaign.OwnerID, a.campaign.ID,
		nil, nil, nil, nil, settings); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sheet_edit": cfg})
}

// syncSheetDerivations refreshes the caches a sheet write feeds — the query
// projection (MAD-418) and the ledger's pool definitions (MAD-419). Both
// rebuild at boot, so a failure here is a report, never a failed write.
func (s *Server) syncSheetDerivations(ctx context.Context, campaignID, entityID string) []string {
	var problems []string
	if err := sheet.SyncEntity(ctx, s.campaigns.DB(), campaignID, entityID); err != nil {
		problems = append(problems, err.Error())
	}
	if s.ledgers != nil {
		if err := s.ledgers.SyncEntity(ctx, campaignID, entityID); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
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
	if s.ledgers != nil {
		_ = s.ledgers.SyncEntity(r.Context(), a.campaign.ID, entity.ID)
	}
	writeJSON(w, http.StatusCreated, importResponse{
		Entity: toCampaignEntityView(entity, true),
		Sheet:  imported,
		Report: report,
	})
}
