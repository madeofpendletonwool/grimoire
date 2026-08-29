package knowledge

import (
	"context"
	"fmt"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/*
The player-visible quest journal (MAD-369).

ListQuests and GetQuest are DM-scope reads — a quest's whole shape is planning
material, and the branch the party has not taken is the campaign's biggest
single spoiler. The journal is the one player-facing read: the quests the DM
has marked public, where each one stands, and the states the party has already
walked through. Nothing else.

Concretely, an entry never carries: unvisited states (their keys or labels —
keys alone can leak "secretly_betrayed"), any state's detail, any edge's
label, detail or requires, or a secret-visibility quest's existence. The
projection is built field by field in this file, not by filtering the DM
shape, so a field added to Quest later cannot leak by accident.
*/

// QuestJournalEntry is one quest as the player journal renders it.
type QuestJournalEntry struct {
	QuestID      string              `json:"id"`
	Name         string              `json:"name"`
	Summary      string              `json:"summary"`
	Status       string              `json:"status"`
	CurrentState QuestJournalState   `json:"current_state"`
	Visited      []QuestJournalState `json:"visited"`
}

// QuestJournalState is one visited state: its key and its human label. The
// key is safe here precisely because the party has been in it.
type QuestJournalState struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// QuestJournal returns the campaign's player-visible quests: public
// visibility only, each with its current state's label and the states
// already visited — initial state first, then the moves in the order they
// happened. No unvisited branch, no state detail, no edge requires.
func (s *Store) QuestJournal(ctx context.Context, scope Scope, campaignID string) ([]QuestJournalEntry, error) {
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, summary, status, state_machine, current_state
		  FROM quests
		 WHERE campaign_id = ? AND visibility = 'public'
		 ORDER BY name COLLATE NOCASE`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("quest journal: %w", err)
	}
	type loaded struct {
		id, name, summary, status, current string
		machine                            campaign.StateMachine
	}
	var quests []loaded
	for rows.Next() {
		var (
			q           loaded
			machineJSON string
		)
		if err := rows.Scan(&q.id, &q.name, &q.summary, &q.status, &machineJSON, &q.current); err != nil {
			rows.Close()
			return nil, err
		}
		// A machine that no longer parses still journals: keys fall back to
		// themselves and the quest reads by name. The DM's own integrity
		// checks own the broken machine; the journal refuses to compound it.
		if m, err := campaign.ParseStateMachine(machineJSON); err == nil {
			q.machine = m
		}
		quests = append(quests, q)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]QuestJournalEntry, 0, len(quests))
	for _, q := range quests {
		entry := QuestJournalEntry{
			QuestID: q.id, Name: q.name, Summary: q.summary, Status: q.status,
		}
		label := func(key string) string {
			if st, ok := q.machine.State(key); ok && st.Label != "" {
				return st.Label
			}
			return key
		}
		entry.CurrentState = QuestJournalState{Key: q.current, Label: label(q.current)}

		visited := []string{q.machine.Initial}
		trows, err := s.db.QueryContext(ctx,
			`SELECT from_state, to_state FROM quest_transitions WHERE quest_id = ? ORDER BY rowid`, q.id)
		if err != nil {
			return nil, fmt.Errorf("quest journal transitions: %w", err)
		}
		for trows.Next() {
			var from, to string
			if err := trows.Scan(&from, &to); err != nil {
				trows.Close()
				return nil, err
			}
			visited = append(visited, from, to)
		}
		if err := trows.Err(); err != nil {
			trows.Close()
			return nil, err
		}
		trows.Close()

		seen := map[string]bool{}
		for _, key := range visited {
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			entry.Visited = append(entry.Visited, QuestJournalState{Key: key, Label: label(key)})
		}
		out = append(out, entry)
	}
	return out, nil
}

// questJournalViews is the shape the player view hands the portal: the same
// entries, through the narrow interface.
func (v *playerView) QuestJournal(ctx context.Context, campaignID string) ([]QuestJournalEntry, error) {
	return v.store.QuestJournal(ctx, v.scope, campaignID)
}
