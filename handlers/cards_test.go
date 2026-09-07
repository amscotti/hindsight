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
)

func secondColumnID(t *testing.T, fix *boardFixture, publicID string) int64 {
	t.Helper()

	cols, err := fix.store.ListColumns(t.Context(), boardID(t, fix, publicID))
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	if len(cols) < 2 {
		t.Fatal("board has fewer than two columns")
	}
	return cols[1].ID
}

func seedBoardCard(t *testing.T, fix *boardFixture, publicID, body, author string) *db.Card {
	t.Helper()

	card, err := fix.store.CreateCard(t.Context(), firstColumnID(t, fix, publicID), body, author)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	return card
}

func cardIDFromBody(t *testing.T, body string) int64 {
	t.Helper()

	const marker = `id="card-`
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("response carries no card node (body: %.200s…)", body)
	}
	rest := body[idx+len(marker):]
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end <= 0 {
		t.Fatalf("card node has no numeric id (body: %.200s…)", body)
	}
	id, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil {
		t.Fatalf("parse card id: %v", err)
	}
	return id
}

func publishedNames(f *recordingPublisher) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, ev := range f.events {
		names = append(names, ev.name)
	}
	return names
}

func TestCreateCardPublishesColumnEventPostCommit(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	col := firstColumnID(t, fix, bid)

	committed := false
	fix.publisher.hook = func(ev publishedEvent) {
		cards, err := fix.store.ListCards(context.Background(), col)
		if err != nil {
			t.Errorf("ListCards inside publish hook: %v", err)
			return
		}
		if len(cards) == 1 && cards[0].Body == "first card" {
			committed = true
		}
	}

	rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/cards",
		url.Values{
			"column_id": {strconv.FormatInt(col, 10)},
			"body":      {"first card"},
		}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	cardID := cardIDFromBody(t, body)
	if !strings.Contains(body, "first card") {
		t.Error("response must render the new card for the acting client")
	}
	if !committed {
		t.Error("broadcast fired before the insert committed")
	}

	events := fix.publisher.events
	if len(events) != 1 {
		t.Fatalf("published %d events, want exactly 1", len(events))
	}
	wantName := "column-" + strconv.FormatInt(col, 10)
	if events[0].boardID != bid || events[0].name != wantName {
		t.Errorf("published %+v, want board %q name %q", events[0], bid, wantName)
	}
	for _, want := range []string{
		`id="card-` + strconv.FormatInt(cardID, 10) + `"`,
		"first card",
		`id="column-` + strconv.FormatInt(col, 10) + `"`,
	} {
		if !strings.Contains(events[0].html, want) {
			t.Errorf("broadcast payload missing %q (payload: %.300s…)", want, events[0].html)
		}
	}

	cards, err := fix.store.ListCards(t.Context(), col)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(cards) != 1 || cards[0].Position != 0 || cards[0].AuthorName != "ana" {
		t.Errorf("stored card = %+v, want one row at position 0 by ana", cards)
	}
}

