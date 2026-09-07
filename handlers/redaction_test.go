package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"hindsight/db"
	"hindsight/realtime"
	"hindsight/templates"
)

// blindBoardStore places a board under blind collection in the voting
// window, standing in for the facilitation controls that own those
// transitions.
type blindBoardStore struct {
	boardStore
}

func (s blindBoardStore) GetBoardByPublicID(ctx context.Context, publicID string) (*db.Board, error) {
	board, err := s.boardStore.GetBoardByPublicID(ctx, publicID)
	if err != nil {
		return nil, err
	}
	board.CardsHidden = true
	board.Phase = "vote"
	return board, nil
}

func (s blindBoardStore) GetBoardByID(ctx context.Context, boardID int64) (*db.Board, error) {
	board, err := s.boardStore.GetBoardByID(ctx, boardID)
	if err != nil {
		return nil, err
	}
	board.CardsHidden = true
	board.Phase = "vote"
	return board, nil
}

// blindFixture routes the card surface through the blind-collection
// store. Joining goes through the base fixture; rows and cookies work
// across both handlers because they share the store and secret.
func blindFixture(t *testing.T) (*boardFixture, string, map[string]*http.Cookie, []*http.Cookie) {
	t.Helper()

	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	if _, err := fix.store.SetPhase(t.Context(), boardID(t, fix, bid), "vote"); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	blind := NewBoards(blindBoardStore{boardStore: fix.store},
		fix.handler.secret, fix.publisher, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /b/{bid}", blind.Show)
	mux.HandleFunc("POST /b/{bid}/join", blind.Join)
	mux.HandleFunc("POST /b/{bid}/cards", blind.CreateCard)
	mux.HandleFunc("PUT /cards/{cid}", blind.UpdateCard)
	mux.HandleFunc("DELETE /cards/{cid}", blind.DeleteCard)
	mux.HandleFunc("POST /cards/{cid}/vote", blind.Vote)
	mux.HandleFunc("POST /cards/{cid}/unvote", blind.Unvote)
	mux.HandleFunc("POST /cards/{cid}/comments", blind.CreateComment)
	mux.HandleFunc("POST /b/{bid}/kudos", blind.CreateKudo)
	mux.HandleFunc("DELETE /kudos/{kid}", blind.DeleteKudo)
	mux.HandleFunc("POST /b/{bid}/actions", blind.CreateAction)
	mux.HandleFunc("PUT /actions/{aid}", blind.UpdateAction)
	mux.HandleFunc("DELETE /actions/{aid}", blind.DeleteAction)
	mux.HandleFunc("POST /b/{bid}/sort", blind.Sort)
	ana := joinAs(t, fix, bid, "zed")
	return &boardFixture{handler: blind, store: fix.store, publisher: fix.publisher, mux: mux},
		bid, creatorCookies, ana
}

func creatorParticipantID(t *testing.T, fix *boardFixture, cookies map[string]*http.Cookie, bid string) int64 {
	t.Helper()

	raw := extractToken(t, fix, cookies, partsCookieName, bid)
	found, err := fix.store.GetParticipantByToken(t.Context(), boardID(t, fix, bid), raw)
	if err != nil {
		t.Fatalf("GetParticipantByToken: %v", err)
	}
	return found.ID
}

func publishedPayloads(f *recordingPublisher, full bool) []publishedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []publishedEvent
	for _, ev := range f.events {
		if strings.HasSuffix(ev.name, fullEventSuffix) == full {
			out = append(out, ev)
		}
	}
	return out
}

