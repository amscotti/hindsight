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

// Input bounds for wall entries; longer input is rejected with 422
// plus the flash fragment. The store enforces the same bounds.
const (
	maxKudoBody = 500
	maxKudoTo   = 200
	maxKudoFrom = 100
)

func checkKudo(to, body, from string) error {
	if to == "" {
		return errors.New("Say who the kudos is for.")
	}
	if len([]rune(to)) > maxKudoTo {
		return errors.New("Kudos recipient is too long (200 characters max).")
	}
	if body == "" {
		return errors.New("Write the kudos message first.")
	}
	if len([]rune(body)) > maxKudoBody {
		return errors.New("Kudos message is too long (500 characters max).")
	}
	if len([]rune(from)) > maxKudoFrom {
		return errors.New("Kudos sender name is too long (100 characters max).")
	}
	return nil
}

// CreateKudo pins an entry to a board's kudos wall. The writer must
// hold a participant cookie; strangers get 403 plus the flash
// fragment and unknown boards are plain 404s. An omitted sender
// defaults to the writer's display name, and an explicit sender must
// match it exactly (anything else is 422, never a silent rewrite).
// The commit lands first, then
// the wall broadcast goes out, and only then does the actor get the
// same wall fragment back: both sides swap the list in whole, so the
// actor converges on one entry no matter which answer wins the race.
// While blind collection hides cards, the broadcast pairs the viewer
// flavor (messages redacted, recipients visible) with the
// full-bodies variant and the actor gets its own visibility.
func (b *Boards) CreateKudo(w http.ResponseWriter, r *http.Request) {
	board, participant, ok := b.participantBoard(w, r, r.PathValue("bid"))
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	to := strings.TrimSpace(r.Form.Get("to"))
	body := strings.TrimSpace(r.Form.Get("body"))
	// Authorship pins to the writer's display name, like card and
	// comment authors: an omitted sender defaults to it, and an
	// explicit one must match it exactly instead of being rewritten.
	from := strings.TrimSpace(r.Form.Get("from"))
	if from == "" {
		from = participant.Name
	} else if from != participant.Name {
		writeFlash(w, http.StatusUnprocessableEntity, "Kudos sender must be your display name.")
		return
	}
	if err := checkKudo(to, body, from); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if _, err := b.store.CreateKudo(r.Context(), board.ID, to, body, from); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, db.ErrInvalidInput) {
			writeFlash(w, http.StatusUnprocessableEntity, "Could not add the kudos.")
			return
		}
		http.Error(w, "add kudos", http.StatusInternalServerError)
		return
	}
	viewerHTML, fullHTML, err := b.renderKudosListPair(r, board)
	if err != nil {
		writeSyncFailure(w)
		return
	}
	if err := b.publishPair(board, "kudos-added", viewerHTML, fullHTML); err != nil {
		writeSyncFailure(w, b.actorVariant(r, board, viewerHTML, fullHTML))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.actorVariant(r, board, viewerHTML, fullHTML)))
}

// DeleteKudo removes one wall entry. The author may remove their own
// entry and the facilitator may remove anyone's; everyone else gets
// 403 plus the flash fragment. The broadcast carries the out-of-band
// delete node so viewers drop the entry wherever it sits.
func (b *Boards) DeleteKudo(w http.ResponseWriter, r *http.Request) {
	kudoID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("kid")), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	kudo, err := b.store.GetKudo(r.Context(), kudoID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load kudos", http.StatusInternalServerError)
		return
	}
	board, err := b.store.GetBoardByID(r.Context(), kudo.BoardID)
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
	if !isFacilitator && (participant == nil || participant.Name != kudo.From) {
		writeFlash(w, http.StatusForbidden, "Only the author or the facilitator can delete these kudos.")
		return
	}
	deleted, err := b.store.DeleteKudo(r.Context(), kudo.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "delete kudos", http.StatusInternalServerError)
		return
	}
	node := `<div id="kudo-` + strconv.FormatInt(deleted.ID, 10) + `" hx-swap-oob="delete"></div>`
	if err := b.events.Publish(board.PublicID, "kudos-removed", node); err != nil {
		writeSyncFailure(w, node)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(node))
}

// kudoViews maps wall entries to render view models, redacting
// messages and senders (but never recipients) for non-facilitator
// viewers while blind collection hides cards.
func kudoViews(kudos []db.Kudo, redact bool) []templates.KudoView {
	views := make([]templates.KudoView, 0, len(kudos))
	for _, kudo := range kudos {
		view := templates.KudoView{
			ID:   kudo.ID,
			To:   kudo.To,
			Body: kudo.Body,
			From: kudo.From,
		}
		if redact {
			view = redactKudoView(view)
		}
		views = append(views, view)
	}
	return views
}

// renderKudosList renders the wall content for one visibility: the
// redacted flavor for non-facilitators while blind collection hides
// cards, full messages otherwise.
func (b *Boards) renderKudosList(r *http.Request, board *db.Board, redact bool) (string, error) {
	kudos, err := b.store.ListKudos(r.Context(), board.ID)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := templates.KudosList(kudoViews(kudos, redact)).Render(r.Context(), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderKudosListPair renders the wall twice like renderColumnPair
// does for columns: the viewer flavor and the full-messages flavor.
func (b *Boards) renderKudosListPair(r *http.Request, board *db.Board) (viewer, full string, err error) {
	full, err = b.renderKudosList(r, board, false)
	if err != nil || !board.CardsHidden {
		return full, full, err
	}
	viewer, err = b.renderKudosList(r, board, true)
	if err != nil {
		return "", "", err
	}
	return viewer, full, nil
}
