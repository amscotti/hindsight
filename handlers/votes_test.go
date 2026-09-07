package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"hindsight/db"
	"hindsight/realtime"
)

// voteGateStore overrides the board read behind card routes so tests can
// place a board in the voting window (or lock it) without building the
// facilitation controls that own those transitions.
type voteGateStore struct {
	boardStore
	phase  string
	locked bool
}

func (s voteGateStore) GetBoardByID(ctx context.Context, boardID int64) (*db.Board, error) {
	board, err := s.boardStore.GetBoardByID(ctx, boardID)
	if err != nil {
		return nil, err
	}
	board.Phase = s.phase
	board.VotingLocked = s.locked
	return board, nil
}

// voteFixture opens the shared fixture and routes the voting endpoints the
// same way the binary wires them, with the board placed in the voting
// window. Joining goes through the base fixture; the participant row and
// cookie work across both handlers because they share the store, secret,
// and publisher.
func voteFixture(t *testing.T) (*boardFixture, string, []*http.Cookie) {
	t.Helper()

	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if _, err := fix.store.SetPhase(t.Context(), board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	voting := NewBoards(fix.store, fix.handler.secret, fix.publisher, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /b/{bid}", voting.Show)
	mux.HandleFunc("POST /b/{bid}/join", voting.Join)
	mux.HandleFunc("POST /cards/{cid}/vote", voting.Vote)
	mux.HandleFunc("POST /cards/{cid}/unvote", voting.Unvote)
	ana := joinAs(t, fix, bid, "ana")
	return &boardFixture{handler: voting, store: fix.store, publisher: fix.publisher, mux: mux}, bid, ana
}

func voteTarget(cardID int64) string {
	return "/cards/" + strconv.FormatInt(cardID, 10) + "/vote"
}

func unvoteTarget(cardID int64) string {
	return "/cards/" + strconv.FormatInt(cardID, 10) + "/unvote"
}

func storedVotes(t *testing.T, fix *boardFixture, cardID int64) int {
	t.Helper()

	card, err := fix.store.GetCard(t.Context(), cardID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	return card.Votes
}

func TestVoteRecordsAndBroadcastsCardFragment(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "prioritize me", "ana")

	committed := false
	fix.publisher.hook = func(ev publishedEvent) {
		if storedVotes(t, fix, card.ID) == 1 {
			committed = true
		}
	}

	rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(card.ID), nil, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST vote status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="card-` + strconv.FormatInt(card.ID, 10) + `"`,
		"1 vote",
		`hx-post="/cards/` + strconv.FormatInt(card.ID, 10) + `/unvote"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("vote response missing %q (body: %.300s…)", want, body)
		}
	}
	if strings.Contains(body, `id="flash"`) {
		t.Errorf("successful vote must not carry a flash fragment (body: %.300s…)", body)
	}
	if !committed {
		t.Error("vote broadcast fired before the transaction committed")
	}

	events := fix.publisher.events
	if len(events) != 1 {
		t.Fatalf("published %d events, want exactly 1", len(events))
	}
	wantName := "vote-changed:" + strconv.FormatInt(card.ID, 10)
	if events[0].boardID != bid || events[0].name != wantName {
		t.Errorf("published %+v, want board %q name %q", events[0], bid, wantName)
	}
	for _, want := range []string{
		`id="card-` + strconv.FormatInt(card.ID, 10) + `"`,
		"1 vote",
		`sse-swap="card-updated:` + strconv.FormatInt(card.ID, 10) +
			`, vote-changed:` + strconv.FormatInt(card.ID, 10),
	} {
		if !strings.Contains(events[0].html, want) {
			t.Errorf("broadcast payload missing %q (payload: %.300s…)", want, events[0].html)
		}
	}
	if got := storedVotes(t, fix, card.ID); got != 1 {
		t.Errorf("stored votes = %d, want 1", got)
	}
}

func TestVoteRejectsDoubleVote(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "once", "ana")

	if rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(card.ID), nil, ana...); rec.Code != http.StatusOK {
		t.Fatalf("first vote status = %d, want 200", rec.Code)
	}
	before := fix.publisher.count()
	rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(card.ID), nil, ana...)
	assertFlashOnly(t, rec, http.StatusForbidden)
	if body := rec.Body.String(); !strings.Contains(body, "already voted") {
		t.Errorf("double-vote flash missing the reason (body: %s)", body)
	}
	if n := fix.publisher.count(); n != before {
		t.Errorf("rejected double vote published %d events, want %d (none new)", n, before)
	}
	if got := storedVotes(t, fix, card.ID); got != 1 {
		t.Errorf("stored votes = %d, want 1 after rejected double vote", got)
	}
}

