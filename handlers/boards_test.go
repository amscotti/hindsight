package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"hindsight/db"
)

type publishedEvent struct {
	boardID string
	name    string
	html    string
}

type recordingPublisher struct {
	mu     sync.Mutex
	events []publishedEvent
	// hook runs synchronously inside Publish while the log is held, so
	// tests can assert what is already committed when a broadcast
	// fires. It must not call back into the publisher.
	hook func(publishedEvent)
}

func (f *recordingPublisher) Publish(boardID, name, html string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev := publishedEvent{boardID: boardID, name: name, html: html}
	f.events = append(f.events, ev)
	if f.hook != nil {
		f.hook(ev)
	}
	return nil
}

func (f *recordingPublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

type boardFixture struct {
	handler   *Boards
	store     *db.Store
	publisher *recordingPublisher
	mux       http.Handler
}

func openFixture(t *testing.T) *boardFixture {
	t.Helper()

	sqldb, secret, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sqldb.Close(); err != nil {
			t.Errorf("close test DB: %v", err)
		}
	})
	store := db.NewStore(sqldb)
	publisher := &recordingPublisher{}
	handler := NewBoards(store, secret, publisher, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handler.Dashboard)
	mux.HandleFunc("GET /new", handler.New)
	mux.HandleFunc("POST /boards", handler.Create)
	mux.HandleFunc("GET /b/{bid}", handler.Show)
	mux.HandleFunc("POST /b/{bid}/join", handler.Join)
	mux.HandleFunc("POST /boards/{bid}/archive", handler.Archive)
	mux.HandleFunc("POST /boards/{bid}/unarchive", handler.Unarchive)
	mux.HandleFunc("POST /boards/{bid}/duplicate", handler.Duplicate)
	mux.HandleFunc("POST /boards/{bid}/regenerate", handler.Regenerate)
	mux.HandleFunc("POST /b/{bid}/cards", handler.CreateCard)
	mux.HandleFunc("PUT /cards/{cid}", handler.UpdateCard)
	mux.HandleFunc("DELETE /cards/{cid}", handler.DeleteCard)
	mux.HandleFunc("POST /cards/{cid}/vote", handler.Vote)
	mux.HandleFunc("POST /cards/{cid}/unvote", handler.Unvote)
	mux.HandleFunc("POST /b/{bid}/phase", handler.SetPhase)
	mux.HandleFunc("POST /b/{bid}/reveal", handler.Reveal)
	mux.HandleFunc("POST /b/{bid}/lock", handler.Lock)
	mux.HandleFunc("POST /b/{bid}/timer", handler.Timer)
	mux.HandleFunc("POST /b/{bid}/focus", handler.Focus)
	mux.HandleFunc("POST /b/{bid}/focus/next", handler.FocusNext)
	mux.HandleFunc("POST /b/{bid}/focus/prev", handler.FocusPrev)
	mux.HandleFunc("POST /b/{bid}/sort", handler.Sort)
	mux.HandleFunc("POST /cards/{cid}/comments", handler.CreateComment)
	mux.HandleFunc("POST /b/{bid}/kudos", handler.CreateKudo)
	mux.HandleFunc("DELETE /kudos/{kid}", handler.DeleteKudo)
	mux.HandleFunc("POST /b/{bid}/actions", handler.CreateAction)
	mux.HandleFunc("PUT /actions/{aid}", handler.UpdateAction)
	mux.HandleFunc("DELETE /actions/{aid}", handler.DeleteAction)
	mux.HandleFunc("GET /b/{bid}/export.md", handler.Export)
	return &boardFixture{handler: handler, store: store, publisher: publisher, mux: mux}
}

func doRequest(t *testing.T, mux http.Handler, method, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func responseCookies(rec *httptest.ResponseRecorder) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		out[c.Name] = c
	}
	return out
}

// createBoard posts the creation form and returns the new public ID plus
// every cookie the response set.
func createBoard(t *testing.T, mux http.Handler, form url.Values) (string, map[string]*http.Cookie) {
	t.Helper()

	rec := doRequest(t, mux, http.MethodPost, "/boards", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /boards status = %d, want 303 (body: %s)", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/b/") || len(location) <= 3 {
		t.Fatalf("POST /boards Location = %q, want /b/{id}", location)
	}
	return strings.TrimPrefix(location, "/b/"), responseCookies(rec)
}

