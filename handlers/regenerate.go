package handlers

import (
	"errors"
	"net/http"

	"hindsight/db"
	"hindsight/templates"
)

// Regenerate rotates the board's facilitator token. The caller proves
// the old token through the facilitator gate (constant-time hash
// compare, like every facilitator control); strangers get 403 plus the
// flash fragment and unknown boards are plain 404s. The new 128-bit
// token invalidates the old hash in one conditional write, so
// concurrent rotations mint exactly one live token and the loser keeps
// nothing to show. The fresh facilitator cookie goes out with the
// answer, whose body renders the raw token exactly once through the
// same fragment creation uses: it never renders again.
func (b *Boards) Regenerate(w http.ResponseWriter, r *http.Request) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	oldRaw, ok := b.facilitatorToken(r, board)
	if !ok {
		writeFlash(w, http.StatusForbidden, "Only the facilitator can do that.")
		return
	}
	newToken, err := db.GenerateToken()
	if err != nil {
		http.Error(w, "mint token", http.StatusInternalServerError)
		return
	}
	rotated, err := b.store.RotateFacilitatorToken(r.Context(),
		board.ID, db.HashToken(oldRaw), db.HashToken(newToken))
	if err != nil {
		if errors.Is(err, db.ErrInvalidInput) {
			http.Error(w, "regenerate token", http.StatusInternalServerError)
			return
		}
		http.Error(w, "regenerate token", http.StatusInternalServerError)
		return
	}
	if !rotated {
		writeFlash(w, http.StatusForbidden, "The facilitator token was already regenerated.")
		return
	}
	writeMapCookie(w, r, b.secret, facCookieName, board.PublicID, newToken, cookieLifespan)
	writeFragment(w, r, templates.FacilitatorTokenReveal(newToken))
}