func TestCreateCardValidationAndGates(t *testing.T) {
	setup := func(t *testing.T) (*boardFixture, string, int64, []*http.Cookie) {
		fix := openFixture(t)
		bid, _ := createBoard(t, fix.mux, defaultCreateForm())
		ana := joinAs(t, fix, bid, "ana")
		return fix, bid, firstColumnID(t, fix, bid), ana
	}

	t.Run("stranger cannot add", func(t *testing.T) {
		fix, bid, col, _ := setup(t)
		assertFlashOnly(t,
			doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/cards",
				url.Values{
					"column_id": {strconv.FormatInt(col, 10)},
					"body":      {"hello"},
				}),
			http.StatusForbidden)
		if n := fix.publisher.count(); n != 0 {
			t.Errorf("rejected add published %d events, want none", n)
		}
	})

	t.Run("locked board rejects adds", func(t *testing.T) {
		fix, bid, col, ana := setup(t)
		board, err := fix.store.GetBoardByPublicID(t.Context(), bid)
		if err != nil {
			t.Fatalf("GetBoardByPublicID: %v", err)
		}
		locked := NewBoards(lockedCardsStore{boardStore: fix.store, lockedID: board.PublicID},
			fix.handler.secret, fix.publisher, nil)
		mux := http.NewServeMux()
		mux.HandleFunc("POST /b/{bid}/cards", locked.CreateCard)
		assertFlashOnly(t,
			doRequest(t, mux, http.MethodPost, "/b/"+bid+"/cards",
				url.Values{
					"column_id": {strconv.FormatInt(col, 10)},
					"body":      {"hello"},
				}, ana...),
			http.StatusForbidden)
		if n := fix.publisher.count(); n != 0 {
			t.Errorf("locked add published %d events, want none", n)
		}
	})

	t.Run("overlong body is rejected", func(t *testing.T) {
		fix, bid, col, ana := setup(t)
		assertFlashOnly(t,
			doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/cards",
				url.Values{
					"column_id": {strconv.FormatInt(col, 10)},
					"body":      {strings.Repeat("x", 2001)},
				}, ana...),
			http.StatusUnprocessableEntity)
		if n := fix.publisher.count(); n != 0 {
			t.Errorf("overlong add published %d events, want none", n)
		}
		cards, err := fix.store.ListCards(t.Context(), col)
		if err != nil {
			t.Fatalf("ListCards: %v", err)
		}
		if len(cards) != 0 {
			t.Errorf("overlong add stored %d cards, want 0", len(cards))
		}
	})

	t.Run("blank body is rejected", func(t *testing.T) {
		fix, bid, col, ana := setup(t)
		assertFlashOnly(t,
			doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/cards",
				url.Values{
					"column_id": {strconv.FormatInt(col, 10)},
					"body":      {"   "},
				}, ana...),
			http.StatusUnprocessableEntity)
	})

	t.Run("unknown ids are plain 404s", func(t *testing.T) {
		fix, bid, col, ana := setup(t)
		otherBid, _ := createBoard(t, fix.mux, url.Values{
			"name":         {"Other"},
			"display_name": {"Other"},
			"template":     {"plus-delta"},
		})
		foreignCol := firstColumnID(t, fix, otherBid)

		for _, target := range []struct {
			name string
			url  string
			form url.Values
		}{
			{"unknown board", "/b/does-not-exist/cards",
				url.Values{"column_id": {strconv.FormatInt(col, 10)}, "body": {"hi"}}},
			{"unknown column", "/b/" + bid + "/cards",
				url.Values{"column_id": {"999999"}, "body": {"hi"}}},
			{"foreign column", "/b/" + bid + "/cards",
				url.Values{"column_id": {strconv.FormatInt(foreignCol, 10)}, "body": {"hi"}}},
		} {
			rec := doRequest(t, fix.mux, http.MethodPost, target.url, target.form, ana...)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: status = %d, want 404 (body: %s)", target.name, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), `id="flash"`) {
				t.Errorf("%s: 404 must not carry a flash fragment", target.name)
			}
		}
		if n := fix.publisher.count(); n != 0 {
			t.Errorf("unknown-id adds published %d events, want none", n)
		}
	})

	t.Run("card bodies escape markup", func(t *testing.T) {
		fix, bid, col, ana := setup(t)
		rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/cards",
			url.Values{
				"column_id": {strconv.FormatInt(col, 10)},
				"body":      {"<script>alert(1)</script>"},
			}, ana...)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST status = %d, want 200", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<script>") {
			t.Error("response renders raw markup; bodies must be escaped")
		}
		if !strings.Contains(rec.Body.String(), "&lt;script&gt;") {
			t.Error("response missing the escaped body")
		}
		if strings.Contains(fix.publisher.events[0].html, "<script>") {
			t.Error("broadcast payload renders raw markup; bodies must be escaped")
		}
	})
}

// lockedCardsStore flips one board's add lock, standing in for the
// facilitation lock without building its controls.
type lockedCardsStore struct {
	boardStore
	lockedID string
}

