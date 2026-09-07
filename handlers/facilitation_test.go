package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hindsight/db"
)

// facilitationSetup opens a board with one joined participant and
// returns the fixture, the public id, the creator (facilitator)
// cookies, and the participant cookies.
func facilitationSetup(t *testing.T) (*boardFixture, string, map[string]*http.Cookie, []*http.Cookie) {
	t.Helper()

	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	return fix, bid, creatorCookies, ana
}

func facCookies(creatorCookies map[string]*http.Cookie) []*http.Cookie {
	return withCookies(creatorCookies, facCookieName, partsCookieName)
}

// controlTargets builds one request per facilitation control. Card and
// column ids come from a seeded board so every target resolves.
func controlTargets(bid string, cardID, columnID int64) []struct {
	name   string
	method string
	target string
	form   url.Values
} {
	return []struct {
		name   string
		method string
		target string
		form   url.Values
	}{
		{"phase", http.MethodPost, "/b/" + bid + "/phase", url.Values{"phase": {"vote"}}},
		{"reveal", http.MethodPost, "/b/" + bid + "/reveal", nil},
		{"lock", http.MethodPost, "/b/" + bid + "/lock", url.Values{"target": {"voting"}, "locked": {"1"}}},
		{"timer", http.MethodPost, "/b/" + bid + "/timer", url.Values{"seconds": {"60"}}},
		{"focus", http.MethodPost, "/b/" + bid + "/focus", url.Values{"card_id": {strconv.FormatInt(cardID, 10)}}},
		{"focus next", http.MethodPost, "/b/" + bid + "/focus/next", nil},
		{"focus prev", http.MethodPost, "/b/" + bid + "/focus/prev", nil},
		{"sort", http.MethodPost, "/b/" + bid + "/sort", url.Values{"column_id": {strconv.FormatInt(columnID, 10)}}},
		{"regenerate", http.MethodPost, "/boards/" + bid + "/regenerate", nil},
	}
}

func TestFacilitationControlsRejectNonFacilitators(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	fac := withCookies(creatorCookies, facCookieName)
	card := seedBoardCard(t, fix, bid, "guarded", "ana")
	col := firstColumnID(t, fix, bid)

	otherBid, otherCookies := createBoard(t, fix.mux, url.Values{
		"name":         {"Other board"},
		"display_name": {"Other"},
		"template":     {"plus-delta"},
	})
	otherFac := withCookies(otherCookies, facCookieName)
	_ = otherBid

	for _, target := range controlTargets(bid, card.ID, col) {
		assertFlashOnly(t,
			doRequest(t, fix.mux, target.method, target.target, target.form),
			http.StatusForbidden)
		assertFlashOnly(t,
			doRequest(t, fix.mux, target.method, target.target, target.form, ana...),
			http.StatusForbidden)
		assertFlashOnly(t,
			doRequest(t, fix.mux, target.method, target.target, target.form, otherFac...),
			http.StatusForbidden)
		bogus := &http.Cookie{Name: facCookieName, Value: "bogus.payload"}
		assertFlashOnly(t,
			doRequest(t, fix.mux, target.method, target.target, target.form, bogus),
			http.StatusForbidden)
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("rejected controls published %d events, want none", n)
	}

	// Unknown boards stay plain 404s, even for facilitators.
	missing := doRequest(t, fix.mux, http.MethodPost, "/b/does-not-exist/phase",
		url.Values{"phase": {"vote"}}, fac...)
	if missing.Code != http.StatusNotFound {
		t.Errorf("phase on unknown board status = %d, want 404", missing.Code)
	}
}

func TestSetPhaseBroadcastsAndValidates(t *testing.T) {
	fix, bid, creatorCookies, _ := facilitationSetup(t)
	fac := facCookies(creatorCookies)

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/phase",
		url.Values{"phase": {"vote"}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("phase status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.Phase != "vote" {
		t.Errorf("stored phase = %q, want vote", board.Phase)
	}
	events := fix.publisher.events
	if len(events) != 1 {
		t.Fatalf("published %d events, want exactly the phase event", len(events))
	}
	if events[0].boardID != bid || events[0].name != "phase" {
		t.Errorf("published %+v, want board %q name phase", events[0], bid)
	}
	var payload struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal([]byte(events[0].html), &payload); err != nil {
		t.Fatalf("phase payload is not JSON: %v (payload: %q)", err, events[0].html)
	}
	if payload.Phase != "vote" {
		t.Errorf("phase payload = %+v, want vote", payload)
	}

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/phase",
			url.Values{"phase": {"bogus"}}, fac...),
		http.StatusUnprocessableEntity)
	if n := fix.publisher.count(); n != 1 {
		t.Errorf("rejected phase published %d events total, want 1", n)
	}
}

