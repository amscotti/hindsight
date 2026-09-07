package handlers

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func actionsTarget(bid string) string {
	return "/b/" + bid + "/actions"
}

func actionTarget(aid int64) string {
	return "/actions/" + strconv.FormatInt(aid, 10)
}

func seedBoardAction(t *testing.T, fix *boardFixture, bid, text, owner, author string) int64 {
	t.Helper()

	action, err := fix.store.CreateAction(t.Context(), boardID(t, fix, bid), text, owner, author, nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	return action.ID
}

func TestCreateActionPublishesAdded(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")

	rec := doRequest(t, fix.mux, http.MethodPost, actionsTarget(bid),
		url.Values{"text": {"ship the fix"}, "owner": {"bo"}}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="action-`,
		"ship the fix",
		"bo",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("action response missing %q (body: %.300s…)", want, body)
		}
	}

	events := fix.publisher.events
	if len(events) != 1 {
		t.Fatalf("published %d events, want exactly 1", len(events))
	}
	if events[0].boardID != bid || events[0].name != "action-added" {
		t.Errorf("published %+v, want board %q name %q", events[0], bid, "action-added")
	}
	for _, want := range []string{`id="action-`, "ship the fix"} {
		if !strings.Contains(events[0].html, want) {
			t.Errorf("broadcast payload missing %q (payload: %.300s…)", want, events[0].html)
		}
	}

	listed, err := fix.store.ListActions(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(listed) != 1 || listed[0].Text != "ship the fix" || listed[0].AuthorName != "ana" {
		t.Errorf("stored actions = %+v, want the single item authored by ana", listed)
	}
}

func TestCreateActionValidation(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	target := actionsTarget(bid)

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"text": {"do it"}}),
		http.StatusForbidden)
	if rec := doRequest(t, fix.mux, http.MethodPost, "/b/does-not-exist/actions",
		url.Values{"text": {"do it"}}, ana...); rec.Code != http.StatusNotFound {
		t.Errorf("POST unknown board status = %d, want 404", rec.Code)
	}
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"text": {"   "}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"text": {strings.Repeat("x", 501)}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"text": {"do it"}, "owner": {strings.Repeat("x", 101)}}, ana...),
		http.StatusUnprocessableEntity)

	if listed, err := fix.store.ListActions(t.Context(), boardID(t, fix, bid)); err != nil {
		t.Fatalf("ListActions: %v", err)
	} else if len(listed) != 0 {
		t.Errorf("rejected actions stored %d rows, want 0", len(listed))
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("rejected actions published %d events, want 0", n)
	}
}

func TestUpdateActionAuthorOrFacilitator(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName)
	ana := joinAs(t, fix, bid, "ana")
	bo := joinAs(t, fix, bid, "bo")
	aid := seedBoardAction(t, fix, bid, "draft", "", "ana")
	target := actionTarget(aid)

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, target,
			url.Values{"text": {"hijacked"}}, bo...),
		http.StatusForbidden)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, target,
			url.Values{"text": {"hijacked"}}),
		http.StatusForbidden)

	rec := doRequest(t, fix.mux, http.MethodPut, target,
		url.Values{"text": {"final"}, "owner": {"cy"}, "done": {"1"}}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("author PUT status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		`id="action-` + strconv.FormatInt(aid, 10) + `"`,
		"final",
		"cy",
		"Done",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("author PUT response missing %q", want)
		}
	}
	events := fix.publisher.events
	if len(events) != 1 {
		t.Fatalf("published %d events, want exactly 1", len(events))
	}
	if events[0].boardID != bid || events[0].name != "action-updated" {
		t.Errorf("published %+v, want board %q name %q", events[0], bid, "action-updated")
	}
	for _, want := range []string{`id="action-`, "final"} {
		if !strings.Contains(events[0].html, want) {
			t.Errorf("broadcast payload missing %q (payload: %.300s…)", want, events[0].html)
		}
	}

	// The facilitator can edit anyone's item; toggling done alone
	// keeps the other columns.
	toggled := doRequest(t, fix.mux, http.MethodPut, target,
		url.Values{"done": {"0"}}, fac...)
	if toggled.Code != http.StatusOK {
		t.Fatalf("facilitator PUT status = %d, want 200", toggled.Code)
	}
	if !strings.Contains(toggled.Body.String(), "Open") {
		t.Error("facilitator reopen response must mark the item open")
	}
	if !strings.Contains(toggled.Body.String(), "final") {
		t.Error("done-only update must keep the existing text")
	}

	if rec := doRequest(t, fix.mux, http.MethodPut, "/actions/999999",
		url.Values{"text": {"x"}}, ana...); rec.Code != http.StatusNotFound {
		t.Errorf("PUT unknown status = %d, want 404", rec.Code)
	}
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, target,
			url.Values{"text": {"   "}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, target,
			url.Values{"text": {strings.Repeat("x", 501)}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPut, target,
			url.Values{"owner": {strings.Repeat("x", 101)}}, ana...),
		http.StatusUnprocessableEntity)
}