func (s lockedCardsStore) GetBoardByPublicID(ctx context.Context, publicID string) (*db.Board, error) {
	board, err := s.boardStore.GetBoardByPublicID(ctx, publicID)
	if err != nil {
		return nil, err
	}
	if board.PublicID == s.lockedID {
		board.CardsLocked = true
	}
	return board, nil
}

func TestUpdateCardEditPublishesCardUpdated(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "before", "ana")

	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(card.ID, 10),
		url.Values{"body": {"after"}}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="card-`+strconv.FormatInt(card.ID, 10)+`"`) ||
		!strings.Contains(body, "after") {
		t.Errorf("PUT response must be the updated card fragment (body: %.200s…)", body)
	}

	events := fix.publisher.events
	if len(events) != 1 {
		t.Fatalf("published %d events, want exactly 1", len(events))
	}
	wantName := "card-updated:" + strconv.FormatInt(card.ID, 10)
	if events[0].boardID != bid || events[0].name != wantName {
		t.Errorf("published %+v, want board %q name %q", events[0], bid, wantName)
	}
	if !strings.Contains(events[0].html, "after") {
		t.Errorf("broadcast payload missing the edited text (payload: %.200s…)", events[0].html)
	}

	updated, err := fix.store.GetCard(t.Context(), card.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if updated.Body != "after" || updated.Position != card.Position || updated.ColumnID != card.ColumnID {
		t.Errorf("stored card = %+v, want edited body with placement kept", updated)
	}
}

func TestUpdateCardValidationAndGates(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "before", "ana")
	target := "/cards/" + strconv.FormatInt(card.ID, 10)

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, target, url.Values{"body": {"x"}}),
		http.StatusForbidden)

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, target,
			url.Values{"body": {strings.Repeat("x", 2001)}}, ana...),
		http.StatusUnprocessableEntity)

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, target, url.Values{}, ana...),
		http.StatusUnprocessableEntity)

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, target,
			url.Values{"column_id": {strconv.FormatInt(secondColumnID(t, fix, bid), 10)}, "position": {"nope"}}, ana...),
		http.StatusUnprocessableEntity)

	for _, unknown := range []string{"/cards/999999", "/cards/not-a-number"} {
		rec := doRequest(t, fix.mux, http.MethodPut, unknown, url.Values{"body": {"x"}}, ana...)
		if rec.Code != http.StatusNotFound {
			t.Errorf("PUT %s status = %d, want 404", unknown, rec.Code)
		}
	}

	if n := fix.publisher.count(); n != 0 {
		t.Errorf("rejected edits published %d events, want none", n)
	}
	kept, err := fix.store.GetCard(t.Context(), card.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if kept.Body != "before" {
		t.Errorf("rejected edit changed the stored body to %q", kept.Body)
	}
}

func TestUpdateCardMoveAcrossColumnsPublishesBothColumns(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	left := firstColumnID(t, fix, bid)
	right := secondColumnID(t, fix, bid)
	moving := seedBoardCard(t, fix, bid, "moving", "ana")

	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(moving.ID, 10),
		url.Values{
			"column_id": {strconv.FormatInt(right, 10)},
			"position":  {"0"},
		}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	names := publishedNames(fix.publisher)
	want := []string{
		"column-" + strconv.FormatInt(left, 10),
		"column-" + strconv.FormatInt(right, 10),
	}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("published %v, want source then destination %v", names, want)
	}
	source, dest := fix.publisher.events[0].html, fix.publisher.events[1].html
	if strings.Contains(source, "moving") {
		t.Error("source column broadcast still carries the moved card")
	}
	if !strings.Contains(dest, "moving") {
		t.Error("destination column broadcast missing the moved card")
	}

	moved, err := fix.store.GetCard(t.Context(), moving.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if moved.ColumnID != right || moved.Position != 0 {
		t.Errorf("stored card at column %d position %d, want %d/0", moved.ColumnID, moved.Position, right)
	}
}

func TestUpdateCardSameColumnReorderPublishesColumn(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	col := firstColumnID(t, fix, bid)
	for _, body := range []string{"alpha", "beta", "gamma"} {
		if _, err := fix.store.CreateCard(t.Context(), col, body, "ana"); err != nil {
			t.Fatalf("CreateCard: %v", err)
		}
	}
	before, err := fix.store.ListCards(t.Context(), col)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	gamma := before[2]

	// Moving within the same column shifts siblings, so viewers need
	// the whole column — a per-card morph would leave them diverged.
	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(gamma.ID, 10),
		url.Values{
			"column_id": {strconv.FormatInt(col, 10)},
			"position":  {"0"},
		}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	names := publishedNames(fix.publisher)
	want := []string{"column-" + strconv.FormatInt(col, 10)}
	if len(names) != len(want) || names[0] != want[0] {
		t.Fatalf("published %v, want exactly the column refresh %v", names, want)
	}
	payload := fix.publisher.events[0].html
	at := func(s string) int { return strings.Index(payload, ">"+s+"<") }
	if at("gamma") < 0 || at("alpha") < 0 || at("beta") < 0 ||
		!(at("gamma") < at("alpha") && at("alpha") < at("beta")) {
		t.Errorf("broadcast payload not in reordered [gamma alpha beta] order (payload: %.300s…)", payload)
	}

	response := rec.Body.String()
	if !strings.Contains(response, `id="column-`+strconv.FormatInt(col, 10)+`"`) {
		t.Errorf("PUT response must be the column partial for the actor (body: %.300s…)", response)
	}

	after, err := fix.store.ListCards(t.Context(), col)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	for i, wantBody := range []string{"gamma", "alpha", "beta"} {
		if after[i].Body != wantBody || after[i].Position != i {
			t.Errorf("stored card %d = %q@%d, want %q@%d",
				i, after[i].Body, after[i].Position, wantBody, i)
		}
	}
}

func TestUpdateCardSameColumnWithoutPositionKeepsOrder(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	col := firstColumnID(t, fix, bid)
	alpha := seedBoardCard(t, fix, bid, "alpha", "ana")
	beta := seedBoardCard(t, fix, bid, "beta", "ana")

	// A same-column id with no position slot edits the text in place and
	// leaves the order alone instead of sliding to the front.
	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(beta.ID, 10),
		url.Values{
			"column_id": {strconv.FormatInt(col, 10)},
			"body":      {"beta edited"},
		}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	names := publishedNames(fix.publisher)
	wantName := "card-updated:" + strconv.FormatInt(beta.ID, 10)
	if len(names) != 1 || names[0] != wantName {
		t.Errorf("published %v, want exactly the per-card event %q", names, wantName)
	}
	kept, err := fix.store.GetCard(t.Context(), beta.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if kept.Body != "beta edited" || kept.Position != 1 || kept.ColumnID != col {
		t.Errorf("stored card = %+v, want edited body with placement kept", kept)
	}
	if got := storedOrder(t, fix, col); len(got) != 2 || got[0] != "alpha" || got[1] != "beta edited" {
		t.Errorf("stored order = %v, want [alpha beta edited]", got)
	}

	// A same-column id with neither body nor position changes nothing.
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(alpha.ID, 10),
			url.Values{"column_id": {strconv.FormatInt(col, 10)}}, ana...),
		http.StatusUnprocessableEntity)
	if n := fix.publisher.count(); n != 1 {
		t.Errorf("no-op move published %d events total, want 1 (the edit only)", n)
	}
	if got := storedOrder(t, fix, col); len(got) != 2 || got[0] != "alpha" || got[1] != "beta edited" {
		t.Errorf("stored order after no-op = %v, want it unchanged", got)
	}
}

func TestUpdateCardMoveClampsOversizedPosition(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	left := firstColumnID(t, fix, bid)
	right := secondColumnID(t, fix, bid)
	alpha := seedBoardCard(t, fix, bid, "alpha", "ana")
	seedBoardCard(t, fix, bid, "beta", "ana")

	// Past-the-end within one column lands last instead of opening a gap.
	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(alpha.ID, 10),
		url.Values{
			"column_id": {strconv.FormatInt(left, 10)},
			"position":  {"99"},
		}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	names := publishedNames(fix.publisher)
	if len(names) != 1 || names[0] != "column-"+strconv.FormatInt(left, 10) {
		t.Errorf("published %v, want exactly the column refresh", names)
	}
	if got := storedOrder(t, fix, left); len(got) != 2 || got[0] != "beta" || got[1] != "alpha" {
		t.Errorf("stored order = %v, want [beta alpha]", got)
	}
	landed, err := fix.store.GetCard(t.Context(), alpha.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if landed.Position != 1 {
		t.Errorf("clamped position = %d, want 1 (last slot)", landed.Position)
	}

	// Past-the-end across columns appends to the destination.
	rec = doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(alpha.ID, 10),
		url.Values{
			"column_id": {strconv.FormatInt(right, 10)},
			"position":  {"99"},
		}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	moved, err := fix.store.GetCard(t.Context(), alpha.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if moved.ColumnID != right || moved.Position != 0 {
		t.Errorf("stored card at column %d position %d, want %d/0 (empty column append)",
			moved.ColumnID, moved.Position, right)
	}
}

func storedOrder(t *testing.T, fix *boardFixture, columnID int64) []string {
	t.Helper()

	cards, err := fix.store.ListCards(t.Context(), columnID)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	var bodies []string
	for _, card := range cards {
		bodies = append(bodies, card.Body)
	}
	return bodies
}

func TestUpdateCardMoveRejects(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "stays", "ana")
	target := "/cards/" + strconv.FormatInt(card.ID, 10)
	otherBid, _ := createBoard(t, fix.mux, url.Values{
		"name":         {"Other"},
		"display_name": {"Other"},
		"template":     {"plus-delta"},
	})
	foreignCol := firstColumnID(t, fix, otherBid)

	move := func(column string) *httptest.ResponseRecorder {
		return doRequest(t, fix.mux, http.MethodPut, target,
			url.Values{"column_id": {column}, "position": {"0"}}, ana...)
	}
	for _, tc := range []struct {
		name   string
		column string
		status int
	}{
		{"unknown column", "999999", http.StatusNotFound},
		{"foreign column", strconv.FormatInt(foreignCol, 10), http.StatusNotFound},
	} {
		if rec := move(tc.column); rec.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.status)
		}
	}
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, target,
			url.Values{
				"column_id": {strconv.FormatInt(secondColumnID(t, fix, bid), 10)},
				"position":  {"-1"},
			}, ana...),
		http.StatusUnprocessableEntity)

	kept, err := fix.store.GetCard(t.Context(), card.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if kept.ColumnID != card.ColumnID || kept.Position != card.Position {
		t.Errorf("rejected move changed placement: %+v", kept)
	}
}

func TestDeleteCardAuthorOrFacilitator(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName)
	ana := joinAs(t, fix, bid, "ana")
	bo := joinAs(t, fix, bid, "bo")
	card := seedBoardCard(t, fix, bid, "doomed", "ana")
	target := "/cards/" + strconv.FormatInt(card.ID, 10)
	wantNode := `<div id="card-` + strconv.FormatInt(card.ID, 10) + `" hx-swap-oob="delete"></div>`

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodDelete, target, nil, bo...),
		http.StatusForbidden)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodDelete, target, nil),
		http.StatusForbidden)
	if _, err := fix.store.GetCard(t.Context(), card.ID); err != nil {
		t.Fatalf("rejected delete removed the card: %v", err)
	}

	rec := doRequest(t, fix.mux, http.MethodDelete, target, nil, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("author DELETE status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != wantNode {
		t.Errorf("author DELETE body = %q, want the out-of-band delete node %q",
			rec.Body.String(), wantNode)
	}
	events := fix.publisher.events
	if len(events) != 1 {
		t.Fatalf("published %d events, want exactly 1", len(events))
	}
	wantName := "card-removed:" + strconv.FormatInt(card.ID, 10)
	if events[0].boardID != bid || events[0].name != wantName || events[0].html != wantNode {
		t.Errorf("published %+v, want board %q name %q with the delete node", events[0], bid, wantName)
	}

	// A facilitator who did not write the card can still remove it.
	other := seedBoardCard(t, fix, bid, "also doomed", "bo")
	otherTarget := "/cards/" + strconv.FormatInt(other.ID, 10)
	if rec := doRequest(t, fix.mux, http.MethodDelete, otherTarget, nil, fac...); rec.Code != http.StatusOK {
		t.Errorf("facilitator DELETE status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	if rec := doRequest(t, fix.mux, http.MethodDelete, "/cards/999999", nil, ana...); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown status = %d, want 404", rec.Code)
	}
	if n := fix.publisher.count(); n != 2 {
		t.Errorf("published %d events, want 2 (one per accepted delete)", n)
	}
}

func TestDeleteGroupedCardPublishesColumnRefreshes(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	bo := joinAs(t, fix, bid, "bo")
	col := firstColumnID(t, fix, bid)
	ctx := t.Context()
	if _, err := fix.store.SetPhase(ctx, boardID(t, fix, bid), "vote"); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}

	leader, err := fix.store.CreateCard(ctx, col, "leader-body", "ana")
	if err != nil {
		t.Fatalf("CreateCard leader: %v", err)
	}
	member, err := fix.store.CreateCard(ctx, col, "member-body", "bo")
	if err != nil {
		t.Fatalf("CreateCard member: %v", err)
	}
	if _, err := fix.store.SetGroup(ctx, member.ID, &leader.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	voter, err := fix.store.CreateParticipant(ctx, boardID(t, fix, bid), "voter", "voter-raw-token")
	if err != nil {
		t.Fatalf("CreateParticipant: %v", err)
	}
	for _, id := range []int64{leader.ID, member.ID} {
		if err := fix.store.Vote(ctx, voter.ID, id); err != nil {
			t.Fatalf("Vote %d: %v", id, err)
		}
	}

	// Deleting the member retotals the leader: viewers get the refreshed
	// column with the decremented sum plus the removal node last.
	rec := doRequest(t, fix.mux, http.MethodDelete, "/cards/"+strconv.FormatInt(member.ID, 10), nil, bo...)
	if rec.Code != http.StatusOK {
		t.Fatalf("member DELETE status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	wantNode := `<div id="card-` + strconv.FormatInt(member.ID, 10) + `" hx-swap-oob="delete"></div>`
	if strings.TrimSpace(rec.Body.String()) != wantNode {
		t.Errorf("member DELETE body = %q, want the bare out-of-band node %q",
			rec.Body.String(), wantNode)
	}
	names := publishedNames(fix.publisher)
	want := []string{
		"column-" + strconv.FormatInt(col, 10),
		"card-removed:" + strconv.FormatInt(member.ID, 10),
	}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("published %v, want the column refresh plus the removal %v", names, want)
	}
	payload := fix.publisher.events[0].html
	for _, want := range []string{"leader-body", "1 vote"} {
		if !strings.Contains(payload, want) {
			t.Errorf("column payload missing %q after member delete (payload: %.300s…)", want, payload)
		}
	}
	if strings.Contains(payload, "member-body") {
		t.Error("column payload still carries the deleted member")
	}

	// Deleting the leader promotes its members into place: the column
	// refresh carries them as top-level cards again.
	other, err := fix.store.CreateCard(ctx, col, "other-body", "bo")
	if err != nil {
		t.Fatalf("CreateCard other: %v", err)
	}
	if _, err := fix.store.SetGroup(ctx, other.ID, &leader.ID); err != nil {
		t.Fatalf("SetGroup other: %v", err)
	}
	fix.publisher.events = nil
	rec = doRequest(t, fix.mux, http.MethodDelete, "/cards/"+strconv.FormatInt(leader.ID, 10), nil, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("leader DELETE status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	names = publishedNames(fix.publisher)
	want = []string{
		"column-" + strconv.FormatInt(col, 10),
		"card-removed:" + strconv.FormatInt(leader.ID, 10),
	}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("published %v, want the column refresh plus the removal %v", names, want)
	}
	payload = fix.publisher.events[0].html
	for _, want := range []string{"other-body", `id="card-` + strconv.FormatInt(other.ID, 10) + `"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("column payload missing promoted member %q (payload: %.300s…)", want, payload)
		}
	}
	if strings.Contains(payload, "leader-body") {
		t.Error("column payload still carries the deleted leader")
	}
	promoted, err := fix.store.GetCard(ctx, other.ID)
	if err != nil {
		t.Fatalf("GetCard other: %v", err)
	}
	if promoted.GroupID != nil {
		t.Errorf("promoted member group_id = %v, want nil after leader delete", promoted.GroupID)
	}
}

