// Package place assembles the structured location dossier (MAD-370): the
// one reading of a place that joins the payload's place block with the graph
// around it. It is a pure assembly over an already-loaded snapshot — no DB,
// no clock, no network — the same shape the integrity rules follow
// (campaign.Check over campaign.LoadSnapshot), which is what makes it
// testable as data.
//
// Two dossiers, one function: the DM's snapshot is campaign.LoadSnapshot's
// whole graph; a player's is the scoped snapshot internal/knowledge builds
// from its own reads. The assembly never decides visibility — secrets,
// private truth and unmet NPCs are absent from the player dossier because
// the rows were never loaded into that snapshot, not because a field was
// blanked here.
package place

import (
	"sort"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

// QuestRef is one quest sited at the place, as the dossier chips it.
type QuestRef struct {
	ID      string
	Name    string
	Summary string
	Status  string
}

// Location is the structured location the dossier returns: the block, and
// everything the graph says about the place, joined. Every list is derived,
// never stored — adding a located_in edge or a secret fact changes the
// dossier with no write to the location entity, which is the rule the
// dossier exists to keep.
type Location struct {
	Location campaign.Entity   // the location itself
	Place    campaign.Place    // the decoded block; the zero Place is valid
	Present  []campaign.Entity // NPCs and creatures here, by edge
	Children []campaign.Entity // locations inside or under this one
	Routes   []campaign.Route  // the travel block's routes out; empty when there is no block
	Items    []campaign.Entity // items here, by edge
	Secrets  []campaign.Fact   // live secret facts about this place, as subject or object
	Events   []campaign.Event  // events sited here
	Quests   []QuestRef        // quests whose site this place is
	// Rumours circulating here. Empty until MAD-374 lands; the field exists
	// so the surfaces ship with the shape and the reading arrives once.
	Rumours []string
}

// Dossier assembles one location's dossier from a snapshot. ok is false
// when the entity is missing from the snapshot, is not a location, or is
// soft-deleted — the caller's 404 or 400, not an error here. An edge whose
// far end the snapshot does not carry (an unmet NPC at a player scope) is
// skipped: the dossier lists who the snapshot knows, never who the graph
// hides.
func Dossier(snap *campaign.Snapshot, entityID string) (Location, bool) {
	var d Location
	if snap == nil {
		return d, false
	}
	entities := make(map[string]campaign.Entity, len(snap.Entities))
	for _, e := range snap.Entities {
		entities[e.ID] = e
	}
	loc, ok := entities[entityID]
	if !ok || loc.Kind != campaign.KindLocation || loc.Status == campaign.StatusDeleted {
		return d, false
	}
	d.Location = loc
	d.Place = campaign.PlaceOf(&loc)
	d.Routes = campaign.RoutesOf(&loc)

	live := func(id string) (campaign.Entity, bool) {
		e, ok := entities[id]
		if !ok || e.Status == campaign.StatusDeleted || id == entityID {
			return e, false
		}
		return e, true
	}
	for _, r := range snap.Relationships {
		if r.RelType != "located_in" && r.RelType != "contains" {
			continue // the dossier reads placement edges only; the rest is the sheet's Ties list
		}
		var other string
		switch entityID {
		case r.FromEntity:
			other = r.ToEntity
		case r.ToEntity:
			other = r.FromEntity
		default:
			continue
		}
		e, ok := live(other)
		if !ok {
			continue
		}
		switch {
		case e.Kind == campaign.KindLocation:
			d.Children = append(d.Children, e)
		case e.Kind == campaign.KindItem:
			d.Items = append(d.Items, e)
		case e.Kind == campaign.KindNPC || e.Kind == campaign.KindCreature:
			d.Present = append(d.Present, e)
		}
	}
	sortByName := func(es []campaign.Entity) {
		sort.Slice(es, func(i, j int) bool {
			if es[i].Name != es[j].Name {
				return es[i].Name < es[j].Name
			}
			return es[i].ID < es[j].ID
		})
	}
	sortByName(d.Present)
	sortByName(d.Children)
	sortByName(d.Items)

	for _, f := range snap.Facts {
		if f.Visibility != campaign.VisibilitySecret || f.SupersededBy != "" || f.Confidence == campaign.ConfidenceProposed {
			continue // live secrets only: retconned history and proposals are not current truth
		}
		if f.SubjectEntity == entityID || f.ObjectEntity == entityID {
			d.Secrets = append(d.Secrets, f)
		}
	}
	sort.Slice(d.Secrets, func(i, j int) bool {
		if d.Secrets[i].CreatedAt != d.Secrets[j].CreatedAt {
			return d.Secrets[i].CreatedAt.Before(d.Secrets[j].CreatedAt)
		}
		return d.Secrets[i].ID < d.Secrets[j].ID
	})

	for _, e := range snap.Events {
		if e.LocationEntity == entityID {
			d.Events = append(d.Events, e)
		}
	}
	sort.Slice(d.Events, func(i, j int) bool {
		if d.Events[i].RealOrdinal != d.Events[j].RealOrdinal {
			return d.Events[i].RealOrdinal < d.Events[j].RealOrdinal
		}
		return d.Events[i].ID < d.Events[j].ID
	})

	quests := make(map[string]campaign.Quest, len(snap.Quests))
	for _, q := range snap.Quests {
		quests[q.ID] = q
	}
	for _, l := range snap.QuestEntities {
		if l.EntityID != entityID || l.Role != campaign.QuestRoleSite {
			continue
		}
		if q, ok := quests[l.QuestID]; ok {
			d.Quests = append(d.Quests, QuestRef{ID: q.ID, Name: q.Name, Summary: q.Summary, Status: q.Status})
		}
	}
	sort.Slice(d.Quests, func(i, j int) bool {
		if d.Quests[i].Name != d.Quests[j].Name {
			return d.Quests[i].Name < d.Quests[j].Name
		}
		return d.Quests[i].ID < d.Quests[j].ID
	})

	if d.Rumours == nil {
		d.Rumours = []string{}
	}
	return d, true
}
