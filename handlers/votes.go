package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"hindsight/db"
)

// Vote records the acting participant's vote for a card. The voter needs
// a participant cookie, and the board must hold the voting window open
// (unlocked, in its voting window); anything else gets 403 plus the
// flash fragment naming the reason. Cap and double-vote violations are
// enforced inside the store transaction and surface the same way. On
// success the commit lands first, then the card broadcast goes out, and
// only then does the actor get the same card fragment back — the same
// commit, publish, respond order as every other card mutation. Unknown
// cards are plain 404s.
func (b *Boards) Vote(w http.ResponseWriter, r *http.Request) {
	b.castVote(w, r, true)
}

// Unvote removes the acting participant's vote from a card. It carries
// the same gates and the same broadcast contract as Vote; removing a
// vote the participant never cast is 403 plus the flash fragment.
func (b *Boards) Unvote(w http.ResponseWriter, r *http.Request) {
	b.castVote(w, r, false)
}

func (b *Boards) castVote(w http.ResponseWriter, r *http.Request, up bool) {
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
	if board.Phase != "vote" {
		writeFlash(w, http.StatusForbidden, "Voting is only open during the vote phase.")
		return
	}
	if board.VotingLocked {
		writeFlash(w, http.StatusForbidden, "Voting is locked.")
		return
	}
	if up {
		err = b.store.Vote(r.Context(), participant.ID, card.ID)
	} else {
		err = b.store.Unvote(r.Context(), participant.ID, card.ID)
	}
	if err != nil {
		switch {
		case errors.Is(err, db.ErrDoubleVote):
			writeFlash(w, http.StatusForbidden, "You have already voted for this card.")
		case errors.Is(err, db.ErrVoteCapExceeded):
			writeFlash(w, http.StatusForbidden,
				fmt.Sprintf("You have used all %d of your votes.", board.VotesPerPerson))
		case errors.Is(err, db.ErrNoVote):
			writeFlash(w, http.StatusForbidden, "You have not voted for this card.")
		case errors.Is(err, db.ErrBoardMismatch):
			writeFlash(w, http.StatusForbidden, "This card belongs to a different board.")
		case errors.Is(err, db.ErrVotingClosed):
			writeFlash(w, http.StatusForbidden, "Voting is closed.")
		case errors.Is(err, db.ErrNotFound):
			http.NotFound(w, r)
		default:
			if up {
				http.Error(w, "record vote", http.StatusInternalServerError)
			} else {
				http.Error(w, "remove vote", http.StatusInternalServerError)
			}
		}
		return
	}
	fresh, err := b.store.GetCard(r.Context(), card.ID)
	if err != nil {
		http.Error(w, "load card", http.StatusInternalServerError)
		return
	}
	// Re-read the board behind the card: blind collection may hide the
	// fragment this vote broadcasts and returns. An expiry noticed here
	// broadcasts before the vote event; the conditional disarm keeps it
	// to one broadcast across concurrent readers.
	board, err = b.store.GetBoardByID(r.Context(), fresh.BoardID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load board", http.StatusInternalServerError)
		return
	}
	b.checkTimerExpiry(r, board)
	participant = b.participant(r, board)
	voted, err := b.votedSet(r.Context(), participant)
	if err != nil {
		http.Error(w, "load votes", http.StatusInternalServerError)
		return
	}
	// A vote on a grouped card retotals its leader, so viewers need the
	// whole column — a per-card morph would leave the sum stale.
	if b.groupedCard(r.Context(), board, fresh) {
		column, err := b.store.GetColumn(r.Context(), fresh.ColumnID)
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
			writeSyncFailure(w, b.actorVariant(r, board, viewerHTML, fullHTML))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(b.actorVariant(r, board, viewerHTML, fullHTML)))
		return
	}
	viewerHTML, fullHTML, err := b.renderCardPair(r, board, fresh, voted)
	if err != nil {
		http.Error(w, "render card", http.StatusInternalServerError)
		return
	}
	if err := b.publishPair(board,
		"vote-changed:"+strconv.FormatInt(fresh.ID, 10), viewerHTML, fullHTML); err != nil {
		writeSyncFailure(w, b.actorVariant(r, board, viewerHTML, fullHTML))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.actorVariant(r, board, viewerHTML, fullHTML)))
}

// groupedCard reports whether a vote on the card moves a group total:
// members always do, and leaders do while they still hold members.
func (b *Boards) groupedCard(ctx context.Context, board *db.Board, card *db.Card) bool {
	if card.GroupID != nil {
		return true
	}
	sums, err := b.store.GroupVoteSums(ctx, board.ID)
	if err != nil {
		return true
	}
	_, ok := sums[card.ID]
	return ok
}