func TestBlindCollectHidesBodiesFromParticipants(t *testing.T) {
	fix, bid, creatorCookies, ana := blindFixture(t)
	col := firstColumnID(t, fix, bid)
	shellSecret, createSecret := "shell-secret-alpha", "create-secret-beta"
	editSecret, author := "edit-secret-gamma", "secret-author-zed"

	// One seeded vote proves counts hide too: ranking leaks through
	// totals, so participants see zero until the flag clears.
	seeded := seedBoardCard(t, fix, bid, shellSecret, author)
	other := seedBoardCard(t, fix, bid, "second-secret-delta", author)
	creatorID := creatorParticipantID(t, fix, creatorCookies, bid)
	if err := fix.store.Vote(t.Context(), creatorID, seeded.ID); err != nil {
		t.Fatalf("seed vote: %v", err)
	}

	checkRedacted := func(label, body string, secrets ...string) {
		t.Helper()
		if !strings.Contains(body, redactedBody) {
			t.Errorf("%s: response carries no redaction placeholder", label)
		}
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Errorf("%s: response leaks %q", label, secret)
			}
		}
		if !strings.Contains(body, `id="card-`+strconv.FormatInt(seeded.ID, 10)+`"`) {
			t.Errorf("%s: response must keep the real card id", label)
		}
		if !strings.Contains(body, `data-position="0"`) {
			t.Errorf("%s: response must keep the real position", label)
		}
		if strings.Contains(body, "1 vote") {
			t.Errorf("%s: response leaks the real vote count", label)
		}
	}

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	checkRedacted("shell", shell, shellSecret, author)

	fragment := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"?fragment=columns", nil, ana...).Body.String()
	checkRedacted("columns fragment", fragment, shellSecret, author)

	join := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
		url.Values{"name": {"late-joiner"}})
	checkRedacted("join", join.Body.String(), shellSecret, author)

	created := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/cards",
		url.Values{
			"column_id": {strconv.FormatInt(col, 10)},
			"body":      {createSecret},
		}, ana...)
	if created.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200 (body: %s)", created.Code, created.Body.String())
	}
	checkRedacted("create response", created.Body.String(), shellSecret, createSecret, "zed")

	edited := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(seeded.ID, 10),
		url.Values{"body": {editSecret}}, ana...)
	if edited.Code != http.StatusOK {
		t.Fatalf("edit status = %d, want 200 (body: %s)", edited.Code, edited.Body.String())
	}
	checkRedacted("edit response", edited.Body.String(), shellSecret, editSecret)

	moved := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(seeded.ID, 10),
		url.Values{
			"column_id": {strconv.FormatInt(secondColumnID(t, fix, bid), 10)},
			"position":  {"0"},
		}, ana...)
	if moved.Code != http.StatusOK {
		t.Fatalf("move status = %d, want 200 (body: %s)", moved.Code, moved.Body.String())
	}
	checkRedacted("move response", moved.Body.String(), shellSecret, editSecret)

	voted := doRequest(t, fix.mux, http.MethodPost, "/cards/"+strconv.FormatInt(seeded.ID, 10)+"/vote", nil, ana...)
	if voted.Code != http.StatusOK {
		t.Fatalf("vote status = %d, want 200 (body: %s)", voted.Code, voted.Body.String())
	}
	checkRedacted("vote response", voted.Body.String(), shellSecret, editSecret)

	unvoted := doRequest(t, fix.mux, http.MethodPost, "/cards/"+strconv.FormatInt(seeded.ID, 10)+"/unvote", nil, ana...)
	if unvoted.Code != http.StatusOK {
		t.Fatalf("unvote status = %d, want 200 (body: %s)", unvoted.Code, unvoted.Body.String())
	}
	checkRedacted("unvote response", unvoted.Body.String(), shellSecret, editSecret)

	// Every participant-facing broadcast must be body-free too.
	for _, ev := range publishedPayloads(fix.publisher, false) {
		for _, secret := range []string{shellSecret, createSecret, editSecret, author, other.Body} {
			if strings.Contains(ev.html, secret) {
				t.Errorf("participant event %q leaks %q", ev.name, secret)
			}
		}
		if strings.Contains(ev.html, "1 vote") || strings.Contains(ev.html, "2 votes") {
			t.Errorf("participant event %q leaks a real vote count", ev.name)
		}
	}
}

func TestBlindCollectFacilitatorSeesFullBodies(t *testing.T) {
	fix, bid, creatorCookies, _ := blindFixture(t)
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)
	col := firstColumnID(t, fix, bid)
	secret := "facilitator-visible-body"

	seeded := seedBoardCard(t, fix, bid, secret, "secret-author-zed")
	creatorID := creatorParticipantID(t, fix, creatorCookies, bid)
	if err := fix.store.Vote(t.Context(), creatorID, seeded.ID); err != nil {
		t.Fatalf("seed vote: %v", err)
	}

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, fac...).Body.String()
	for _, want := range []string{secret, "secret-author-zed", "1 vote"} {
		if !strings.Contains(shell, want) {
			t.Errorf("facilitator shell missing %q", want)
		}
	}
	if strings.Contains(shell, redactedBody) {
		t.Error("facilitator shell must not carry the redaction placeholder")
	}

	fragment := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"?fragment=columns", nil, fac...).Body.String()
	if !strings.Contains(fragment, secret) {
		t.Error("facilitator columns fragment missing the card body")
	}

	created := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/cards",
		url.Values{
			"column_id": {strconv.FormatInt(col, 10)},
			"body":      {"fac-created-body"},
		}, fac...)
	if created.Code != http.StatusOK {
		t.Fatalf("facilitator create status = %d, want 200", created.Code)
	}
	if !strings.Contains(created.Body.String(), "fac-created-body") {
		t.Error("facilitator create response must render the full body")
	}

	// The full-bodies broadcast variants carry the secrets the base
	// events redact.
	fulls := publishedPayloads(fix.publisher, true)
	if len(fulls) == 0 {
		t.Fatal("blind collection published no full-bodies variants")
	}
	joined := ""
	for _, ev := range fulls {
		joined += ev.html
	}
	for _, want := range []string{secret, "fac-created-body"} {
		if !strings.Contains(joined, want) {
			t.Errorf("full-bodies variants missing %q", want)
		}
	}
	for _, ev := range fulls {
		if strings.HasSuffix(ev.name, fullEventSuffix) == false {
			t.Errorf("full variant %q must carry the suffix", ev.name)
		}
	}
}