func defaultCreateForm() url.Values {
	return url.Values{
		"name":         {"Sprint 12 retro"},
		"context":      {"Looking back"},
		"display_name": {"Facilitator"},
		"template":     {"mad-sad-glad"},
	}
}

func withCookies(cookies map[string]*http.Cookie, names ...string) []*http.Cookie {
	var out []*http.Cookie
	for _, name := range names {
		if c, ok := cookies[name]; ok {
			out = append(out, c)
		}
	}
	return out
}

func TestCreateSeedsTemplateColumnsInOrder(t *testing.T) {
	fix := openFixture(t)

	bid, cookies := createBoard(t, fix.mux, defaultCreateForm())
	kitchen := withCookies(cookies, facCookieName, partsCookieName, revealCookieName)
	if len(kitchen) != 3 {
		t.Fatalf("creation set %d cookies, want facilitator + participant + reveal", len(kitchen))
	}

	cols, err := fix.store.ListColumns(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	want := []string{"Mad", "Sad", "Glad"}
	if len(cols) != len(want) {
		t.Fatalf("seeded %d columns, want %d", len(cols), len(want))
	}
	for i, title := range want {
		if cols[i].Title != title || cols[i].Position != i {
			t.Errorf("column %d = %q@%d, want %q@%d",
				i, cols[i].Title, cols[i].Position, title, i)
		}
	}

	// The token reveal renders exactly once: first view shows it, the
	// second view must not.
	first := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, kitchen...)
	if first.Code != http.StatusOK {
		t.Fatalf("first GET status = %d, want 200", first.Code)
	}
	firstBody := first.Body.String()
	if !strings.Contains(firstBody, `id="facilitator-token-value"`) {
		t.Error("first board view must render the one-time facilitator token")
	}
	for _, want := range []string{
		`id="copy-participant-link"`,
		`id="copy-facilitator-link"`,
		`/b/` + bid,
		`id="column-`,
	} {
		if !strings.Contains(firstBody, want) {
			t.Errorf("first board view missing %q", want)
		}
	}

	secondCookies := withCookies(responseCookies(first), facCookieName, partsCookieName)
	second := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, secondCookies...)
	if strings.Contains(second.Body.String(), `id="facilitator-token-value"`) {
		t.Error("second board view must not render the facilitator token again")
	}

	if n := fix.publisher.count(); n != 0 {
		t.Errorf("board creation published %d events, want none", n)
	}
}

func TestCreateAndJoinRejectOverlongInput(t *testing.T) {
	fix := openFixture(t)

	overlong := defaultCreateForm()
	overlong.Set("name", strings.Repeat("x", 201))
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/boards", overlong),
		http.StatusUnprocessableEntity)

	overlong = defaultCreateForm()
	overlong.Set("context", strings.Repeat("x", 2001))
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/boards", overlong),
		http.StatusUnprocessableEntity)

	overlong = defaultCreateForm()
	overlong.Set("display_name", strings.Repeat("x", 101))
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/boards", overlong),
		http.StatusUnprocessableEntity)

	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
			url.Values{"name": {strings.Repeat("x", 101)}}),
		http.StatusUnprocessableEntity)
}

// seedFailStore fails board seeding on demand, standing in for any
// mid-seed store failure.
type seedFailStore struct {
	boardStore
	err error
}

func (s seedFailStore) CreateSeededBoard(context.Context, db.SeededBoard) (*db.Board, error) {
	return nil, s.err
}

