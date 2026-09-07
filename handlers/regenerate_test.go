package handlers

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func regenerateTarget(bid string) string {
	return "/boards/" + bid + "/regenerate"
}

func TestRegenerateRotatesToken(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)
	facOnly := withCookies(creatorCookies, facCookieName)
	oldRaw := extractToken(t, fix, creatorCookies, facCookieName, bid)

	rec := doRequest(t, fix.mux, http.MethodPost, regenerateTarget(bid), nil, fac...)
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="facilitator-token-value"`) {
		t.Error("regenerate answer must render the raw token exactly once")
	}
	newRaw := extractToken(t, fix, responseCookies(rec), facCookieName, bid)
	if newRaw == "" || newRaw == oldRaw {
		t.Errorf("regenerate must set a fresh facilitator cookie (old %q new %q)", oldRaw, newRaw)
	}
	if !strings.Contains(body, newRaw) {
		t.Error("regenerate answer must carry the new raw token")
	}

	// The old token is dead: presenting it is forbidden.
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/phase",
			url.Values{"phase": {"vote"}}, facOnly...),
		http.StatusForbidden)

	// The new token works on a facilitator control.
	newCookies := withCookies(responseCookies(rec), facCookieName, partsCookieName)
	phase := doRequest(t, fix.mux, http.MethodPost, "/b/"+bid+"/phase",
		url.Values{"phase": {"vote"}}, newCookies...)
	if phase.Code != http.StatusOK {
		t.Fatalf("phase with the new token status = %d, want 200 (body: %s)",
			phase.Code, phase.Body.String())
	}

	// The once-only reveal never renders again: later views carry no
	// token section (the share link keeps working, as after creation).
	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, newCookies...).Body.String()
	if strings.Contains(shell, `id="facilitator-token-value"`) {
		t.Error("board shell must not render the one-time token again after regeneration")
	}
	if !strings.Contains(shell, "?fac="+newRaw) {
		t.Error("board shell must keep offering the working facilitator share link")
	}
}

func TestRegenerateRejectsNonFacilitators(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	ana := joinAs(t, fix, bid, "ana")

	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, regenerateTarget(bid), nil),
		http.StatusForbidden)
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, regenerateTarget(bid), nil, ana...),
		http.StatusForbidden)
	bogus := &http.Cookie{Name: facCookieName, Value: "bogus.payload"}
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, regenerateTarget(bid), nil, bogus),
		http.StatusForbidden)

	// A facilitator token from another board grants nothing here.
	_, otherCookies := createBoard(t, fix.mux, url.Values{
		"name":         {"Other board"},
		"display_name": {"Other"},
		"template":     {"plus-delta"},
	})
	assertFlashOnly(t,
		doRequest(t, fix.mux, http.MethodPost, regenerateTarget(bid), nil,
			withCookies(otherCookies, facCookieName)...),
		http.StatusForbidden)

	if n := fix.publisher.count(); n != 0 {
		t.Errorf("rejected regenerations published %d events, want none", n)
	}

	missing := doRequest(t, fix.mux, http.MethodPost, regenerateTarget("does-not-exist"), nil,
		withCookies(creatorCookies, facCookieName)...)
	if missing.Code != http.StatusNotFound {
		t.Errorf("regenerate on unknown board status = %d, want 404", missing.Code)
	}
}

func TestRegenerateLoserKeepsNothing(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)

	first := doRequest(t, fix.mux, http.MethodPost, regenerateTarget(bid), nil, fac...)
	if first.Code != http.StatusOK {
		t.Fatalf("first regenerate status = %d, want 200", first.Code)
	}
	// The winner's old cookie now presents a dead token: the second
	// rotation is forbidden and shows nothing new.
	second := doRequest(t, fix.mux, http.MethodPost, regenerateTarget(bid), nil, fac...)
	assertFlashOnly(t, second, http.StatusForbidden)
	if strings.Contains(second.Body.String(), `id="facilitator-token-value"`) {
		t.Error("losing rotation must not render a token")
	}
}

func TestFacilitatorPanelOffersRegenerate(t *testing.T) {
	fix := openFixture(t)
	bid, creatorCookies := createBoard(t, fix.mux, defaultCreateForm())
	fac := withCookies(creatorCookies, facCookieName, partsCookieName)
	ana := joinAs(t, fix, bid, "ana")

	shell := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, fac...).Body.String()
	for _, want := range []string{
		"Regenerate facilitator token",
		"/boards/" + bid + "/regenerate",
		`id="facilitator-token-slot"`,
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("facilitator shell missing %q", want)
		}
	}

	plain := doRequest(t, fix.mux, http.MethodGet, "/b/"+bid, nil, ana...).Body.String()
	if strings.Contains(plain, "/regenerate") {
		t.Error("participant shell must not offer token regeneration")
	}
}
