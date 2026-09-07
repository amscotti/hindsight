package handlers

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func kudosTarget(bid string) string {
	return "/b/" + bid + "/kudos"
}

func seedBoardKudo(t *testing.T, fix *boardFixture, bid, to, body, from string) int64 {
	t.Helper()

	kudo, err := fix.store.CreateKudo(t.Context(), boardID(t, fix, bid), to, body, from)
	if err != nil {
		t.Fatalf("CreateKudo: %v", err)
	}
	return kudo.ID
}

func TestCreateKudoPublishesWallEvent(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")

	rec := doRequest(t, fix.mux, http.MethodPost, kudosTarget(bid),
		url.Values{"to": {"bo"}, "body": {"shipped the fix"}, "from": {"ana"}}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="kudo-`,
		"shipped the fix",
		"bo",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("kudos response missing %q (body: %.300s…)", want, body)
		}
	}

	events := fix.publisher.events
	if len(events) != 1 {
		t.Fatalf("published %d events, want exactly 1", len(events))
	}
	if events[0].boardID != bid || events[0].name != "kudos-added" {
		t.Errorf("published %+v, want board %q name %q", events[0], bid, "kudos-added")
	}
	for _, want := range []string{`id="kudo-`, "shipped the fix"} {
		if !strings.Contains(events[0].html, want) {
			t.Errorf("broadcast payload missing %q (payload: %.300s…)", want, events[0].html)
		}
	}

	listed, err := fix.store.ListKudos(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListKudos: %v", err)
	}
	if len(listed) != 1 || listed[0].Body != "shipped the fix" || listed[0].From != "ana" {
		t.Errorf("stored kudos = %+v, want the single entry", listed)
	}
}

func TestCreateKudoDefaultsSenderAndValidates(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	target := kudosTarget(bid)

	rec := doRequest(t, fix.mux, http.MethodPost, target,
		url.Values{"to": {"bo"}, "body": {"quiet thanks"}}, ana...)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST without sender status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ana") {
		t.Error("omitted sender must default to the participant name")
	}

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"to": {"bo"}, "body": {"hi"}}),
		http.StatusForbidden)
	if rec := doRequest(t, fix.mux, http.MethodPost, "/b/does-not-exist/kudos",
		url.Values{"to": {"bo"}, "body": {"hi"}}, ana...); rec.Code != http.StatusNotFound {
		t.Errorf("POST unknown board status = %d, want 404", rec.Code)
	}
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"to": {"   "}, "body": {"hi"}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"to": {"bo"}, "body": {"   "}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"to": {"bo"}, "body": {strings.Repeat("x", 501)}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"to": {strings.Repeat("x", 201)}, "body": {"hi"}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"to": {"bo"}, "body": {"hi"}, "from": {strings.Repeat("x", 101)}}, ana...),
		http.StatusUnprocessableEntity)

	if listed, err := fix.store.ListKudos(t.Context(), boardID(t, fix, bid)); err != nil {
		t.Fatalf("ListKudos: %v", err)
	} else if len(listed) != 1 {
		t.Errorf("rejected kudos stored %d rows, want 1 (only the valid entry)", len(listed))
	}
}

func TestDeleteKudoAuthorOrFacilitator(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName)
	ana := joinAs(t, fix, bid, "ana")
	bo := joinAs(t, fix, bid, "bo")
	kid := seedBoardKudo(t, fix, bid, "ana", "doomed thanks", "ana")
	target := "/kudos/" + strconv.FormatInt(kid, 10)
	wantNode := `<div id="kudo-` + strconv.FormatInt(kid, 10) + `" hx-swap-oob="delete"></div>`

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodDelete, target, nil, bo...),
		http.StatusForbidden)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodDelete, target, nil),
		http.StatusForbidden)
	if _, err := fix.store.GetKudo(t.Context(), kid); err != nil {
		t.Fatalf("rejected delete removed the kudo: %v", err)
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
	if events[0].boardID != bid || events[0].name != "kudos-removed" || events[0].html != wantNode {
		t.Errorf("published %+v, want board %q name %q with the delete node", events[0], bid, "kudos-removed")
	}

	other := seedBoardKudo(t, fix, bid, "bo", "also doomed", "bo")
	otherTarget := "/kudos/" + strconv.FormatInt(other, 10)
	if rec := doRequest(t, fix.mux, http.MethodDelete, otherTarget, nil, fac...); rec.Code != http.StatusOK {
		t.Errorf("facilitator DELETE status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	if rec := doRequest(t, fix.mux, http.MethodDelete, "/kudos/999999", nil, ana...); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown status = %d, want 404", rec.Code)
	}
	if n := fix.publisher.count(); n != 2 {
		t.Errorf("published %d events, want 2 (one per accepted delete)", n)
	}
}

func TestBoardShellRendersWalls(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	creator := withCookies(creatorCookies, facCookieName, partsCookieName)

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, creator...).Body.String()
	for _, want := range []string{
		`id="kudos-wall"`,
		`id="kudos-list"`,
		`sse-swap="kudos-added"`,
		`id="actions-wall"`,
		`id="actions-list"`,
		`sse-swap="action-added, action-updated"`,
		`id="wall-removals"`,
		`sse-swap="kudos-removed, action-removed"`,
		`hx-swap="none"`,
		"No kudos yet.",
		"No action items yet.",
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("board shell missing %q", want)
		}
	}
	if got := strings.Count(shell, "sse-connect="); got != 1 {
		t.Errorf("shell holds %d stream subscriptions, want exactly 1", got)
	}

	// Joining returns the walls too, so a fresh participant sees them
	// without a second fetch.
	join := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
		url.Values{"name": {"ana"}})
	joinBody := join.Body.String()
	for _, want := range []string{`id="kudos-wall"`, `id="actions-wall"`} {
		if !strings.Contains(joinBody, want) {
			t.Errorf("join columns missing %q", want)
		}
	}
}

