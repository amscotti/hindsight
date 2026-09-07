package realtime

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// nextEvent reads one event, failing when the subscriber goes quiet.
func nextEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()

	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("subscriber channel closed while events were expected")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for subscriber event")
		return Event{}
	}
}

func TestTwoSubscribersReceiveInOrder(t *testing.T) {
	b := NewBroker()

	first, _ := b.Subscribe("board-1", 0)
	second, _ := b.Subscribe("board-1", 0)

	published := []Event{
		{Name: "column-1", HTML: "<div>one</div>"},
		{Name: "column-1", HTML: "<div>two</div>"},
		{Name: "card-updated:9", HTML: "<div>three</div>"},
	}
	for _, ev := range published {
		b.Publish("board-1", ev.Name, ev.HTML)
	}

	for _, ch := range []<-chan Event{first, second} {
		for i, want := range published {
			got := nextEvent(t, ch)
			if got.ID != uint64(i+1) || got.Name != want.Name || got.HTML != want.HTML {
				t.Errorf("subscriber got %+v, want id=%d name=%q html=%q",
					got, i+1, want.Name, want.HTML)
			}
		}
		select {
		case ev := <-ch:
			t.Errorf("subscriber received unexpected extra event %+v", ev)
		default:
		}
	}

	b.Unsubscribe("board-1", first)
	b.Unsubscribe("board-1", second)
}

func TestUnsubscribeCleansUpWithoutGoroutineLeak(t *testing.T) {
	b := NewBroker()
	baseline := runtime.NumGoroutine()

	const subscribers = 50
	chans := make([]<-chan Event, 0, subscribers)
	for range subscribers {
		ch, _ := b.Subscribe("board-1", 0)
		chans = append(chans, ch)
	}
	b.Publish("board-1", "column-1", "<div>hi</div>")
	for _, ch := range chans {
		b.Unsubscribe("board-1", ch)
	}

	// Closed channels drain their last queued event, then report closed.
	timeout := time.After(2 * time.Second)
	for _, ch := range chans {
	drain:
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					break drain
				}
			case <-timeout:
				t.Fatalf("subscriber channel not closed after unsubscribe")
			}
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d, want <= baseline %d after dropping 50 subscribers",
				runtime.NumGoroutine(), baseline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReconnectReplaysExactlyTheGap(t *testing.T) {
	b := NewBroker()

	for _, name := range []string{"e1", "e2", "e3", "e4", "e5"} {
		b.Publish("board-1", name, "<div>"+name+"</div>")
	}

	live, replay := b.Subscribe("board-1", 2)
	if len(replay) != 3 {
		t.Fatalf("replay len = %d, want exactly the 3 missed events", len(replay))
	}
	for i, want := range []string{"e3", "e4", "e5"} {
		if replay[i].ID != uint64(i+3) || replay[i].Name != want {
			t.Errorf("replay[%d] = %+v, want id=%d name=%q", i, replay[i], i+3, want)
		}
	}

	// The same subscription keeps streaming live events after the replay.
	b.Publish("board-1", "e6", "<div>e6</div>")
	if got := nextEvent(t, live); got.ID != 6 || got.Name != "e6" {
		t.Errorf("live event = %+v, want id=6 name=e6", got)
	}
	b.Unsubscribe("board-1", live)

	// A client that is already current gets an empty replay.
	if _, replay := b.Subscribe("board-1", 6); len(replay) != 0 {
		t.Errorf("current replay = %+v, want empty", replay)
	}

	// A fresh connect with no id must resync instead of guessing.
	if _, replay := b.Subscribe("board-1", 0); len(replay) != 1 ||
		replay[0].Name != SyncRequiredName {
		t.Errorf("fresh replay = %+v, want one %q event", replay, SyncRequiredName)
	}

	// An id that fell out of the ring must resync instead of replaying
	// a partial gap.
	for range 120 {
		b.Publish("board-2", "tick", "<div>tick</div>")
	}
	if _, replay := b.Subscribe("board-2", 1); len(replay) != 1 ||
		replay[0].Name != SyncRequiredName {
		t.Errorf("evicted replay = %+v, want one %q event", replay, SyncRequiredName)
	}
}

