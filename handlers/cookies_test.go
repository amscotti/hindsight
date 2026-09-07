package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVerifySignedMapRoundTrip(t *testing.T) {
	v := signMap("secret", map[string]string{"b1": "tok1"})
	got, ok := verifySignedMap("secret", v)
	if !ok || got["b1"] != "tok1" {
		t.Fatalf("round trip failed: %v %v", got, ok)
	}
}

func TestVerifySignedMapRejects(t *testing.T) {
	valid := signMap("secret", map[string]string{"b1": "tok1"})
	if _, ok := verifySignedMap("secret", ""); ok {
		t.Fatal("empty accepted")
	}
	if _, ok := verifySignedMap("secret", "abc"); ok {
		t.Fatal("no-dot accepted")
	}
	if _, ok := verifySignedMap("secret", ".abc"); ok {
		t.Fatal("empty half accepted")
	}
	if _, ok := verifySignedMap("secret", "!!!.!!!"); ok {
		t.Fatal("bad base64 accepted")
	}
	if _, ok := verifySignedMap("secret", valid+"x"); ok {
		t.Fatal("tampered accepted")
	}
	if _, ok := verifySignedMap("other", valid); ok {
		t.Fatal("wrong secret accepted")
	}
	if _, ok := verifySignedMap("secret", strings.Repeat("a", maxCookieValueBytes+1)); ok {
		t.Fatal("oversize accepted")
	}
	if _, ok := verifySignedMap("secret", valid+".extra"); ok {
		t.Fatal("three parts accepted")
	}
}

func TestWriteMapCookiePreservesAndTamperLogsOut(t *testing.T) {
	// valid merge preserves other boards
	v := signMap("s", map[string]string{"b1": "t1"})
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: partsCookieName, Value: v})
	w := httptest.NewRecorder()
	writeMapCookie(w, r, "s", partsCookieName, "b2", "t2", time.Hour)
	var merged map[string]string
	for _, c := range w.Result().Cookies() {
		if c.Name == partsCookieName {
			var ok bool
			merged, ok = verifySignedMap("s", c.Value)
			if !ok {
				t.Fatal("merged cookie invalid")
			}
		}
	}
	if merged["b1"] != "t1" || merged["b2"] != "t2" {
		t.Fatalf("merge lost entries: %v", merged)
	}

	// tampered cookie fails closed: next write carries only the new entry
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.AddCookie(&http.Cookie{Name: partsCookieName, Value: v + "tamper"})
	w2 := httptest.NewRecorder()
	writeMapCookie(w2, r2, "s", partsCookieName, "b2", "t2", time.Hour)
	for _, c := range w2.Result().Cookies() {
		if c.Name == partsCookieName {
			got, ok := verifySignedMap("s", c.Value)
			if !ok {
				t.Fatal("rewritten cookie invalid")
			}
			if _, has := got["b1"]; has {
				t.Fatal("tampered entry survived")
			}
			if got["b2"] != "t2" {
				t.Fatalf("new entry missing: %v", got)
			}
		}
	}
}

func TestWriteMapCookieCapsEntries(t *testing.T) {
	entries := map[string]string{}
	for i := 0; i < maxCookieEntries; i++ {
		entries["b"+string(rune('a'+i%26))+string(rune('0'+i/26))] = "tok"
	}
	v := signMap("s", entries)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: partsCookieName, Value: v})
	w := httptest.NewRecorder()
	writeMapCookie(w, r, "s", partsCookieName, "newboard", "newtok", time.Hour)
	for _, c := range w.Result().Cookies() {
		if c.Name == partsCookieName {
			got, ok := verifySignedMap("s", c.Value)
			if !ok || len(got) > maxCookieEntries {
				t.Fatalf("cap exceeded: %v %v", len(got), ok)
			}
			if got["newboard"] != "newtok" {
				t.Fatalf("new entry missing: %v", got)
			}
		}
	}
}

func TestCookieSecureIgnoresForwardedProto(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	if cookieSecure(r) {
		t.Fatal("trusted spoofable header")
	}
}
