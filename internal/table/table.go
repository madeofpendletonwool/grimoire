// Package table is the table screen (MAD-425, stage 8 of MAD-417): one
// URL, cast to the TV or projected — the room's public surface of the
// same state the party board serves.
//
// The screen is a reference, not a snapshot: the token resolves to the
// campaign and every read re-derives state live, so the projector never
// needs a refresh. What it may read is decided the way every mechanical
// surface here decides — by construction, while the view structs are
// built, the knowledge layer's PlayerView pattern applied one more
// time:
//
//	the party strips   the board's observer standing — the campaign's
//	                   visibility config with no self exception, so a
//	                   word-mode table never spells a number
//	the battle         the public shape only: round, turn, the order —
//	                   names and initiative, which were rolled in the open
//	the other side     nothing, until the DM reveals a foe — then exactly
//	                   what the reveal says: hit points or the health word
//	the dice           the public feed, the same query a player's feed
//	                   window runs — a secret roll is absent, not blanked
//
// The narrow interfaces below carry that posture in their types: the
// rolls window has no dm parameter to pass, and the wide dice store
// reaches this package only through an adapter that drops it. A leaky
// read cannot be written, not just should not be.
package table

import (
	"context"

	"github.com/madeofpendletonwool/grimoire/internal/board"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
)

/* ---------- the narrow reads (the PlayerView pattern, once more) ---------- */

// Campaigns resolves a campaign's name.
type Campaigns interface {
	GetCampaign(ctx context.Context, id string) (*campaign.Campaign, error)
}

// Boards is the party board's window: one snapshot for one standing.
// The screen always asks as an observer — board.PlayerStanding() with
// no owned characters — so the strips arrive in the campaign's config
// shape with no self exception to claim.
type Boards interface {
	Snapshot(ctx context.Context, campaignID string, viewer board.Standing) (*board.Snapshot, error)
}

// Combats is the tracker's window: the active battle and its order.
type Combats interface {
	Active(ctx context.Context, campaignID string) (*combat.Combat, []combat.Combatant, error)
}

// Rolls is the dice engine's public-only window. The wide store's Feed
// takes a dm flag; this interface does not have one, which is the whole
// point — the screen's dice cannot be asked for a secret.
type Rolls interface {
	// Feed lists the campaign's public rolls past after; after 0 is
	// the newest window.
	Feed(ctx context.Context, campaignID string, after int64, limit int) ([]dice.RollRow, error)
	// LatestSeq is the highest public roll seq — the resume cursor.
	LatestSeq(ctx context.Context, campaignID string) (int64, error)
}

// PublicRolls adapts the wide dice store onto the public-only window:
// every read passes dm=false, and no caller of this package can change
// that.
func PublicRolls(s *dice.Store) Rolls {
	return publicRolls{s}
}

type publicRolls struct{ s *dice.Store }

func (p publicRolls) Feed(ctx context.Context, campaignID string, after int64, limit int) ([]dice.RollRow, error) {
	return p.s.Feed(ctx, campaignID, after, limit, false)
}

func (p publicRolls) LatestSeq(ctx context.Context, campaignID string) (int64, error) {
	return p.s.LatestSeq(ctx, campaignID, false)
}

/* ---------- the views ---------- */

// Monster is one revealed foe on the room's screen. The DM's reveal
// decides the shape: exact hit points, or the table's health word. An
// unrevealed foe never reaches this struct — the projector shows its
// name in the order and nothing else.
type Monster struct {
	Name   string `json:"name"`
	HP     *int   `json:"hp,omitempty"`     // reveal mode hp
	MaxHP  *int   `json:"max_hp,omitempty"` // reveal mode hp
	Health string `json:"health,omitempty"` // reveal mode word
	Down   bool   `json:"down,omitempty"`
	Dead   bool   `json:"dead,omitempty"`
}

// Roll is one public die the room just watched land. It is the feed
// view's lean shape: no ids, no visibility field (everything here is
// public by construction), nothing the projection needs to know.
type Roll struct {
	Seq           int64  `json:"seq"`
	ActorName     string `json:"actor_name,omitempty"`
	CharacterName string `json:"character_name,omitempty"`
	Detail        string `json:"detail,omitempty"`
	Notation      string `json:"notation"`
	Total         int    `json:"total"`
	Natural20     bool   `json:"natural_20,omitempty"`
	Natural1      bool   `json:"natural_1,omitempty"`
}