func TestVoteEnforcesCap(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	cap := board.VotesPerPerson
	cols, err := fix.store.ListColumns(t.Context(), board.ID)
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	var ids []int64
	for i := 0; i < cap+1; i++ {
		card, err := fix.store.CreateCard(t.Context(), cols[0].ID, "option "+strconv.Itoa(i), "ana")
		if err != nil {
			t.Fatalf("CreateCard: %v", err)
		}
		ids = append(ids, card.ID)
	}
	for _, id := range ids[:cap] {
		if rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(id), nil, ana...); rec.Code != http.StatusOK {
			t.Fatalf("vote %d status = %d, want 200", id, rec.Code)
		}
	}
	rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(ids[cap]), nil, ana...)
	assertFlashOnly(t, rec, http.StatusForbidden)
	if body := rec.Body.String(); !strings.Contains(body, strconv.Itoa(cap)) {
		t.Errorf("cap flash names no limit (body: %s)", body)
	}
	if got := storedVotes(t, fix, ids[cap]); got != 0 {
		t.Errorf("stored votes on capped card = %d, want 0", got)
	}
}

func TestUnvoteRemovesAndBroadcasts(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "reconsider", "ana")

	if rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(card.ID), nil, ana...); rec.Code != http.StatusOK {
		t.Fatalf("vote status = %d, want 200", rec.Code)
	}
	before := fix.publisher.count()
	rec := doRequest(t, fix.mux, http.MethodPost, unvoteTarget(card.ID), nil, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST unvote status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="card-` + strconv.FormatInt(card.ID, 10) + `"`,
		"0 votes",
		`hx-post="/cards/` + strconv.FormatInt(card.ID, 10) + `/vote"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unvote response missing %q (body: %.300s…)", want, body)
		}
	}
	if n := fix.publisher.count(); n != before+1 {
		t.Fatalf("published %d events, want one more than %d", n, before)
	}
	last := fix.publisher.events[len(fix.publisher.events)-1]
	wantName := "vote-changed:" + strconv.FormatInt(card.ID, 10)
	if last.boardID != bid || last.name != wantName {
		t.Errorf("published %+v, want board %q name %q", last, bid, wantName)
	}
	if !strings.Contains(last.html, "0 votes") {
		t.Errorf("unvote broadcast missing the decremented count (payload: %.200s…)", last.html)
	}
	if got := storedVotes(t, fix, card.ID); got != 0 {
		t.Errorf("stored votes = %d, want 0 after unvote", got)
	}
}

func TestUnvoteWithoutVoteRejected(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "never liked", "ana")

	rec := doRequest(t, fix.mux, http.MethodPost, unvoteTarget(card.ID), nil, ana...)
	assertFlashOnly(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "not voted") {
		t.Errorf("unvote flash missing the reason (body: %s)", rec.Body.String())
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("rejected unvote published %d events, want none", n)
	}
}

func TestVoteRequiresParticipant(t *testing.T) {
	fix, bid, _ := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "stranger bait", "ana")

	rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(card.ID), nil)
	assertFlashOnly(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "Join the board first") {
		t.Errorf("stranger vote flash missing the join hint (body: %s)", rec.Body.String())
	}
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, unvoteTarget(card.ID), nil),
		http.StatusForbidden)
	if got := storedVotes(t, fix, card.ID); got != 0 {
		t.Errorf("stored votes = %d, want 0 after stranger votes", got)
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("stranger votes published %d events, want none", n)
	}
}

func TestVoteGatedOnPhase(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	collectMux := http.NewServeMux()
	collectMux.HandleFunc("POST /cards/{cid}/vote", fix.handler.Vote)
	collectMux.HandleFunc("POST /cards/{cid}/unvote", fix.handler.Unvote)
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "too early", "ana")

	rec := doRequest(t, collectMux, http.MethodPost, voteTarget(card.ID), nil, ana...)
	assertFlashOnly(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "vote phase") {
		t.Errorf("collect-window vote flash missing the reason (body: %s)", rec.Body.String())
	}
	assertFlashOnly(t,
		doRequest(t, collectMux, http.MethodPost, unvoteTarget(card.ID), nil, ana...),
		http.StatusForbidden)
	if got := storedVotes(t, fix, card.ID); got != 0 {
		t.Errorf("stored votes = %d, want 0 while gated", got)
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("gated votes published %d events, want none", n)
	}
}

