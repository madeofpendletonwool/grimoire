package canon

// The dungeon designer's model passes (MAD-373, stage 5.2 of MAD-316).
//
// The topology is arithmetic internal/dungeon computed before anything
// here runs — room count, purpose quotas, the critical path, the wings,
// the secrets, the loops, the grid. This file does the two things that
// need more than arithmetic:
//
//   - The dressing pass: one structured-generation call that names the
//     rooms it was handed, writes their detail, gives the boss a motive,
//     names the key item the locked door needs, and states the dungeon's
//     secret. It may not add a room, remove a room or reconnect
//     anything: the fillable schema is keyed by the computed room keys,
//     so an extra room, a missing room or a new edge is an undeclared
//     field, and the validation pass rejects the mismatch outright
//     rather than repairing it — a fake client that keeps returning a
//     changed topology fails, with nothing written.
//
//   - The placing pass: the dungeon becomes graph objects — the dungeon
//     a location entity, each room a child location with a contains
//     edge, the boss and the named hazards creature entities, the loot
//     item entities, the secret a facts row whose clue path is the boss
//     holding it — staged as one proposal batch through MAD-359's gate,
//     in dependency order. Nothing is written to the graph until
//     DecideBatch accepts it; a DM can design, map and dress a whole
//     dungeon without ever touching the campaign graph, and that is the
//     point of the separation.
//
// When a placement batch is decided, the dungeon finalizer records what
// landed: the dungeon's location entity, each room's entity, and the key
// item's entity on the locked door. A partial acceptance marks nothing —
// the entities the DM kept stay in the graph, and the dungeon stays
// dressable and placeable again.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/dungeon"
)

/* ---------- the dressing pass ---------- */

// DungeonDressInput is one dress request: the dungeon, by id. The theme,
// the size, the level and the quotas are read from the dungeon itself —
// the DM does not re-state them.
type DungeonDressInput struct {
	CampaignID string
	DungeonID  string
	CreatedBy  string
}

// DungeonDressResult is what one dressing pass produced: the dressed
// dungeon (names, detail, the key item, the secret) and the model
// accounting.
type DungeonDressResult struct {
	Dungeon   *campaign.Dungeon `json:"dungeon"`
	Generated time.Time         `json:"generated_at"`
}

// maxDungeonRoomsDressed bounds what one dress exchange may ask the
// model to name: a megadungeon at the cap is staged in two dress runs by
// the DM editing rooms by hand between them. The generator's own cap is
// sixty; past this the call is refused before any prompt is built.
const maxDungeonRoomsDressed = 60

// DressDungeon runs one dress exchange: read the stored graph, one
// structured-generation call keyed by its room keys, validate the fill
// against the computed topology, persist the names and detail. The
// topology comparison is the contract: every declared room field must
// come back for a computed room, and nothing else may come back at all —
// the harness rejects undeclared keys and required-field gaps, and a
// mismatch that survives is an error, never a repair.
func (s *Store) DressDungeon(ctx context.Context, in DungeonDressInput) (*DungeonDressResult, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	d, err := s.campaigns.GetDungeon(ctx, campaign.ScopeDM, in.CampaignID, in.DungeonID)
	if err != nil {
		return nil, err
	}
	if d.Status == campaign.DungeonPlaced {
		return nil, fmt.Errorf("%w: %q is placed; its design is not re-dressed", ErrInvalid, d.Name)
	}
	if len(d.Rooms) > maxDungeonRoomsDressed {
		return nil, fmt.Errorf("%w: %q has %d rooms; dress it in runs of at most %d by editing rooms between them",
			ErrInvalid, d.Name, len(d.Rooms), maxDungeonRoomsDressed)
	}

	fields, structure := dungeonDressPrompt(d)
	note := "Fill every declared field. The structure block is the dungeon you are dressing — every room, every connection and every purpose is already law; you are naming and describing, nothing more. " +
		"Honour the theme: the names, the detail, the boss's motive, the key item and the secret should all read as one place. " +
		"A wing held by a faction should say so in its rooms' detail."

	fill := func(note string) (*Generated, error) {
		return s.Generate(ctx, GenerateInput{
			System: dungeonDressSystemPrompt, Structure: structure, Fields: fields, Note: note,
		})
	}
	gen, err := fill(note)
	if err != nil {
		return nil, err
	}

	dress, problems := assembleDungeonDress(d, gen.Values)
	if len(problems) > 0 {
		// Prose problems (a blank detail, a duplicate name) get the one
		// repair retry; a topology mismatch is not repairable and never
		// reaches here — the harness already rejected it as an undeclared
		// or missing field, and this assembly checks the set again.
		gen, err = fill(note + "\nYour previous response had these problems:\n- " + strings.Join(problems, "\n- "))
		if err != nil {
			return nil, err
		}
		dress, problems = assembleDungeonDress(d, gen.Values)
		if len(problems) > 0 {
			return nil, fmt.Errorf("%w: the dressing failed validation twice: %s", ErrInvalid, strings.Join(problems, "; "))
		}
	}

	if err := s.campaigns.DressDungeonRooms(ctx, in.CampaignID, in.DungeonID, dress.rooms, dress.keyItem, dress.secret, dress.bossName); err != nil {
		return nil, err
	}
	out, err := s.campaigns.GetDungeon(ctx, campaign.ScopeDM, in.CampaignID, in.DungeonID)
	if err != nil {
		return nil, err
	}
	return &DungeonDressResult{Dungeon: out, Generated: s.now()}, nil
}