func TestCreateSeedFailureLeavesNoResidue(t *testing.T) {
	fix := openFixture(t)
	failing := NewBoards(seedFailStore{boardStore: fix.store, err: errors.New("seed boom")},
		fix.handler.secret, fix.publisher, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /boards", failing.Create)

	rec := doRequest(t, mux, http.MethodPost, "/boards", defaultCreateForm())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /boards status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
	stats, err := fix.store.ListBoardsWithStats(t.Context())
	if err != nil {
		t.Fatalf("ListBoardsWithStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("failed create left %d board rows, want 0 (no half-seeded residue)", len(stats))
	}
}

func boardID(t *testing.T, fix *boardFixture, publicID string) int64 {
	t.Helper()

	board, err := fix.store.GetBoardByPublicID(t.Context(), publicID)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	return board.ID
}

func TestJoinGateJoinSuffixAndShell(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	creator := withCookies(creatorCookies, facCookieName, partsCookieName)

	// Voteless visitors get the join gate, not the shell.
	gate := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil)
	gateBody := gate.Body.String()
	if gate.Code != http.StatusOK {
		t.Fatalf("gate status = %d, want 200", gate.Code)
	}
	for _, want := range []string{`id="join-gate"`, "Join board", `name="name"`} {
		if !strings.Contains(gateBody, want) {
			t.Errorf("join gate missing %q", want)
		}
	}
	if strings.Contains(gateBody, `id="board-columns"`) {
		t.Error("join gate must not render board columns")
	}

	// Joining sets the participant cookie and returns the columns HTML.
	join := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
		url.Values{"name": {"ana"}})
	if join.Code != http.StatusOK {
		t.Fatalf("join status = %d, want 200", join.Code)
	}
	joinBody := join.Body.String()
	if !strings.Contains(joinBody, `id="board-columns"`) {
		t.Error("join response must be the board columns HTML")
	}
	for _, title := range []string{"Mad", "Sad", "Glad"} {
		if !strings.Contains(joinBody, title) {
			t.Errorf("join columns missing %q", title)
		}
	}
	anaCookies := withCookies(responseCookies(join), partsCookieName)
	if len(anaCookies) != 1 {
		t.Fatal("join must set the participant cookie")
	}

	// Joined visitors see the shell with their name.
	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, anaCookies...)
	shellBody := shell.Body.String()
	if !strings.Contains(shellBody, `id="board-columns"`) {
		t.Error("joined view must render the board shell")
	}
	if !strings.Contains(shellBody, "Joined as ana") {
		t.Error("shell must show the participant display name")
	}
	if strings.Contains(shellBody, `id="copy-facilitator-link"`) {
		t.Error("plain participant must not get the facilitator share button")
	}
	if !strings.Contains(shellBody, `id="copy-participant-link"`) {
		t.Error("shell must offer the participant share link")
	}

	// Duplicate display names are suffixed.
	for i, want := range []string{"ana-2", "ana-3"} {
		dup := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
			url.Values{"name": {"ana"}})
		if dup.Code != http.StatusOK {
			t.Fatalf("duplicate join %d status = %d, want 200", i, dup.Code)
		}
		view := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil,
			withCookies(responseCookies(dup), partsCookieName)...)
		if !strings.Contains(view.Body.String(), "Joined as "+want) {
			t.Errorf("duplicate join %d: shell missing %q", i, "Joined as "+want)
		}
	}

	// Re-joining with a valid cookie keeps the existing row (upsert).
	again := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
		url.Values{"name": {"someone-else"}}, anaCookies...)
	if again.Code != http.StatusOK {
		t.Fatalf("re-join status = %d, want 200", again.Code)
	}
	view := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, anaCookies...)
	if !strings.Contains(view.Body.String(), "Joined as ana") {
		t.Error("re-join must keep the existing participant row")
	}

	// The creator (facilitator + participant) skips the gate too.
	creatorView := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, creator...)
	if !strings.Contains(creatorView.Body.String(), `id="board-columns"`) {
		t.Error("facilitator without a fresh join must still see the shell")
	}

	// Bad joins are rejected with the flash fragment.
	blank := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
		url.Values{"name": {"  "}})
	assertFlashOnly(t, blank, http.StatusUnprocessableEntity)

	missing := doRequest(t, fix.mux, http.MethodPost, "/b/does-not-exist/join",
		url.Values{"name": {"ana"}})
	if missing.Code != http.StatusNotFound {
		t.Errorf("join on unknown board status = %d, want 404", missing.Code)
	}
	unknown := doRequest(t, fix.mux, http.MethodGet, "/b/does-not-exist", nil)
	if unknown.Code != http.StatusNotFound {
		t.Errorf("GET unknown board status = %d, want 404", unknown.Code)
	}
}

