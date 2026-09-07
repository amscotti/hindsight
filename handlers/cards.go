package handlers

import (
	"bytes"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"hindsight/db"
	"hindsight/templates"
)

// maxCardBody caps card text; longer input is rejected with 422 plus
// the flash fragment. The store enforces the same bound as a backstop.
const maxCardBody = 2000

func checkCardBody(body string) error {
	if body == "" {
		return errors.New("Write the card text first.")
	}
	if len([]rune(body)) > maxCardBody {
		return errors.New("Card text is too long (2000 characters max).")
	}
	return nil
}

// CreateCard appends a card to a board column. The writer must hold a
// participant cookie and the board must accept new cards; strangers and
// locked boards get 403 plus the flash fragment. Unknown boards and
// columns are plain 404s. The commit lands first, then the column
// broadcast goes out, and only then does the acting client get the same
// column fragment back: both sides morph by node id, so the actor
// converges on one card no matter which answer wins the race. While
// blind collection hides cards, the broadcast pairs the viewer flavor
// with the full-bodies variant and the actor gets its own visibility.
func (b *Boards) CreateCard(w http.ResponseWriter, r *http.Request) {
	board, participant, ok := b.participantBoard(w, r, r.PathValue("bid"))
	if !ok {
		return
	}
	if board.CardsLocked {
		writeFlash(w, http.StatusForbidden, "Adding cards is locked.")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	columnID, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get("column_id")), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	column, err := b.store.GetColumn(r.Context(), columnID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load column", http.StatusInternalServerError)
		return
	}
	if column.BoardID != board.ID {
		http.NotFound(w, r)
		return
	}
	body := strings.TrimSpace(r.Form.Get("body"))
	if err := checkCardBody(body); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if _, err := b.store.CreateCard(r.Context(), column.ID, body, participant.Name); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, db.ErrInvalidInput) {
			writeFlash(w, http.StatusUnprocessableEntity, "Write the card text first.")
			return
		}
		http.Error(w, "add card", http.StatusInternalServerError)
		return
	}
	voted, err := b.votedSet(r.Context(), participant)
	if err != nil {
		http.Error(w, "load votes", http.StatusInternalServerError)
		return
	}
	viewerHTML, fullHTML, err := b.renderColumnPair(r, board, column, voted)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "render column", http.StatusInternalServerError)
		return
	}
	if err := b.publishPair(board,
		"column-"+strconv.FormatInt(column.ID, 10), viewerHTML, fullHTML); err != nil {
		writeSyncFailure(w, b.actorVariant(r, board, viewerHTML, fullHTML))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.actorVariant(r, board, viewerHTML, fullHTML)))
}