func TestCardBroadcastPairsToggleWithHiddenFlag(t *testing.T) {
	t.Run("open collection publishes once", func(t *testing.T) {
		fix := openFixture(t)
		bid, _ := createBoard(t, fix.mux, defaultCreateForm())
		ana := joinAs(t, fix, bid, "ana")
		rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/cards",
			url.Values{
				"column_id": {strconv.FormatInt(firstColumnID(t, fix, bid), 10)},
				"body":      {"open-body"},
			}, ana...)
		if rec.Code != http.StatusOK {
			t.Fatalf("create status = %d, want 200", rec.Code)
		}
		if n := fix.publisher.count(); n != 1 {
			t.Fatalf("open create published %d events, want exactly 1", n)
		}
		if strings.HasSuffix(fix.publisher.events[0].name, fullEventSuffix) {
			t.Errorf("open create must not publish a full-bodies variant")
		}
	})

	t.Run("blind collection publishes viewer plus full variant", func(t *testing.T) {
		fix, bid, _, ana := blindFixture(t)
		rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/cards",
			url.Values{
				"column_id": {strconv.FormatInt(firstColumnID(t, fix, bid), 10)},
				"body":      {"blind-body"},
			}, ana...)
		if rec.Code != http.StatusOK {
			t.Fatalf("create status = %d, want 200", rec.Code)
		}
		if n := fix.publisher.count(); n != 2 {
			t.Fatalf("blind create published %d events, want 2 (viewer + full)", n)
		}
		base, full := fix.publisher.events[0], fix.publisher.events[1]
		if base.name+fullEventSuffix != full.name {
			t.Errorf("variant pair = %q + %q, want base and suffixed names", base.name, full.name)
		}
		if strings.Contains(base.html, "blind-body") {
			t.Error("viewer event must redact the body")
		}
		if !strings.Contains(full.html, "blind-body") {
			t.Error("full variant must carry the body")
		}
	})
}

// emptyColumnStore hides one column's cards, standing in for a card
// whose column listing no longer contains it (a move landing between
// the card read and the fragment render).
type emptyColumnStore struct {
	boardStore
	emptyID int64
}

func (s emptyColumnStore) ListCards(ctx context.Context, columnID int64) ([]db.Card, error) {
	if columnID == s.emptyID {
		return nil, nil
	}
	return s.boardStore.ListCards(ctx, columnID)
}

func TestRedactedFlagCoversPlaceholderBodies(t *testing.T) {
	fix, bid, creatorCookies, _ := blindFixture(t)
	col := firstColumnID(t, fix, bid)
	card := seedBoardCard(t, fix, bid, redactedBody, "secret-author")
	creatorID := creatorParticipantID(t, fix, creatorCookies, bid)
	if err := fix.store.Vote(t.Context(), creatorID, card.ID); err != nil {
		t.Fatalf("seed vote: %v", err)
	}
	card, gerr := fix.store.GetCard(t.Context(), card.ID)
	if gerr != nil {
		t.Fatalf("GetCard: %v", gerr)
	}
	board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	board.CardsHidden = true
	evac := NewBoards(emptyColumnStore{boardStore: fix.handler.store, emptyID: col},
		fix.handler.secret, fix.publisher, nil)
	req := httptest.NewRequest(http.MethodGet, "/b/"+bid, nil)

	// The card misses its own column render, so the viewer treatment
	// must come from the redacted flag — never from comparing the body
	// against the placeholder, which this card literally carries.
	viewer, full, err := evac.renderCardPair(req, board, card, nil)
	if err != nil {
		t.Fatalf("renderCardPair: %v", err)
	}
	for _, leak := range []string{"secret-author", "1 vote"} {
		if strings.Contains(viewer, leak) {
			t.Errorf("viewer fragment leaks %q for a placeholder-bodied card", leak)
		}
	}
	if !strings.Contains(viewer, redactedBody) {
		t.Error("viewer fragment carries no redaction placeholder")
	}
	for _, want := range []string{"secret-author", "1 vote"} {
		if !strings.Contains(full, want) {
			t.Errorf("full fragment missing %q", want)
		}
	}
}