// funnelPublisher relays handler publishes into a live broker so tests
// can assert the exact stream bytes subscribers observe.
type funnelPublisher struct {
	broker *realtime.Broker
}

func (f funnelPublisher) Publish(boardID, name, html string) error {
	f.broker.Publish(boardID, name, html)
	return nil
}

func TestCardEventsReachLiveStreamInOrder(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	broker := realtime.NewBroker()
	fix.handler.events = funnelPublisher{broker: broker}
	fix.handler.broker = broker
	mux := eventsMux(fix)

	ana := joinAs(t, fix, bid, "ana")
	rec := startEventsStream(t, mux, "/b/"+bid+"/events", nil, ana...)
	waitForSubscribed(t, broker, bid, 1)

	col := firstColumnID(t, fix, bid)
	added := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/cards",
		url.Values{
			"column_id": {strconv.FormatInt(col, 10)},
			"body":      {"streamed"},
		}, ana...)
	if added.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", added.Code)
	}
	cardID := cardIDFromBody(t, added.Body.String())

	edited := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(cardID, 10),
		url.Values{"body": {"streamed edited"}}, ana...)
	if edited.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", edited.Code)
	}
	removed := doRequest(t, fix.mux, http.MethodDelete,
		"/cards/"+strconv.FormatInt(cardID, 10), nil, ana...)
	if removed.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", removed.Code)
	}

	var ids []uint64
	var names []string
	for range 3 {
		msg := nextMessage(t, rec)
		ids = append(ids, streamID(t, msg))
		names = append(names, streamName(t, msg))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("stream ids %v are not strictly increasing", ids)
		}
	}
	wantNames := []string{
		"column-" + strconv.FormatInt(col, 10),
		"card-updated:" + strconv.FormatInt(cardID, 10),
		"card-removed:" + strconv.FormatInt(cardID, 10),
	}
	for i, want := range wantNames {
		if names[i] != want {
			t.Errorf("stream event %d = %q, want %q (all: %v)", i, names[i], want, names)
		}
	}

	// A client that is already current replays nothing: the next live
	// publish arrives first, proving stale sequence values change
	// nothing on the wire.
	latest := ids[len(ids)-1]
	second := startEventsStream(t, mux, "/b/"+bid+"/events",
		map[string]string{"Last-Event-ID": strconv.FormatUint(latest, 10)}, ana...)
	broker.Publish(bid, "column-"+strconv.FormatInt(col, 10), "<div>marker</div>")
	msg := nextMessage(t, second)
	if got := streamID(t, msg); got != latest+1 {
		t.Errorf("current client first replayed id %d, want only the live %d", got, latest+1)
	}
	if got := streamName(t, msg); got != "column-"+strconv.FormatInt(col, 10) {
		t.Errorf("current client first replayed %q, want the live marker", got)
	}
}

