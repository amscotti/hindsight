package handlers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func commentTarget(cardID int64) string {
	return "/cards/" + strconv.FormatInt(cardID, 10) + "/comments"
}

func TestCreateCommentRendersInCardAndPublishes(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "discuss me", "ana")

	committed := false
	fix.publisher.hook = func(ev publishedEvent) {
		listed, err := fix.store.ListComments(context.Background(), card.ID)
		if err != nil {
			t.Errorf("ListComments inside publish hook: %v", err)
			return
		}
		if len(listed) == 1 && listed[0].Body == "first comment" {
			committed = true
		}
	}

	rec := doRequest(t, fix.mux, http.MethodPost, commentTarget(card.ID),
		url.Values{"comment": {"first comment"}}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="card-` + strconv.FormatInt(card.ID, 10) + `"`,
		"first comment",
		"1 comment",
		`id="comments-` + strconv.FormatInt(card.ID, 10) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment response missing %q (body: %.300s…)", want, body)
		}
	}
	if !committed {
		t.Error("broadcast fired before the insert committed")
	}

	events := fix.publisher.events
	if len(events) != 1 {
		t.Fatalf("published %d events, want exactly 1", len(events))
	}
	wantName := "card-updated:" + strconv.FormatInt(card.ID, 10)
	if events[0].boardID != bid || events[0].name != wantName {
		t.Errorf("published %+v, want board %q name %q", events[0], bid, wantName)
	}
	for _, want := range []string{"first comment", "1 comment"} {
		if !strings.Contains(events[0].html, want) {
			t.Errorf("broadcast payload missing %q (payload: %.300s…)", want, events[0].html)
		}
	}

	second := doRequest(t, fix.mux, http.MethodPost, commentTarget(card.ID),
		url.Values{"comment": {"second comment"}}, ana...)
	if second.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200", second.Code)
	}
	for _, want := range []string{"first comment", "second comment", "2 comments"} {
		if !strings.Contains(second.Body.String(), want) {
			t.Errorf("second comment response missing %q", want)
		}
	}
	if n := fix.publisher.count(); n != 2 {
		t.Errorf("published %d events, want 2 (one per comment)", n)
	}
}

func TestCreateCommentGates(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "gated", "ana")
	target := commentTarget(card.ID)

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target, url.Values{"comment": {"hi"}}),
		http.StatusForbidden)

	otherBid, _ := createBoard(t, fix.mux, defaultCreateForm())
	stranger := joinAs(t, fix, otherBid, "outsider")
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target, url.Values{"comment": {"hi"}}, stranger...),
		http.StatusForbidden)

	if rec := doRequest(t, fix.mux, http.MethodPost, "/cards/999999/comments",
		url.Values{"comment": {"hi"}}, ana...); rec.Code != http.StatusNotFound {
		t.Errorf("POST unknown card status = %d, want 404", rec.Code)
	}

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target, url.Values{"comment": {"   "}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"comment": {strings.Repeat("x", 1001)}}, ana...),
		http.StatusUnprocessableEntity)

	if listed, err := fix.store.ListComments(t.Context(), card.ID); err != nil {
		t.Fatalf("ListComments: %v", err)
	} else if len(listed) != 0 {
		t.Errorf("rejected comments stored %d rows, want 0", len(listed))
	}
	if n := fix.publisher.count(); n != 0 {
		t.Errorf("rejected comments published %d events, want 0", n)
	}
}

func TestCreateCommentPublishFailure(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	broken := NewBoards(fix.store, fix.handler.secret, failingPublisher{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cards/{cid}/comments", broken.CreateComment)
	ana := joinAs(t, fix, bid, "ana")
	card := seedBoardCard(t, fix, bid, "kept anyway", "ana")

	rec := doRequest(t, mux, http.MethodPost, commentTarget(card.ID),
		url.Values{"comment": {"kept comment"}}, ana...)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST with broken broadcast status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="card-` + strconv.FormatInt(card.ID, 10) + `"`,
		"kept comment",
		`id="flash"`,
		"reload to resync",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failure body missing %q (body: %.300s…)", want, body)
		}
	}
	listed, err := fix.store.ListComments(t.Context(), card.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(listed) != 1 || listed[0].Body != "kept comment" {
		t.Errorf("failed broadcast rolled back the commit: %+v", listed)
	}
}

func TestCommentRedactionWhileHidden(t *testing.T) {
	fix, bid, creatorCookies, ana := blindFixture(t)
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)
	card := seedBoardCard(t, fix, bid, "hidden card", "ana")
	secret := "hidden thread secret"
	if _, err := fix.store.CreateComment(t.Context(), card.ID, secret, "ana"); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if strings.Contains(shell, secret) {
		t.Error("participant shell leaks the comment body")
	}
	if !strings.Contains(shell, redactedBody) {
		t.Error("participant shell carries no redaction placeholder")
	}
	if !strings.Contains(shell, "1 comment") {
		t.Error("participant shell must keep the visible comment count")
	}

	facShell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, fac...).Body.String()
	if !strings.Contains(facShell, secret) {
		t.Error("facilitator shell missing the full comment body")
	}

	posted := doRequest(t, fix.mux, http.MethodPost, commentTarget(card.ID),
		url.Values{"comment": {"another secret"}}, ana...)
	if posted.Code != http.StatusOK {
		t.Fatalf("participant comment status = %d, want 200", posted.Code)
	}
	if strings.Contains(posted.Body.String(), "another secret") {
		t.Error("participant comment response leaks the new body")
	}
	if !strings.Contains(posted.Body.String(), "2 comments") {
		t.Error("participant comment response must keep the visible count")
	}
	for _, ev := range publishedPayloads(fix.publisher, false) {
		if !strings.HasPrefix(ev.name, "card-updated:") {
			continue
		}
		for _, leak := range []string{secret, "another secret"} {
			if strings.Contains(ev.html, leak) {
				t.Errorf("participant event %q leaks comment %q", ev.name, leak)
			}
		}
	}
	fulls := publishedPayloads(fix.publisher, true)
	joined := ""
	for _, ev := range fulls {
		joined += ev.html
	}
	if !strings.Contains(joined, "another secret") {
		t.Error("full-bodies variants missing the new comment")
	}
}