// UpdateCard edits a card's text, moves it, or both. Any participant
// may edit or move; strangers get 403 plus the flash fragment. A move
// across columns refreshes both affected columns so every viewer sees
// the card leave and land; a reorder within one column refreshes that
// column so shifted siblings converge; a text-only edit morphs the card
// node in place through the per-card event. A same-column request
// without a position leaves the order alone. A drop may carry the slot
// seen at drag start (from_column_id plus from_position); when a remote
// update moved the card meanwhile the drop is rejected with 409 plus
// the fresh column and a retry hint, persisting and publishing nothing.
// Unknown ids are plain 404s.
func (b *Boards) UpdateCard(w http.ResponseWriter, r *http.Request) {
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
	voted, err := b.votedSet(r.Context(), participant)
	if err != nil {
		http.Error(w, "load votes", http.StatusInternalServerError)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	var newBody *string
	if _, present := r.Form["body"]; present {
		trimmed := strings.TrimSpace(r.Form.Get("body"))
		if err := checkCardBody(trimmed); err != nil {
			writeFlash(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		newBody = &trimmed
	}
	// Grouping arrives on the same endpoint: a present group_id nests
	// the card under the leader, an empty one ungroups it, and an
	// absent one leaves grouping alone.
	var groupLeader *int64
	groupGiven := false
	if _, present := r.Form["group_id"]; present {
		groupGiven = true
		if raw := strings.TrimSpace(r.Form.Get("group_id")); raw != "" {
			leaderID, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				writeFlash(w, http.StatusUnprocessableEntity, "Card group is invalid.")
				return
			}
			groupLeader = &leaderID
		}
	}
	var destColumn *db.Column
	newPosition := 0
	positionGiven := false
	if raw := strings.TrimSpace(r.Form.Get("column_id")); raw != "" {
		destID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		destColumn, err = b.store.GetColumn(r.Context(), destID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "load column", http.StatusInternalServerError)
			return
		}
		if destColumn.BoardID != board.ID {
			http.NotFound(w, r)
			return
		}
		if raw := strings.TrimSpace(r.Form.Get("position")); raw != "" {
			pos, err := strconv.Atoi(raw)
			if err != nil {
				writeFlash(w, http.StatusUnprocessableEntity, "Card position must be a number.")
				return
			}
			newPosition = pos
			positionGiven = true
		}
	}
	// A drop carries the slot seen when the drag started, so a remote
	// update landing mid-drag is detected instead of silently
	// overwriting it. Both halves are required together; anything
	// else is a broken client, not a drop.
	expectMove := false
	var expectedColumn int64
	var expectedPosition int
	if destColumn != nil {
		rawColumn, rawPosition := strings.TrimSpace(r.Form.Get("from_column_id")), strings.TrimSpace(r.Form.Get("from_position"))
		if rawColumn != "" || rawPosition != "" {
			fromColumn, colErr := strconv.ParseInt(rawColumn, 10, 64)
			fromPosition, posErr := strconv.Atoi(rawPosition)
			if rawColumn == "" || rawPosition == "" || colErr != nil || posErr != nil || fromColumn <= 0 || fromPosition < 0 {
				writeFlash(w, http.StatusUnprocessableEntity, "Card drag context is invalid; reload and try again.")
				return
			}
			expectMove, expectedColumn, expectedPosition = true, fromColumn, fromPosition
		}
	}
	// A same-column request without a destination slot is not a move:
	// the order stays as it is instead of sliding to the front.
	if destColumn != nil && destColumn.ID == card.ColumnID && !positionGiven {
		destColumn = nil
		expectMove = false
	}
	if newBody == nil && destColumn == nil && !groupGiven {
		writeFlash(w, http.StatusUnprocessableEntity, "Nothing to update.")
		return
	}
	oldColumnID := card.ColumnID
	// The stale-checked move runs before any group or body edit so a
	// 409 rejects the whole request before anything persists.
	movedColumns := false
	movedEarly := false
	if destColumn != nil && expectMove {
		if err := b.store.MoveCardExpected(r.Context(), card.ID, destColumn.ID, newPosition, expectedColumn, expectedPosition); err != nil {
			if errors.Is(err, db.ErrStalePosition) {
				b.writeStaleDrop(w, r, board, destColumn, voted)
				return
			}
			if errors.Is(err, db.ErrNotFound) || errors.Is(err, db.ErrCrossColumnBoard) {
				http.NotFound(w, r)
				return
			}
			if errors.Is(err, db.ErrInvalidInput) {
				writeFlash(w, http.StatusUnprocessableEntity, "Card position is invalid.")
				return
			}
			http.Error(w, "move card", http.StatusInternalServerError)
			return
		}
		movedColumns = destColumn.ID != oldColumnID
		movedEarly = true
		fresh, err := b.store.GetCard(r.Context(), card.ID)
		if err != nil {
			http.Error(w, "load card", http.StatusInternalServerError)
			return
		}
		card = fresh
	}
	// Columns whose rendering the regrouping changes: the card's own
	// plus the old and new leaders' homes, so nested members and summed
	// counts converge everywhere.
	var oldLeaderColumn, newLeaderColumn int64
	if groupGiven {
		if card.GroupID != nil {
			if oldLeader, err := b.store.GetCard(r.Context(), *card.GroupID); err == nil {
				oldLeaderColumn = oldLeader.ColumnID
			}
		}
		grouped, err := b.store.SetGroup(r.Context(), card.ID, groupLeader)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if errors.Is(err, db.ErrInvalidInput) {
				writeFlash(w, http.StatusUnprocessableEntity, "Cards can only group under another card on this board.")
				return
			}
			http.Error(w, "group card", http.StatusInternalServerError)
			return
		}
		card = grouped
		// The resolved leader's home refreshes: a requested member
		// may resolve to a leader in another column.
		if card.GroupID != nil {
			if newLeader, err := b.store.GetCard(r.Context(), *card.GroupID); err == nil {
				newLeaderColumn = newLeader.ColumnID
			}
		}
	}
	if newBody != nil {
		updated, err := b.store.UpdateCardBody(r.Context(), card.ID, *newBody)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if errors.Is(err, db.ErrInvalidInput) {
				writeFlash(w, http.StatusUnprocessableEntity, "Write the card text first.")
				return
			}
			http.Error(w, "edit card", http.StatusInternalServerError)
			return
		}
		card = updated
	}
	if destColumn != nil && !movedEarly {
		var moveErr error
		if expectMove {
			moveErr = b.store.MoveCardExpected(r.Context(), card.ID, destColumn.ID, newPosition, expectedColumn, expectedPosition)
		} else {
			moveErr = b.store.MoveCard(r.Context(), card.ID, destColumn.ID, newPosition)
		}
		if moveErr != nil {
			if errors.Is(moveErr, db.ErrStalePosition) {
				b.writeStaleDrop(w, r, board, destColumn, voted)
				return
			}
			if errors.Is(moveErr, db.ErrNotFound) || errors.Is(moveErr, db.ErrCrossColumnBoard) {
				http.NotFound(w, r)
				return
			}
			if errors.Is(moveErr, db.ErrInvalidInput) {
				writeFlash(w, http.StatusUnprocessableEntity, "Card position is invalid.")
				return
			}
			http.Error(w, "move card", http.StatusInternalServerError)
			return
		}
		movedColumns = destColumn.ID != oldColumnID
		fresh, err := b.store.GetCard(r.Context(), card.ID)
		if err != nil {
			http.Error(w, "load card", http.StatusInternalServerError)
			return
		}
		card = fresh
	}
	if groupGiven {
		// Regrouping renests members and retotals leaders, so every
		// touched column refreshes at once: the card's home (after any
		// move in the same request), its pre-move home, and both
		// leaders' homes. The actor gets its own column fragment back.
		seen := map[int64]bool{}
		var refresh []int64
		for _, id := range []int64{oldColumnID, card.ColumnID, oldLeaderColumn, newLeaderColumn} {
			if id == 0 || seen[id] {
				continue
			}
			seen[id] = true
			refresh = append(refresh, id)
		}
		var actorPartials []string
		var cardViewer, cardFull string
		for _, id := range refresh {
			column, err := b.store.GetColumn(r.Context(), id)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					http.NotFound(w, r)
					return
				}
				http.Error(w, "load column", http.StatusInternalServerError)
				return
			}
			viewerHTML, fullHTML, err := b.renderColumnPair(r, board, column, voted)
			if err != nil {
				http.Error(w, "render column", http.StatusInternalServerError)
				return
			}
			if id == card.ColumnID {
				cardViewer, cardFull = viewerHTML, fullHTML
			}
			if err := b.publishPair(board,
				"column-"+strconv.FormatInt(column.ID, 10), viewerHTML, fullHTML); err != nil {
				actorPartials = append(actorPartials, b.actorVariant(r, board, viewerHTML, fullHTML))
			}
		}
		if len(actorPartials) > 0 {
			writeSyncFailure(w, actorPartials...)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(b.actorVariant(r, board, cardViewer, cardFull)))
		return
	}
	if movedColumns {
		oldColumn, err := b.store.GetColumn(r.Context(), oldColumnID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "load column", http.StatusInternalServerError)
			return
		}
		oldViewer, oldFull, err := b.renderColumnPair(r, board, oldColumn, voted)
		if err != nil {
			http.Error(w, "render column", http.StatusInternalServerError)
			return
		}
		newViewer, newFull, err := b.renderColumnPair(r, board, destColumn, voted)
		if err != nil {
			http.Error(w, "render column", http.StatusInternalServerError)
			return
		}
		if err := b.publishPair(board,
			"column-"+strconv.FormatInt(oldColumn.ID, 10), oldViewer, oldFull); err != nil {
			writeSyncFailure(w,
				b.actorVariant(r, board, oldViewer, oldFull),
				b.actorVariant(r, board, newViewer, newFull))
			return
		}
		if err := b.publishPair(board,
			"column-"+strconv.FormatInt(destColumn.ID, 10), newViewer, newFull); err != nil {
			writeSyncFailure(w,
				b.actorVariant(r, board, oldViewer, oldFull),
				b.actorVariant(r, board, newViewer, newFull))
			return
		}
	} else if destColumn != nil {
		// Same-column reorder: siblings shifted, so viewers need the
		// whole column — a per-card morph cannot move its neighbors.
		// The actor gets the same column fragment back.
		viewerHTML, fullHTML, err := b.renderColumnPair(r, board, destColumn, voted)
		if err != nil {
			http.Error(w, "render column", http.StatusInternalServerError)
			return
		}
		if err := b.publishPair(board,
			"column-"+strconv.FormatInt(destColumn.ID, 10), viewerHTML, fullHTML); err != nil {
			writeSyncFailure(w, b.actorVariant(r, board, viewerHTML, fullHTML))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(b.actorVariant(r, board, viewerHTML, fullHTML)))
		return
	} else {
		viewerHTML, fullHTML, err := b.renderCardPair(r, board, card, voted)
		if err != nil {
			http.Error(w, "render card", http.StatusInternalServerError)
			return
		}
		if err := b.publishPair(board,
			"card-updated:"+strconv.FormatInt(card.ID, 10), viewerHTML, fullHTML); err != nil {
			writeSyncFailure(w, b.actorVariant(r, board, viewerHTML, fullHTML))
			return
		}
	}
	viewerHTML, fullHTML, err := b.renderCardPair(r, board, card, voted)
	if err != nil {
		http.Error(w, "render card", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.actorVariant(r, board, viewerHTML, fullHTML)))
}

