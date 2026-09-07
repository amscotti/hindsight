// Package realtime is the per-board SSE broker: per-board monotonic ids,
// the replay ring, Last-Event-ID replay, and dirty-marking. Publish only
// after DB commit; SSE framing, heartbeats (:ping), flushing, and blind
// filtering live in handlers at the stream edge.
package realtime