func TestConcurrentSameNameJoinsConverge(t *testing.T) {
	fix := openFixture(t)
	bid, _ := createBoard(t, fix.mux, defaultCreateForm())
	numericID := boardID(t, fix, bid)

	// Racing joins with the same display name must all succeed with
	// distinct suffixed names, mirroring the vote-cap race pattern.
	const joiners = 8
	codes := make([]int, joiners)
	recs := make([]*httptest.ResponseRecorder, joiners)
	var wg sync.WaitGroup
	for i := range recs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			form := url.Values{"name": {"ana"}}
			req := httptest.NewRequest(http.MethodPost, "/b/"+bid+"/join",
				strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			fix.mux.ServeHTTP(rec, req)
			codes[i] = rec.Code
			recs[i] = rec
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("join %d status = %d, want 200", i, code)
		}
	}
	names := map[string]bool{}
	for _, rec := range recs {
		raw := extractToken(t, fix, responseCookies(rec), partsCookieName, bid)
		participant, err := fix.store.GetParticipantByToken(t.Context(), numericID, raw)
		if err != nil {
			t.Fatalf("join token resolves to no participant: %v", err)
		}
		names[participant.Name] = true
	}
	want := map[string]bool{"ana": true}
	for i := 2; i <= joiners; i++ {
		want["ana-"+strconv.Itoa(i)] = true
	}
	if len(names) != len(want) {
		t.Fatalf("distinct names = %v, want %v", names, want)
	}
	for name := range want {
		if !names[name] {
			t.Errorf("missing suffixed name %q in %v", name, names)
		}
	}
}

func assertFlashOnly(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()

	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`id="flash"`, `hx-swap-oob="true"`, "flash-error"} {
		if !strings.Contains(body, want) {
			t.Errorf("error body missing %q (body: %s)", want, body)
		}
	}
	if strings.Contains(body, `id="board-`) || strings.Contains(body, "<li") {
		t.Errorf("error body must carry only the flash fragment (body: %s)", body)
	}
}

func TestFacilitatorGuardRejectsStrangers(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName)

	otherBid, otherCookies := createBoard(t, fix.mux, url.Values{
		"name":         {"Other board"},
		"display_name": {"Other"},
		"template":     {"plus-delta"},
	})
	otherFac := withCookies(otherCookies, facCookieName)

	// A plain participant joins but is not a facilitator.
	join := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/join",
		url.Values{"name": {"ana"}})
	participant := withCookies(responseCookies(join), partsCookieName)

	targets := []struct {
		name   string
		method string
		target string
		form   url.Values
	}{
		{"archive", http.MethodPost, "/boards/" + bid + "/archive", nil},
		{"unarchive", http.MethodPost, "/boards/" + bid + "/unarchive", nil},
		{"duplicate", http.MethodPost, "/boards/" + bid + "/duplicate",
			url.Values{"carry_actions": {"1"}}},
	}
	for _, target := range targets {
		// No cookies at all.
		assertFlashOnly(t,
			doRequest(t, fix.mux, target.method, target.target, target.form),
			http.StatusForbidden)
		// Participant cookie is not enough.
		assertFlashOnly(t,
			doRequest(t, fix.mux, target.method, target.target, target.form, participant...),
			http.StatusForbidden)
		// Another board's facilitator cookie is not enough.
		assertFlashOnly(t,
			doRequest(t, fix.mux, target.method, target.target, target.form, otherFac...),
			http.StatusForbidden)
		// Tampered cookie value is not enough.
		bogus := &http.Cookie{Name: facCookieName, Value: "bogus.payload"}
		assertFlashOnly(t,
			doRequest(t, fix.mux, target.method, target.target, target.form, bogus),
			http.StatusForbidden)
		_ = otherBid
	}

	// The real facilitator passes the guard.
	archived := doRequest(t, fix.mux, http.MethodPost, "/boards/"+bid+"/archive", nil, fac...)
	if archived.Code != http.StatusOK {
		t.Fatalf("facilitator archive status = %d, want 200", archived.Code)
	}
}