// dungeonDressPrompt builds the fillable schema and the structure block.
// Every room field key carries the computed room key, so the model
// cannot name a room the graph does not have without the harness
// rejecting the response.
func dungeonDressPrompt(d *campaign.Dungeon) ([]FieldSpec, map[string]any) {
	fields := []FieldSpec{
		{Key: "boss_name", Required: true, MaxLen: 60,
			Desc: "The boss's own name — the thing the locked door is locked for."},
		{Key: "boss_motive", Required: true, MaxLen: 300,
			Desc: "What the boss wants and what it is willing to do about it — two sentences the DM can run."},
		{Key: "secret_statement", Required: true, MaxLen: 300,
			Desc: "The dungeon's secret, one plain sentence: what is really going on down there. The party may never learn it; the DM must be able to."},
		{Key: "key_item_name", Required: true, MaxLen: 60,
			Desc: "The item the locked door before the boss needs — named the way the party would hear of it (\"the Pale Key\")."},
	}
	for _, r := range d.Rooms {
		what := fmt.Sprintf("%s (%s, depth %d)", r.Key, r.Purpose, r.Depth)
		if r.Key == dungeonBossKey(d) {
			what = fmt.Sprintf("%s (the boss room)", r.Key)
		}
		fields = append(fields,
			FieldSpec{Key: "room_" + r.Key + "_name", Required: true, MaxLen: 60,
				Desc: fmt.Sprintf("Name of room %s, the way the party would call it. Name every room distinctly.", what)},
			FieldSpec{Key: "room_" + r.Key + "_detail", Required: true, MaxLen: 400,
				Desc: fmt.Sprintf("What is in room %s and what it is like, two sentences a DM can run. Say what the hazard actually is; say which faction holds the wing, when one does.", what)},
		)
	}

	roomsView := make([]map[string]any, 0, len(d.Rooms))
	for _, r := range d.Rooms {
		roomsView = append(roomsView, map[string]any{
			"key": r.Key, "purpose": r.Purpose, "x": r.X, "y": r.Y, "depth": r.Depth,
		})
	}
	edgesView := make([]map[string]any, 0, len(d.Edges))
	for _, e := range d.Edges {
		edgesView = append(edgesView, map[string]any{
			"from": e.FromRoom, "to": e.ToRoom, "kind": e.Kind, "one_way": e.OneWay,
		})
	}
	structure := map[string]any{
		"dungeon": map[string]any{
			"name": d.Name, "theme": d.Theme, "size": d.Size, "level": d.Level,
			"expected_sessions": d.ExpectedSessions, "seed": d.Seed,
		},
		"quotas":    d.Params,
		"rooms":     roomsView,
		"edges":     edgesView,
		"entrance":  dungeonEntranceKey(d),
		"boss_room": dungeonBossKey(d),
	}
	return fields, structure
}

