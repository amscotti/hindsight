package handlers

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"hindsight/db"
	"hindsight/templates"
)

// Input bounds for action items; longer input is rejected with 422
// plus the flash fragment. The store enforces the same bounds too.
const (
	maxActionText  = 500
	maxActionOwner = 100
)

// CreateAction adds an action item to a board's wall. The writer must
// hold a participant cookie and becomes the item's author; strangers
// get 403 plus the flash fragment and unknown boards are plain 404s.
// Action texts are commitments, not ideas, so they stay visible to
// everyone at all times: the broadcast carries a single flavor. The
// commit lands first, then the wall broadcast goes out, and only then
// does the actor get the same wall fragment back, converging like the
// kudos wall.
func (b *Boards) CreateAction(w http.ResponseWriter, r *http.Request) {
	board, participant, ok := b.participantBoard(w, r, r.PathValue("bid"))
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	text := strings.TrimSpace(r.Form.Get("text"))
	owner := strings.TrimSpace(r.Form.Get("owner"))
	if err := checkActionFields(&text, &owner); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if _, err := b.store.CreateAction(r.Context(), board.ID, text, owner, participant.Name, nil); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, db.ErrInvalidInput) {
			writeFlash(w, http.StatusUnprocessableEntity, "Describe the action first.")
			return
		}
		http.Error(w, "add action", http.StatusInternalServerError)
		return
	}
	html, err := b.renderActionsList(r, board)
	if err != nil {
		writeSyncFailure(w)
		return
	}
	if err := b.events.Publish(board.PublicID, "action-added", html); err != nil {
		writeSyncFailure(w, html)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

// UpdateAction edits an action item's text and owner and flips its
// done flag. The author may edit their own item and the facilitator
// may edit anyone's; everyone else gets 403 plus the flash fragment.
// Only the present fields change; unknown ids are plain 404s. The
// commit lands first, then the wall broadcast goes out carrying the
// whole list (so every viewer converges no matter which answer wins
// the race), and the actor gets the edited node back to morph in
// place.
func (b *Boards) UpdateAction(w http.ResponseWriter, r *http.Request) {
	actionID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("aid")), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	action, err := b.store.GetAction(r.Context(), actionID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load action", http.StatusInternalServerError)
		return
	}
	board, err := b.store.GetBoardByID(r.Context(), action.BoardID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load board", http.StatusInternalServerError)
		return
	}
	participant := b.participant(r, board)
	_, isFacilitator := b.facilitatorToken(r, board)
	if !isFacilitator && (participant == nil || participant.Name != action.AuthorName) {
		writeFlash(w, http.StatusForbidden, "Only the author or the facilitator can update this action.")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	var text, owner *string
	var done *bool
	if _, present := r.Form["text"]; present {
		trimmed := strings.TrimSpace(r.Form.Get("text"))
		text = &trimmed
	}
	if _, present := r.Form["owner"]; present {
		trimmed := strings.TrimSpace(r.Form.Get("owner"))
		owner = &trimmed
	}
	if _, present := r.Form["done"]; present {
		flag := isTruthy(r.Form.Get("done"))
		done = &flag
	}
	if text == nil && owner == nil && done == nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Nothing to update.")
		return
	}
	if err := checkActionFields(text, owner); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	updated, err := b.store.UpdateAction(r.Context(), action.ID, text, owner, done)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, db.ErrInvalidInput) {
			writeFlash(w, http.StatusUnprocessableEntity, "Describe the action first.")
			return
		}
		http.Error(w, "update action", http.StatusInternalServerError)
		return
	}
	node, err := renderActionNode(r, updated)
	if err != nil {
		writeSyncFailure(w)
		return
	}
	html, err := b.renderActionsList(r, board)
	if err != nil {
		writeSyncFailure(w)
		return
	}
	if err := b.events.Publish(board.PublicID, "action-updated", html); err != nil {
		writeSyncFailure(w, node)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(node))
}

// DeleteAction removes one action item. Only the facilitator may
// delete, even the author; everyone else gets 403 plus the flash
// fragment. The broadcast carries the out-of-band delete node so
// viewers drop the item wherever it sits.
func (b *Boards) DeleteAction(w http.ResponseWriter, r *http.Request) {
	actionID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("aid")), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	action, err := b.store.GetAction(r.Context(), actionID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load action", http.StatusInternalServerError)
		return
	}
	board, err := b.store.GetBoardByID(r.Context(), action.BoardID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load board", http.StatusInternalServerError)
		return
	}
	if _, ok := b.facilitatorToken(r, board); !ok {
		writeFlash(w, http.StatusForbidden, "Only the facilitator can delete this action.")
		return
	}
	deleted, err := b.store.DeleteAction(r.Context(), action.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "delete action", http.StatusInternalServerError)
		return
	}
	node := `<div id="action-` + strconv.FormatInt(deleted.ID, 10) + `" hx-swap-oob="delete"></div>`
	if err := b.events.Publish(board.PublicID, "action-removed", node); err != nil {
		writeSyncFailure(w, node)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(node))
}

// checkActionFields validates a new item's text and owner together so
// creation and edits share one bound.
func checkActionFields(text, owner *string) error {
	if text != nil {
		if *text == "" {
			return errors.New("Describe the action first.")
		}
		if len([]rune(*text)) > maxActionText {
			return errors.New("Action text is too long (500 characters max).")
		}
	}
	if owner != nil && len([]rune(*owner)) > maxActionOwner {
		return errors.New("Action owner name is too long (100 characters max).")
	}
	return nil
}

// actionViews maps store items to render view models in wall order.
func actionViews(actions []db.Action) []templates.ActionView {
	views := make([]templates.ActionView, 0, len(actions))
	for _, action := range actions {
		views = append(views, templates.ActionView{
			ID:          action.ID,
			Text:        action.Text,
			Owner:       action.Owner,
			Done:        action.Done,
			CarriedFrom: action.CarriedFrom,
		})
	}
	return views
}

// renderActionNode renders one item for the editing actor's in-place
// morph.
func renderActionNode(r *http.Request, action *db.Action) (string, error) {
	var buf bytes.Buffer
	view := templates.ActionView{
		ID:          action.ID,
		Text:        action.Text,
		Owner:       action.Owner,
		Done:        action.Done,
		CarriedFrom: action.CarriedFrom,
	}
	if err := templates.ActionItem(view).Render(r.Context(), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderActionsList renders the wall content: open and done items in
// id order.
func (b *Boards) renderActionsList(r *http.Request, board *db.Board) (string, error) {
	actions, err := b.store.ListActions(r.Context(), board.ID)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := templates.ActionsList(actionViews(actions)).Render(r.Context(), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}