func TestArchiveHidesButPreserves(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)

	if _, err := fix.store.CreateCard(t.Context(),
		firstColumnID(t, fix, bid), "keep me", "ana"); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	archived := doRequest(t, fix.mux, http.MethodPost, "/boards/"+bid+"/archive", nil, fac...)
	if archived.Code != http.StatusOK {
		t.Fatalf("archive status = %d, want 200", archived.Code)
	}
	archivedBody := archived.Body.String()
	if !strings.Contains(archivedBody, "board-"+bid) {
		t.Error("archive response must be the dashboard row partial")
	}
	if !strings.Contains(archivedBody, "Unarchive") {
		t.Error("archived row must offer unarchive")
	}

	dash := doRequest(t, fix.mux, http.MethodGet, "/", nil, fac...).Body.String()
	active, after := splitDashboard(t, dash)
	if strings.Contains(active, "/b/"+bid) {
		t.Error("archived board must leave the active list")
	}
	if !strings.Contains(after, "/b/"+bid) {
		t.Error("archived board must appear in the archived list")
	}

	// Content survives the archive flag.
	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, fac...)
	for _, want := range []string{"Mad", "Sad", "Glad"} {
		if !strings.Contains(shell.Body.String(), want) {
			t.Errorf("archived board view missing column %q", want)
		}
	}

	restored := doRequest(t, fix.mux, http.MethodPost, "/boards/"+bid+"/unarchive", nil, fac...)
	if restored.Code != http.StatusOK {
		t.Fatalf("unarchive status = %d, want 200", restored.Code)
	}
	if !strings.Contains(restored.Body.String(), "Archive") {
		t.Error("restored row must offer archive again")
	}
	dashAgain := doRequest(t, fix.mux, http.MethodGet, "/", nil, fac...).Body.String()
	activeAgain, _ := splitDashboard(t, dashAgain)
	if !strings.Contains(activeAgain, "/b/"+bid) {
		t.Error("unarchived board must return to the active list")
	}
}

func splitDashboard(t *testing.T, body string) (active, archived string) {
	t.Helper()

	idx := strings.Index(body, "Archived boards")
	if idx < 0 {
		t.Fatalf("dashboard missing archived section (body: %.200s…)", body)
	}
	return body[:idx], body[idx:]
}

