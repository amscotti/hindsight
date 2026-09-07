package handlers

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func dropForm(destColumn int64, destPosition string, fromColumn int64, fromPosition string, withContext bool) url.Values {
	form := url.Values{
		"column_id": {strconv.FormatInt(destColumn, 10)},
		"position":  {destPosition},
	}
	if withContext {
		form.Set("from_column_id", strconv.FormatInt(fromColumn, 10))
		form.Set("from_position", fromPosition)
	}
	return form
}

func TestDropWithFreshContextMoves(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	left := firstColumnID(t, fix, bid)
	right := secondColumnID(t, fix, bid)
	moving := seedBoardCard(t, fix, bid, "moving", "ana")

	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(moving.ID, 10),
		dropForm(right, "0", left, "0", true), ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	moved, err := fix.store.GetCard(t.Context(), moving.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if moved.ColumnID != right || moved.Position != 0 {
		t.Errorf("stored card at column %d position %d, want %d/0", moved.ColumnID, moved.Position, right)
	}
	if got := publishedNames(fix.publisher); len(got) != 2 {
		t.Errorf("fresh drop published %v, want the source then destination column refresh", got)
	}
}

func TestDropWithStaleContextReturnsConflict(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	left := firstColumnID(t, fix, bid)
	right := secondColumnID(t, fix, bid)
	moving := seedBoardCard(t, fix, bid, "moving", "ana")
	seedBoardCard(t, fix, bid, "stays", "ana")

	// A remote move lands before the drop: the dragged slot is stale.
	if err := fix.store.MoveCard(t.Context(), moving.ID, right, 0); err != nil {
		t.Fatalf("remote MoveCard: %v", err)
	}

	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(moving.ID, 10),
		dropForm(right, "1", left, "0", true), ana...)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="column-` + strconv.FormatInt(right, 10) + `"`,
		`id="flash"`,
		`hx-swap-oob="true"`,
		"try the drop again",
		"moving",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("409 body missing %q (body: %.300s…)", want, body)
		}
	}

	// The rejected drop moves nothing and publishes nothing.
	kept, err := fix.store.GetCard(t.Context(), moving.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if kept.ColumnID != right || kept.Position != 0 {
		t.Errorf("rejected drop left the card at column %d position %d, want %d/0",
			kept.ColumnID, kept.Position, right)
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("rejected drop published %d events, want none", n)
	}
}

func TestDropWithoutContextStillMoves(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	right := secondColumnID(t, fix, bid)
	moving := seedBoardCard(t, fix, bid, "moving", "ana")

	// Drops without drag context (older clients, scripts) keep the
	// unconditional move behavior.
	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(moving.ID, 10),
		dropForm(right, "0", 0, "", false), ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	moved, err := fix.store.GetCard(t.Context(), moving.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if moved.ColumnID != right {
		t.Errorf("stored card at column %d, want %d", moved.ColumnID, right)
	}
}

func TestDropWithBrokenContextRejected(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	left := firstColumnID(t, fix, bid)
	right := secondColumnID(t, fix, bid)
	moving := seedBoardCard(t, fix, bid, "stays", "ana")
	target := "/cards/" + strconv.FormatInt(moving.ID, 10)

	cases := []struct {
		name string
		form url.Values
	}{
		{"column without position", url.Values{
			"column_id": {strconv.FormatInt(right, 10)}, "position": {"0"},
			"from_column_id": {strconv.FormatInt(left, 10)},
		}},
		{"position without column", url.Values{
			"column_id": {strconv.FormatInt(right, 10)}, "position": {"0"},
			"from_position": {"0"},
		}},
		{"non-numeric position", url.Values{
			"column_id": {strconv.FormatInt(right, 10)}, "position": {"0"},
			"from_column_id": {strconv.FormatInt(left, 10)}, "from_position": {"nope"},
		}},
		{"negative position", url.Values{
			"column_id": {strconv.FormatInt(right, 10)}, "position": {"0"},
			"from_column_id": {strconv.FormatInt(left, 10)}, "from_position": {"-1"},
		}},
	}
	for _, tc := range cases {
		assertFlashOnly(t, doRequest(t, fix.mux, http.MethodPut, target, tc.form, ana...),
			http.StatusUnprocessableEntity)
	}

	kept, err := fix.store.GetCard(t.Context(), moving.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if kept.ColumnID != left || kept.Position != 0 {
		t.Errorf("rejected drop moved the card to column %d position %d, want %d/0",
			kept.ColumnID, kept.Position, left)
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("rejected drops published %d events, want none", n)
	}
}

func TestStaleDropUnderBlindCollectStaysRedacted(t *testing.T) {
	fix, bid, _, ana := blindFixture(t)
	left := firstColumnID(t, fix, bid)
	right := secondColumnID(t, fix, bid)
	moving := seedBoardCard(t, fix, bid, "hidden-moving-body", "ana")

	if err := fix.store.MoveCard(t.Context(), moving.ID, right, 0); err != nil {
		t.Fatalf("remote MoveCard: %v", err)
	}
	rec := doRequest(t, fix.mux, http.MethodPut, "/cards/"+strconv.FormatInt(moving.ID, 10),
		dropForm(right, "1", left, "0", true), ana...)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, redactedBody) {
		t.Error("blind 409 body carries no redaction placeholder")
	}
	if strings.Contains(body, "hidden-moving-body") {
		t.Error("blind 409 body leaks the card body")
	}
}