func TestRevealClearsHiddenAndLiftsRedaction(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	fac := facCookies(creatorCookies)
	secret := "reveal-me-secret-body"
	seeded := seedBoardCard(t, fix, bid, secret, "secret-author")

	if _, err := fix.store.SetCardsHidden(t.Context(), boardID(t, fix, bid), true); err != nil {
		t.Fatalf("SetCardsHidden: %v", err)
	}
	_ = seeded
	hidden := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if strings.Contains(hidden, secret) {
		t.Fatal("participant shell leaks the body while hidden")
	}
	if !strings.Contains(hidden, redactedBody) {
		t.Fatal("participant shell carries no redaction placeholder while hidden")
	}

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/reveal", nil, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("reveal status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.CardsHidden {
		t.Error("reveal left cards_hidden set")
	}
	names := publishedNames(fix.publisher)
	if len(names) != 1 || names[0] != "cards-revealed" {
		t.Fatalf("published %v, want exactly cards-revealed", names)
	}

	// After reveal every viewer reads full bodies again.
	open := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if !strings.Contains(open, secret) {
		t.Error("participant shell missing the body after reveal")
	}
	if strings.Contains(open, redactedBody) {
		t.Error("participant shell still redacted after reveal")
	}
	fragment := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"?fragment=columns", nil, ana...).Body.String()
	if !strings.Contains(fragment, secret) {
		t.Error("participant columns fragment missing the body after reveal")
	}
}

func TestLockTogglesPublishColumnRefreshes(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	fac := facCookies(creatorCookies)
	seedBoardCard(t, fix, bid, "lockable", "ana")

	for _, tc := range []struct {
		target string
		check  func(boardID string) bool
	}{
		{"voting", func(bid string) bool {
			board, _ := fix.store.GetBoardByPublicID(t.Context(), bid)
			return board.VotingLocked
		}},
		{"cards", func(bid string) bool {
			board, _ := fix.store.GetBoardByPublicID(t.Context(), bid)
			return board.CardsLocked
		}},
	} {
		rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/lock",
			url.Values{"target": {tc.target}, "locked": {"1"}}, fac...)
		if rec.Code != http.StatusOK {
			t.Fatalf("lock %s status = %d, want 200 (body: %s)", tc.target, rec.Code, rec.Body.String())
		}
		if !tc.check(bid) {
			t.Errorf("lock %s did not persist", tc.target)
		}
	}
	// Every column refreshes so vote controls and add forms converge.
	cols, err := fix.store.ListColumns(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	names := publishedNames(fix.publisher)
	if len(names) != 2*len(cols) {
		t.Fatalf("published %v, want a refresh per column per lock (%d)", names, 2*len(cols))
	}
	for _, ev := range fix.publisher.events {
		if !strings.HasPrefix(ev.name, "column-") {
			t.Errorf("lock published %q, want only column refreshes", ev.name)
		}
	}

	// Unlocking flips the flags back.
	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/lock",
		url.Values{"target": {"voting"}, "locked": {"0"}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlock status = %d, want 200", rec.Code)
	}
	board, _ := fix.store.GetBoardByPublicID(t.Context(), bid)
	if board.VotingLocked {
		t.Error("unlock left voting_locked set")
	}

	// Locked boards render converged controls: the add form gives way
	// to a notice and vote buttons disable.
	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if !strings.Contains(shell, "Adding cards is locked") {
		t.Error("locked shell must tell viewers adding is locked")
	}

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/lock",
			url.Values{"target": {"bogus"}, "locked": {"1"}}, fac...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/lock",
			url.Values{"target": {"voting"}, "locked": {"maybe"}}, fac...),
		http.StatusUnprocessableEntity)
	_ = cols
}

func waitForTimerEnded(t *testing.T, f *recordingPublisher, timeout time.Duration) publishedEvent {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, ev := range f.events {
			if ev.name == "timer-ended" {
				f.mu.Unlock()
				return ev
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %q event published within %v", "timer-ended", timeout)
	return publishedEvent{}
}

func countPublished(f *recordingPublisher, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, ev := range f.events {
		if ev.name == name {
			n++
		}
	}
	return n
}

func TestTimerSetCancelAndFire(t *testing.T) {
	fix, bid, creatorCookies, _ := facilitationSetup(t)
	fac := facCookies(creatorCookies)

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/timer",
		url.Values{"seconds": {"1"}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("timer set status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.TimerEndsAt == nil {
		t.Fatal("timer set left no server expiry")
	}
	if countPublished(fix.publisher, "timer") != 1 {
		t.Fatalf("published %v, want exactly the timer event", publishedNames(fix.publisher))
	}
	var setPayload struct {
		EndsAt *time.Time `json:"ends_at"`
	}
	for _, ev := range fix.publisher.events {
		if ev.name == "timer" {
			if err := json.Unmarshal([]byte(ev.html), &setPayload); err != nil {
				t.Fatalf("timer payload is not JSON: %v", err)
			}
		}
	}
	if setPayload.EndsAt == nil {
		t.Error("timer payload carries no expiry")
	}

	// The server fires the expiry with no client involved.
	ended := waitForTimerEnded(t, fix.publisher, 6*time.Second)
	var endPayload struct {
		At time.Time `json:"at"`
	}
	if err := json.Unmarshal([]byte(ended.html), &endPayload); err != nil {
		t.Fatalf("timer-ended payload is not JSON: %v", err)
	}
	if endPayload.At.IsZero() {
		t.Error("timer-ended payload carries no instant")
	}
	cleared, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if cleared.TimerEndsAt != nil {
		t.Error("fired timer must disarm server-side")
	}

	// Canceling before expiry fires nothing.
	rec = doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/timer",
		url.Values{"seconds": {"1"}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("timer re-arm status = %d, want 200", rec.Code)
	}
	before := countPublished(fix.publisher, "timer-ended")
	rec = doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/timer",
		url.Values{"seconds": {"0"}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("timer cancel status = %d, want 200", rec.Code)
	}
	disarmed, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if disarmed.TimerEndsAt != nil {
		t.Error("cancel left the timer armed")
	}
	time.Sleep(1500 * time.Millisecond)
	if got := countPublished(fix.publisher, "timer-ended"); got != before {
		t.Errorf("canceled timer still fired (%d timer-ended events, want %d)", got, before)
	}

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/timer",
			url.Values{"seconds": {"nope"}}, fac...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/timer",
			url.Values{"seconds": {"-5"}}, fac...),
		http.StatusUnprocessableEntity)

	for _, tc := range []struct {
		seconds string
		want    int
	}{
		{"86400", http.StatusOK},
		{"86401", http.StatusUnprocessableEntity},
		{"9999999999", http.StatusUnprocessableEntity},
	} {
		rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/timer",
			url.Values{"seconds": {tc.seconds}}, fac...)
		if rec.Code != tc.want {
			t.Errorf("timer seconds=%s status = %d, want %d (body: %s)", tc.seconds, rec.Code, tc.want, rec.Body.String())
		}
	}
	// A rejected over-long arm must not persist or broadcast.
	if n := countPublished(fix.publisher, "timer"); n != 4 {
		t.Errorf("published %d timer events after bound checks, want 3", n)
	}
}