func TestCreateKudoPublishFailure(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	broken := NewBoards(fix.store, fix.handler.secret, failingPublisher{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /b/{bid}/kudos", broken.CreateKudo)
	ana := joinAs(t, fix, bid, "ana")

	rec := doRequest(t, mux, http.MethodPost, kudosTarget(bid),
		url.Values{"to": {"bo"}, "body": {"kept anyway"}}, ana...)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST with broken broadcast status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="kudo-`,
		"kept anyway",
		`id="flash"`,
		"reload to resync",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failure body missing %q (body: %.300s…)", want, body)
		}
	}
	listed, err := fix.store.ListKudos(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListKudos: %v", err)
	}
	if len(listed) != 1 || listed[0].Body != "kept anyway" {
		t.Errorf("failed broadcast rolled back the commit: %+v", listed)
	}
}

func TestDeleteKudoPublishFailure(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	kid := seedBoardKudo(t, fix, bid, "bo", "doomed thanks", "ana")
	broken := NewBoards(fix.store, fix.handler.secret, failingPublisher{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /kudos/{kid}", broken.DeleteKudo)

	rec := doRequest(t, mux, http.MethodDelete, "/kudos/"+strconv.FormatInt(kid, 10), nil, ana...)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE with broken broadcast status = %d, want 500", rec.Code)
	}
	wantNode := `<div id="kudo-` + strconv.FormatInt(kid, 10) + `" hx-swap-oob="delete"></div>`
	body := rec.Body.String()
	for _, want := range []string{wantNode, `id="flash"`, "reload to resync"} {
		if !strings.Contains(body, want) {
			t.Errorf("failure body missing %q (body: %.300s…)", want, body)
		}
	}
	listed, err := fix.store.ListKudos(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListKudos: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("failed broadcast resurrected the delete: %+v", listed)
	}
}

func TestKudosRedactionWhileHidden(t *testing.T) {
	fix, bid, creatorCookies, ana := blindFixture(t)
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)
	secret := "hidden wall secret"
	seedBoardKudo(t, fix, bid, "bo", secret, "ana")

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if strings.Contains(shell, secret) {
		t.Error("participant shell leaks the kudo body")
	}
	if !strings.Contains(shell, redactedBody) {
		t.Error("participant shell carries no redaction placeholder")
	}
	// Recipients stay visible: only bodies redact.
	if !strings.Contains(shell, "bo") {
		t.Error("participant shell must keep the kudo recipient visible")
	}

	facShell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, fac...).Body.String()
	if !strings.Contains(facShell, secret) {
		t.Error("facilitator shell missing the full kudo body")
	}

	posted := doRequest(t, fix.mux, http.MethodPost, kudosTarget(bid),
		url.Values{"to": {"cy"}, "body": {"another secret"}}, ana...)
	if posted.Code != http.StatusOK {
		t.Fatalf("participant kudos status = %d, want 200", posted.Code)
	}
	if strings.Contains(posted.Body.String(), "another secret") {
		t.Error("participant kudos response leaks the new body")
	}

	for _, ev := range publishedPayloads(fix.publisher, false) {
		if ev.name != "kudos-added" {
			continue
		}
		for _, leak := range []string{secret, "another secret"} {
			if strings.Contains(ev.html, leak) {
				t.Errorf("participant event %q leaks kudo %q", ev.name, leak)
			}
		}
	}
	fulls := publishedPayloads(fix.publisher, true)
	joined := ""
	for _, ev := range fulls {
		joined += ev.html
	}
	if !strings.Contains(joined, "another secret") {
		t.Error("full-bodies variants missing the new kudo")
	}
}

func TestCreateKudoPinsSenderToParticipant(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")
	facOnly := withCookies(creatorCookies, facCookieName)
	target := kudosTarget(bid)

	// An explicit sender matching the writer passes through.
	matching := doRequest(t, fix.mux, http.MethodPost, target,
		url.Values{"to": {"bo"}, "body": {"honest thanks"}, "from": {"ana"}}, ana...)
	if matching.Code != http.StatusOK {
		t.Fatalf("matching sender status = %d, want 200 (body: %s)",
			matching.Code, matching.Body.String())
	}

	// A forged sender is rejected, never silently rewritten.
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"to": {"bo"}, "body": {"forged thanks"}, "from": {"bo"}}, ana...),
		http.StatusUnprocessableEntity)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"to": {"bo"}, "body": {"forged thanks"}, "from": {"Facilitator"}}, ana...),
		http.StatusUnprocessableEntity)

	listed, err := fix.store.ListKudos(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListKudos: %v", err)
	}
	if len(listed) != 1 || listed[0].Body != "honest thanks" || listed[0].From != "ana" {
		t.Errorf("stored kudos = %+v, want only the honestly-sent entry", listed)
	}
	if n := fix.publisher.count(); n != 1 {
		t.Errorf("published %d events, want 1 (rejected kudos publish nothing)", n)
	}

	// A facilitator without a participant row stays voteless: 403 like
	// every other participant-gated write.
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, target,
			url.Values{"to": {"bo"}, "body": {"facilitator thanks"}}, facOnly...),
		http.StatusForbidden)
}
