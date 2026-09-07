package handlers

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"hindsight/realtime"
)

// streamCapture is a goroutine-safe SSE sink: the handler writes chunks
// from its own goroutine while the test pulls them with a timeout.
type streamCapture struct {
	header http.Header
	mu     sync.Mutex
	code   int
	chunks chan string
}

func newStreamCapture() *streamCapture {
	return &streamCapture{header: make(http.Header), chunks: make(chan string, 256)}
}

func (s *streamCapture) Header() http.Header { return s.header }

func (s *streamCapture) Write(p []byte) (int, error) {
	s.chunks <- string(p)
	return len(p), nil
}

func (s *streamCapture) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = code
}

func (s *streamCapture) Flush() {}

func (s *streamCapture) status() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

// startEventsStream serves the events route in the background with a
// cancelable request context. Cleanup cancels and waits for the handler
// to return, so a stuck stream fails the test instead of the suite.
func startEventsStream(t *testing.T, mux http.Handler, target string, headers map[string]string, cookies ...*http.Cookie) *streamCapture {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := newStreamCapture()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mux.ServeHTTP(rec, req)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Errorf("events stream did not exit after cancel")
		}
	})
	return rec
}

// nextMessage accumulates raw writes until the blank line terminating
// one SSE message. A single event takes several writes (id, name, data
// lines), so tests must frame on the terminator, not on write chunks.
func nextMessage(t *testing.T, rec *streamCapture) string {
	t.Helper()

	var sb strings.Builder
	deadline := time.After(3 * time.Second)
	for {
		select {
		case chunk := <-rec.chunks:
			sb.WriteString(chunk)
			if strings.Contains(sb.String(), "\n\n") {
				return sb.String()
			}
		case <-deadline:
			t.Fatalf("timed out waiting for stream message (partial: %q)", sb.String())
			return ""
		}
	}
}

// waitForSubscribed blocks until the broker holds at least n live
// streams for the board. Fresh boards send no opening marker, so without
// this barrier a mutation could publish before the stream subscribes and
// the stream would legitimately open with a resync replay instead.
func waitForSubscribed(t *testing.T, broker *realtime.Broker, bid string, n int) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for broker.SubscriberCount(bid) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d subscriber(s) on board", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// eventsMux routes only the stream, mirroring the wiring in main.go.
func eventsMux(fix *boardFixture) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /b/{bid}/events", fix.handler.Events)
	return mux
}

// joinAs creates a participant and returns its cookie.
func joinAs(t *testing.T, fix *boardFixture, bid, name string) []*http.Cookie {
	t.Helper()

	join := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
		url.Values{"name": {name}})
	if join.Code != http.StatusOK {
		t.Fatalf("join status = %d, want 200", join.Code)
	}
	return withCookies(responseCookies(join), partsCookieName)
}

func TestEventsRejectsStrangers(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	mux := eventsMux(fix)

	// Unknown boards are a plain 404, before any auth check.
	missing := doRequest(t, mux, http.MethodGet, "/b/does-not-exist/events", nil)
	if missing.Code != http.StatusNotFound {
		t.Errorf("unknown board events status = %d, want 404", missing.Code)
	}

	// Visitors without a participant cookie cannot stream.
	assertFlashOnly(t,
		doRequest(t, mux, http.MethodGet, "/b/"+bid+"/events", nil),
		http.StatusForbidden)

	// A forged participant cookie gains nothing.
	bogus := &http.Cookie{Name: partsCookieName, Value: "bogus.payload"}
	assertFlashOnly(t,
		doRequest(t, mux, http.MethodGet, "/b/"+bid+"/events", nil, bogus),
		http.StatusForbidden)
}

func TestEventsAdmitsParticipantAndFacilitator(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	broker := realtime.NewBroker()
	fix.handler.broker = broker
	mux := eventsMux(fix)

	participant := joinAs(t, fix, bid, "ana")
	rec := startEventsStream(t, mux, "/b/"+bid+"/events", nil, participant...)
	waitForSubscribed(t, broker, bid, 1)
	broker.Publish(bid, "hello", "<div>hi</div>")
	first := nextMessage(t, rec)

	if rec.status() != http.StatusOK {
		t.Errorf("stream status = %d, want 200", rec.status())
	}
	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if !strings.Contains(first, "event: hello") {
		t.Errorf("fresh stream first chunk = %q, want the live event", first)
	}

	// The facilitator cookie alone (no participant cookie) also streams.
	facOnly := withCookies(creatorCookies, facCookieName)
	facRec := startEventsStream(t, mux, "/b/"+bid+"/events", nil, facOnly...)
	waitForSubscribed(t, broker, bid, 2)
	broker.Publish(bid, "hello2", "<div>hi2</div>")
	// The facilitator subscribed after "hello" was published, so it
	// correctly opens with a resync replay before the live event.
	if chunk := nextMessage(t, facRec); !strings.Contains(chunk, "event: "+realtime.SyncRequiredName) {
		t.Errorf("facilitator stream first chunk = %q, want a %q replay", chunk, realtime.SyncRequiredName)
	}
	if chunk := nextMessage(t, facRec); !strings.Contains(chunk, "event: hello2") {
		t.Errorf("facilitator stream second chunk = %q, want the live event", chunk)
	}
}