func streamID(t *testing.T, msg string) uint64 {
	t.Helper()

	const marker = "id: "
	idx := strings.Index(msg, marker)
	if idx < 0 {
		t.Fatalf("stream message carries no id (message: %q)", msg)
	}
	rest := msg[idx+len(marker):]
	end := strings.Index(rest, "\n")
	id, err := strconv.ParseUint(strings.TrimSpace(rest[:end]), 10, 64)
	if err != nil {
		t.Fatalf("parse stream id: %v", err)
	}
	return id
}

func streamName(t *testing.T, msg string) string {
	t.Helper()

	const marker = "event: "
	idx := strings.Index(msg, marker)
	if idx < 0 {
		t.Fatalf("stream message carries no name (message: %q)", msg)
	}
	rest := msg[idx+len(marker):]
	end := strings.Index(rest, "\n")
	return strings.TrimSpace(rest[:end])
}

func TestBoardShellStreamAndCardNodes(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	creator := withCookies(creatorCookies, facCookieName, partsCookieName)
	if _, err := fix.store.CreateCard(t.Context(),
		firstColumnID(t, fix, bid), "<em>live</em>", "ana"); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, creator...).Body.String()
	if got := strings.Count(shell, "sse-connect="); got != 1 {
		t.Errorf("shell holds %d stream subscriptions, want exactly 1", got)
	}
	cols, err := fix.store.ListColumns(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	for _, col := range cols {
		if !strings.Contains(shell, `sse-swap="column-`+strconv.FormatInt(col.ID, 10)+`"`) {
			t.Errorf("shell missing the listener for column %d", col.ID)
		}
	}
	if !strings.Contains(shell, `id="card-`) {
		t.Error("shell must render existing cards inside their columns")
	}
	if strings.Contains(shell, "<em>live</em>") {
		t.Error("shell renders raw markup; stored bodies must be escaped")
	}
	if !strings.Contains(shell, "&lt;em&gt;live&lt;/em&gt;") {
		t.Error("shell missing the escaped card body")
	}
}