func TestDuplicateCopiesColumnsAndOptionallyActions(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, url.Values{
		"name":         {"Source"},
		"display_name": {"Facilitator"},
		"template":     {"start-stop-continue"},
	})
	fac := withCookies(creatorCookies, facCookieName)

	srcID := boardID(t, fix, bid)
	open, err := fix.store.CreateAction(t.Context(), srcID, "carry me", "ana", "ana", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	doneAction, err := fix.store.CreateAction(t.Context(), srcID, "leave me", "", "ana", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if err := fix.store.SetActionDone(t.Context(), doneAction.ID, true); err != nil {
		t.Fatalf("SetActionDone: %v", err)
	}

	// Without the carry flag only columns copy.
	plain := doRequest(t, fix.mux, http.MethodPost, "/boards/"+bid+"/duplicate",
		url.Values{}, fac...)
	if plain.Code != http.StatusSeeOther {
		t.Fatalf("duplicate status = %d, want 303", plain.Code)
	}
	plainBid := strings.TrimPrefix(plain.Header().Get("Location"), "/b/")
	assertColumnTitles(t, fix, plainBid, []string{"Start", "Stop", "Continue"})
	if actions, err := fix.store.ListOpenActions(t.Context(), boardID(t, fix, plainBid)); err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	} else if len(actions) != 0 {
		t.Errorf("plain duplicate carried %d actions, want 0", len(actions))
	}

	// With the carry flag open actions copy with provenance.
	carried := doRequest(t, fix.mux, http.MethodPost, "/boards/"+bid+"/duplicate",
		url.Values{"carry_actions": {"1"}}, fac...)
	if carried.Code != http.StatusSeeOther {
		t.Fatalf("duplicate with carry status = %d, want 303", carried.Code)
	}
	carriedBid := strings.TrimPrefix(carried.Header().Get("Location"), "/b/")
	if carriedBid == bid || carriedBid == plainBid {
		t.Errorf("duplicate did not mint a fresh share ID: %q", carriedBid)
	}
	assertColumnTitles(t, fix, carriedBid, []string{"Start", "Stop", "Continue"})
	got, err := fix.store.ListOpenActions(t.Context(), boardID(t, fix, carriedBid))
	if err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	}
	if len(got) != 1 || got[0].Text != "carry me" || got[0].Owner != "ana" {
		t.Fatalf("carried actions = %+v, want the single open item", got)
	}
	if got[0].CarriedFrom == nil || *got[0].CarriedFrom != open.ID {
		t.Errorf("carried_from = %v, want %d", got[0].CarriedFrom, open.ID)
	}

	// The duplicator keeps facilitator access to the copy.
	nextCookies := withCookies(responseCookies(carried), facCookieName)
	view := doRequest(t, fix.mux, http.MethodGet, "/b/"+carriedBid, nil, nextCookies...)
	if view.Code != http.StatusOK {
		t.Fatalf("GET duplicate status = %d, want 200", view.Code)
	}
	if !strings.Contains(view.Body.String(), `id="copy-facilitator-link"`) {
		t.Error("duplicator must hold facilitator rights on the copy")
	}

	// Duplicating an unknown board is a plain 404.
	missing := doRequest(t, fix.mux, http.MethodPost, "/boards/does-not-exist/duplicate",
		url.Values{}, fac...)
	if missing.Code != http.StatusNotFound {
		t.Errorf("duplicate unknown status = %d, want 404", missing.Code)
	}
}

func TestCreateCarriesOpenActionsWithProvenance(t *testing.T) {
	fix := openFixture(t)
	srcBid, _ := createBoard(t, fix.mux, defaultCreateForm())
	srcID := boardID(t, fix, srcBid)
	open, err := fix.store.CreateAction(t.Context(), srcID, "carry me", "ana", "ana", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	done, err := fix.store.CreateAction(t.Context(), srcID, "leave me", "", "ana", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if err := fix.store.SetActionDone(t.Context(), done.ID, true); err != nil {
		t.Fatalf("SetActionDone: %v", err)
	}

	form := defaultCreateForm()
	form.Set("carry_from", srcBid)
	newBid, _ := createBoard(t, fix.mux, form)
	assertColumnTitles(t, fix, newBid, []string{"Mad", "Sad", "Glad"})

	got, err := fix.store.ListOpenActions(t.Context(), boardID(t, fix, newBid))
	if err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	}
	if len(got) != 1 || got[0].Text != "carry me" || got[0].Owner != "ana" {
		t.Fatalf("carried actions = %+v, want only the open item", got)
	}
	if got[0].CarriedFrom == nil || *got[0].CarriedFrom != open.ID {
		t.Errorf("carried_from = %v, want %d", got[0].CarriedFrom, open.ID)
	}

	unknown := defaultCreateForm()
	unknown.Set("carry_from", "does-not-exist")
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/boards", unknown),
		http.StatusUnprocessableEntity)
}

func assertColumnTitles(t *testing.T, fix *boardFixture, publicID string, want []string) {
	t.Helper()

	cols, err := fix.store.ListColumns(t.Context(), boardID(t, fix, publicID))
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	if len(cols) != len(want) {
		t.Fatalf("columns = %d, want %d", len(cols), len(want))
	}
	for i, title := range want {
		if cols[i].Title != title || cols[i].Position != i {
			t.Errorf("column %d = %q@%d, want %q@%d",
				i, cols[i].Title, cols[i].Position, title, i)
		}
	}
}