func TestEventsReplaysGapThenStreamsLive(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	broker := realtime.NewBroker()
	fix.handler.broker = broker
	mux := eventsMux(fix)

	broker.Publish(bid, "column-1", "<div>one</div>")
	broker.Publish(bid, "column-2", "line-a\nline-b")
	broker.Publish(bid, "column-1", "<div>three</div>")

	participant := joinAs(t, fix, bid, "ana")
	rec := startEventsStream(t, mux, "/b/"+bid+"/events",
		map[string]string{"Last-Event-ID": "1"}, participant...)

	// Exactly the missed events replay, in order, with framing intact.
	var replay strings.Builder
	replay.WriteString(nextMessage(t, rec))
	replay.WriteString(nextMessage(t, rec))
	got := replay.String()
	for _, want := range []string{"id: 2", "id: 3", "event: column-2", "data: line-a", "data: line-b"} {
		if !strings.Contains(got, want) {
			t.Errorf("replay missing %q (chunks: %q)", want, got)
		}
	}
	if strings.Contains(got, realtime.SyncRequiredName) {
		t.Errorf("replay must not resync when the gap is buffered (chunks: %q)", got)
	}

	// The same stream keeps delivering live publishes after the replay.
	broker.Publish(bid, "column-9", "<div>live</div>")
	live := nextMessage(t, rec)
	for _, want := range []string{"id: 4", "event: column-9", "data: <div>live</div>"} {
		if !strings.Contains(live, want) {
			t.Errorf("live chunk missing %q (chunk: %q)", want, live)
		}
	}
}

func TestEventsHeartbeat(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	fix.handler.broker = realtime.NewBroker()
	fix.handler.heartbeat = 30 * time.Millisecond
	mux := eventsMux(fix)

	if fix.handler.heartbeat <= 0 {
		t.Fatal("test heartbeat override must be positive")
	}

	participant := joinAs(t, fix, bid, "ana")
	rec := startEventsStream(t, mux, "/b/"+bid+"/events", nil, participant...)

	// Fresh boards send no opening marker; wait straight for pings.
	deadline := time.Now().Add(3 * time.Second)
	for {
		chunk := nextMessage(t, rec)
		if strings.Contains(chunk, ":ping") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no :ping heartbeat within 3s (last chunk: %q)", chunk)
		}
	}
}

func TestEventsHeartbeatDefaultsToTwelveSeconds(t *testing.T) {
	fix := openFixture(t)

	if fix.handler.heartbeat != 12*time.Second {
		t.Errorf("default heartbeat = %v, want 12s", fix.handler.heartbeat)
	}
}

