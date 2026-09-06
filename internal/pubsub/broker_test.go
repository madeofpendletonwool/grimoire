package pubsub

import (
	"sync"
	"testing"
	"time"
)

// TestNotifyWakesSubscriber is the basic contract: a notify after
// subscribe pings, a notify before anyone listens does not.
func TestNotifyWakesSubscriber(t *testing.T) {
	b := New()
	wake, stop := b.Subscribe("c1")
	defer stop()

	b.Notify("c1")
	select {
	case <-wake:
	default:
		t.Fatal("the notify never pinged the subscriber")
	}

	// Pings are lossy: two notifies while no query runs leave one wake.
	b.Notify("c1")
	b.Notify("c1")
	n := 0
	for {
		select {
		case <-wake:
			n++
		default:
		}
		if n > 0 {
			break
		}
	}
	if n != 1 {
		t.Fatalf("expected one pending wake, got %d", n)
	}
}

// TestNotifyIsScopedToCampaign: a ping on one campaign never wakes
// another's subscriber.
func TestNotifyIsScopedToCampaign(t *testing.T) {
	b := New()
	wake, stop := b.Subscribe("c1")
	defer stop()
	b.Notify("c2")
	select {
	case <-wake:
		t.Fatal("a notify on another campaign woke this subscriber")
	default:
	}
}

// TestStopRemovesSubscriber: after cancel, a notify finds nobody and the
// broker's campaign bookkeeping is empty again.
func TestStopRemovesSubscriber(t *testing.T) {
	b := New()
	wake, stop := b.Subscribe("c1")
	stop()
	b.Notify("c1") // must not panic on the dropped campaign entry
	select {
	case <-wake:
		t.Fatal("a cancelled subscriber was pinged")
	default:
	}
	if len(b.subs) != 0 {
		t.Fatalf("campaign bookkeeping not cleaned: %+v", b.subs)
	}
}

// TestPresenceCountsOpenStreams: presence is the set of users with at
// least one SubscribeAs stream, deduped, sorted, and released by the
// last cancel only. Anonymous subscriptions never count.
func TestPresenceCountsOpenStreams(t *testing.T) {
	b := New()
	_, stopAda1 := b.SubscribeAs("c1", "ada")
	_, stopAda2 := b.SubscribeAs("c1", "ada")
	_, stopBob := b.SubscribeAs("c1", "bob")
	_, stopAnon := b.Subscribe("c1")
	defer stopAnon()

	if got := b.Present("c1"); len(got) != 2 || got[0] != "ada" || got[1] != "bob" {
		t.Fatalf("presence = %v, want [ada bob]", got)
	}

	// One of ada's two streams closing keeps her present.
	stopAda1()
	if got := b.Present("c1"); len(got) != 2 {
		t.Fatalf("presence after one of two streams closed = %v, want both users", got)
	}

	stopAda2()
	if got := b.Present("c1"); len(got) != 1 || got[0] != "bob" {
		t.Fatalf("presence after ada left = %v, want [bob]", got)
	}

	stopBob()
	if got := b.Present("c1"); len(got) != 0 {
		t.Fatalf("presence after everyone left = %v, want empty", got)
	}
}

// TestPresenceIsScopedToCampaign: streaming on one campaign does not
// make a user present on another.
func TestPresenceIsScopedToCampaign(t *testing.T) {
	b := New()
	_, stop := b.SubscribeAs("c1", "ada")
	defer stop()
	if got := b.Present("c2"); len(got) != 0 {
		t.Fatalf("presence leaked across campaigns: %v", got)
	}
}

// TestConcurrentSubscribeNotify drives the broker from several
// goroutines at once — the race suite's target, and the shape the SSE
// handlers impose (streams open and close while writers notify).
func TestConcurrentSubscribeNotify(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wake, stop := b.SubscribeAs("c1", "user")
			defer stop()
			b.Notify("c1")
			select {
			case <-wake:
			case <-time.After(2 * time.Second):
				t.Error("no wake under concurrency")
			}
		}(i)
	}
	wg.Wait()
	if got := b.Present("c1"); len(got) != 0 {
		t.Fatalf("presence survived all cancels: %v", got)
	}
}