func TestVoteGatedOnLock(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	locked := NewBoards(voteGateStore{boardStore: fix.store, phase: "vote", locked: true},
		fix.handler.secret, fix.publisher, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cards/{cid}/vote", locked.Vote)
	mux.HandleFunc("POST /cards/{cid}/unvote", locked.Unvote)
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "locked out", "ana")

	rec := doRequest(t, mux, http.MethodPost, voteTarget(card.ID), nil, ana...)
	assertFlashOnly(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "locked") {
		t.Errorf("locked vote flash missing the reason (body: %s)", rec.Body.String())
	}
	assertFlashOnly(t,
		doRequest(t, mux, http.MethodPost, unvoteTarget(card.ID), nil, ana...),
		http.StatusForbidden)
	if got := storedVotes(t, fix, card.ID); got != 0 {
		t.Errorf("stored votes = %d, want 0 while locked", got)
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("locked votes published %d events, want none", n)
	}
}

// mismatchVoteStore forces the defensive cross-board path in the vote tx.
// The handler always resolves the participant against the card's board,
// so a real store can never return ErrBoardMismatch here; the fake
// returns it wrapped (pinning the errors.Is mapping) for both directions.
type mismatchVoteStore struct {
	voteGateStore
}

func (s mismatchVoteStore) Vote(ctx context.Context, participantID, cardID int64) error {
	return fmt.Errorf("record vote: %w", db.ErrBoardMismatch)
}

func (s mismatchVoteStore) Unvote(ctx context.Context, participantID, cardID int64) error {
	return fmt.Errorf("remove vote: %w", db.ErrBoardMismatch)
}

func TestVoteBoardMismatchRejected(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	mismatched := NewBoards(mismatchVoteStore{voteGateStore{boardStore: fix.store, phase: "vote"}},
		fix.handler.secret, fix.publisher, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cards/{cid}/vote", mismatched.Vote)
	mux.HandleFunc("POST /cards/{cid}/unvote", mismatched.Unvote)
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "foreign card", "ana")

	for _, target := range []string{voteTarget(card.ID), unvoteTarget(card.ID)} {
		rec := doRequest(t, mux, http.MethodPost, target, nil, ana...)
		assertFlashOnly(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), "different board") {
			t.Errorf("POST %s flash missing the reason (body: %s)", target, rec.Body.String())
		}
	}
	if got := storedVotes(t, fix, card.ID); got != 0 {
		t.Errorf("stored votes = %d, want 0 after mismatched votes", got)
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("mismatched votes published %d events, want none", n)
	}
}

func TestVoteUnknownCardIsPlain404(t *testing.T) {
	fix, _, ana := voteFixture(t)

	for _, target := range []string{"/cards/999999/vote", "/cards/not-a-number/vote", "/cards/999999/unvote"} {
		rec := doRequest(t, fix.mux, http.MethodPost, target, nil, ana...)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s status = %d, want 404", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `id="flash"`) {
			t.Errorf("POST %s: 404 must not carry a flash fragment", target)
		}
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("unknown-card votes published %d events, want none", n)
	}
}

