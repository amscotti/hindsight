package handlers

import (
	"fmt"
	"html"
	"net/http"
)

// writeFlash answers a rejected mutation with the machine status plus
// exactly one human fragment: the out-of-band flash node the client
// swaps into place. It carries nothing else.
func writeFlash(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w,
		`<div id="flash" hx-swap-oob="true" class="flash-error">%s</div>`,
		html.EscapeString(message))
}

// syncFailureMessage is the retry hint sent when a mutation commits but
// its broadcast fails: the actor's fragment below is already fresh, and
// other viewers converge on reload.
const syncFailureMessage = "Saved, but live updates failed; reload to resync."

// writeSyncFailure answers a post-commit publish failure. The commit
// stands, so the actor gets the fresh fragment(s) with a 5xx status plus
// the retry-hint flash instead of a bare 500, converging without a
// reload. The commit-then-publish-then-respond order is unchanged.
// partials must be pre-rendered server fragments (already escaped at
// render time); they are written verbatim, so never pass raw user input
// here — only the flash message itself is escaped by this helper.
func writeSyncFailure(w http.ResponseWriter, partials ...string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusInternalServerError)
	for _, partial := range partials {
		_, _ = w.Write([]byte(partial))
	}
	_, _ = fmt.Fprintf(w,
		`<div id="flash" hx-swap-oob="true" class="flash-error">%s</div>`,
		html.EscapeString(syncFailureMessage))
}