func TestStreamFiltersFullVariantByRole(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	broker := realtime.NewBroker()
	fix.handler.events = funnelPublisher{broker: broker}
	fix.handler.broker = broker
	mux := eventsMux(fix)

	ana := joinAs(t, fix, bid, "ana")
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)
	participantStream := startEventsStream(t, mux, "/b/"+bid+"/events", nil, ana...)
	facilitatorStream := startEventsStream(t, mux, "/b/"+bid+"/events", nil, fac...)
	waitForSubscribed(t, broker, bid, 2)

	broker.Publish(bid, "column-1", "<redacted-column/>")
	broker.Publish(bid, "column-1"+fullEventSuffix, "<full-column/>")
	broker.Publish(bid, "column-9", "<marker/>")

	// The participant never sees the full variant: base, id-only skip
	// advancing Last-Event-ID, then marker.
	first := nextMessage(t, participantStream)
	if !strings.Contains(first, "event: column-1\n") || !strings.Contains(first, "<redacted-column/>") {
		t.Errorf("participant first message = %q, want the redacted base", first)
	}
	skip := nextMessage(t, participantStream)
	if !strings.Contains(skip, "id: ") || strings.Contains(skip, "event:") {
		t.Errorf("participant skip message = %q, want an id-only block", skip)
	}
	second := nextMessage(t, participantStream)
	if !strings.Contains(second, "event: column-9") {
		t.Errorf("participant third message = %q, want the marker (full variant skipped)", second)
	}

	// The facilitator gets the variant renamed to the base name, so
	// the same swap targets converge on full bodies.
	_ = nextMessage(t, facilitatorStream) // redacted base, converges next
	renamed := nextMessage(t, facilitatorStream)
	if !strings.Contains(renamed, "event: column-1\n") || !strings.Contains(renamed, "<full-column/>") {
		t.Errorf("facilitator variant message = %q, want it renamed to the base name with full bodies", renamed)
	}
	third := nextMessage(t, facilitatorStream)
	if !strings.Contains(third, "event: column-9") {
		t.Errorf("facilitator third message = %q, want the marker", third)
	}
}

func TestCardSwapsStayGranularAroundDrafts(t *testing.T) {
	t.Run("vote broadcast touches only the voted card", func(t *testing.T) {
		fix, bid, ana := voteFixture(t)
		voted := seedBoardCard(t, fix, bid, "alpha", "ana")
		draft := seedBoardCard(t, fix, bid, "beta-draft-marker", "ana")

		if rec := doRequest(t, fix.mux, http.MethodPost, voteTarget(voted.ID), nil, ana...); rec.Code != http.StatusOK {
			t.Fatalf("vote status = %d, want 200", rec.Code)
		}
		if n := fix.publisher.count(); n != 1 {
			t.Fatalf("vote published %d events, want exactly 1", n)
		}
		payload := fix.publisher.events[0].html
		for _, want := range []string{`id="card-` + strconv.FormatInt(voted.ID, 10) + `"`, "alpha"} {
			if !strings.Contains(payload, want) {
				t.Errorf("vote payload missing %q", want)
			}
		}
		// A draft typed in the sibling card survives because the morph
		// targets this node alone: the payload must not mention it.
		for _, leak := range []string{`id="card-` + strconv.FormatInt(draft.ID, 10) + `"`, "beta-draft-marker"} {
			if strings.Contains(payload, leak) {
				t.Errorf("vote payload leaks sibling content %q; the draft would be clobbered", leak)
			}
		}
	})

	t.Run("edit broadcast touches only the edited card", func(t *testing.T) {
		fix := openFixture(t)
		bid, _ := createBoard(t, fix.mux, defaultCreateForm())
		ana := joinAs(t, fix, bid, "ana")
		edited := seedBoardCard(t, fix, bid, "before", "ana")
		draft := seedBoardCard(t, fix, bid, "gamma-draft-marker", "ana")

		if rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(edited.ID, 10),
			url.Values{"body": {"after"}}, ana...); rec.Code != http.StatusOK {
			t.Fatalf("edit status = %d, want 200", rec.Code)
		}
		payload := fix.publisher.events[0].html
		if !strings.Contains(payload, `id="card-`+strconv.FormatInt(edited.ID, 10)+`"`) {
			t.Error("edit payload missing the edited card node")
		}
		for _, leak := range []string{`id="card-` + strconv.FormatInt(draft.ID, 10) + `"`, "gamma-draft-marker"} {
			if strings.Contains(payload, leak) {
				t.Errorf("edit payload leaks sibling content %q; the draft would be clobbered", leak)
			}
		}
	})
}