func TestVotePublishFailureConvergesActor(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if _, err := fix.store.SetPhase(t.Context(), board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	broken := NewBoards(voteGateStore{boardStore: fix.store, phase: "vote"},
		fix.handler.secret, failingPublisher{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cards/{cid}/vote", broken.Vote)
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "kept anyway", "ana")

	rec := doRequest(t, mux, http.MethodPost, voteTarget(card.ID), nil, ana...)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST vote with broken broadcast status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="card-` + strconv.FormatInt(card.ID, 10) + `"`,
		"1 vote",
		`id="flash"`,
		`hx-swap-oob="true"`,
		"reload to resync",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failure body missing %q (body: %.300s…)", want, body)
		}
	}
	if got := storedVotes(t, fix, card.ID); got != 1 {
		t.Errorf("failed broadcast rolled back the commit: votes = %d, want 1", got)
	}
}

func TestVoteButtonWiringInCardFragment(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "buttoned", "ana")

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	cid := strconv.FormatInt(card.ID, 10)
	for _, want := range []string{
		`sse-swap="card-updated:` + cid + `, vote-changed:` + cid + `, card-removed:` + cid + `"`,
		`hx-post="/cards/` + cid + `/vote"`,
		`hx-target="#card-` + cid + `"`,
		`hx-swap="morph:outerHTML"`,
		`hx-disabled-elt="this"`,
		`class="vote-button"`,
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("card fragment missing vote wiring %q", want)
		}
	}
	at := strings.Index(shell, `id="card-`+cid+`"`)
	if at < 0 {
		t.Fatal("shell missing the card node")
	}
	node := shell[at:]
	if end := strings.Index(node, "</article>"); end > 0 {
		node = node[:end]
	}
	if strings.Contains(node, "x-data") {
		t.Error("card fragment must carry bindings only; scopes live above the swap target")
	}
}

func TestShellReflectsVotedState(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	voted := seedBoardCard(t, fix, bid, "liked", "ana")
	plain := seedBoardCard(t, fix, bid, "unliked", "ana")

	if rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(voted.ID), nil, ana...); rec.Code != http.StatusOK {
		t.Fatalf("vote status = %d, want 200", rec.Code)
	}
	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	vid, pid := strconv.FormatInt(voted.ID, 10), strconv.FormatInt(plain.ID, 10)
	if !strings.Contains(shell, `hx-post="/cards/`+vid+`/unvote"`) {
		t.Error("voted card must offer the unvote control on reread")
	}
	if !strings.Contains(shell, `hx-post="/cards/`+pid+`/vote"`) {
		t.Error("unvoted card must offer the vote control on reread")
	}
}

func TestConnectionBadgeAndPollFallback(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	seedBoardCard(t, fix, bid, "badge", "ana")

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	for _, want := range []string{
		`id="connection-badge"`,
		`id="flash"`,
		"htmx:sseOpen",
		"htmx:sseError",
		"5000",
		"?fragment=columns",
		`data-columns-url="/b/` + bid + `?fragment=columns"`,
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("board shell missing connection piece %q", want)
		}
	}
	if got := strings.Count(shell, `id="connection-badge"`); got != 1 {
		t.Errorf("shell holds %d connection badges, want exactly 1", got)
	}

	// Viewers who join late get the same badge and flash home with the
	// columns fragment, since they never load the shell first.
	late := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
		url.Values{"name": {"late"}})
	lateBody := late.Body.String()
	for _, want := range []string{`id="connection-badge"`, `id="flash"`, `id="board-columns"`} {
		if !strings.Contains(lateBody, want) {
			t.Errorf("join fragment missing %q", want)
		}
	}
}

func TestColumnsFragmentEndpoint(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	seedBoardCard(t, fix, bid, "fragment", "ana")

	rec := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"?fragment=columns", nil, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("fragment status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`id="board-columns"`, "fragment", `id="connection-badge"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("fragment body missing %q", want)
		}
	}

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"?fragment=columns", nil),
		http.StatusForbidden)

	missing := doRequest(t, fix.mux, http.MethodGet, "/b/does-not-exist?fragment=columns", nil, ana...)
	if missing.Code != http.StatusNotFound {
		t.Errorf("fragment on unknown board status = %d, want 404", missing.Code)
	}
}

func TestBoardShellKeepsViewerVoteState(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "keeper", "ana")
	if rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(card.ID), nil, ana...); rec.Code != http.StatusOK {
		t.Fatalf("vote status = %d, want 200", rec.Code)
	}

	// Broadcasts carry the acting voter's button state (one payload for
	// every viewer), so the shell must embed the keeper that re-applies
	// each viewer's own Vote/Unvote control after foreign swaps. Pin its
	// stable markers here so a removed script fails loudly instead of
	// stranding every multi-voter board on the wrong button.
	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	for _, want := range []string{
		"vote-keeper",
		"__voted",
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("board shell missing vote keeper piece %q", want)
		}
	}
}

func TestVoteDeniesRenderThroughShellHandler(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "denied", "ana")

	// A denied vote carries only the flash node, which the shell's
	// response-error handler renders: assert the exact contract here so
	// the client piece has something stable to parse.
	if rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(card.ID), nil, ana...); rec.Code != http.StatusOK {
		t.Fatalf("first vote status = %d, want 200", rec.Code)
	}
	denied := doRequest(t, fix.mux, http.MethodPost, voteTarget(card.ID), nil, ana...)
	assertFlashOnly(t, denied, http.StatusForbidden)

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if !strings.Contains(shell, "htmx:responseError") {
		t.Error("board shell must handle denied swaps by rendering the flash text")
	}
}

