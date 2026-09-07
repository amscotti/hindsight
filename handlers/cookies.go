package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Cookie names for the two identity maps plus the single-use creation
// reveal. Values are HMAC-signed with the server secret so clients
// cannot forge board rights.
const (
	facCookieName    = "hindsight_fac"
	partsCookieName  = "hindsight_parts"
	revealCookieName = "hindsight_reveal"
)

// Bounds for signed map cookies. Cookies are attacker-controlled input,
// so verification rejects anything over maxCookieValueBytes before
// decoding, and writers cap the merged map so Set-Cookie stays well
// under browser/server header limits. A tampered cookie fails closed:
// its entries are unrecoverable (the signature covers the whole map),
// so the next write/drop starts from an empty map — effectively a
// logout for every board in that cookie.
const (
	maxCookieValueBytes = 8 * 1024
	maxCookieEntries    = 50
	maxTokenLen         = 512
	maxBoardIDLen       = 128
)

// cookieLifespan is the 1-year expiry for identity cookies.
const cookieLifespan = 365 * 24 * time.Hour

// signMap encodes a board-to-token map as base64url(json) plus a
// base64url HMAC signature over the payload.
func signMap(secret string, entries map[string]string) string {
	payload, err := json.Marshal(entries)
	if err != nil {
		payload = []byte("{}")
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	sealed := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encoded + "." + sealed
}

// verifySignedMap checks the HMAC in constant time and decodes the map.
// Any tampering, truncation, or decoding failure rejects the cookie.
func verifySignedMap(secret, value string) (map[string]string, bool) {
	if len(value) == 0 || len(value) > maxCookieValueBytes {
		return nil, false
	}
	encoded, sealed, ok := strings.Cut(value, ".")
	if !ok || encoded == "" || sealed == "" || strings.Contains(sealed, ".") {
		return nil, false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return nil, false
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return nil, false
	}
	if len(encoded) > maxCookieValueBytes {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) > maxCookieValueBytes {
		return nil, false
	}
	var entries map[string]string
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false
	}
	if entries == nil {
		entries = map[string]string{}
	}
	if len(entries) > maxCookieEntries {
		return nil, false
	}
	for k, v := range entries {
		if len(k) == 0 || len(k) > maxBoardIDLen || len(v) == 0 || len(v) > maxTokenLen {
			return nil, false
		}
	}
	return entries, true
}

// cookieSecure reports whether the request arrived over TLS, which
// decides the Secure cookie attribute. X-Forwarded-Proto is deliberately
// ignored: any direct client can spoof it, and honoring it without a
// trusted-proxy check would either set Secure over plain HTTP (silent
// logout) or clear it behind a stripping proxy (cookie over HTTP).
// Deployments behind a TLS-terminating proxy must terminate TLS at the
// proxy and forward over TLS to the app, or front the app with TLS.
func cookieSecure(r *http.Request) bool {
	return r.TLS != nil
}

// writeMapCookie merges one board entry into the named signed cookie.
// A cookie that fails verification cannot be merged (its contents are
// untrusted) so the write starts from an empty map: other boards in a
// tampered cookie are logged out. Over-long ids/tokens are dropped and
// a full map resets to the single new entry to stay under header limits.
func writeMapCookie(w http.ResponseWriter, r *http.Request, secret, name, boardID, token string, lifespan time.Duration) {
	entries := map[string]string{}
	if c, err := r.Cookie(name); err == nil {
		if valid, ok := verifySignedMap(secret, c.Value); ok {
			entries = valid
		}
	}
	if boardID == "" || len(boardID) > maxBoardIDLen || token == "" || len(token) > maxTokenLen {
		return
	}
	entries[boardID] = token
	if len(entries) > maxCookieEntries {
		entries = map[string]string{boardID: token}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    signMap(secret, entries),
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(lifespan),
		MaxAge:   int(lifespan.Seconds()),
	})
}

// dropMapEntry removes one board entry, expiring the cookie when empty.
// An unverifiable cookie fails closed: it is expired rather than
// re-signed, logging out every board in that cookie.
func dropMapEntry(w http.ResponseWriter, r *http.Request, secret, name, boardID string, lifespan time.Duration) {
	entries := map[string]string{}
	if c, err := r.Cookie(name); err == nil {
		if valid, ok := verifySignedMap(secret, c.Value); ok {
			entries = valid
		}
	}
	delete(entries, boardID)
	if len(entries) == 0 {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Path:     "/",
			HttpOnly: true,
			Secure:   cookieSecure(r),
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
			Expires:  time.Unix(0, 0).UTC(),
		})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    signMap(secret, entries),
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(lifespan),
		MaxAge:   int(lifespan.Seconds()),
	})
}

// lookupMapCookie returns the stored token for one board, if the cookie
// verifies and carries that entry.
func lookupMapCookie(r *http.Request, secret, name, boardID string) (string, bool) {
	c, err := r.Cookie(name)
	if err != nil {
		return "", false
	}
	entries, ok := verifySignedMap(secret, c.Value)
	if !ok {
		return "", false
	}
	raw, ok := entries[boardID]
	if !ok || raw == "" {
		return "", false
	}
	return raw, true
}
