package handlers

import (
	"net/http"
	"strings"
	"testing"
)

// archiveAsFacilitator flags a board through the facilitator route,
// returning the dashboard row partial status for assertion.
func archiveAsFacilitator(t *testing.T, fix *boardFixture, bid string, cookies map[string]*http.Cookie) {
	t.Helper()

	rec := doRequest(t, fix.mux, http.MethodPost, "/boards/"+bid+"/archive", nil,
		withCookies(cookies, facCookieName)...)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive status = %d, want 200", rec.Code)
	}
}

func TestDashboardStatusFilterSplitsActiveAndArchived(t *testing.T) {
	fix := openFixture(t)
	alphaForm := defaultCreateForm()
	alphaForm.Set("name", "Alpha sprint retro")
	alphaBid, _ := createBoard(t, fix.mux, alphaForm)
	betaForm := defaultCreateForm()
	betaForm.Set("name", "Beta planning")
	betaBid, betaCookies := createBoard(t, fix.mux, betaForm)
	archiveAsFacilitator(t, fix, betaBid, betaCookies)

	for _, target := range []string{"/", "/?status=all", "/?status=bogus"} {
		dash := doRequest(t, fix.mux, http.MethodGet, target, nil).Body.String()
		for _, want := range []string{"/b/" + alphaBid, "/b/" + betaBid} {
			if !strings.Contains(dash, want) {
				t.Errorf("GET %s missing %q", target, want)
			}
		}
	}

	active := doRequest(t, fix.mux, http.MethodGet, "/?status=active", nil).Body.String()
	if !strings.Contains(active, "/b/"+alphaBid) {
		t.Error("active filter hides the active board")
	}
	if strings.Contains(active, "/b/"+betaBid) {
		t.Error("active filter leaks the archived board")
	}

	archived := doRequest(t, fix.mux, http.MethodGet, "/?status=archived", nil).Body.String()
	if !strings.Contains(archived, "/b/"+betaBid) {
		t.Error("archived filter hides the archived board")
	}
	if strings.Contains(archived, "/b/"+alphaBid) {
		t.Error("archived filter leaks the active board")
	}

	// The filter control marks the current view and stays on one page:
	// links, not a forked dashboard.
	for _, want := range []string{`aria-label="Board filter"`, `aria-current="page"`, "status=active", "status=archived"} {
		if !strings.Contains(active, want) {
			t.Errorf("filtered dashboard missing %q", want)
		}
	}
}

func TestDashboardFilterCombinesWithSearch(t *testing.T) {
	fix := openFixture(t)
	alphaForm := defaultCreateForm()
	alphaForm.Set("name", "Alpha sprint retro")
	alphaBid, _ := createBoard(t, fix.mux, alphaForm)
	gammaForm := defaultCreateForm()
	gammaForm.Set("name", "Gamma retro night")
	gammaBid, gammaCookies := createBoard(t, fix.mux, gammaForm)
	archiveAsFacilitator(t, fix, gammaBid, gammaCookies)

	// Search narrows within the selected shelf.
	match := doRequest(t, fix.mux, http.MethodGet, "/?q=retro&status=active", nil).Body.String()
	if !strings.Contains(match, "/b/"+alphaBid) {
		t.Error("filtered search hides the matching active board")
	}
	if strings.Contains(match, "/b/"+gammaBid) {
		t.Error("filtered search leaks the archived board into the active shelf")
	}

	// The two controls preserve each other: filter links carry the
	// query, and the search form carries the shelf.
	if !strings.Contains(match, "q=retro") {
		t.Error("filter links drop the search query")
	}
	shelf := doRequest(t, fix.mux, http.MethodGet, "/?status=archived", nil).Body.String()
	if !strings.Contains(shelf, `name="status"`) {
		t.Error("search form drops the active shelf")
	}
}

func TestDashboardCountsCardsAndOpenActions(t *testing.T) {
	fix := openFixture(t)
	form := defaultCreateForm()
	form.Set("name", "Counted retro")
	bid, _ := createBoard(t, fix.mux, form)
	numeric := boardID(t, fix, bid)

	if _, err := fix.store.CreateCard(t.Context(), firstColumnID(t, fix, bid), "one", "ana"); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if _, err := fix.store.CreateCard(t.Context(), firstColumnID(t, fix, bid), "two", "bo"); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if _, err := fix.store.CreateAction(t.Context(), numeric, "still open", "ana", "ana", nil); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	finished, err := fix.store.CreateAction(t.Context(), numeric, "already done", "", "bo", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if err := fix.store.SetActionDone(t.Context(), finished.ID, true); err != nil {
		t.Fatalf("SetActionDone: %v", err)
	}

	dash := doRequest(t, fix.mux, http.MethodGet, "/", nil).Body.String()
	if !strings.Contains(dash, "2 cards · 1 open action") {
		t.Errorf("dashboard row counts the wrong totals (body: %.500s…)", dash)
	}
}
