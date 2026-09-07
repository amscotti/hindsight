package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var (
	columnIDPattern = regexp.MustCompile(`id="column-([0-9]+)"`)
	cardIDPattern   = regexp.MustCompile(`id="card-([0-9]+)"`)
)

// postFormThrough posts a form through the full production wiring,
// carrying the cookie jar along, and returns the recorder plus the
// cookies the response set.
func postFormThrough(t *testing.T, h http.Handler, target string, form url.Values, jar []*http.Cookie) (*httptest.ResponseRecorder, []*http.Cookie) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range jar {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := append([]*http.Cookie{}, jar...)
	out = append(out, rec.Result().Cookies()...)
	return rec, out
}

func firstMatch(t *testing.T, pattern *regexp.Regexp, body, what string) string {
	t.Helper()

	m := pattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("response carries no %s id", what)
	}
	return m[1]
}

// TestProductionWiringLimitsVotes proves the limiter guards the wired
// vote route end to end: one client hammering votes through the real
// route table is answered 429 with the flash fragment, while the
// board, card, and earlier votes all work.
func TestProductionWiringLimitsVotes(t *testing.T) {
	isolateDB(t)
	h := mustHandler(t)

	created, jar := postFormThrough(t, h, "/boards", url.Values{
		"name":         {"rate night"},
		"display_name": {"facilitator"},
		"template":     {"mad-sad-glad"},
	}, nil)
	if created.Code != http.StatusSeeOther {
		t.Fatalf("POST /boards status = %d, want 303 (body: %s)", created.Code, created.Body.String())
	}
	bid := strings.TrimPrefix(created.Header().Get("Location"), "/b/")

	// The facilitator cookie from creation opens the voting window.
	if rec, _ := postFormThrough(t, h, "/b/"+bid+"/phase", url.Values{"phase": {"vote"}}, jar); rec.Code != http.StatusOK {
		t.Fatalf("open voting window status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// One card to vote on: read a column id off the board shell, add
	// the card, and read its id back out of the column fragment.
	getReq := httptest.NewRequest(http.MethodGet, "/b/"+bid, nil)
	for _, c := range jar {
		getReq.AddCookie(c)
	}
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET board status = %d, want 200", getRec.Code)
	}
	columnID := firstMatch(t, columnIDPattern, getRec.Body.String(), "column")

	added, jar := postFormThrough(t, h, "/b/"+bid+"/cards",
		url.Values{"column_id": {columnID}, "body": {"vote magnet"}}, jar)
	if added.Code != http.StatusOK {
		t.Fatalf("add card status = %d, want 200 (body: %s)", added.Code, added.Body.String())
	}
	cardID := firstMatch(t, cardIDPattern, added.Body.String(), "card")

	// Hammer votes from the one client identity. The burst covers the
	// first requests; the tail must be 429 with the flash fragment.
	var last *httptest.ResponseRecorder
	for i := 0; i < 40; i++ {
		rec, next := postFormThrough(t, h, "/cards/"+cardID+"/vote", nil, jar)
		jar, last = next, rec
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("40th vote status = %d, want 429", last.Code)
	}
	body, err := io.ReadAll(last.Result().Body)
	if err != nil {
		t.Fatalf("read 429 body: %v", err)
	}
	for _, want := range []string{`id="flash"`, `hx-swap-oob="true"`, "flash-error"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("429 body missing %q (body: %s)", want, body)
		}
	}
}