func TestDashboardSearchCountsAndNewPage(t *testing.T) {
	fix := openFixture(t)
	alphaForm := defaultCreateForm()
	alphaForm.Set("name", "Alpha sprint retro")
	alphaBid, _ := createBoard(t, fix.mux, alphaForm)
	betaForm := defaultCreateForm()
	betaForm.Set("name", "Beta planning")
	betaForm.Set("template", "plus-delta")
	betaBid, _ := createBoard(t, fix.mux, betaForm)

	if _, err := fix.store.CreateCard(t.Context(),
		firstColumnID(t, fix, alphaBid), "a card", "ana"); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	dash := doRequest(t, fix.mux, http.MethodGet, "/", nil).Body.String()
	for _, want := range []string{
		`id="board-search"`,
		"Active boards · 2",
		"/b/" + alphaBid,
		"/b/" + betaBid,
		"1 card",
		"Start a new retro board",
	} {
		if !strings.Contains(dash, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}

	filtered := doRequest(t, fix.mux, http.MethodGet, "/?q=alpha", nil).Body.String()
	if !strings.Contains(filtered, "/b/"+alphaBid) {
		t.Error("search must keep matching boards")
	}
	if strings.Contains(filtered, "/b/"+betaBid) {
		t.Error("search must hide non-matching boards")
	}

	picker := doRequest(t, fix.mux, http.MethodGet, "/new", nil)
	pickerBody := picker.Body.String()
	if picker.Code != http.StatusOK {
		t.Fatalf("GET /new status = %d, want 200", picker.Code)
	}
	for _, want := range []string{
		`name="name"`, `name="context"`, `name="template"`,
		"went-well-better-kudos", "mad-sad-glad", "start-stop-continue",
		"four-ls", "kudos-mix", "plus-delta",
		`name="carry_from"`,
	} {
		if !strings.Contains(pickerBody, want) {
			t.Errorf("creation page missing %q", want)
		}
	}
	defaultIdx := strings.Index(pickerBody, `value="went-well-better-kudos"`)
	legacyIdx := strings.Index(pickerBody, `value="mad-sad-glad"`)
	if defaultIdx < 0 || legacyIdx < 0 || defaultIdx > legacyIdx {
		t.Error("went-well-better-kudos must be the first template option")
	}
}

func TestCreateEmptyTemplateSeedsDefaultColumns(t *testing.T) {
	fix := openFixture(t)

	form := url.Values{
		"name":         {"Default template retro"},
		"display_name": {"Facilitator"},
	}
	bid, _ := createBoard(t, fix.mux, form)
	cols, err := fix.store.ListColumns(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	want := []string{"Went well", "Could be better", "Kudos"}
	if len(cols) != len(want) {
		t.Fatalf("seeded %d columns, want %d", len(cols), len(want))
	}
	for i, title := range want {
		if cols[i].Title != title || cols[i].Position != i {
			t.Errorf("column %d = %q@%d, want %q@%d",
				i, cols[i].Title, cols[i].Position, title, i)
		}
	}
}

func TestCreateUnknownTemplateRejected(t *testing.T) {
	fix := openFixture(t)

	form := defaultCreateForm()
	form.Set("template", "not-a-template")
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/boards", form),
		http.StatusUnprocessableEntity)
}

func firstColumnID(t *testing.T, fix *boardFixture, publicID string) int64 {
	t.Helper()

	cols, err := fix.store.ListColumns(t.Context(), boardID(t, fix, publicID))
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	if len(cols) == 0 {
		t.Fatal("board has no columns")
	}
	return cols[0].ID
}

func TestFacilitatorShareLinkClaimsAccess(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	facRaw := extractToken(t, fix, creatorCookies, facCookieName, bid)

	// A stranger following the facilitator link gains facilitator rights…
	claim := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"?fac="+facRaw, nil)
	if claim.Code != http.StatusSeeOther {
		t.Fatalf("claim status = %d, want 303", claim.Code)
	}
	if location := claim.Header().Get("Location"); location != "/b/"+bid {
		t.Errorf("claim Location = %q, want clean board URL", location)
	}
	claimed := withCookies(responseCookies(claim), facCookieName)
	if len(claimed) != 1 {
		t.Fatal("claim must set the facilitator cookie")
	}
	view := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, claimed...)
	if !strings.Contains(view.Body.String(), `id="copy-facilitator-link"`) {
		t.Error("claimed browser must see facilitator controls")
	}

	// …while a forged token gains nothing.
	forged := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid+"?fac=forged-token", nil)
	if forged.Code != http.StatusOK {
		t.Fatalf("forged claim status = %d, want 200 (gate, no rights)", forged.Code)
	}
	if len(withCookies(responseCookies(forged), facCookieName)) != 0 {
		t.Error("forged token must not set the facilitator cookie")
	}
	if !strings.Contains(forged.Body.String(), `id="join-gate"`) {
		t.Error("forged claim must still render the join gate")
	}
}