func TestTimerOverwriteBroadcastsPriorExpiry(t *testing.T) {
	fix, bid, creatorCookies, _ := facilitationSetup(t)
	fac := facCookies(creatorCookies)
	armExpiredTimer(t, fix, bid)

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/timer",
		url.Values{"seconds": {"60"}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("timer overwrite status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := countPublished(fix.publisher, "timer-ended"); n != 1 {
		t.Errorf("published %d timer-ended events on overwrite, want exactly 1", n)
	}
	if n := countPublished(fix.publisher, "timer"); n != 1 {
		t.Errorf("published %d timer events on overwrite, want exactly 1", n)
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.TimerEndsAt == nil {
		t.Error("overwrite must leave the new timer armed")
	}
}

func TestTimerExpiryDetectedOnBoardRead(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	fac := facCookies(creatorCookies)

	// Arm a timer that is already past: the next board read must notice
	// and broadcast the expiry exactly once.
	past := time.Now().UTC().Add(-time.Second)
	if _, err := fix.store.SetTimerEndsAt(t.Context(), boardID(t, fix, bid), &past); err != nil {
		t.Fatalf("SetTimerEndsAt: %v", err)
	}
	rec := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("board read status = %d, want 200", rec.Code)
	}
	waitForTimerEnded(t, fix.publisher, 3*time.Second)
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.TimerEndsAt != nil {
		t.Error("expired timer must disarm on read")
	}
	_ = fac
}

// armExpiredTimer places a board timer already past its expiry, so the
// next board read on any path must notice and broadcast it.
func armExpiredTimer(t *testing.T, fix *boardFixture, bid string) {
	t.Helper()

	past := time.Now().UTC().Add(-time.Second)
	if _, err := fix.store.SetTimerEndsAt(t.Context(), boardID(t, fix, bid), &past); err != nil {
		t.Fatalf("SetTimerEndsAt: %v", err)
	}
}

func TestVoteNoticesExpiredTimer(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	fac := facCookies(creatorCookies)
	if _, err := fix.store.SetPhase(t.Context(), boardID(t, fix, bid), "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	card := seedBoardCard(t, fix, bid, "timed vote", "ana")
	armExpiredTimer(t, fix, bid)

	rec := doRequest(t, fix.mux, http.MethodPost,
		"/cards/"+strconv.FormatInt(card.ID, 10)+"/vote", nil, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("vote status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := countPublished(fix.publisher, "timer-ended"); n != 1 {
		t.Errorf("published %d timer-ended events, want exactly 1 (names: %v)",
			n, publishedNames(fix.publisher))
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.TimerEndsAt != nil {
		t.Error("expired timer must disarm on the vote re-read")
	}
	_ = fac
}

func TestLockNoticesExpiredTimer(t *testing.T) {
	fix, bid, creatorCookies, _ := facilitationSetup(t)
	fac := facCookies(creatorCookies)
	seedBoardCard(t, fix, bid, "lockable", "ana")
	armExpiredTimer(t, fix, bid)

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/lock",
		url.Values{"target": {"voting"}, "locked": {"1"}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("lock status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := countPublished(fix.publisher, "timer-ended"); n != 1 {
		t.Errorf("published %d timer-ended events, want exactly 1 (names: %v)",
			n, publishedNames(fix.publisher))
	}
}

func TestSortNoticesExpiredTimer(t *testing.T) {
	fix, bid, creatorCookies, _ := facilitationSetup(t)
	fac := facCookies(creatorCookies)
	col := firstColumnID(t, fix, bid)
	seedBoardCard(t, fix, bid, "sortable", "ana")
	armExpiredTimer(t, fix, bid)

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/sort",
		url.Values{"column_id": {strconv.FormatInt(col, 10)}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("sort status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := countPublished(fix.publisher, "timer-ended"); n != 1 {
		t.Errorf("published %d timer-ended events, want exactly 1 (names: %v)",
			n, publishedNames(fix.publisher))
	}
}

func TestStepFocusNoticesExpiredTimer(t *testing.T) {
	fix, bid, creatorCookies, _ := facilitationSetup(t)
	fac := facCookies(creatorCookies)
	seedBoardCard(t, fix, bid, "queued", "ana")
	armExpiredTimer(t, fix, bid)

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/focus/next", nil, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("focus next status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := countPublished(fix.publisher, "timer-ended"); n != 1 {
		t.Errorf("published %d timer-ended events, want exactly 1 (names: %v)",
			n, publishedNames(fix.publisher))
	}
	if n := countPublished(fix.publisher, "focus"); n != 1 {
		t.Errorf("published %d focus events, want 1 alongside the expiry", n)
	}
}

func TestTimerExpiryBroadcastExactlyOnce(t *testing.T) {
	fix, bid, _, ana := facilitationSetup(t)
	numeric := boardID(t, fix, bid)
	armExpiredTimer(t, fix, bid)

	// Race the server-side fire against board reads: the instant-matched
	// disarm lets exactly one path win the broadcast.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			fix.handler.fireTimer(numeric, bid)
		}()
		go func() {
			defer wg.Done()
			doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...)
		}()
	}
	wg.Wait()
	if n := countPublished(fix.publisher, "timer-ended"); n != 1 {
		t.Errorf("published %d timer-ended events under race, want exactly 1", n)
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.TimerEndsAt != nil {
		t.Error("expired timer must disarm exactly once under race")
	}
}

func TestFocusSetClearAndBroadcast(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	fac := facCookies(creatorCookies)
	card := seedBoardCard(t, fix, bid, "spotlight me", "ana")

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/focus",
		url.Values{"card_id": {strconv.FormatInt(card.ID, 10)}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("focus status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.FocusedCardID == nil || *board.FocusedCardID != card.ID {
		t.Errorf("focus = %v, want %d", board.FocusedCardID, card.ID)
	}
	if n := countPublished(fix.publisher, "focus"); n != 1 {
		t.Fatalf("published %v, want exactly the focus event", publishedNames(fix.publisher))
	}
	var payload struct {
		CardID *int64 `json:"card_id"`
	}
	if err := json.Unmarshal([]byte(fix.publisher.events[0].html), &payload); err != nil {
		t.Fatalf("focus payload is not JSON: %v", err)
	}
	if payload.CardID == nil || *payload.CardID != card.ID {
		t.Errorf("focus payload = %+v, want card %d", payload, card.ID)
	}

	// Clearing spotlights nothing.
	rec = doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/focus",
		url.Values{"card_id": {""}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("focus clear status = %d, want 200", rec.Code)
	}
	cleared, _ := fix.store.GetBoardByPublicID(t.Context(), bid)
	if cleared.FocusedCardID != nil {
		t.Errorf("focus after clear = %v, want nil", cleared.FocusedCardID)
	}

	unknown := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/focus",
		url.Values{"card_id": {"999999"}}, fac...)
	if unknown.Code != http.StatusNotFound {
		t.Errorf("focus unknown card status = %d, want 404", unknown.Code)
	}
	otherBid, _ := createBoard(t, fix.mux, url.Values{
		"name":         {"Other"},
		"display_name": {"Other"},
		"template":     {"plus-delta"},
	})
	foreign := seedBoardCard(t, fix, otherBid, "foreign", "bo")
	foreignRec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/focus",
		url.Values{"card_id": {strconv.FormatInt(foreign.ID, 10)}}, fac...)
	if foreignRec.Code != http.StatusNotFound {
		t.Errorf("focus foreign card status = %d, want 404", foreignRec.Code)
	}
	_ = ana
}

func TestFocusNextPrevOrderAndDiscussedSkip(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	bo := joinAs(t, fix, bid, "bo")
	fac := facCookies(creatorCookies)
	if _, err := fix.store.SetPhase(t.Context(), boardID(t, fix, bid), "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	col := firstColumnID(t, fix, bid)
	make := func(body string) int64 {
		t.Helper()
		card, err := fix.store.CreateCard(t.Context(), col, body, "ana")
		if err != nil {
			t.Fatalf("CreateCard: %v", err)
		}
		return card.ID
	}
	low, mid, high := make("low"), make("mid"), make("high")
	// Votes: high 2, mid 1, low 0. Queue order: high, mid, low.
	vote := func(cookies []*http.Cookie, id int64) {
		t.Helper()
		if rec := doRequest(t, fix.mux, http.MethodPost,
			"/cards/"+strconv.FormatInt(id, 10)+"/vote", nil, cookies...); rec.Code != http.StatusOK {
			t.Fatalf("vote %d status = %d, want 200", id, rec.Code)
		}
	}
	vote(ana, high)
	vote(bo, high)
	vote(ana, mid)
	fix.publisher.events = nil

	next := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/focus/next", nil, fac...)
	if next.Code != http.StatusOK {
		t.Fatalf("focus next status = %d, want 200", next.Code)
	}
	board, _ := fix.store.GetBoardByPublicID(t.Context(), bid)
	if board.FocusedCardID == nil || *board.FocusedCardID != high {
		t.Errorf("first next focus = %v, want top-voted %d", board.FocusedCardID, high)
	}

	again := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/focus/next", nil, fac...)
	if again.Code != http.StatusOK {
		t.Fatalf("second next status = %d, want 200", again.Code)
	}
	board, _ = fix.store.GetBoardByPublicID(t.Context(), bid)
	if board.FocusedCardID == nil || *board.FocusedCardID != mid {
		t.Errorf("second next focus = %v, want %d", board.FocusedCardID, mid)
	}
	highCard, err := fix.store.GetCard(t.Context(), high)
	if err != nil {
		t.Fatalf("GetCard high: %v", err)
	}
	if !highCard.Discussed {
		t.Error("advancing past the top card must mark it discussed")
	}

	// Stepping back from mid finds nothing undiscussed before it
	// (high is discussed), so focus clears.
	prev := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/focus/prev", nil, fac...)
	if prev.Code != http.StatusOK {
		t.Fatalf("focus prev status = %d, want 200", prev.Code)
	}
	board, _ = fix.store.GetBoardByPublicID(t.Context(), bid)
	if board.FocusedCardID != nil {
		t.Errorf("prev focus = %v, want nil (high is discussed)", board.FocusedCardID)
	}

	// Advancing marks discussed cards on the column refreshes, so every
	// viewer converges on the queue state.
	columns := 0
	for _, ev := range fix.publisher.events {
		switch {
		case ev.name == "focus":
		case strings.HasPrefix(ev.name, "column-"):
			columns++
		default:
			t.Errorf("next/prev published unexpected event %q", ev.name)
		}
	}
	if columns == 0 {
		t.Error("next/prev must refresh affected columns for the discussed flags")
	}
	if n := countPublished(fix.publisher, "focus"); n != 3 {
		t.Errorf("published %d focus events, want 3 (one per step)", n)
	}
	_ = low
}

func TestGroupingSumsVotesAndNestsRendering(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	bo := joinAs(t, fix, bid, "bo")
	fac := facCookies(creatorCookies)
	if _, err := fix.store.SetPhase(t.Context(), boardID(t, fix, bid), "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	col := firstColumnID(t, fix, bid)
	leader, err := fix.store.CreateCard(t.Context(), col, "leader-body", "ana")
	if err != nil {
		t.Fatalf("CreateCard leader: %v", err)
	}
	member, err := fix.store.CreateCard(t.Context(), col, "member-body", "bo")
	if err != nil {
		t.Fatalf("CreateCard member: %v", err)
	}
	for _, tc := range []struct {
		cookies []*http.Cookie
		id      int64
	}{
		{ana, leader.ID},
		{ana, member.ID},
		{bo, member.ID},
	} {
		if rec := doRequest(t, fix.mux, http.MethodPost,
			"/cards/"+strconv.FormatInt(tc.id, 10)+"/vote", nil, tc.cookies...); rec.Code != http.StatusOK {
			t.Fatalf("vote %d status = %d, want 200", tc.id, rec.Code)
		}
	}
	fix.publisher.events = nil

	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(member.ID, 10),
		url.Values{"group_id": {strconv.FormatInt(leader.ID, 10)}}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("group status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	stored, err := fix.store.GetCard(t.Context(), member.ID)
	if err != nil {
		t.Fatalf("GetCard member: %v", err)
	}
	if stored.GroupID == nil || *stored.GroupID != leader.ID {
		t.Errorf("member group_id = %v, want %d", stored.GroupID, leader.ID)
	}
	// The leader now shows the group sum: 1 own + 2 member votes.
	body := rec.Body.String()
	if !strings.Contains(body, "3 votes") {
		t.Errorf("group response missing the summed count (body: %.300s…)", body)
	}
	if !strings.Contains(body, "member-body") {
		t.Errorf("group response must nest the member under the leader (body: %.300s…)", body)
	}
	names := publishedNames(fix.publisher)
	if len(names) != 1 || names[0] != "column-"+strconv.FormatInt(col, 10) {
		t.Errorf("published %v, want exactly the column refresh", names)
	}

	// Shell renders the same nesting with the summed count.
	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if !strings.Contains(shell, "3 votes") || !strings.Contains(shell, "member-body") {
		t.Error("shell missing the grouped rendering with summed votes")
	}

	// Ungrouping restores both cards.
	ungroup := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(member.ID, 10),
		url.Values{"group_id": {""}}, ana...)
	if ungroup.Code != http.StatusOK {
		t.Fatalf("ungroup status = %d, want 200", ungroup.Code)
	}
	fresh, err := fix.store.GetCard(t.Context(), member.ID)
	if err != nil {
		t.Fatalf("GetCard member: %v", err)
	}
	if fresh.GroupID != nil {
		t.Errorf("group_id after ungroup = %v, want nil", fresh.GroupID)
	}

	// Invalid group targets are rejected without publishing.
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(member.ID, 10),
			url.Values{"group_id": {strconv.FormatInt(member.ID, 10)}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(member.ID, 10),
			url.Values{"group_id": {"nope"}}, ana...),
		http.StatusUnprocessableEntity)
	missing := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(member.ID, 10),
		url.Values{"group_id": {"999999"}}, ana...)
	if missing.Code != http.StatusNotFound {
		t.Errorf("group unknown leader status = %d, want 404", missing.Code)
	}
	otherBid, _ := createBoard(t, fix.mux, url.Values{
		"name":         {"Other"},
		"display_name": {"Other"},
		"template":     {"plus-delta"},
	})
	foreign := seedBoardCard(t, fix, otherBid, "foreign", "bo")
	foreignRec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(member.ID, 10),
		url.Values{"group_id": {strconv.FormatInt(foreign.ID, 10)}}, ana...)
	if foreignRec.Code != http.StatusNotFound {
		t.Errorf("group foreign leader status = %d, want 404", foreignRec.Code)
	}
	_ = fac
	_ = bo
}

func TestGroupLeaderMergesMembersFlat(t *testing.T) {
	fix, bid, _, ana := facilitationSetup(t)
	ctx := t.Context()
	col := firstColumnID(t, fix, bid)
	if _, err := fix.store.SetPhase(ctx, boardID(t, fix, bid), "vote"); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}

	leader, err := fix.store.CreateCard(ctx, col, "merge-leader", "ana")
	if err != nil {
		t.Fatalf("CreateCard leader: %v", err)
	}
	member, err := fix.store.CreateCard(ctx, col, "merge-member", "bo")
	if err != nil {
		t.Fatalf("CreateCard member: %v", err)
	}
	final, err := fix.store.CreateCard(ctx, col, "merge-final", "cy")
	if err != nil {
		t.Fatalf("CreateCard final: %v", err)
	}
	if _, err := fix.store.SetGroup(ctx, member.ID, &leader.ID); err != nil {
		t.Fatalf("SetGroup member: %v", err)
	}
	voter, err := fix.store.CreateParticipant(ctx, boardID(t, fix, bid), "voter", "voter-raw-token")
	if err != nil {
		t.Fatalf("CreateParticipant: %v", err)
	}
	for _, id := range []int64{leader.ID, member.ID, final.ID} {
		if err := fix.store.Vote(ctx, voter.ID, id); err != nil {
			t.Fatalf("Vote %d: %v", id, err)
		}
	}

	// Grouping the leader under the final card merges its members flat:
	// every card renders under the final leader with the summed count.
	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(leader.ID, 10),
		url.Values{"group_id": {strconv.FormatInt(final.ID, 10)}}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("group status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"merge-leader", "merge-member", "merge-final", "3 votes"} {
		if !strings.Contains(body, want) {
			t.Errorf("group response missing %q (body: %.300s…)", want, body)
		}
	}
	names := publishedNames(fix.publisher)
	if len(names) != 1 || names[0] != "column-"+strconv.FormatInt(col, 10) {
		t.Errorf("published %v, want exactly the column refresh", names)
	}
	stored, err := fix.store.GetCard(ctx, member.ID)
	if err != nil {
		t.Fatalf("GetCard member: %v", err)
	}
	if stored.GroupID == nil || *stored.GroupID != final.ID {
		t.Errorf("member group_id = %v, want merged leader %d", stored.GroupID, final.ID)
	}
	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	for _, want := range []string{"merge-leader", "merge-member", "merge-final", "3 votes"} {
		if !strings.Contains(shell, want) {
			t.Errorf("shell missing grouped rendering %q", want)
		}
	}
}

func TestGroupUnderMemberRefreshesResolvedLeaderColumn(t *testing.T) {
	fix, bid, _, ana := facilitationSetup(t)
	ctx := t.Context()
	left := firstColumnID(t, fix, bid)
	right := secondColumnID(t, fix, bid)

	resolved, err := fix.store.CreateCard(ctx, right, "far-leader", "ana")
	if err != nil {
		t.Fatalf("CreateCard leader: %v", err)
	}
	proxy, err := fix.store.CreateCard(ctx, left, "near-proxy", "bo")
	if err != nil {
		t.Fatalf("CreateCard proxy: %v", err)
	}
	if _, err := fix.store.SetGroup(ctx, proxy.ID, &resolved.ID); err != nil {
		t.Fatalf("SetGroup proxy: %v", err)
	}
	moving, err := fix.store.CreateCard(ctx, left, "moving-card", "ana")
	if err != nil {
		t.Fatalf("CreateCard moving: %v", err)
	}

	// The requested proxy resolves to the leader in the other column,
	// so both columns refresh: the mover's home and the resolved sum.
	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(moving.ID, 10),
		url.Values{"group_id": {strconv.FormatInt(proxy.ID, 10)}}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("group status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	names := publishedNames(fix.publisher)
	want := []string{
		"column-" + strconv.FormatInt(left, 10),
		"column-" + strconv.FormatInt(right, 10),
	}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("published %v, want the mover and resolved leader columns %v", names, want)
	}
	stored, err := fix.store.GetCard(ctx, moving.ID)
	if err != nil {
		t.Fatalf("GetCard moving: %v", err)
	}
	if stored.GroupID == nil || *stored.GroupID != resolved.ID {
		t.Errorf("moving group_id = %v, want resolved leader %d", stored.GroupID, resolved.ID)
	}
	if !strings.Contains(fix.publisher.events[1].html, "far-leader") {
		t.Error("resolved column payload missing the leader")
	}
}

func TestGroupedCardsStayRedactedWhileHidden(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	fac := facCookies(creatorCookies)
	col := firstColumnID(t, fix, bid)
	leader, err := fix.store.CreateCard(t.Context(), col, "hidden-leader", "ana")
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	member, err := fix.store.CreateCard(t.Context(), col, "hidden-member", "bo")
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if _, err := fix.store.SetGroup(t.Context(), member.ID, &leader.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	if _, err := fix.store.SetCardsHidden(t.Context(), boardID(t, fix, bid), true); err != nil {
		t.Fatalf("SetCardsHidden: %v", err)
	}

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if strings.Contains(shell, "hidden-leader") || strings.Contains(shell, "hidden-member") {
		t.Error("participant shell leaks grouped bodies while hidden")
	}
	if !strings.Contains(shell, redactedBody) {
		t.Error("participant shell carries no redaction placeholder for the group")
	}
	full := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, fac...).Body.String()
	if !strings.Contains(full, "hidden-leader") || !strings.Contains(full, "hidden-member") {
		t.Error("facilitator shell missing grouped bodies while hidden")
	}
}

func TestVoteOnGroupedCardRefreshesColumn(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	fac := facCookies(creatorCookies)
	if _, err := fix.store.SetPhase(t.Context(), boardID(t, fix, bid), "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	col := firstColumnID(t, fix, bid)
	leader, err := fix.store.CreateCard(t.Context(), col, "voted-leader", "ana")
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	member, err := fix.store.CreateCard(t.Context(), col, "voted-member", "ana")
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if _, err := fix.store.SetGroup(t.Context(), member.ID, &leader.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	fix.publisher.events = nil

	rec := doRequest(t, fix.mux, http.MethodPost,
		"/cards/"+strconv.FormatInt(member.ID, 10)+"/vote", nil, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("vote status = %d, want 200", rec.Code)
	}
	// The leader sum changed too, so viewers need the whole column —
	// a per-card morph would leave the total stale.
	names := publishedNames(fix.publisher)
	if len(names) != 1 || names[0] != "column-"+strconv.FormatInt(col, 10) {
		t.Fatalf("published %v, want exactly the column refresh", names)
	}
	if !strings.Contains(rec.Body.String(), "1 vote") {
		t.Errorf("vote response missing the updated sum (body: %.200s…)", rec.Body.String())
	}
	_ = fac
}

func TestSortColumnByVotesReordersAndBroadcasts(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	bo := joinAs(t, fix, bid, "bo")
	fac := facCookies(creatorCookies)
	if _, err := fix.store.SetPhase(t.Context(), boardID(t, fix, bid), "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	col := firstColumnID(t, fix, bid)
	var ids []int64
	for _, body := range []string{"first", "second", "third"} {
		card, err := fix.store.CreateCard(t.Context(), col, body, "ana")
		if err != nil {
			t.Fatalf("CreateCard: %v", err)
		}
		ids = append(ids, card.ID)
	}
	// Votes: third 2, first 1, second 0.
	vote := func(cookies []*http.Cookie, id int64) {
		t.Helper()
		if rec := doRequest(t, fix.mux, http.MethodPost,
			"/cards/"+strconv.FormatInt(id, 10)+"/vote", nil, cookies...); rec.Code != http.StatusOK {
			t.Fatalf("vote %d status = %d, want 200", id, rec.Code)
		}
	}
	vote(ana, ids[2])
	vote(bo, ids[2])
	vote(ana, ids[0])
	fix.publisher.events = nil

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/sort",
		url.Values{"column_id": {strconv.FormatInt(col, 10)}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("sort status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	names := publishedNames(fix.publisher)
	if len(names) != 1 || names[0] != "column-"+strconv.FormatInt(col, 10) {
		t.Fatalf("published %v, want exactly the sorted column refresh", names)
	}
	payload := fix.publisher.events[0].html
	at := func(s string) int { return strings.Index(payload, ">"+s+"<") }
	if at("third") < 0 || at("first") < 0 || at("second") < 0 ||
		!(at("third") < at("first") && at("first") < at("second")) {
		t.Errorf("sorted payload not in [third first second] order (payload: %.300s…)", payload)
	}

	missing := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/sort",
		url.Values{"column_id": {"999999"}}, fac...)
	if missing.Code != http.StatusNotFound {
		t.Errorf("sort unknown column status = %d, want 404", missing.Code)
	}
}

func TestBoardShellRendersFacilitationSurfaces(t *testing.T) {
	fix, bid, creatorCookies, ana := facilitationSetup(t)
	fac := facCookies(creatorCookies)

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, fac...).Body.String()
	for _, want := range []string{
		`id="facilitator-panel"`,
		`id="phase-badge"`,
		`id="spotlight"`,
		`id="timer"`,
		`id="fac-events"`,
		`sse:cards-revealed`,
		`sse:phase`,
		`sse:focus`,
		`sse:timer`,
		`name="phase"`,
		`name="seconds"`,
		`visibilitychange`,
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("facilitator shell missing %q", want)
		}
	}
	if got := strings.Count(shell, "sse-connect="); got != 1 {
		t.Errorf("shell holds %d stream subscriptions, want exactly 1", got)
	}

	viewer := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	for _, want := range []string{`id="phase-badge"`, `id="spotlight"`, `id="timer"`, `id="fac-events"`} {
		if !strings.Contains(viewer, want) {
			t.Errorf("participant shell missing shared surface %q", want)
		}
	}
	if strings.Contains(viewer, `id="facilitator-panel"`) {
		t.Error("participant shell must not render the facilitator panel")
	}
}

func TestTimerRestoreAfterRestart(t *testing.T) {
	fix, bid, creatorCookies, _ := facilitationSetup(t)
	fac := facCookies(creatorCookies)

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/timer",
		url.Values{"seconds": {"2"}}, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("timer set status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.TimerEndsAt == nil {
		t.Fatal("timer set left no server expiry")
	}
	endsAt := *board.TimerEndsAt

	// Simulate a restart: drop every in-memory fire while the
	// persisted instant stays armed.
	fix.handler.timerMu.Lock()
	for _, pending := range fix.handler.armedTimers {
		pending.timer.Stop()
	}
	fix.handler.armedTimers = map[int64]armedTimer{}
	fix.handler.timerMu.Unlock()
	fix.publisher.events = nil

	if err := fix.handler.RestoreArmedTimers(t.Context()); err != nil {
		t.Fatalf("RestoreArmedTimers: %v", err)
	}
	ended := waitForTimerEnded(t, fix.publisher, 6*time.Second)
	var payload struct {
		At time.Time `json:"at"`
	}
	if err := json.Unmarshal([]byte(ended.html), &payload); err != nil {
		t.Fatalf("timer-ended payload is not JSON: %v", err)
	}
	if !payload.At.Equal(endsAt) {
		t.Errorf("restored timer-ended at %v, want the persisted %v", payload.At, endsAt)
	}
	if n := countPublished(fix.publisher, "timer-ended"); n != 1 {
		t.Errorf("published %d timer-ended events, want exactly 1", n)
	}
	cleared, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if cleared.TimerEndsAt != nil {
		t.Error("restored timer must disarm server-side after firing")
	}

	// Past instants never re-arm: they surface on the next board read
	// instead of scheduling a stale fire.
	armExpiredTimer(t, fix, bid)
	if err := fix.handler.RestoreArmedTimers(t.Context()); err != nil {
		t.Fatalf("RestoreArmedTimers with expired timer: %v", err)
	}
	fix.handler.timerMu.Lock()
	n := len(fix.handler.armedTimers)
	fix.handler.timerMu.Unlock()
	if n != 0 {
		t.Errorf("restore armed %d fires for a past instant, want none", n)
	}
}

// errListStore forces ListArmedTimers to fail so the restore error
// path is covered without a corrupt database.
type errListStore struct {
	boardStore
	err error
}

func (s errListStore) ListArmedTimers(ctx context.Context) ([]db.ArmedTimer, error) {
	return nil, s.err
}

func TestRestoreArmedTimersListError(t *testing.T) {
	fix, _, _, _ := facilitationSetup(t)
	want := errors.New("list armed timers boom")
	b := NewBoards(errListStore{boardStore: fix.handler.store, err: want}, "test-secret", fix.publisher, nil)
	if err := b.RestoreArmedTimers(t.Context()); !errors.Is(err, want) {
		t.Fatalf("RestoreArmedTimers err = %v, want %v", err, want)
	}
	b.timerMu.Lock()
	n := len(b.armedTimers)
	b.timerMu.Unlock()
	if n != 0 {
		t.Errorf("failed restore armed %d fires, want none", n)
	}
}

func TestFireTimerIgnoresStalePublicID(t *testing.T) {
	fix, bid, _, _ := facilitationSetup(t)
	numeric := boardID(t, fix, bid)
	armExpiredTimer(t, fix, bid)

	// The public ID never rotates (Regenerate only rotates the
	// facilitator token), so a stale caller-supplied channel must be
	// ignored: the fire re-reads the board and publishes on the fresh
	// public ID.
	fix.handler.fireTimer(numeric, "stale-public-id")
	if n := countPublished(fix.publisher, "timer-ended"); n != 1 {
		t.Fatalf("published %d timer-ended events, want exactly 1", n)
	}
	fix.publisher.mu.Lock()
	got := fix.publisher.events[len(fix.publisher.events)-1]
	fix.publisher.mu.Unlock()
	if got.boardID != bid {
		t.Errorf("timer-ended board = %q, want fresh public id %q", got.boardID, bid)
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if board.TimerEndsAt != nil {
		t.Error("stale fire must still disarm server-side")
	}
}
