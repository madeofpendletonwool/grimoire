package campaign

// Rumours with truth values (MAD-374): the type layer. The tables are owned
// by migration 0024; the CRUD and every scoped read live in
// internal/knowledge (the epistemic layer — a rumour is a belief in
// circulation); the checks live in internal/canon. This file is only the
// shape both of those join against, the same split Fact carries.
//
// Truth is DM-only, always. A player-scope read returns the statement, the
// variant and who said it, and never the column — internal/knowledge
// enforces that in the SQL itself, and the reflection leak test asserts it.

import "time"

// The truth values a rumour carries. 'true' attests its fact (fact_id is
// the canon fact), 'false' contradicts or invents, 'distorted' twists its
// fact into something adjacent but wrong.
const (
	RumorTruthTrue      = "true"
	RumorTruthFalse     = "false"
	RumorTruthDistorted = "distorted"
)

// How far a rumour has travelled. The generator weights holder candidates
// by it; the DM reads it as reach.
const (
	RumorSpreadLocal      = "local"
	RumorSpreadRegional   = "regional"
	RumorSpreadWidespread = "widespread"
)

// The life of a rumour. 'circulating' is the default; 'debunked' and
// 'confirmed' are the DM's verdicts once the table settles it; 'dormant' is
// one nobody has repeated in a long while — rumor_dead_end's companion.
const (
	RumorStatusCirculating = "circulating"
	RumorStatusDebunked    = "debunked"
	RumorStatusConfirmed   = "confirmed"
	RumorStatusDormant     = "dormant"
)

// Rumor is one statement in circulation. FactID is the canon fact the
// rumour attests (truth 'true') or the one it distorts ('distorted'), and
// empty for a rumour invented whole. AboutEntity is who or where the
// rumour is about, when it is about one thing. DMOnly marks the rumour the
// DM planted for their own eyes: it never reaches a player scope at all.
type Rumor struct {
	ID          string
	CampaignID  string
	Statement   string
	Truth       string
	AboutEntity string
	FactID      string
	Origin      string
	Spread      string
	Status      string
	DMOnly      bool
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RumorHolder is who repeats a rumour, in their own words: Variant is the
// drifted wording this holder actually says, and SinceEvent the timeline
// event that first put the words in their mouth. EntityID is an entity id
// or the literal 'party' (a fact-less false rumour a knower carries is a
// holding here — awareness's fact foreign key cannot express it, a limit
// documented in migration 0024 rather than engineered around).
type RumorHolder struct {
	RumorID    string
	EntityID   string
	Variant    string
	SinceEvent string
	CreatedAt  time.Time
}
