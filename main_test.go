package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hindsight/handlers"
	"hindsight/realtime"
)

// isolateDB points the handler at a throwaway database so server tests
// never touch the working tree.
func isolateDB(t *testing.T) {
	t.Helper()

	t.Setenv("DB_PATH", filepath.Join(t.TempDir(), "hindsight.db"))
}

func mustHandler(t *testing.T) http.Handler {
	t.Helper()

	h, sqldb, err := newHandler()
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	t.Cleanup(func() {
		if err := sqldb.Close(); err != nil {
			t.Errorf("close test DB: %v", err)
		}
	})
	return h
}

func serve(t *testing.T, h http.Handler, target string) (int, string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return res.StatusCode, string(body)
}

func TestHealthzOK(t *testing.T) {
	isolateDB(t)
	h := mustHandler(t)

	code, body := serve(t, h, "/healthz")
	if code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", code, http.StatusOK)
	}
	if strings.TrimSpace(body) != "ok" {
		t.Fatalf("GET /healthz body = %q, want %q", body, "ok")
	}
}

func TestLandingRendersStyled(t *testing.T) {
	isolateDB(t)
	h := mustHandler(t)

	code, body := serve(t, h, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", code, http.StatusOK)
	}
	for _, want := range []string{
		"<main",
		"/static/fonts.css",
		"/static/app.css",
		"/static/vendor/htmx.min.js",
		"/static/vendor/sse.js",
		"/static/vendor/idiomorph-ext.min.js",
		"/static/vendor/alpine.min.js",
		"Start a new retro",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET / body missing %q", want)
		}
	}
}

func TestLandingHasZeroCDNReferences(t *testing.T) {
	isolateDB(t)
	h := mustHandler(t)

	_, body := serve(t, h, "/")
	lowered := strings.ToLower(body)
	for _, bad := range []string{"https://", "http://", "//cdn", "cdn.jsdelivr", "unpkg.com"} {
		if strings.Contains(lowered, bad) {
			t.Errorf("GET / body references external asset %q; all assets must be vendored", bad)
		}
	}
}

func TestVendorAssetsServedLocally(t *testing.T) {
	isolateDB(t)
	h := mustHandler(t)

	for _, asset := range []string{
		"/static/vendor/htmx.min.js",
		"/static/vendor/sse.js",
		"/static/vendor/idiomorph-ext.min.js",
		"/static/vendor/alpine.min.js",
		"/static/fonts.css",
		"/static/app.css",
	} {
		code, body := serve(t, h, asset)
		if code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", asset, code, http.StatusOK)
			continue
		}
		if len(body) == 0 {
			t.Errorf("GET %s returned empty body", asset)
		}
	}
}

func TestStaticDirectoryListingIsDisabled(t *testing.T) {
	isolateDB(t)
	h := mustHandler(t)

	for _, target := range []string{"/static/", "/static/vendor/"} {
		code, _ := serve(t, h, target)
		if code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want %d", target, code, http.StatusNotFound)
		}
	}
}

func TestEventsUnknownBoardIsNotFound(t *testing.T) {
	isolateDB(t)
	h := mustHandler(t)

	code, _ := serve(t, h, "/b/does-not-exist/events")
	if code != http.StatusNotFound {
		t.Fatalf("GET /b/does-not-exist/events status = %d, want %d", code, http.StatusNotFound)
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	isolateDB(t)
	h := mustHandler(t)

	code, _ := serve(t, h, "/no-such-page")
	if code != http.StatusNotFound {
		t.Fatalf("GET /no-such-page status = %d, want %d", code, http.StatusNotFound)
	}
}

// TestWiredPublisherReachesLiveBrokerSubscriber proves the publisher the
// board routes are wired with delivers to live broker subscribers: it
// publishes through the handlers.Publisher seam (the same adapter value
// openBoards hands to NewBoards) and asserts the event arrives on a
// subscribed stream with its payload intact.
func TestWiredPublisherReachesLiveBrokerSubscriber(t *testing.T) {
	broker := realtime.NewBroker()
	var pub handlers.Publisher = brokerPublisher{broker: broker}

	ch, _ := broker.Subscribe("board-1", 0)
	defer broker.Unsubscribe("board-1", ch)

	if err := pub.Publish("board-1", "column-7", "<div>live</div>"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Name != "column-7" || ev.HTML != "<div>live</div>" {
			t.Errorf("subscriber got name=%q html=%q, want column-7 with the payload", ev.Name, ev.HTML)
		}
		if ev.ID == 0 {
			t.Error("subscriber event carries no per-board id")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("live broker subscriber received nothing published through the wired publisher")
	}
}
