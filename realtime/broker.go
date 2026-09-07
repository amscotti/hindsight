package realtime

import "sync"

// historySize caps the per-board replay ring: reconnects can replay at
// most this many missed events before they must resync instead.
const historySize = 100

// subscriberQueue bounds how far one subscriber may lag. A full queue
// marks the subscriber dirty and queues a resync marker instead of
// silently dropping the gap or blocking the publisher.
const subscriberQueue = 32

// SyncRequiredName tells a client its stream has an unreplayable gap:
// it must refetch full board state.
const SyncRequiredName = "sync-required"

// Event is one broadcast message. ID is the per-board monotonic
// sequence number; Name selects the client swap target; HTML is the
// opaque fragment payload ("reload" for resync markers).
type Event struct {
	ID   uint64
	Name string
	HTML string
}

// subscriber is one live stream: a buffered queue plus a dirty flag
// recording that a resync marker is already queued behind the backlog.
type subscriber struct {
	ch    chan Event
	dirty bool
}

// boardState holds one board's sequence counter, replay ring, and live
// subscriber set. IDs start at 1; 0 means "no id" on subscribe. mu guards
// nextID, history, and subs (including each subscriber's dirty flag), so
// publishes to different boards proceed in parallel.
type boardState struct {
	mu      sync.Mutex
	nextID  uint64
	history []Event
	subs    map[*subscriber]struct{}
}

// Broker is the broadcast registry shared by all boards: mu guards only
// the board map, while each boardState has its own mutex guarding that
// board's sequence, history, and subscribers. Subscribe, Publish, and
// Unsubscribe are safe for concurrent use and Publish never blocks.
type Broker struct {
	mu     sync.Mutex
	boards map[string]*boardState
}

// NewBroker builds the registry shared by all boards.
func NewBroker() *Broker {
	return &Broker{boards: make(map[string]*boardState)}
}

// boardFor returns the board state, creating it on first use. The map
// mutex is held only for the lookup/insert.
func (b *Broker) boardFor(boardID string) *boardState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.boardLocked(boardID)
}
func (b *Broker) boardLocked(boardID string) *boardState {
	state, ok := b.boards[boardID]
	if !ok {
		state = &boardState{subs: make(map[*subscriber]struct{})}
		b.boards[boardID] = state
	}
	return state
}

// Publish appends the event to the board's ring, assigns the next
// per-board monotonic id, and relays it to every subscriber without
// blocking. It returns the assigned id.
func (b *Broker) Publish(boardID, name, html string) uint64 {
	state := b.boardFor(boardID)
	state.mu.Lock()
	defer state.mu.Unlock()

	state.nextID++
	ev := Event{ID: state.nextID, Name: name, HTML: html}
	if len(state.history) == historySize {
		copy(state.history, state.history[1:])
		state.history[len(state.history)-1] = ev
	} else {
		state.history = append(state.history, ev)
	}
	for sub := range state.subs {
		relay(sub, ev, state.nextID)
	}
	return ev.ID
}

// relay delivers one event without blocking. When the subscriber queue
// is full the subscriber is marked dirty and a resync marker is queued
// behind the backlog (dropping backlog slots only to fit the marker),
// so the client learns of the gap instead of missing it silently. Dirty
// subscribers stay quiet until they resubscribe.
func relay(sub *subscriber, ev Event, latest uint64) {
	if sub.dirty {
		return
	}
	select {
	case sub.ch <- ev:
		return
	default:
	}
	sub.dirty = true
	syncEv := Event{ID: latest, Name: SyncRequiredName, HTML: "reload"}
	for {
		select {
		case sub.ch <- syncEv:
			return
		default:
		}
		select {
		case <-sub.ch:
		default:
			return
		}
	}
}

// Subscribe registers a live stream for the board and reports what the
// reconnecting client missed after lastID: nothing when the board has no
// history yet or the client is already current, the buffered gap when it
// is fully replayable, and a single resync marker when the client is
// fresh (lastID 0 with history) or the gap overflowed the ring. A
// sync-required marker means "refetch full board state" regardless of
// the id it carries: the id is a best-effort framing value (the latest
// id when the marker was queued) and may lag the board's true latest
// once the subscriber is dirty. The replay is a copy the caller may keep.
func (b *Broker) Subscribe(boardID string, lastID uint64) (<-chan Event, []Event) {
	state := b.boardFor(boardID)
	state.mu.Lock()
	defer state.mu.Unlock()

	latest := state.nextID
	var replay []Event
	switch {
	case len(state.history) == 0:
		replay = []Event{}
	case lastID == 0:
		replay = []Event{{ID: latest, Name: SyncRequiredName, HTML: "reload"}}
	case lastID >= latest:
		replay = []Event{}
	case lastID+1 >= state.history[0].ID:
		for _, ev := range state.history {
			if ev.ID > lastID {
				replay = append(replay, ev)
			}
		}
		if replay == nil {
			replay = []Event{}
		}
	default:
		replay = []Event{{ID: latest, Name: SyncRequiredName, HTML: "reload"}}
	}

	sub := &subscriber{ch: make(chan Event, subscriberQueue)}
	state.subs[sub] = struct{}{}
	return sub.ch, replay
}

// SubscriberCount reports how many live streams watch the board.
// Tests use it to wait until a stream has subscribed before mutating,
// so live delivery assertions never race the subscribe itself.
func (b *Broker) SubscriberCount(boardID string) int {
	b.mu.Lock()
	state, ok := b.boards[boardID]
	b.mu.Unlock()
	if !ok {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.subs)
}

// Unsubscribe drops the subscriber and closes its channel so stream
// loops terminate. Unknown boards or channels are a no-op.
func (b *Broker) Unsubscribe(boardID string, ch <-chan Event) {
	b.mu.Lock()
	state, ok := b.boards[boardID]
	b.mu.Unlock()
	if !ok {
		return
	}
	state.mu.Lock()
	for sub := range state.subs {
		if (<-chan Event)(sub.ch) == ch {
			delete(state.subs, sub)
			close(sub.ch)
			// Evict idle entries that carry no replay value: boards are
			// bounded by the DB row count and entries with history are
			// kept for future reconnects, but a never-published board
			// holds no history and no subscribers.
			evict := len(state.subs) == 0 && len(state.history) == 0
			state.mu.Unlock()
			if evict {
				b.mu.Lock()
				if cur, ok := b.boards[boardID]; ok && cur == state {
					cur.mu.Lock()
					empty := len(cur.subs) == 0 && len(cur.history) == 0
					cur.mu.Unlock()
					if empty {
						delete(b.boards, boardID)
					}
				}
				b.mu.Unlock()
			}
			return
		}
	}
	state.mu.Unlock()
}
