package campaign

// Handouts and maps (MAD-490): the type layer. The table is owned by
// migration 0039; the CRUD and every scoped read live in
// internal/knowledge (the visibility layer — a handout is material the DM
// hands to the party, which is a scope question); the routes live in
// internal/server. This file is only the shape both of those join against,
// the same split Rumor carries.
//
// A handout is deliberately not a graph node: no entity row, no facts, no
// awareness. Draft and retired rows exist only for the DM; every player
// read path filters on status = 'published' in the SQL itself, the same
// rule internal/knowledge enforces for every other scoped read.

import "time"

// What a handout carries. 'handout' is reading material (markdown body,
// optionally an image); 'map' is an image with optional notes under it.
const (
	HandoutKindHandout = "handout"
	HandoutKindMap     = "map"
)

// The lifecycle. Draft is DM-only; published is readable by every member;
// retired leaves the portal but keeps the row for history — the DM's
// record of what the party was once handed.
const (
	HandoutStatusDraft     = "draft"
	HandoutStatusPublished = "published"
	HandoutStatusRetired   = "retired"
)

// Handout is one piece of party-scope material. Body is markdown; the
// image columns reference a file in the handouts directory beside the
// database (never a URL — nothing loads from a third party). PublishedAt
// is the last hand-out, zero when the handout has never been published.
type Handout struct {
	ID          string
	CampaignID  string
	Kind        string
	Title       string
	Body        string
	ImageRef    string
	ImageMime   string
	ImageBytes  int64
	Status      string
	CreatedBy   string
	CreatedAt   time.Time
	PublishedAt time.Time
	UpdatedAt   time.Time
}
