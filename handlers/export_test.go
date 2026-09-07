package handlers

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedExportBoard builds a board with a fixed shape for the markdown
// contract: tied vote counts, a group with member votes, threaded
// remarks, verbatim markdown-metacharacter cards, kudos, and open plus
// done actions. IDs grow from a fresh
// database, so ordering assertions stay deterministic.
func seedExportBoard(t *testing.T, fix *boardFixture) (bid string, creatorCookies map[string]*http.Cookie) {
	t.Helper()

	bid, creatorCookies = createBoard(t, fix.mux, defaultCreateForm())
	ctx := t.Context()
	numeric := boardID(t, fix, bid)
	if _, err := fix.store.SetPhase(ctx, numeric, "vote"); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}

	cols, err := fix.store.ListColumns(ctx, numeric)
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	if len(cols) != 3 {
		t.Fatalf("seeded %d columns, want 3", len(cols))
	}

	mustCard := func(columnID int64, body, author string) int64 {
		t.Helper()
		card, err := fix.store.CreateCard(ctx, columnID, body, author)
		if err != nil {
			t.Fatalf("CreateCard %q: %v", body, err)
		}
		return card.ID
	}

	slow := mustCard(cols[0].ID, "slow builds", "ana")
	flaky := mustCard(cols[0].ID, "flaky tests", "bo")
	quiet := mustCard(cols[0].ID, "noisy alerts", "")
	leader := mustCard(cols[1].ID, "low morale", "ana")
	lunches := mustCard(cols[1].ID, "missed lunches", "lee")
	overtime := mustCard(cols[1].ID, "overtime", "ana")

	// Markdown metacharacters render verbatim in export (no escaping).
	mustCard(cols[2].ID, "# shipped heading", "ana")
	mustCard(cols[2].ID, "check [x](https://example.com)", "bo")
	mustCard(cols[2].ID, "- list item", "lee")

	for _, member := range []int64{lunches, overtime} {
		id := member
		if _, err := fix.store.SetGroup(ctx, id, &leader); err != nil {
			t.Fatalf("SetGroup %d under %d: %v", id, leader, err)
		}
	}

	participantID := func(cookieName, name string, cookies map[string]*http.Cookie) int64 {
		t.Helper()
		raw := extractToken(t, fix, cookies, cookieName, bid)
		row, err := fix.store.GetParticipantByToken(ctx, numeric, raw)
		if err != nil {
			t.Fatalf("GetParticipantByToken %s: %v", name, err)
		}
		return row.ID
	}
	// The creator's participant row comes from the participant cookie
	// creation sets alongside the facilitator one.
	facID := participantID(partsCookieName, "facilitator", creatorCookies)

	joinRow := func(name string) int64 {
		t.Helper()
		rec := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
			url.Values{"name": {name}})
		if rec.Code != http.StatusOK {
			t.Fatalf("join %s status = %d, want 200", name, rec.Code)
		}
		joined := responseCookies(rec)
		if len(withCookies(joined, partsCookieName)) != 1 {
			t.Fatalf("join %s set no participant cookie", name)
		}
		return participantID(partsCookieName, name, joined)
	}
	anaID := joinRow("ana")
	boID := joinRow("bo")
	leeID := joinRow("lee")

	// Tied leaders: slow builds and flaky tests both land on two votes,
	// so the lower id renders first.
	mustVote := func(participantID, cardID int64) {
		t.Helper()
		if err := fix.store.Vote(ctx, participantID, cardID); err != nil {
			t.Fatalf("Vote participant %d card %d: %v", participantID, cardID, err)
		}
	}
	mustVote(facID, slow)
	mustVote(anaID, slow)
	mustVote(boID, flaky)
	mustVote(leeID, flaky)
	mustVote(anaID, leader)
	mustVote(boID, lunches)
	mustVote(leeID, lunches)

	if _, err := fix.store.SetDiscussed(ctx, quiet, true); err != nil {
		t.Fatalf("SetDiscussed: %v", err)
	}

	mustComment := func(cardID int64, body, author string) {
		t.Helper()
		if _, err := fix.store.CreateComment(ctx, cardID, body, author); err != nil {
			t.Fatalf("CreateComment %q: %v", body, err)
		}
	}
	mustComment(slow, "agreed, still flakes", "bo")
	mustComment(slow, "plus slow tests", "ana")
	mustComment(lunches, "lunch helps", "lee")

	if _, err := fix.store.CreateKudo(ctx, numeric, "bo", "shipped the fix", "ana"); err != nil {
		t.Fatalf("CreateKudo: %v", err)
	}
	if _, err := fix.store.CreateKudo(ctx, numeric, "ana", "great facilitation", ""); err != nil {
		t.Fatalf("CreateKudo: %v", err)
	}

	// One open action carries provenance from an earlier board.
	srcBid, _ := createBoard(t, fix.mux, url.Values{
		"name":         {"Earlier board"},
		"display_name": {"Facilitator"},
		"template":     {"plus-delta"},
	})
	srcAction, err := fix.store.CreateAction(ctx, boardID(t, fix, srcBid), "source chore", "", "ana", nil)
	if err != nil {
		t.Fatalf("CreateAction source: %v", err)
	}
	if _, err := fix.store.CreateAction(ctx, numeric, "fix the pipeline", "ana", "ana", nil); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	carriedFrom := srcAction.ID
	if _, err := fix.store.CreateAction(ctx, numeric, "rotate the secrets", "", "bo", &carriedFrom); err != nil {
		t.Fatalf("CreateAction carried: %v", err)
	}
	doneAction, err := fix.store.CreateAction(ctx, numeric, "order new stickers", "lee", "lee", nil)
	if err != nil {
		t.Fatalf("CreateAction done: %v", err)
	}
	if err := fix.store.SetActionDone(ctx, doneAction.ID, true); err != nil {
		t.Fatalf("SetActionDone: %v", err)
	}
	return bid, creatorCookies
}