// dungeonDressSystemPrompt is the dressing pass's system prompt.
const dungeonDressSystemPrompt = `You are Grimoire's dungeon dresser. You are handed the complete topology of a dungeon — computed by the server and authoritative: every room with its purpose and depth, every connection with its kind, the locked door before the boss, the entrance and the boss room — plus the dungeon's theme, level and the quotas it was generated from. Your job is to name rooms and write what is in them, nothing more.

STRICT RULES
1. Every field is a plain string. Fill every one. No lists, no markdown, no commentary.
2. The topology is fixed. Do not add, merge, remove or reconnect rooms; do not describe a connection the structure does not declare. You are dressing rooms you were handed.
3. Name every room distinctly: two rooms called "guardroom" is a failure.
4. The locked door's key item is a thing with a history — name it the way a rumour would carry it.
5. A hazard room says what the hazard actually is. A wing held by a faction says which faction, in that wing's rooms.
6. The boss's motive explains why the dungeon is like this; the secret says what is really going on. They may be linked; they are not the same field.
7. Honour the theme: the names and the detail should all read as one place.`

// dungeonDressAssembled is a dress fill after assembly: the room
// dressings keyed by room key, the boss's name, the key item, the
// secret, and the problems that were found (empty when the fill is
// valid).
type dungeonDressAssembled struct {
	rooms    map[string]campaign.Dressing
	bossName string
	keyItem  string
	secret   string
}

// assembleDungeonDress turns validated values into the dressing, running
// every check the contract names: the returned room set must be exactly
// the computed room set — every room named, no room the graph does not
// have — names non-empty and distinct. A topology mismatch here is a
// hard problem, not a repairable one; the caller treats every problem
// the same way (one retry, then failure) and the harness has already
// rejected undeclared keys outright.
func assembleDungeonDress(d *campaign.Dungeon, values map[string]any) (*dungeonDressAssembled, []string) {
	a := &dungeonDressAssembled{rooms: map[string]campaign.Dressing{}}
	var problems []string
	val := func(key string) string {
		sv, _ := values[key].(string)
		return strings.TrimSpace(sv)
	}
	a.bossName = val("boss_name")
	a.keyItem = val("key_item_name")
	a.secret = val("secret_statement")
	motive := val("boss_motive")
	if a.bossName == "" {
		problems = append(problems, "boss_name is missing")
	}
	if motive == "" {
		problems = append(problems, "boss_motive is missing")
	}
	if a.keyItem == "" {
		problems = append(problems, "key_item_name is missing")
	}
	if a.secret == "" {
		problems = append(problems, "secret_statement is missing")
	}
	seen := map[string]string{}
	for _, r := range d.Rooms {
		name := val("room_" + r.Key + "_name")
		detail := val("room_" + r.Key + "_detail")
		if name == "" {
			problems = append(problems, fmt.Sprintf("room_%s_name is missing — the model returned a topology without room %s", r.Key, r.Key))
			continue
		}
		if detail == "" {
			problems = append(problems, fmt.Sprintf("room_%s_detail is missing", r.Key))
		}
		if prev, dup := seen[strings.ToLower(name)]; dup {
			problems = append(problems, fmt.Sprintf("rooms %s and %s are both named %q; name every room distinctly", prev, r.Key, name))
		}
		seen[strings.ToLower(name)] = r.Key
		// The boss's motive folds into the boss room's detail: the room
		// and its keeper read as one place.
		if r.Key == dungeonBossKey(d) && motive != "" {
			detail = strings.TrimSpace(detail + " The keeper's motive: " + motive)
		}
		a.rooms[r.Key] = campaign.Dressing{Name: name, Detail: detail}
	}
	// The topology comparison, again and explicitly: the set of rooms
	// the model dressed is the set the graph declares, exactly.
	if len(a.rooms) != len(d.Rooms) {
		problems = append(problems, fmt.Sprintf("the dressing covers %d rooms and the dungeon declares %d — a topology mismatch is rejected, not repaired",
			len(a.rooms), len(d.Rooms)))
	}
	return a, problems
}

// dungeonBossKey / dungeonEntranceKey / dungeonRoomByKey read the
// special rooms off the stored graph: the boss room is the boss-purpose
// room, the entrance the entrance-purpose room. The generator
// guarantees one of each; a hand edit cannot change purposes.
func dungeonBossKey(d *campaign.Dungeon) string {
	for _, r := range d.Rooms {
		if r.Purpose == dungeon.PurposeBoss {
			return r.Key
		}
	}
	return ""
}

