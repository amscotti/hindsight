package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hindsight/db"
	"hindsight/realtime"
)

// eventBroker is the consumer-side realtime contract: the stream below
// subscribes and unsubscribes only. *realtime.Broker implements it;
// tests substitute a real broker over throwaway boards.
type eventBroker interface {
	Subscribe(boardID string, lastID uint64) (<-chan realtime.Event, []realtime.Event)
	Unsubscribe(boardID string, ch <-chan realtime.Event)
}

// defaultHeartbeat paces the SSE comment keeping idle streams (and
// tunnel proxies) from going quiet.
const defaultHeartbeat = 12 * time.Second

// Events streams board updates as server-sent events. Strangers without
// a participant or facilitator cookie get 403 plus the flash fragment
// (join first); unknown boards are a plain 404. Fresh connects and
// unreplayable gaps open with a resync marker; replayable gaps replay
// in order before live events. Every write is flushed.
func (b *Boards) Events(w http.ResponseWriter, r *http.Request) {
	bid := r.PathValue("bid")
	board, err := b.store.GetBoardByPublicID(r.Context(), bid)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load board", http.StatusInternalServerError)
		return
	}
	_, facilitator := b.facilitatorToken(r, board)
	if b.participant(r, board) == nil && !facilitator {
		writeFlash(w, http.StatusForbidden, "Join the board first.")
		return
	}
	if b.broker == nil {
		http.Error(w, "realtime unavailable", http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	var lastID uint64
	// A malformed Last-Event-ID is treated as a fresh connect (lastID 0):
	// the broker then replays from history or opens with sync-required.
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if parsed, perr := strconv.ParseUint(raw, 10, 64); perr == nil {
			lastID = parsed
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	interval := b.heartbeat
	if interval <= 0 {
		interval = defaultHeartbeat
	}
	// The server-wide WriteTimeout (30s in main.go) covers the whole
	// response and would cut the stream mid-session. Every write re-arms
	// the deadline two heartbeats ahead, so a live stream runs for the
	// whole session while a dead peer still disconnects on schedule.
	// ResponseWriters without deadline support (handler tests) ignore
	// the extension error and keep the surrounding deadline.
	rc := http.NewResponseController(w)
	extend := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(2 * interval))
	}

	ch, replay := b.broker.Subscribe(board.PublicID, lastID)
	defer b.broker.Unsubscribe(board.PublicID, ch)
	// A subscribe is a board read: an expired timer disarms exactly
	// once here and broadcasts to every subscriber, this one included.
	// The check runs after subscribing so the expiry reaches this
	// stream live instead of hiding behind the opening resync marker.
	b.checkTimerExpiry(r, board)
	for _, ev := range replay {
		// Blind collection filters here, at the stream edge: the
		// broker stays role-free and every replayed message gets
		// the same per-viewer treatment as live ones. Filtered
		// events still advance the client's Last-Event-ID with an
		// id-only block (no dispatch, per SSE), so reconnects do
		// not re-request history the viewer can never see.
		extend()
		if out, ok := filterStreamEvent(ev, facilitator); ok {
			if err := writeStreamEvent(w, out); err != nil {
				return
			}
			flusher.Flush()
		} else if err := writeSkipEvent(w, ev.ID); err != nil {
			return
		} else {
			flusher.Flush()
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			extend()
			if out, ok := filterStreamEvent(ev, facilitator); ok {
				if err := writeStreamEvent(w, out); err != nil {
					return
				}
				flusher.Flush()
			} else if err := writeSkipEvent(w, ev.ID); err != nil {
				return
			} else {
				flusher.Flush()
			}
		case <-ticker.C:
			extend()
			if _, err := io.WriteString(w, ":ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
			// Each heartbeat tick is a board read too: a timer armed
			// after subscribing still expires on time for connected
			// viewers, and a missed in-memory fire heals here. The
			// conditional disarm keeps concurrent ticks to one
			// broadcast.
			if fresh, err := b.store.GetBoardByPublicID(r.Context(), bid); err == nil {
				b.checkTimerExpiry(r, fresh)
			}
		}
	}
}

// writeSkipEvent advances a filtered-out viewer's Last-Event-ID without
// dispatching an event: per SSE an id-only block updates the client's
// last ID while firing no message event.
func writeSkipEvent(w io.Writer, id uint64) error {
	_, err := fmt.Fprintf(w, "id: %d\n\n", id)
	return err
}

// writeStreamEvent frames one event: id, name, then one data line per
// payload line (payloads may carry multi-line HTML), plus the blank
// line terminating the SSE message. Event names are server-generated;
// newlines are stripped so a name can never break SSE framing (a colon
// in the value is legal SSE grammar and needs no escaping).
func writeStreamEvent(w io.Writer, ev realtime.Event) error {
	name := strings.ReplaceAll(strings.ReplaceAll(ev.Name, "\r", ""), "\n", "")
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\n", ev.ID, name); err != nil {
		return err
	}
	if ev.HTML == "" {
		if _, err := io.WriteString(w, "data:\n"); err != nil {
			return err
		}
	} else {
		for _, line := range strings.Split(ev.HTML, "\n") {
			if _, err := io.WriteString(w, "data: "+strings.TrimSuffix(line, "\r")+"\n"); err != nil {
				return err
			}
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}
