package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"hindsight/db"
)

// maxCommentBody caps remark text; longer input is rejected with 422
// plus the flash fragment. The store enforces the same bound.
const maxCommentBody = 1000

func checkCommentBody(body string) error {
	if body == "" {
		return errors.New("Write the comment first.")
	}
	if len([]rune(body)) > maxCommentBody {
		return errors.New("Comments are too long (1000 characters max).")
	}
	return nil
}

// CreateComment appends a remark to a card. The writer must hold a
// participant cookie for the card's board; strangers and visitors
// from other boards get 403 plus the flash fragment. Unknown cards
// are plain 404s. Remarks render inside the card fragment, so the
// commit lands first, then the per-card broadcast goes out carrying
// the whole thread, and only then does the actor get the same card
// fragment back — the same commit, publish, respond order as every
// other card mutation. Blind collection redacts the thread through
// the card pair path, exactly like the card body.
func (b *Boards) CreateComment(w http.ResponseWriter, r *http.Request) {
	cardID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("cid")), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	card, err := b.store.GetCard(r.Context(), cardID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load card", http.StatusInternalServerError)
		return
	}
	board, err := b.store.GetBoardByID(r.Context(), card.BoardID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load board", http.StatusInternalServerError)
		return
	}
	participant := b.participant(r, board)
	if participant == nil {
		writeFlash(w, http.StatusForbidden, "Join the board first.")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	body := strings.TrimSpace(r.Form.Get("comment"))
	if err := checkCommentBody(body); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if _, err := b.store.CreateComment(r.Context(), card.ID, body, participant.Name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, db.ErrInvalidInput) {
			writeFlash(w, http.StatusUnprocessableEntity, "Write the comment first.")
			return
		}
		http.Error(w, "add comment", http.StatusInternalServerError)
		return
	}
	fresh, err := b.store.GetCard(r.Context(), card.ID)
	if err != nil {
		http.Error(w, "load card", http.StatusInternalServerError)
		return
	}
	voted, err := b.votedSet(r.Context(), participant)
	if err != nil {
		http.Error(w, "load votes", http.StatusInternalServerError)
		return
	}
	viewerHTML, fullHTML, err := b.renderCardPair(r, board, fresh, voted)
	if err != nil {
		http.Error(w, "render card", http.StatusInternalServerError)
		return
	}
	if err := b.publishPair(board,
		"card-updated:"+strconv.FormatInt(card.ID, 10), viewerHTML, fullHTML); err != nil {
		writeSyncFailure(w, b.actorVariant(r, board, viewerHTML, fullHTML))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.actorVariant(r, board, viewerHTML, fullHTML)))
}