func TestShareIDsAndTokensCarryFullEntropy(t *testing.T) {
	fix := openFixture(t)

	seen := map[string]bool{}
	for range 20 {
		bid, cookies := createBoard(t, fix.mux, defaultCreateForm())
		if seen[bid] {
			t.Fatalf("duplicate share ID %q", bid)
		}
		seen[bid] = true
		decodeToken(t, "share ID", bid)
		decodeToken(t, "facilitator token",
			extractToken(t, fix, cookies, facCookieName, bid))
		decodeToken(t, "participant token",
			extractToken(t, fix, cookies, partsCookieName, bid))
	}
}

func decodeToken(t *testing.T, label, value string) {
	t.Helper()

	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("%s %q is not base64url: %v", label, value, err)
	}
	if len(raw) < 16 {
		t.Errorf("%s decodes to %d bytes, want >= 16 (128-bit)", label, len(raw))
	}
}

func extractToken(t *testing.T, fix *boardFixture, cookies map[string]*http.Cookie, name, publicID string) string {
	t.Helper()

	c, ok := cookies[name]
	if !ok {
		t.Fatalf("missing cookie %q", name)
	}
	tokens, valid := verifySignedMap(fix.handler.secret, c.Value)
	if !valid {
		t.Fatalf("cookie %q failed verification", name)
	}
	raw, ok := tokens[publicID]
	if !ok {
		t.Fatalf("cookie %q has no entry for board %q", name, publicID)
	}
	return raw
}

func TestArchiveUnarchiveUnknownBoard404(t *testing.T) {
	fix := openFixture(t)

	for _, target := range []string{"/boards/does-not-exist/archive", "/boards/does-not-exist/unarchive"} {
		rec := doRequest(t, fix.mux, http.MethodPost, target, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s status = %d, want 404", target, rec.Code)
		}
	}
}

func TestUnvotedLeaderRendersZeroDespiteStoredCounter(t *testing.T) {
	cards := []db.Card{
		{ID: 1, ColumnID: 7, Body: "leader", Votes: 9},
		{ID: 2, ColumnID: 7, Body: "member", Votes: 5, GroupID: int64ptr(1)},
	}
	forest := cardForest(cards, nil, map[int64]int{}, map[int64][]db.Comment{}, false)
	if len(forest) != 1 {
		t.Fatalf("forest has %d tops, want 1", len(forest))
	}
	if forest[0].Votes != 0 {
		t.Errorf("unvoted leader votes = %d, want 0", forest[0].Votes)
	}
}

func TestMultiColumnShellRendersAllColumns(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)

	cols, err := fix.store.ListColumns(t.Context(), boardID(t, fix, bid))
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	for i, col := range cols {
		if _, err := fix.store.CreateCard(t.Context(), col.ID, "card-"+strconv.Itoa(i), "ana"); err != nil {
			t.Fatalf("CreateCard: %v", err)
		}
	}
	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, fac...).Body.String()
	for i := range cols {
		if !strings.Contains(shell, "card-"+strconv.Itoa(i)) {
			t.Errorf("shell missing card-%d", i)
		}
	}
}

func TestHostHeaderInjectionSanitized(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)

	req := httptest.NewRequest(http.MethodGet, "/b/"+bid, nil)
	req.Host = "evil.com\r\nX-Injected: 1"
	for _, c := range fac {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	fix.mux.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "evil.com") {
		t.Error("malicious host must not be reflected in share links")
	}
}

func int64ptr(v int64) *int64 { return &v }
