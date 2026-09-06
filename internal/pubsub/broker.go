package pubsub

// The campaign pub/sub (MAD-423, stage 6 of MAD-417): one in-process
// broker, topics are campaigns. It grew out of the dice feed's broker
// (MAD-420) — the party board needed the same wake-on-write fan-out for
// every mechanical store, so the pattern became general and the dice
// store moved onto it.
//
// The broker carries no payloads. A notify is only a ping: each
// subscriber wakes and re-queries from its own stores with its own
// scope, so visibility stays enforced in the query and view layers
// exactly as if the reader had polled. That is the house leak posture —
// the push channel can never widen what a reader may see, only make the
// allowed read arrive sooner.
//
// Process-local by design: the app is one binary over embedded SQLite,
// and every stream keeps a slow poll as the safety net for a ping any
// subscriber misses (the dice stream's pattern).

import (
	"sort"
	"sync"
)

// sub is one open stream: its wake channel and, when the stream counts
// as presence, the user behind it.
type sub struct {
	ch   chan struct{}
	user string // empty when the subscription is anonymous (the dice feed)
}

// Broker fans pings out to per-campaign topics and tracks presence.
// The zero value works; New exists for symmetry with the stores.
type Broker struct {
	mu   sync.Mutex
	subs map[string]map[*sub]struct{}
}

// New returns a ready broker.
func New() *Broker {
	return &Broker{subs: make(map[string]map[*sub]struct{})}
}

// Subscribe wakes when anything notifies this campaign. The returned
// cancel must be called when the reader goes away. The channel is
// buffered to one and pings are lossy by design — a pending wake already
// means "re-query", so a second ping before the query runs changes
// nothing.
func (b *Broker) Subscribe(campaignID string) (<-chan struct{}, func()) {
	return b.subscribe(campaignID, "")
}

// SubscribeAs is Subscribe with presence: the user counts as connected
// to the campaign until cancel runs. Presence is the party board's
// "who is at the table" — one open board stream is being at the table,
// nothing more. A user with several open streams counts once.
func (b *Broker) SubscribeAs(campaignID, userID string) (<-chan struct{}, func()) {
	return b.subscribe(campaignID, userID)
}

func (b *Broker) subscribe(campaignID, userID string) (<-chan struct{}, func()) {
	s := &sub{ch: make(chan struct{}, 1), user: userID}
	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[string]map[*sub]struct{})
	}
	if b.subs[campaignID] == nil {
		b.subs[campaignID] = make(map[*sub]struct{})
	}
	b.subs[campaignID][s] = struct{}{}
	b.mu.Unlock()
	return s.ch, func() { b.drop(campaignID, s) }
}

// Present lists the users with at least one presence-carrying stream on
// this campaign, sorted for stable rendering.
func (b *Broker) Present(campaignID string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := make(map[string]struct{})
	for s := range b.subs[campaignID] {
		if s.user != "" {
			seen[s.user] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// Notify pings every subscriber on a campaign. It never blocks: a
// subscriber whose wake is already pending will re-query on that wake.
func (b *Broker) Notify(campaignID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs[campaignID] {
		select {
		case s.ch <- struct{}{}:
		default: // already pinged; the pending wake will re-query
		}
	}
}

func (b *Broker) drop(campaignID string, s *sub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if subs := b.subs[campaignID]; subs != nil {
		delete(subs, s)
		if len(subs) == 0 {
			delete(b.subs, campaignID)
		}
	}
}
