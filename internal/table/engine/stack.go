package engine

// The stack and its trigger queue (MAD-325). Objects go onto the stack in
// three modes — cast (a real object), activated, triggered (declarations)
// — and come off it top first as players pass; what a resolving ability
// does is the effect declared when it went on, applied as an
// EFFECT_DECLARED row: the engine owns ordering, timing and arithmetic,
// never oracle text (ADR 11).
//
// Triggered abilities never jump straight onto the stack. A declaration
// lands as TRIGGER_FIRED into the queue; the queue drains in APNAP order
// (CR 603.3b: the active player's triggers first, then each other player
// in turn order) at the next priority grant, which CR 117.5 places after
// the state-based actions — so triggers that fire mid-resolution wait for
// the resolution's own priority grant rather than interleaving.

import (
	"fmt"
	"sort"
	"strings"
)

// applyDeclareTrigger records a triggered ability: the table (today) or
// the trigger registry (MAD-335, against structural events) says "this
// triggers", and the row enters the queue to reach the stack at the next
// priority grant. Declaring a trigger holds no priority and spends none.
func applyDeclareTrigger(s *State, a Action) ([]Event, error) {
	if err := s.requireActive(a); err != nil {
		return nil, err
	}
	if err := s.requireSeat(a); err != nil {
		return nil, err
	}
	effect := strings.TrimSpace(a.Effect)
	if effect == "" {
		return nil, fmt.Errorf("%w: DECLARE_TRIGGER needs its effect declared", ErrInvalid)
	}
	if a.Object != 0 {
		if _, err := s.object(a.Object); err != nil {
			return nil, err
		}
	}
	return []Event{a.stamp(Event{Kind: EventTriggerFired, Object: a.Object, Card: a.Card,
		Effect: effect, Controller: a.Seat, Targets: a.Targets})}, nil
}

// flushTriggers drains the trigger queue onto the stack in APNAP order.
// It runs when the position hands priority to someone (PrioritySeat != 0):
// every grant is preceded by the CR 117.5 check, and a queue that is still
// holding anything while a player holds priority is a queue that must
// drain now. When nobody holds priority — untap, cleanup — the queue
// waits, exactly the "fires mid-resolution, stacks at the next grant"
// contract. Engine-asserted rows: the pushes ride the grant already made
// and hand priority to no one.
func (s *State) flushTriggers() []Event {
	if s.Status != StatusActive || s.PrioritySeat == 0 || len(s.TriggerQueue) == 0 {
		return nil
	}
	var evs []Event
	for _, it := range s.apnapQueue() {
		sys := Action{Kind: ActionDeclareTrigger, Seat: it.Controller, Source: "system"}
		evs = append(evs, sys.stamp(Event{Kind: EventStackPushed, Mode: "triggered",
			Card: it.Card, Ability: it.Effect, Controller: it.Controller, Targets: it.Targets}))
	}
	return evs
}

// apnapQueue orders the waiting triggers for the stack: the active
// player's first, then each other player's in turn order — the sort key is
// the player's distance from the active player in turn order. The sort is
// stable, so one player's own triggers keep declaration order, which is
// the order they chose them in (CR 603.3b). Pushed first means lowest on
// the stack means resolves last.
func (s *State) apnapQueue() []TriggerItem {
	out := append([]TriggerItem{}, s.TriggerQueue...)
	n := len(s.Order)
	start := 0
	for i, seat := range s.Order {
		if seat == s.TurnSeat {
			start = i
			break
		}
	}
	dist := make(map[int]int, n)
	for i, seat := range s.Order {
		dist[seat] = (i - start + n) % n
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, iok := dist[out[i].Controller]
		dj, jok := dist[out[j].Controller]
		if !iok {
			di = n + 1
		}
		if !jok {
			dj = n + 1
		}
		return di < dj
	})
	return out
}

// drainQueuedTrigger removes the queue entry a triggered STACK_PUSHED
// corresponds to: exact match first, then the controller's earliest — the
// flush pushes one row per queued item in order, so this always finds its
// own unless the log is foreign, in which case the fold stays total and
// the push simply stands.
func (s *State) drainQueuedTrigger(item StackItem) {
	exact := -1
	byController := -1
	for i, q := range s.TriggerQueue {
		if q.Controller == item.Controller {
			if byController < 0 {
				byController = i
			}
			if q.Effect == item.Ability && q.Card == item.Card {
				exact = i
				break
			}
		}
	}
	if exact < 0 {
		exact = byController
	}
	if exact < 0 {
		return
	}
	s.TriggerQueue = append(s.TriggerQueue[:exact], s.TriggerQueue[exact+1:]...)
}