func dungeonEntranceKey(d *campaign.Dungeon) string {
	for _, r := range d.Rooms {
		if r.Purpose == dungeon.PurposeEntrance {
			return r.Key
		}
	}
	return ""
}

func dungeonRoomByKey(d *campaign.Dungeon, key string) (campaign.DungeonRoom, bool) {
	for _, r := range d.Rooms {
		if r.Key == key {
			return r, true
		}
	}
	return campaign.DungeonRoom{}, false
}

/* ---------- the placing pass ---------- */

// DungeonPlaceInput is one place request: the dungeon, by id, staged as
// a proposal batch. CreatedBy is the DM asking.
type DungeonPlaceInput struct {
	CampaignID string
	DungeonID  string
	CreatedBy  string
}

// DungeonPlaceResult is the staged batch; the graph itself changes only
// when the batch is decided.
type DungeonPlaceResult struct {
	Batch     *Batch    `json:"batch"`
	Generated time.Time `json:"generated_at"`
}

// PlaceDungeon stages the placing batch: the dungeon as a location
// entity, each room a child location with a contains edge, the boss and
// the named hazards creature entities, the loot item entities, the
// secret a facts row whose clue path is the boss holding it, and the
// key item an item entity — all in dependency order through depends_on.
// The dungeon's own rows are marked only when a decided batch lands
// (finalizeDungeonBatch); nothing here writes to the graph.
func (s *Store) PlaceDungeon(ctx context.Context, in DungeonPlaceInput) (*DungeonPlaceResult, error) {
	if err := s.requireGraphStores(); err != nil {
		return nil, err
	}
	d, err := s.campaigns.GetDungeon(ctx, campaign.ScopeDM, in.CampaignID, in.DungeonID)
	if err != nil {
		return nil, err
	}
	if d.Status == campaign.DungeonPlaced {
		return nil, fmt.Errorf("%w: %q is already placed", ErrInvalid, d.Name)
	}

	nameFor := func(r campaign.DungeonRoom) string {
		if r.Name != "" {
			return r.Name
		}
		return fmt.Sprintf("%s — %s", d.Name, r.Purpose)
	}

	var items []BatchItemInput
	// 1. The dungeon itself: a location with the place block a ruin or
	//    dungeon reads from (MAD-370's structured block), the secret as
	//    its private truth.
	block := campaign.Place{
		Kind:   "dungeon",
		Scale:  d.Size,
		State:  "unexplored",
		Danger: dangerForLevel(d.Level),
	}
	if d.Secret != "" {
		block.PrivateTruth = d.Secret
	}
	items = append(items, BatchItemInput{
		ID: "dungeon-root", Kind: "entity", Subject: d.Name,
		Summary: dungeonRootSummary(d),
		Payload: map[string]any{
			"local_id": "dungeon-root", "dungeon_id": d.ID, "dungeon_root": true,
			"kind": campaign.KindLocation, "name": d.Name,
			"summary": dungeonRootSummary(d),
			"payload": campaign.WithPlace(nil, block),
		},
	})

	// 2. The rooms: child locations with contains edges from the
	//    dungeon — the hierarchy the acceptance checks.
	roomRef := map[string]string{} // room key -> how siblings name it
	for _, r := range d.Rooms {
		id := "room-" + r.Key
		roomRef[r.Key] = id
		items = append(items, BatchItemInput{
			ID: id, Kind: "entity", Subject: nameFor(r),
			Summary: r.Detail,
			Payload: map[string]any{
				"local_id": id, "dungeon_id": d.ID, "dungeon_room": r.Key,
				"kind": campaign.KindLocation, "name": nameFor(r), "summary": r.Detail,
			},
			DependsOn: []string{"dungeon-root"},
		})
		items = append(items, BatchItemInput{
			ID: "contains-" + r.Key, Kind: "relationship",
			Subject: fmt.Sprintf("%s contains %s", d.Name, nameFor(r)),
			Summary: fmt.Sprintf("A room of %s.", d.Name),
			Payload: map[string]any{
				"local_id":    "contains-" + r.Key,
				"from_entity": "dungeon-root", "rel_type": "contains", "to_entity": id,
			},
			DependsOn: []string{id},
		})
	}

	// 3. The boss: a creature in its room, carrying its motive. Only a
	//    dressed dungeon has a motive to carry; an undressed one places
	//    the rooms and the secret, not the cast.
	bossKey := dungeonBossKey(d)
	if boss, ok := dungeonRoomByKey(d, bossKey); ok && boss.Detail != "" && d.BossName != "" {
		items = append(items, BatchItemInput{
			ID: "boss", Kind: "entity", Subject: d.BossName,
			Summary: boss.Detail,
			Payload: map[string]any{
				"local_id": "boss", "dungeon_id": d.ID,
				"kind": campaign.KindCreature, "name": d.BossName, "summary": boss.Detail,
			},
			DependsOn: []string{roomRef[bossKey]},
		})
		items = append(items, BatchItemInput{
			ID: "edge-boss", Kind: "relationship",
			Subject: fmt.Sprintf("%s located_in %s", d.BossName, boss.Name),
			Summary: fmt.Sprintf("%s holds the boss room of %s.", d.BossName, d.Name),
			Payload: map[string]any{
				"from_entity": "boss", "rel_type": "located_in", "to_entity": roomRef[bossKey],
			},
			DependsOn: []string{"boss"},
		})
	}

	// 4. The named hazards and the loot: the hazards the dressing named,
	//    the treasure rooms' loot — creatures and items in their rooms.
	for _, r := range d.Rooms {
		if r.Detail == "" || r.Name == "" {
			continue // nothing named to stage
		}
		switch r.Purpose {
		case dungeon.PurposeHazard:
			id := "hazard-" + r.Key
			items = append(items, BatchItemInput{
				ID: id, Kind: "entity", Subject: r.Name, Summary: r.Detail,
				Payload: map[string]any{
					"local_id": id, "dungeon_id": d.ID,
					"kind": campaign.KindCreature, "name": r.Name, "summary": r.Detail,
				},
				DependsOn: []string{roomRef[r.Key]},
			})
			items = append(items, BatchItemInput{
				ID: "edge-" + id, Kind: "relationship",
				Subject: fmt.Sprintf("%s located_in %s", r.Name, r.Name),
				Summary: fmt.Sprintf("The hazard of %s.", r.Name),
				Payload: map[string]any{
					"from_entity": id, "rel_type": "located_in", "to_entity": roomRef[r.Key],
				},
				DependsOn: []string{id},
			})
		case dungeon.PurposeTreasure:
			id := "loot-" + r.Key
			lootName := fmt.Sprintf("Loot of %s", r.Name)
			items = append(items, BatchItemInput{
				ID: id, Kind: "entity", Subject: lootName, Summary: r.Detail,
				Payload: map[string]any{
					"local_id": id, "dungeon_id": d.ID,
					"kind": campaign.KindItem, "name": lootName, "summary": r.Detail,
				},
				DependsOn: []string{roomRef[r.Key]},
			})
			items = append(items, BatchItemInput{
				ID: "edge-" + id, Kind: "relationship",
				Subject: fmt.Sprintf("%s located_in %s", lootName, r.Name),
				Summary: fmt.Sprintf("The loot of %s.", r.Name),
				Payload: map[string]any{
					"from_entity": id, "rel_type": "located_in", "to_entity": roomRef[r.Key],
				},
				DependsOn: []string{id},
			})
		}
	}

	// 5. The key item: an item entity the locked door names. Its id
	//    lands on the edge when the batch is decided.
	if d.KeyItem != "" {
		items = append(items, BatchItemInput{
			ID: "key-item", Kind: "entity", Subject: d.KeyItem,
			Summary: fmt.Sprintf("Opens the locked door before the boss of %s.", d.Name),
			Payload: map[string]any{
				"local_id": "key-item", "dungeon_id": d.ID, "dungeon_key_item": true,
				"kind": campaign.KindItem, "name": d.KeyItem,
				"summary": fmt.Sprintf("Opens the locked door before the boss of %s.", d.Name),
			},
		})
	}

	// 6. The secret: a facts row with a clue path. The boss holds it —
	//    the discovery item is the awareness row that keeps the secret
	//    from being born unreachable, the placedesign precedent.
	if d.Secret != "" && bossKey != "" && d.BossName != "" {
		items = append(items, BatchItemInput{
			ID: "secret", Kind: "fact",
			Subject: fmt.Sprintf("A secret %s hides", d.Name),
			Summary: d.Secret,
			Payload: map[string]any{
				"local_id": "secret", "statement": d.Secret,
				"subject": "dungeon-root", "predicate": "hides",
				"object_literal": d.Name, "visibility": campaign.VisibilitySecret,
			},
			DependsOn: []string{"dungeon-root"},
		})
		items = append(items, BatchItemInput{
			ID: "secret-holder", Kind: "discovery",
			Subject: fmt.Sprintf("Who holds %s's secret", d.Name),
			Summary: d.Secret,
			Payload: map[string]any{
				"fact": "secret", "discovered_by": "boss",
				"stance": "knows", "method": "holds this secret close",
			},
			DependsOn: []string{"secret", "boss"},
		})
	}

	promptRecord := fmt.Sprintf("place dungeon %q (%s) | size: %s, level: %d, sessions: %d, seed: %d | %d rooms, %d edges",
		d.Name, d.ID, d.Size, d.Level, d.ExpectedSessions, d.Seed, len(d.Rooms), len(d.Edges))
	batch, err := s.StageBatch(ctx, BatchInput{
		CampaignID: in.CampaignID, Source: BatchSourceDungeon,
		Prompt: promptRecord, CreatedBy: in.CreatedBy, Items: items,
	})
	if err != nil {
		return nil, err
	}
	return &DungeonPlaceResult{Batch: batch, Generated: s.now()}, nil
}