func TestBoardShellScopesAndTimerPlacement(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "scoped", "ana")
	cid := strconv.FormatInt(card.ID, 10)

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()

	// Exactly one global hook re-initializes swapped subtrees.
	if got := strings.Count(shell, "htmx:afterSwap"); got != 1 {
		t.Errorf("shell holds %d htmx:afterSwap hooks, want exactly 1", got)
	}
	if !strings.Contains(shell, "Alpine.initTree") {
		t.Error("shell missing the Alpine re-init hook for swapped subtrees")
	}

	// Scopes live at shell level or above, never inside a card node.
	mainAt := strings.Index(shell, "<main")
	columnsAt := strings.Index(shell, `id="board-columns"`)
	if mainAt < 0 || columnsAt < 0 || mainAt > columnsAt {
		t.Fatal("shell must render main wrapping the columns root")
	}
	if !strings.Contains(shell[mainAt:columnsAt], "x-data") {
		t.Error("board shell must scope local UI state above the swap targets")
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

	// The countdown placeholder lives outside any swapped subtree.
	timerAt := strings.Index(shell, `id="timer"`)
	if timerAt < 0 {
		t.Error("shell missing the countdown placeholder")
	} else if timerAt > columnsAt {
		t.Error("countdown placeholder must live outside the swapped columns subtree")
	}

	// The join gate carries the same shell scope, so bindings arriving
	// with the joined columns resolve instead of erroring.
	gate := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil).Body.String()
	gateMain := strings.Index(gate, "<main")
	if gateMain < 0 {
		t.Fatal("join gate missing the main element")
	}
	if !strings.Contains(gate[gateMain:], `x-data="boardDnD()"`) {
		t.Error("join gate must scope local UI state above the joining columns")
	}
}

func TestSortRefusedWhileBlind(t *testing.T) {
	fix, bid, creatorCookies, _ := blindFixture(t)
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)
	col := firstColumnID(t, fix, bid)

	low := seedBoardCard(t, fix, bid, "low-body", "ana")
	high := seedBoardCard(t, fix, bid, "high-body", "ana")
	creatorID := creatorParticipantID(t, fix, creatorCookies, bid)
	if err := fix.store.Vote(t.Context(), creatorID, high.ID); err != nil {
		t.Fatalf("seed vote: %v", err)
	}

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/sort",
		url.Values{"column_id": {strconv.FormatInt(col, 10)}}, fac...)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("sort status = %d, want 403 while blind", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="flash"`) {
		t.Error("sort refusal must carry the flash fragment")
	}

	// Vote order must not persist: low keeps position 0.
	after, err := fix.store.GetCard(t.Context(), low.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if after.Position != 0 {
		t.Errorf("low card position = %d, want 0 (sort must not persist while blind)", after.Position)
	}
	_ = high
}

func TestRedactedViewClearsStateAndSender(t *testing.T) {
	view := redactCardView(templates.CardView{
		Body:       "secret",
		AuthorName: "ana",
		Votes:      3,
		Voted:      true,
		Discussed:  true,
		Members: []templates.CardView{{
			Body: "member-secret", AuthorName: "bo", Votes: 2, Voted: true,
		}},
		Comments: []templates.CommentView{{Body: "remark-secret", AuthorName: "cy"}},
	})
	if view.Voted || view.Discussed || view.Votes != 0 {
		t.Error("redacted card must clear votes, voted, and discussed state")
	}
	if view.Members[0].Body != redactedBody || view.Members[0].Voted {
		t.Error("redacted card must redact group members recursively")
	}

	kudo := redactKudoView(templates.KudoView{To: "bo", Body: "kind words", From: "ana"})
	if kudo.From != "" || kudo.Body != redactedBody || kudo.To != "bo" {
		t.Errorf("redacted kudo = %+v, want body+sender hidden and recipient kept", kudo)
	}
}

func TestStreamFilterIgnoresUnknownFullSuffix(t *testing.T) {
	ev := realtime.Event{Name: "timer-ended:full"}
	if _, ok := filterStreamEvent(ev, false); !ok {
		t.Error("unknown suffixed name must pass through for participants")
	}
	renamed, ok := filterStreamEvent(realtime.Event{Name: "vote-changed:9:full"}, true)
	if !ok || renamed.Name != "vote-changed:9" {
		t.Errorf("known full variant renamed to %q (ok=%v), want vote-changed:9", renamed.Name, ok)
	}
}