// failingPublisher stands in for a broken downstream: the commit has
// already happened, so the mutation must still report the failure.
type failingPublisher struct{}

func (failingPublisher) Publish(_, _, _ string) error { return errPublishBoom }

type boomError string

func (e boomError) Error() string { return string(e) }

const errPublishBoom = boomError("broadcast boom")

func TestPublishFailureSurfacesServerError(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	broken := NewBoards(fix.store, fix.handler.secret, failingPublisher{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /b/{bid}/cards", broken.CreateCard)
	ana := joinAs(t, fix, bid, "ana")
	col := firstColumnID(t, fix, bid)

	rec := doRequest(t, mux, http.MethodPost, "/b/"+bid+"/cards",
		url.Values{
			"column_id": {strconv.FormatInt(col, 10)},
			"body":      {"kept anyway"},
		}, ana...)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST with broken broadcast status = %d, want 500", rec.Code)
	}
	// The commit stands, so the actor converges on the fresh column
	// fragment plus the retry-hint flash instead of a bare 500.
	body := rec.Body.String()
	for _, want := range []string{
		`id="column-` + strconv.FormatInt(col, 10) + `"`,
		"kept anyway",
		`id="flash"`,
		`hx-swap-oob="true"`,
		"reload to resync",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failure body missing %q (body: %.300s…)", want, body)
		}
	}
	cards, err := fix.store.ListCards(t.Context(), col)
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(cards) != 1 || cards[0].Body != "kept anyway" {
		t.Errorf("failed broadcast rolled back the commit: %+v", cards)
	}
}

func TestPublishFailureOnMoveStillConvergesActor(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	col := firstColumnID(t, fix, bid)
	card := seedBoardCard(t, fix, bid, "moved anyway", "ana")

	broken := NewBoards(fix.store, fix.handler.secret, failingPublisher{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /cards/{cid}", broken.UpdateCard)

	rec := doRequest(t, mux, http.MethodPut, "/cards/"+strconv.FormatInt(card.ID, 10),
		url.Values{
			"column_id": {strconv.FormatInt(col, 10)},
			"position":  {"0"},
		}, ana...)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT with broken broadcast status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="column-` + strconv.FormatInt(col, 10) + `"`,
		"moved anyway",
		`id="flash"`,
		"reload to resync",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failure body missing %q (body: %.300s…)", want, body)
		}
	}
}