func TestExportMarkdownMatchesGoldenFile(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := seedExportBoard(t, fix)
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)

	rec := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"/export.md", nil, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET export status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if ctype := rec.Header().Get("Content-Type"); ctype != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/markdown with UTF-8", ctype)
	}
	if disp := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(disp, "attachment;") || !strings.HasSuffix(disp, `.md"`) {
		t.Errorf("Content-Disposition = %q, want an attachment with an .md filename", disp)
	}

	want, err := os.ReadFile(filepath.Join("testdata", "export.md"))
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}
	if got := rec.Body.String(); got != string(want) {
		t.Errorf("export markdown differs from golden file.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBoardShellOffersExportAndPrint(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, fac...).Body.String()
	for _, want := range []string{
		`id="export-section"`,
		`id="export-download"`,
		"/b/" + bid + "/export.md",
		`id="copy-export"`,
		`x-data="exportCopy()"`,
		"navigator.clipboard",
		`id="print-board"`,
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("board shell missing export affordance %q", want)
		}
	}

	// Print styling ships in the app stylesheet, not inline: the
	// shell must link it, and the stylesheet must carry the print
	// rules that hide controls and force full-strength cards.
	if !strings.Contains(shell, `/static/app.css`) {
		t.Error("shell must link the app stylesheet")
	}
	raw, err := os.ReadFile("../static/app.css")
	if err != nil {
		t.Fatalf("read app stylesheet: %v", err)
	}
	for _, want := range []string{"@media print", "#facilitator-panel", ".card.discussed"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("app stylesheet missing print rule %q", want)
		}
	}

	// Joined participants get the same download affordance.
	ana := joinAs(t, fix, bid, "ana")
	viewer := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if !strings.Contains(viewer, `id="export-download"`) {
		t.Error("participant shell missing the export download link")
	}
}

func TestExportBlindCollectionAdmitsOnlyFacilitators(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := seedExportBoard(t, fix)
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)
	ana := joinAs(t, fix, bid, "ana")

	if _, err := fix.store.SetCardsHidden(t.Context(), boardID(t, fix, bid), true); err != nil {
		t.Fatalf("SetCardsHidden: %v", err)
	}

	// Participants cannot smuggle a hidden board out through export.
	hidden := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"/export.md", nil, ana...)
	assertFlashOnly(t, hidden, http.StatusForbidden)
	if strings.Contains(hidden.Body.String(), "slow builds") {
		t.Error("hidden export denial leaks a card body")
	}

	// Facilitators always read the full board, hidden or not.
	full := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"/export.md", nil, fac...)
	if full.Code != http.StatusOK {
		t.Fatalf("facilitator export on hidden board status = %d, want 200", full.Code)
	}
	for _, want := range []string{"slow builds", "missed lunches", "shipped the fix", "fix the pipeline"} {
		if !strings.Contains(full.Body.String(), want) {
			t.Errorf("facilitator export missing %q", want)
		}
	}

	// Revealed boards export fully for every joined viewer.
	if _, err := fix.store.SetCardsHidden(t.Context(), boardID(t, fix, bid), false); err != nil {
		t.Fatalf("SetCardsHidden reveal: %v", err)
	}
	open := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"/export.md", nil, ana...)
	if open.Code != http.StatusOK {
		t.Fatalf("participant export on revealed board status = %d, want 200", open.Code)
	}
	for _, want := range []string{"slow builds", "low morale", "great facilitation", "order new stickers"} {
		if !strings.Contains(open.Body.String(), want) {
			t.Errorf("revealed export missing %q", want)
		}
	}

	// Strangers without a participant row get no export either way.
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"/export.md", nil),
		http.StatusForbidden)

	// Unknown boards stay a plain 404.
	if missing := doRequest(t, fix.mux, http.MethodGet, "/b/does-not-exist/export.md", nil, fac...); missing.Code != http.StatusNotFound {
		t.Errorf("export on unknown board status = %d, want 404", missing.Code)
	}
}