func TestSlowSubscriberGetsSyncRequired(t *testing.T) {
	b := NewBroker()

	ch, _ := b.Subscribe("board-1", 0)

	// A lagging subscriber must never stall the publisher.
	const total = 64
	for i := range total {
		_ = i
		b.Publish("board-1", "column-1", "<div>burst</div>")
	}

	// Everything queued before the resync marker keeps arrival order
	// with strictly increasing ids.
	var lastID uint64
	sawSync := false
	timeout := time.After(2 * time.Second)
drain:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("subscriber channel closed while backlog was expected")
			}
			if ev.Name == SyncRequiredName {
				sawSync = true
				break drain
			}
			if ev.ID <= lastID {
				t.Fatalf("out-of-order backlog id %d after %d", ev.ID, lastID)
			}
			lastID = ev.ID
		case <-timeout:
			t.Fatalf("slow subscriber never received %q", SyncRequiredName)
		}
	}
	if !sawSync {
		t.Fatalf("backlog contained no %q marker", SyncRequiredName)
	}

	// Once marked dirty the subscriber is quiet until it resubscribes:
	// later publishes add nothing behind the marker.
	for range 5 {
		b.Publish("board-1", "column-1", "<div>later</div>")
	}
	select {
	case ev := <-ch:
		t.Errorf("dirty subscriber received %+v, want silence after %q", ev, SyncRequiredName)
	default:
	}

	b.Unsubscribe("board-1", ch)
}

func TestFreshSubscribeOnEmptyBoardSendsNoStaleID(t *testing.T) {
	b := NewBroker()

	ch, replay := b.Subscribe("fresh-board", 0)
	if len(replay) != 0 {
		t.Fatalf("empty-board replay = %+v, want empty (no id: 0 marker)", replay)
	}
	b.Unsubscribe("fresh-board", ch)

	// The idle empty entry is evicted; entries with history are kept
	// for future reconnects.
	b.mu.Lock()
	_, kept := b.boards["fresh-board"]
	b.mu.Unlock()
	if kept {
		t.Fatalf("empty idle board entry was not evicted")
	}
}

func TestPublishIDsMonotonicPerBoard(t *testing.T) {
	b := NewBroker()

	const events = 5
	aIDs := make([]uint64, 0, events)
	cIDs := make([]uint64, 0, events)
	for range events {
		aIDs = append(aIDs, b.Publish("board-a", "column-1", "<div>a</div>"))
		cIDs = append(cIDs, b.Publish("board-c", "column-1", "<div>c</div>"))
	}

	for i := range events {
		if want := uint64(i + 1); aIDs[i] != want || cIDs[i] != want {
			t.Fatalf("board ids = %v / %v, want each board numbered 1..%d",
				aIDs, cIDs, events)
		}
	}
	for i := 1; i < events; i++ {
		if aIDs[i] <= aIDs[i-1] || cIDs[i] <= cIDs[i-1] {
			t.Fatalf("ids not strictly monotonic: %v / %v", aIDs, cIDs)
		}
	}
}

func TestConcurrentPublishKeepsIDsUnique(t *testing.T) {
	b := NewBroker()

	const writers = 8
	const perWriter = 25
	ids := make(chan uint64, writers*perWriter)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				ids <- b.Publish("board-1", "column-1", "<div>x</div>")
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := map[uint64]bool{}
	var max uint64
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate publish id %d", id)
		}
		seen[id] = true
		if id > max {
			max = id
		}
	}
	if len(seen) != writers*perWriter || max != writers*perWriter {
		t.Fatalf("collected %d ids with max %d, want %d contiguous ids",
			len(seen), max, writers*perWriter)
	}
}