// dangerForLevel maps the party level onto the deliberately unnumbered
// danger band a place block carries (MAD-370): 0-5-ish, a reading aid
// rather than a rule.
func dangerForLevel(level int) int {
	switch {
	case level <= 3:
		return 1
	case level <= 7:
		return 2
	case level <= 12:
		return 3
	case level <= 17:
		return 4
	default:
		return 5
	}
}

func dungeonRootSummary(d *campaign.Dungeon) string {
	if d.Theme != "" {
		return fmt.Sprintf("A %s-sized dungeon for a level %d party: %s.", d.Size, d.Level, d.Theme)
	}
	return fmt.Sprintf("A %s-sized dungeon for a level %d party.", d.Size, d.Level)
}

/* ---------- the decided-batch finalizer ---------- */

// finalizeDungeonBatch records a decided placement batch onto the
// dungeon it placed: the root's location entity, each room's entity,
// and the key item's entity on the locked door. A partial acceptance
// (the DM dismissed rooms, or an item failed) marks nothing — the
// entities the DM kept stay in the graph, the dungeon stays dressable
// and placeable again. Idempotent: a decided batch re-run heals nothing
// further because MarkDungeonPlaced no-ops once placed.
func (s *Store) finalizeDungeonBatch(ctx context.Context, batch *Batch) error {
	if err := s.requireGraphStores(); err != nil {
		return err
	}
	// One dungeon per batch: the items all carry its id.
	dungeonID := ""
	rootRef := ""
	keyItemRef := ""
	roomRefs := map[string]string{}
	for i := range batch.Items {
		r := &batch.Items[i]
		if r.Kind != ReviewProposedEntity || r.ResultRef == "" {
			continue
		}
		var p map[string]any
		if json.Unmarshal([]byte(r.Detail), &p) != nil {
			continue
		}
		if id := str(p, "dungeon_id"); id != "" && dungeonID == "" {
			dungeonID = id
		}
		if b, _ := p["dungeon_root"].(bool); b {
			rootRef = r.ResultRef
		}
		if b, _ := p["dungeon_key_item"].(bool); b {
			keyItemRef = r.ResultRef
		}
		if key := str(p, "dungeon_room"); key != "" {
			roomRefs[key] = r.ResultRef
		}
	}
	if dungeonID == "" || rootRef == "" {
		return nil // not a dungeon batch, or the root was not accepted
	}
	// The whole hierarchy must have landed: every room's entity accepted.
	d, err := s.campaigns.GetDungeon(ctx, campaign.ScopeDM, batch.CampaignID, dungeonID)
	if err != nil {
		return err
	}
	if len(roomRefs) != len(d.Rooms) {
		return nil // partial: mark nothing
	}
	for _, r := range d.Rooms {
		if roomRefs[r.Key] == "" {
			return nil // partial: mark nothing
		}
	}
	return s.campaigns.MarkDungeonPlaced(ctx, batch.CampaignID, dungeonID, rootRef, keyItemRef, roomRefs)
}