// staleDropMessage is the retry hint for a drop rejected on a stale
// slot: nothing moved, and the fresh column fragment below already
// shows the current order.
const staleDropMessage = "Someone else moved cards while you were dragging; the column was refreshed, try the drop again."

// writeStaleDrop answers a stale drop with 409 plus the fresh
// destination column in the actor's visibility, so the client refetches
// and shows the retry hint. Nothing is published: the placement never
// changed.
func (b *Boards) writeStaleDrop(w http.ResponseWriter, r *http.Request, board *db.Board, destColumn *db.Column, voted map[int64]bool) {
	viewerHTML, fullHTML, err := b.renderColumnPair(r, board, destColumn, voted)
	if err != nil {
		http.Error(w, "render column", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusConflict)
	_, _ = w.Write([]byte(b.actorVariant(r, board, viewerHTML, fullHTML)))
	_, _ = w.Write([]byte(`<div id="flash" hx-swap-oob="true" class="flash-error">` + html.EscapeString(staleDropMessage) + `</div>`))
}

// DeleteCard removes a card. The author may remove their own card and
// the facilitator may remove anyone's; everyone else gets 403 plus the
// flash fragment. The broadcast carries the out-of-band delete node so
// viewers drop the card wherever it sits, plus a refresh of every
// column a grouped delete changes: deleting a leader promotes its
// members into place, and deleting a member retotals its leader, so
// those columns refresh through the redaction-aware pair path.
func (b *Boards) DeleteCard(w http.ResponseWriter, r *http.Request) {
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
	_, isFacilitator := b.facilitatorToken(r, board)
	if !isFacilitator && (participant == nil || participant.Name != card.AuthorName) {
		writeFlash(w, http.StatusForbidden, "Only the author or the facilitator can delete this card.")
		return
	}
	deleted, affected, err := b.store.DeleteCard(r.Context(), card.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "delete card", http.StatusInternalServerError)
		return
	}
	node := `<div id="card-` + strconv.FormatInt(deleted.ID, 10) + `" hx-swap-oob="delete"></div>`
	// Column refreshes publish before the OOB delete node so a partial
	// publish failure leaves the actor with the refreshed columns and
	// the delete last (at-most-once ordering, no strand on half success).
	if len(affected) > 0 {
		voted, err := b.votedSet(r.Context(), participant)
		if err != nil {
			http.Error(w, "load votes", http.StatusInternalServerError)
			return
		}
		var actorPartials []string
		failed := false
		for _, id := range affected {
			column, err := b.store.GetColumn(r.Context(), id)
			if err != nil {
				if errors.Is(err, db.ErrNotFound) {
					http.NotFound(w, r)
					return
				}
				http.Error(w, "load column", http.StatusInternalServerError)
				return
			}
			viewerHTML, fullHTML, err := b.renderColumnPair(r, board, column, voted)
			if err != nil {
				http.Error(w, "render column", http.StatusInternalServerError)
				return
			}
			if err := b.publishPair(board,
				"column-"+strconv.FormatInt(column.ID, 10), viewerHTML, fullHTML); err != nil {
				failed = true
			}
			actorPartials = append(actorPartials, b.actorVariant(r, board, viewerHTML, fullHTML))
		}
		if failed {
			writeSyncFailure(w, append([]string{node}, actorPartials...)...)
			return
		}
	}
	if err := b.events.Publish(board.PublicID,
		"card-removed:"+strconv.FormatInt(deleted.ID, 10), node); err != nil {
		if len(affected) > 0 {
			voted, err := b.votedSet(r.Context(), participant)
			if err != nil {
				http.Error(w, "load votes", http.StatusInternalServerError)
				return
			}
			var actorPartials []string
			for _, id := range affected {
				column, err := b.store.GetColumn(r.Context(), id)
				if err != nil {
					http.Error(w, "load column", http.StatusInternalServerError)
					return
				}
				viewerHTML, fullHTML, err := b.renderColumnPair(r, board, column, voted)
				if err != nil {
					http.Error(w, "render column", http.StatusInternalServerError)
					return
				}
				actorPartials = append(actorPartials, b.actorVariant(r, board, viewerHTML, fullHTML))
			}
			writeSyncFailure(w, append([]string{node}, actorPartials...)...)
			return
		}
		writeSyncFailure(w, node)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(node))
}