func TestEventsSubscribeTriggersExpiredTimer(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	broker := realtime.NewBroker()
	fix.handler.events = funnelPublisher{broker: broker}
	fix.handler.broker = broker
	mux := eventsMux(fix)

	participant := joinAs(t, fix, bid, "ana")
	armExpiredTimer(t, fix, bid)

	rec := startEventsStream(t, mux, "/b/"+bid+"/events", nil, participant...)
	// Fresh boards send no opening marker: the subscribe itself is a
	// board read, so the expired timer disarms and the expiry reaches
	// this stream live as the first message.
	first := nextMessage(t, rec)
	if !strings.Contains(first, "event: timer-ended") {
		t.Fatalf("stream first chunk = %q, want the timer-ended expiry", first)
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.TimerEndsAt != nil {
		t.Error("expired timer must disarm on subscribe")
	}
}

func TestEventsHeartbeatTickFiresExpiredTimer(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	fix.handler.broker = realtime.NewBroker()
	fix.handler.heartbeat = 30 * time.Millisecond
	mux := eventsMux(fix)

	participant := joinAs(t, fix, bid, "ana")
	_ = startEventsStream(t, mux, "/b/"+bid+"/events", nil, participant...)

	// Arm the expiry after subscribing: no read path can notice it, so
	// only the heartbeat tick fires it.
	armExpiredTimer(t, fix, bid)
	waitForTimerEnded(t, fix.publisher, 3*time.Second)
	if n := countPublished(fix.publisher, "timer-ended"); n != 1 {
		t.Errorf("published %d timer-ended events, want exactly 1", n)
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.TimerEndsAt != nil {
		t.Error("expired timer must disarm on the heartbeat tick")
	}
}

func TestEventsBlindModeFiltersPerRole(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	if _, err := fix.store.SetCardsHidden(t.Context(), boardID(t, fix, bid), true); err != nil {
		t.Fatalf("SetCardsHidden: %v", err)
	}
	broker := realtime.NewBroker()
	fix.handler.broker = broker
	mux := eventsMux(fix)

	participant := joinAs(t, fix, bid, "ana")
	facOnly := withCookies(creatorCookies, facCookieName)
	partRec := startEventsStream(t, mux, "/b/"+bid+"/events", nil, participant...)
	facRec := startEventsStream(t, mux, "/b/"+bid+"/events", nil, facOnly...)
	waitForSubscribed(t, broker, bid, 2)

	const secret = "super-secret-body"
	broker.Publish(bid, "card-updated", "<div>•••</div>")
	broker.Publish(bid, "card-updated:full", "<div>"+secret+"</div>")

	// The participant sees only the redacted base event: the :full
	// variant is filtered at the stream edge and yields an id-only
	// skip advancing Last-Event-ID, so the next dispatch after the
	// base is the following live publish.
	partBase := nextMessage(t, partRec)
	partSkip := nextMessage(t, partRec)
	broker.Publish(bid, "hello", "<div>hi</div>")
	partLive := nextMessage(t, partRec)
	partGot := partBase + partSkip + partLive
	if strings.Contains(partGot, secret) {
		t.Errorf("participant stream leaked full body: %q", partGot)
	}
	if !strings.Contains(partBase, "•••") {
		t.Errorf("participant stream missing redacted base event: %q", partBase)
	}
	if !strings.Contains(partSkip, "id: ") || strings.Contains(partSkip, "event:") {
		t.Errorf("filtered :full must advance Last-Event-ID with an id-only block: %q", partSkip)
	}
	if !strings.Contains(partLive, "event: hello") {
		t.Errorf("participant stream did not advance past filtered event: %q", partLive)
	}

	facGot := nextMessage(t, facRec) + nextMessage(t, facRec) + nextMessage(t, facRec)
	if !strings.Contains(facGot, secret) {
		t.Errorf("facilitator stream missing full body: %q", facGot)
	}
	if strings.Count(facGot, "event: card-updated") != 2 {
		t.Errorf("facilitator :full variant must be renamed to base name: %q", facGot)
	}
}

func TestEventsMalformedLastEventIDActsAsFreshConnect(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	broker := realtime.NewBroker()
	fix.handler.broker = broker
	mux := eventsMux(fix)

	broker.Publish(bid, "column-1", "<div>one</div>")
	participant := joinAs(t, fix, bid, "ana")
	rec := startEventsStream(t, mux, "/b/"+bid+"/events",
		map[string]string{"Last-Event-ID": "abc"}, participant...)
	waitForSubscribed(t, broker, bid, 1)
	// Malformed IDs coerce to a fresh connect: no replay framing assert
	// needed beyond the stream staying usable for live events.
	broker.Publish(bid, "column-9", "<div>live</div>")
	var got strings.Builder
	got.WriteString(nextMessage(t, rec))
	got.WriteString(nextMessage(t, rec))
	if !strings.Contains(got.String(), "event: column-9") {
		t.Errorf("stream with malformed Last-Event-ID missed live event: %q", got.String())
	}
}

// TestEventsStreamOutlivesServerWriteTimeout pins the contract between
// the SSE endpoint and the server-wide WriteTimeout main.go sets: the
// stream extends its write deadline on every write (two heartbeats
// ahead), so a live board stream is never cut by the global 30s cap
// while a dead peer still disconnects on schedule. This needs a real
// server because ResponseController deadlines only exist there; the
// streamCapture the other stream tests use has no deadline support.
func TestEventsStreamOutlivesServerWriteTimeout(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	broker := realtime.NewBroker()
	fix.handler.broker = broker
	fix.handler.heartbeat = 40 * time.Millisecond
	mux := eventsMux(fix)

	ts := httptest.NewUnstartedServer(mux)
	ts.Config.WriteTimeout = 150 * time.Millisecond
	ts.Start()

	participant := joinAs(t, fix, bid, "ana")

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/b/"+bid+"/events", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for _, c := range participant {
		req.AddCookie(c)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("connect stream: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		resp.Body.Close()
		ts.Close()
	})

	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()

	waitForSubscribed(t, broker, bid, 1)

	// Outlive the server's write cap: with no per-write extension the
	// connection dies at 150ms and nothing published later arrives.
	time.Sleep(400 * time.Millisecond)
	broker.Publish(bid, "late-event", "<div>late</div>")

	deadline := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed early; the server write deadline cut it")
			}
			if strings.Contains(line, "event: late-event") {
				return
			}
		case <-deadline:
			t.Fatal("no event after the write timeout; the stream was cut")
		}
	}
}

func TestBoardShellWiresSyncRequiredRefetch(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	creator := withCookies(creatorCookies, facCookieName, partsCookieName)

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, creator...).Body.String()
	if !strings.Contains(shell, "sse:sync-required") {
		t.Error("board shell must subscribe to the sync-required resync marker")
	}
	if !strings.Contains(shell, `msg.type === "sync-required"`) {
		t.Error("board shell must route sync-required to a columns refetch")
	}
	if got := strings.Count(shell, "window.refreshColumnsRoot(columnsUrl())"); got < 2 {
		t.Errorf("shell refetches columns %d times, want the reveal and resync paths", got)
	}

	fragment := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"?fragment=columns",
		nil, creator...).Body.String()
	if !strings.Contains(fragment, "sse:sync-required") {
		t.Error("columns fragment must subscribe to the sync-required resync marker")
	}
}