// Snapshot is one screen read: the campaign's name, the party in the
// config's observer shape, the public battle, the revealed foes, and —
// on the first paint — the newest public rolls with the cursor they
// leave the stream holding. The steady-state stream read omits the
// rolls (they ride their own events), so two identical views serialize
// identically for the change detection.
type Snapshot struct {
	Campaign string            `json:"campaign"`
	Members  []board.Member    `json:"members"`
	Combat   *board.CombatView `json:"combat,omitempty"`
	Monsters []Monster         `json:"monsters,omitempty"`
	Rolls    []Roll            `json:"rolls,omitempty"`
	Latest   int64             `json:"latest,omitempty"`
}

// rollWindow bounds the first paint's dice: the last few throws, big.
const rollWindow = 8

// View derives the screen's board half: strips, battle, revealed foes.
// It is the steady-state read the stream change-detects on.
func (s *Store) View(ctx context.Context, campaignID string) (*Snapshot, error) {
	c, err := s.campaigns.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	bs, err := s.boards.Snapshot(ctx, campaignID, observerStanding)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Campaign: c.Name, Members: bs.Members, Combat: bs.Combat}
	// Presence is the members' business — who holds a stream open is
	// not the room's screen to say.
	for i := range snap.Members {
		snap.Members[i].Present = false
	}

	if s.combats != nil {
		fight, order, err := s.combats.Active(ctx, campaignID)
		if err != nil {
			return nil, err
		}
		if fight != nil {
			for i := range order {
				foe := &order[i]
				if foe.Side != combat.SideFoe || foe.Reveal == combat.RevealOff {
					continue
				}
				m := Monster{Name: foe.Name, Down: foe.Downed && !foe.Dead, Dead: foe.Dead}
				switch foe.Reveal {
				case combat.RevealHP:
					hp, max := foe.HP, foe.EffectiveMax()
					m.HP, m.MaxHP = &hp, &max
				case combat.RevealWord:
					m.Health = board.HealthWord(foe.HP, foe.EffectiveMax(), foe.Downed, foe.Dead)
				}
				snap.Monsters = append(snap.Monsters, m)
			}
		}
	}
	return snap, nil
}

// Snapshot is the first paint: the view plus the newest public rolls
// and the cursor the stream resumes from.
func (s *Store) Snapshot(ctx context.Context, campaignID string) (*Snapshot, error) {
	snap, err := s.View(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	if s.rolls != nil {
		rows, err := s.rolls.Feed(ctx, campaignID, 0, rollWindow)
		if err != nil {
			return nil, err
		}
		snap = snap.withRolls(rows)
		var latest int64
		for _, r := range rows {
			if r.Seq > latest {
				latest = r.Seq
			}
		}
		if latest == 0 {
			if latest, err = s.rolls.LatestSeq(ctx, campaignID); err != nil {
				return nil, err
			}
		}
		snap.Latest = latest
	}
	return snap, nil
}

// withRolls copies the view beside its dice — the steady-state read
// must not mutate the shared strip slice the board built.
func (s *Snapshot) withRolls(rows []dice.RollRow) *Snapshot {
	out := &Snapshot{
		Campaign: s.Campaign, Members: s.Members,
		Combat: s.Combat, Monsters: s.Monsters,
	}
	for _, r := range rows {
		out.Rolls = append(out.Rolls, toRoll(r))
	}
	return out
}

// RollsAfter lists the public rolls past the cursor, oldest first, with
// the new cursor — the stream's dice half.
func (s *Store) RollsAfter(ctx context.Context, campaignID string, after int64, limit int) ([]Roll, int64, error) {
	if s.rolls == nil {
		return nil, after, nil
	}
	rows, err := s.rolls.Feed(ctx, campaignID, after, limit)
	if err != nil {
		return nil, after, err
	}
	out := make([]Roll, 0, len(rows))
	for _, r := range rows {
		out = append(out, toRoll(r))
		if r.Seq > after {
			after = r.Seq
		}
	}
	return out, after, nil
}

// observerStanding is the screen's fixed position: an observer with no
// character of their own, reading the campaign's configured shape.
var observerStanding = board.PlayerStanding()

// toRoll renders one public roll for the room: who threw it, what the
// dice were, and the total the table gasped at.
func toRoll(r dice.RollRow) Roll {
	v := Roll{
		Seq: r.Seq, ActorName: r.ActorName, CharacterName: r.CharacterName,
		Detail: r.Detail, Notation: r.Notation(),
	}
	if r.Result != nil {
		v.Total, v.Natural20, v.Natural1 = r.Result.Total, r.Result.Natural20, r.Result.Natural1
	}
	return v
}