func TestDeleteActionFacilitatorOnly(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName)
	ana := joinAs(t, fix, bid, "ana")
	bo := joinAs(t, fix, bid, "bo")
	aid := seedBoardAction(t, fix, bid, "doomed", "", "ana")
	target := actionTarget(aid)
	wantNode := `<div id="action-` + strconv.FormatInt(aid, 10) + `" hx-swap-oob="delete"></div>`

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodDelete, target, nil, bo...),
		http.StatusForbidden)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodDelete, target, nil, ana...),
		http.StatusForbidden)
	if _, err := fix.store.GetAction(t.Context(), aid); err != nil {
		t.Fatalf("rejected delete removed the action: %v", err)
	}

	rec := doRequest(t, fix.mux, http.MethodDelete, target, nil, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("facilitator DELETE status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != wantNode {
		t.Errorf("facilitator DELETE body = %q, want the out-of-band delete node %q",
			rec.Body.String(), wantNode)
	}
	events := fix.publisher.events
	if len(events) != 1 {
		t.Fatalf("published %d events, want exactly 1", len(events))
	}
	if events[0].boardID != bid || events[0].name != "action-removed" || events[0].html != wantNode {
		t.Errorf("published %+v, want board %q name %q with the delete node", events[0], bid, "action-removed")
	}

	if rec := doRequest(t, fix.mux, http.MethodDelete, "/actions/999999", nil, fac...); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown status = %d, want 404", rec.Code)
	}
}

func TestCreateActionPublishFailure(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	broken := NewBoards(fix.store, fix.handler.secret, failingPublisher{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /b/{bid}/actions", broken.CreateAction)
	ana := joinAs(t, fix, bid, "ana")

	rec := doRequest(t, mux, http.MethodPost, actionsTarget(bid),
		url.Values{"text": {"kept anyway"}}, ana...)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST with broken broadcast status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="action-`,
		"kept anyway",
		`id="flash"`,
		"reload to resync",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failure body missing %q (body: %.300s…)", want, body)
		}
	}
	listed, err := fix.store.ListActions(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(listed) != 1 || listed[0].Text != "kept anyway" {
		t.Errorf("failed broadcast rolled back the commit: %+v", listed)
	}
}

func TestUpdateActionPublishFailure(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	aid := seedBoardAction(t, fix, bid, "draft", "", "ana")
	broken := NewBoards(fix.store, fix.handler.secret, failingPublisher{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /actions/{aid}", broken.UpdateAction)

	rec := doRequest(t, mux, http.MethodPut, actionTarget(aid),
		url.Values{"text": {"updated anyway"}}, ana...)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT with broken broadcast status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="action-` + strconv.FormatInt(aid, 10) + `"`,
		"updated anyway",
		`id="flash"`,
		"reload to resync",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failure body missing %q (body: %.300s…)", want, body)
		}
	}
	listed, err := fix.store.ListActions(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(listed) != 1 || listed[0].Text != "updated anyway" {
		t.Errorf("failed broadcast rolled back the commit: %+v", listed)
	}
}

func TestDeleteActionPublishFailure(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName)
	aid := seedBoardAction(t, fix, bid, "doomed", "", "ana")
	broken := NewBoards(fix.store, fix.handler.secret, failingPublisher{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /actions/{aid}", broken.DeleteAction)

	rec := doRequest(t, mux, http.MethodDelete, actionTarget(aid), nil, fac...)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE with broken broadcast status = %d, want 500", rec.Code)
	}
	wantNode := `<div id="action-` + strconv.FormatInt(aid, 10) + `" hx-swap-oob="delete"></div>`
	body := rec.Body.String()
	for _, want := range []string{wantNode, `id="flash"`, "reload to resync"} {
		if !strings.Contains(body, want) {
			t.Errorf("failure body missing %q (body: %.300s…)", want, body)
		}
	}
	listed, err := fix.store.ListActions(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("failed broadcast resurrected the delete: %+v", listed)
	}
}

func TestActionsStayVisibleWhileHidden(t *testing.T) {
	fix, bid, _, ana := blindFixture(t)
	aid := seedBoardAction(t, fix, bid, "visible commitment", "zed", "zed")

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if !strings.Contains(shell, "visible commitment") {
		t.Error("participant shell must keep action texts visible while hidden")
	}

	toggled := doRequest(t, fix.mux, http.MethodPut, actionTarget(aid),
		url.Values{"done": {"1"}}, ana...)
	if toggled.Code != http.StatusOK {
		t.Fatalf("author toggle status = %d, want 200", toggled.Code)
	}
	if !strings.Contains(toggled.Body.String(), "visible commitment") {
		t.Error("participant toggle response must keep the action text visible")
	}
	if n := len(publishedPayloads(fix.publisher, true)); n != 0 {
		t.Errorf("action updates published %d full-bodies variants, want 0 (never redacted)", n)
	}
}