// participantBoard loads the board behind a card-writing route and
// requires a participant cookie. Unknown boards are plain 404s;
// visitors without a participant row get 403 plus the flash fragment.
func (b *Boards) participantBoard(w http.ResponseWriter, r *http.Request, publicID string) (*db.Board, *db.Participant, bool) {
	board, err := b.store.GetBoardByPublicID(r.Context(), publicID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return nil, nil, false
		}
		http.Error(w, "load board", http.StatusInternalServerError)
		return nil, nil, false
	}
	participant := b.participant(r, board)
	if participant == nil {
		writeFlash(w, http.StatusForbidden, "Join the board first.")
		return nil, nil, false
	}
	return board, participant, true
}

// renderColumn renders one column with its current cards for a column
// broadcast. The voted set marks each card's control for the acting
// viewer; redact swaps bodies, authors, and vote counts for
// placeholders for non-facilitator viewers. Grouped cards nest under
// their leader with the summed vote count, reusing the same forest as
// the shell so broadcasts and reads never diverge.
func (b *Boards) renderColumn(r *http.Request, board *db.Board, column *db.Column, voted map[int64]bool, redact bool) (string, error) {
	cards, err := b.store.ListCards(r.Context(), column.ID)
	if err != nil {
		return "", err
	}
	sums, err := b.store.GroupVoteSums(r.Context(), board.ID)
	if err != nil {
		return "", err
	}
	comments, err := b.store.ListCommentsByBoard(r.Context(), board.ID)
	if err != nil {
		return "", err
	}
	view := templates.ColumnView{
		ID:           column.ID,
		Title:        column.Title,
		Color:        column.Color,
		Position:     column.Position,
		VotingLocked: board.VotingLocked,
		CardsLocked:  board.CardsLocked,
		Cards:        cardForest(cards, voted, sums, comments, redact),
	}
	var buf bytes.Buffer
	if err := templates.Column(board.PublicID, view).Render(r.Context(), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderColumnPair renders a column twice: the viewer flavor (redacted
// for non-facilitators while blind collection hides cards) and the
// full-bodies flavor backing the facilitator broadcast variant. When
// collection is open both flavors are identical and render once.
func (b *Boards) renderColumnPair(r *http.Request, board *db.Board, column *db.Column, voted map[int64]bool) (viewer, full string, err error) {
	full, err = b.renderColumn(r, board, column, voted, false)
	if err != nil || !board.CardsHidden {
		return full, full, err
	}
	viewer, err = b.renderColumn(r, board, column, voted, true)
	if err != nil {
		return "", "", err
	}
	return viewer, full, nil
}

// renderCardHTML renders one card for a per-card broadcast. The node
// comes from the same forest as the column renders, so a leader keeps
// its nested members and summed count here too. A nested member
// renders as its own node so the morph can target it in place. Redact
// swaps the body, author, and vote count for placeholders for
// non-facilitator viewers.
func (b *Boards) renderCardHTML(r *http.Request, board *db.Board, card *db.Card, voted map[int64]bool, redact bool) (string, error) {
	cards, err := b.store.ListCards(r.Context(), card.ColumnID)
	if err != nil {
		return "", err
	}
	sums, err := b.store.GroupVoteSums(r.Context(), board.ID)
	if err != nil {
		return "", err
	}
	byCard, err := b.store.ListCommentsByBoard(r.Context(), board.ID)
	if err != nil {
		return "", err
	}
	mapped := cardView(*card, voted[card.ID], byCard[card.ID])
	if sum, ok := sums[card.ID]; ok {
		mapped.Votes = sum
	}
	for _, view := range cardForest(cards, voted, sums, byCard, redact) {
		if view.ID == card.ID {
			mapped = view
			break
		}
		for _, nested := range view.Members {
			if nested.ID == card.ID {
				mapped = nested
				break
			}
		}
	}
	// cardForest already redacts when asked; a leader missing from its
	// own column render (a member grouped across columns) still needs
	// the viewer treatment.
	if redact && !mapped.Redacted {
		mapped = redactCardView(mapped)
	}
	var buf bytes.Buffer
	if err := templates.Card(mapped).Render(r.Context(), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderCardPair renders one card twice like renderColumnPair does for
// columns: the viewer flavor and the full-bodies flavor.
func (b *Boards) renderCardPair(r *http.Request, board *db.Board, card *db.Card, voted map[int64]bool) (viewer, full string, err error) {
	full, err = b.renderCardHTML(r, board, card, voted, false)
	if err != nil || !board.CardsHidden {
		return full, full, err
	}
	viewer, err = b.renderCardHTML(r, board, card, voted, true)
	if err != nil {
		return "", "", err
	}
	return viewer, full, nil
}