func TestVoteStreamReachesSubscriber(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "streamed vote", "ana")

	broker := realtime.NewBroker()
	fix.handler.events = funnelPublisher{broker: broker}
	fix.handler.broker = broker
	mux := eventsMux(fix)
	rec := startEventsStream(t, mux, "/b/"+bid+"/events", nil, ana...)
	waitForSubscribed(t, broker, bid, 1)

	if voted := doRequest(t, fix.mux, http.MethodPost, voteTarget(card.ID), nil, ana...); voted.Code != http.StatusOK {
		t.Fatalf("vote status = %d, want 200", voted.Code)
	}
	msg := nextMessage(t, rec)
	wantName := "vote-changed:" + strconv.FormatInt(card.ID, 10)
	if !strings.Contains(msg, "event: "+wantName) {
		t.Errorf("stream message missing event %q (message: %q)", wantName, msg)
	}
	if !strings.Contains(msg, "1 vote") {
		t.Errorf("stream message missing the updated count (message: %q)", msg)
	}
}

func TestUnvoteStreamReachesSubscriber(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "streamed unvote", "ana")

	if rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(card.ID), nil, ana...); rec.Code != http.StatusOK {
		t.Fatalf("vote status = %d, want 200", rec.Code)
	}
	broker := realtime.NewBroker()
	fix.handler.events = funnelPublisher{broker: broker}
	fix.handler.broker = broker
	mux := eventsMux(fix)
	rec := startEventsStream(t, mux, "/b/"+bid+"/events", nil, ana...)
	waitForSubscribed(t, broker, bid, 1)

	if unvoted := doRequest(t, fix.mux, http.MethodPost, unvoteTarget(card.ID), nil, ana...); unvoted.Code != http.StatusOK {
		t.Fatalf("unvote status = %d, want 200", unvoted.Code)
	}
	msg := nextMessage(t, rec)
	wantName := "vote-changed:" + strconv.FormatInt(card.ID, 10)
	if !strings.Contains(msg, "event: "+wantName) {
		t.Errorf("stream message missing event %q (message: %q)", wantName, msg)
	}
}

func TestJoinResponseCarriesVoteControls(t *testing.T) {
	fix, bid, _ := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "join controls", "ana")
	cid := strconv.FormatInt(card.ID, 10)

	join := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
		url.Values{"name": {"jo"}})
	if join.Code != http.StatusOK {
		t.Fatalf("join status = %d, want 200", join.Code)
	}
	if body := join.Body.String(); !strings.Contains(body, `hx-post="/cards/`+cid+`/vote"`) {
		t.Error("join columns must carry the vote control")
	}
}

func TestVoteButtonPendingState(t *testing.T) {
	fix, bid, ana := voteFixture(t)
	card := seedBoardCard(t, fix, bid, "pending", "ana")
	cid := strconv.FormatInt(card.ID, 10)

	// Pending is client-side (the disabled control plus the request class
	// htmx applies mid-flight); the server fragment must carry the hooks
	// that drive idle to pending and back.
	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	start := strings.Index(shell, `hx-post="/cards/`+cid+`/vote"`)
	if start < 0 {
		t.Fatal("vote control missing from shell")
	}
	open := strings.LastIndex(shell[:start], "<button")
	close := strings.Index(shell[start:], ">")
	if open < 0 || close < 0 {
		t.Fatal("vote control has no button tag")
	}
	tag := shell[open : start+close]
	if !strings.Contains(tag, "hx-disabled-elt") {
		t.Error("vote control must disable itself while its request is in flight")
	}
	// Pending styling ships in the app stylesheet, not inline: the
	// shell must link it, and the stylesheet must carry the rule htmx
	// applies mid-flight.
	if !strings.Contains(shell, `/static/app.css`) {
		t.Error("shell must link the app stylesheet")
	}
	raw, err := os.ReadFile("../static/app.css")
	if err != nil {
		t.Fatalf("read app stylesheet: %v", err)
	}
	if !strings.Contains(string(raw), ".vote-button.htmx-request") {
		t.Error("app stylesheet must style the in-flight vote control")
	}
}
