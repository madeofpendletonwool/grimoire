package campaign

import (
	"context"
	"database/sql"
	"fmt"
)

// Fixture is the seed campaign's ids, handed back so later stages' tests can
// reach the entities, facts, contradiction and quest the fixture plants.
// Seeded with deterministic rows and dm_authored provenance: no model, no
// clock dependence beyond monotonic timestamps.
type Fixture struct {
	Campaign *Campaign

	// Entities.
	Thalia, Bran, Keth, Mira                string // the party
	Duke, Elara, Tom, Venn                  string // NPCs
	Cult, Blackwater, Monastery, Mines, Key string // faction, locations, item
	Verdant                                 string // deity

	// Facts.
	FactMinesOwned, FactKeyOpensCrypt string
	FactDukeVisited, FactDukeNever    string // the registered contradiction pair

	// The contradiction register entry linking the pair above.
	ContradictionID string

	// The quest with a real (branching) state machine.
	QuestID string

	// Events, in play order.
	EventAmbush, EventSurvivors string
}

// Seed builds the fixture campaign: a dozen-plus entities, facts with
// provenance, one registered contradiction (both sides downgraded to
// 'contested'), and one quest with a real branching state machine partway
// through its run. It is the shared substrate for later stages' tests —
// awareness, the canon engine, scoped retrieval all start here.
//
// owner and player must already exist as users rows (the membership table
// foreign-keys them); an empty player skips the player membership.
func Seed(ctx context.Context, db *sql.DB, owner, player string) (*Fixture, error) {
	s, err := New(db)
	if err != nil {
		return nil, err
	}
	fx := &Fixture{}

	c, err := s.CreateCampaign(ctx, owner, "The Withering Kingdom", "dnd5e",
		"A dark fantasy campaign about a kingdom slowly consumed by an ancient forest. "+
			"Levels 1-12: political intrigue, exploration, occasional nasty horror.")
	if err != nil {
		return nil, err
	}
	fx.Campaign = c
	cid := c.ID

	// A second member, the pattern every campaign will use once invites land.
	if player != "" {
		if err := s.AddMember(ctx, cid, player, RolePlayer, ""); err != nil {
			return nil, err
		}
	}

	mk := func(kind, name, summary string, payload map[string]any) (string, error) {
		e, err := s.CreateEntity(ctx, cid, kind, name, summary, payload)
		if err != nil {
			return "", err
		}
		return e.ID, nil
	}
	if fx.Thalia, err = mk(KindPC, "Thalia", "Human fighter, the shield of the party.", map[string]any{"level": 5, "class": "fighter"}); err != nil {
		return nil, err
	}
	if fx.Bran, err = mk(KindPC, "Bran", "Halfling rogue with a talent for locks and lies.", map[string]any{"level": 5, "class": "rogue"}); err != nil {
		return nil, err
	}
	if fx.Keth, err = mk(KindPC, "Keth", "Dwarf cleric of the Forge Father.", map[string]any{"level": 5, "class": "cleric"}); err != nil {
		return nil, err
	}
	if fx.Mira, err = mk(KindPC, "Mira", "Elf wizard, far too curious for her own good.", map[string]any{"level": 5, "class": "wizard"}); err != nil {
		return nil, err
	}
	if fx.Duke, err = mk(KindNPC, "Duke Aldric Vane", "Ruler of the eastern marches. Pale, precise, never seen eating.", map[string]any{"cr": 9}); err != nil {
		return nil, err
	}
	if fx.Elara, err = mk(KindNPC, "Lady Elara", "The Duke's chancellor. Unfailingly kind; unfailingly loyal to something else.", nil); err != nil {
		return nil, err
	}
	if fx.Tom, err = mk(KindNPC, "Tom the Innkeeper", "Keeps the Waystone in Blackwater. Sees everything, says little.", nil); err != nil {
		return nil, err
	}
	if _, err := s.AddEntityName(ctx, cid, fx.Tom, "Thomas Vane", NameAlias); err != nil {
		return nil, fmt.Errorf("seed alias: %w", err)
	}
	if fx.Venn, err = mk(KindNPC, "Brother Venn", "Itinerant priest who tends the sick of the mining camps.", nil); err != nil {
		return nil, err
	}
	if fx.Cult, err = mk(KindFaction, "Cult of the Root", "Worshippers of the Verdant God, working to bring the forest back.", nil); err != nil {
		return nil, err
	}
	if fx.Blackwater, err = mk(KindLocation, "Blackwater", "A market town on the kingdom's damp eastern edge.", nil); err != nil {
		return nil, err
	}
	if fx.Monastery, err = mk(KindLocation, "Greyfall Monastery", "A ruined monastery leaning over the falls above Blackwater.", nil); err != nil {
		return nil, err
	}
	if fx.Mines, err = mk(KindLocation, "Eastern Mines", "Silver and worse, dug into the hills behind Greyfall.", nil); err != nil {
		return nil, err
	}
	if fx.Key, err = mk(KindItem, "The Silver Key", "A heavy key of blackened silver, warm to the touch.", nil); err != nil {
		return nil, err
	}
	if fx.Verdant, err = mk(KindDeity, "The Verdant God", "The sleeping god of the ancient forest. Its dreams are instructions.", nil); err != nil {
		return nil, err
	}

	// The player characters are members of the campaign with their pcs.
	if player != "" {
		if err := s.SetMemberCharacter(ctx, cid, player, fx.Thalia); err != nil {
			return nil, err
		}
	}

	dm := func(session string) []ProvenanceInput {
		return []ProvenanceInput{{Method: MethodDMAuthored, SessionID: session}}
	}
	mkFact := func(subject, predicate, objectEntity, objectLiteral, statement, visibility, session string) (string, error) {
		f, err := s.CreateFact(ctx, cid, subject, predicate, objectEntity, objectLiteral, statement,
			ConfidenceCanon, visibility, owner, dm(session))
		if err != nil {
			return "", err
		}
		return f.ID, nil
	}
	if fx.FactMinesOwned, err = mkFact(fx.Duke, "owns", fx.Mines, "",
		"The Duke owns the Eastern Mines through a holding charter.", VisibilityPublic, "seed"); err != nil {
		return nil, err
	}
	if fx.FactKeyOpensCrypt, err = mkFact(fx.Key, "opens", fx.Monastery, "",
		"The Silver Key opens the crypt beneath Greyfall Monastery.", VisibilitySecret, "seed"); err != nil {
		return nil, err
	}
	if fx.FactDukeVisited, err = mkFact(fx.Duke, "visited", "", "the mines",
		"The Duke personally visited the Eastern Mines last winter.", VisibilityPublic, "seed-a"); err != nil {
		return nil, err
	}
	if fx.FactDukeNever, err = mkFact(fx.Duke, "visited", "", "nowhere",
		"The Duke has not left his keep in three years.", VisibilityPublic, "seed-b"); err != nil {
		return nil, err
	}

	// The registered contradiction: both sides credibly sourced, both
	// downgraded to contested, nothing picking a winner.
	con, err := s.RegisterContradiction(ctx, cid, fx.Duke, "visited",
		[]FactVersionSide{
			{FactID: fx.FactDukeVisited, Label: "the steward's ledger"},
			{FactID: fx.FactDukeNever, Label: "the housekeeper's testimony"},
		},
		"The sources disagree on when the Duke last traveled.")
	if err != nil {
		return nil, fmt.Errorf("seed contradiction: %w", err)
	}
	fx.ContradictionID = con.ID

	// Relationships from the controlled vocabulary, the load-bearing ones
	// justified by facts.
	mkRel := func(from, rel, to string, strength int64, fact string) error {
		_, err := s.CreateRelationship(ctx, cid, from, rel, to, strength, fact, "")
		return err
	}
	if err := mkRel(fx.Elara, "secretly_controls", fx.Duke, 0, fx.FactDukeNever); err != nil {
		return nil, fmt.Errorf("seed relationship elara: %w", err)
	}
	if err := mkRel(fx.Elara, "member_of", fx.Cult, 0, ""); err != nil {
		return nil, fmt.Errorf("seed relationship elara cult: %w", err)
	}
	if err := mkRel(fx.Cult, "worships", fx.Verdant, 0, ""); err != nil {
		return nil, fmt.Errorf("seed relationship cult: %w", err)
	}
	if err := mkRel(fx.Tom, "located_in", fx.Blackwater, 0, ""); err != nil {
		return nil, fmt.Errorf("seed relationship tom: %w", err)
	}
	if err := mkRel(fx.Mines, "located_in", fx.Blackwater, 0, ""); err != nil {
		return nil, fmt.Errorf("seed relationship mines: %w", err)
	}
	if err := mkRel(fx.Monastery, "located_in", fx.Blackwater, 0, ""); err != nil {
		return nil, fmt.Errorf("seed relationship monastery: %w", err)
	}

	// The timeline: play order and in-world order deliberately diverge —
	// event 2 is a flashback in play but earlier in the world.
	day := func(v int64) *int64 { return &v }
	ambush, err := s.CreateEvent(ctx, cid, "",
		"The missing miners' caravan is found wrecked on the Greyfall road.", day(34), fx.Blackwater)
	if err != nil {
		return nil, fmt.Errorf("seed event ambush: %w", err)
	}
	fx.EventAmbush = ambush.ID
	survivors, err := s.CreateEvent(ctx, cid, "",
		"Flashback: the miners are marched into the hills by robed figures.", day(31), fx.Mines)
	if err != nil {
		return nil, fmt.Errorf("seed event survivors: %w", err)
	}
	fx.EventSurvivors = survivors.ID
	for _, p := range []string{fx.Thalia, fx.Bran, fx.Keth, fx.Mira} {
		if err := s.AddParticipant(ctx, cid, fx.EventAmbush, p, "party"); err != nil {
			return nil, fmt.Errorf("seed participant: %w", err)
		}
	}
	if err := s.LinkEvents(ctx, cid, fx.EventSurvivors, fx.EventAmbush, LinkCaused); err != nil {
		return nil, fmt.Errorf("seed link: %w", err)
	}

	// The quest: a real state machine with a branch, partway through. The
	// branch is the point — trust or accuse the survivor — and the endings
	// are marked terminal so the quest checks read it honestly.
	q, err := s.CreateQuest(ctx, cid, QuestInput{
		Name:    "The Missing Miners",
		Summary: "Find the miners who vanished on the Greyfall road.",
		Machine: StateMachine{
			Initial: "unknown",
			States: []State{
				{Key: "unknown", Label: "Miners missing"},
				{Key: "wreckage_found", Label: "Wreckage found"},
				{Key: "survivors_found", Label: "Survivors found"},
				{Key: "trusted_survivor", Label: "Survivor trusted"},
				{Key: "accused_survivor", Label: "Survivor accused"},
				{Key: "cult_revealed", Label: "The cult revealed"},
				{Key: "assaulted_cult", Label: "The cult broken", Terminal: TerminalSuccess},
				{Key: "abandoned", Label: "The search abandoned", Terminal: TerminalAbandoned},
			},
			Edges: []StateEdge{
				{From: "unknown", To: "wreckage_found", Label: "find the caravan"},
				{From: "wreckage_found", To: "survivors_found", Label: "track the survivors"},
				{From: "survivors_found", To: "trusted_survivor", Label: "trust the survivor"},
				{From: "survivors_found", To: "accused_survivor", Label: "accuse the survivor"},
				{From: "trusted_survivor", To: "cult_revealed", Label: "follow the tip"},
				{From: "accused_survivor", To: "cult_revealed", Label: "break the story"},
				{From: "cult_revealed", To: "assaulted_cult", Label: "storm the chapterhouse"},
				{From: "cult_revealed", To: "abandoned", Label: "walk away"},
				{From: "unknown", To: "abandoned", Label: "give up the search"},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("seed quest: %w", err)
	}
	fx.QuestID = q.ID
	if _, err := s.TransitionQuest(ctx, cid, fx.QuestID, "wreckage_found", fx.EventAmbush); err != nil {
		return nil, fmt.Errorf("seed transition 1: %w", err)
	}
	if _, err := s.TransitionQuest(ctx, cid, fx.QuestID, "survivors_found", fx.EventSurvivors); err != nil {
		return nil, fmt.Errorf("seed transition 2: %w", err)
	}
	return fx, nil
}
