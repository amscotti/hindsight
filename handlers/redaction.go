package handlers

import (
	"net/http"
	"strings"

	"hindsight/db"
	"hindsight/realtime"
	"hindsight/templates"
)

// redactedBody is the placeholder non-facilitators see while blind
// collection hides card bodies. Ids and positions stay real so morph
// swaps and drop targets keep working; bodies, authors, and vote
// counts stay hidden until the flag clears.
const redactedBody = "•••"

// fullEventSuffix marks the facilitator-full variant of a card-carrying
// broadcast. Participants never receive suffixed events: the stream
// drops them. Facilitators receive the variant renamed to the base
// name, so client swap targets stay identical for both roles.
const fullEventSuffix = ":full"

// seesFullCards reports whether the viewer gets unredacted card
// bodies: everyone when collection is open, facilitators only while
// blind collection hides cards.
func (b *Boards) seesFullCards(r *http.Request, board *db.Board) bool {
	if !board.CardsHidden {
		return true
	}
	_, ok := b.facilitatorToken(r, board)
	return ok
}

// redactCardView hides the body, author, and vote count while keeping
// the addressing fields (id, column, position) real. Remarks render
// inside the card, so their bodies and authors redact here too while
// the visible count stays. Voted and Discussed clear as well: Voted is
// per-viewer button state that must not leak another viewer's activity
// through broadcasts, and Discussed would leak facilitation progress.
// Group members redact recursively. The flag marks the view as redacted
// so later passes apply the viewer treatment without comparing body
// text.
func redactCardView(view templates.CardView) templates.CardView {
	view.Body = redactedBody
	view.AuthorName = ""
	view.Votes = 0
	view.Voted = false
	view.Discussed = false
	for i := range view.Comments {
		view.Comments[i].Body = redactedBody
		view.Comments[i].AuthorName = ""
	}
	for i := range view.Members {
		view.Members[i] = redactCardView(view.Members[i])
	}
	view.Redacted = true
	return view
}

// redactKudoView hides a wall entry's message and sender while keeping
// the recipient visible. Commitments on the action wall never redact.
func redactKudoView(view templates.KudoView) templates.KudoView {
	view.Body = redactedBody
	view.From = ""
	return view
}

// publishPair broadcasts a card-carrying payload. While blind
// collection hides cards the viewer-facing (possibly redacted)
// fragment goes out under the base name and the full-bodies fragment
// under the suffixed variant, so the stream can filter per role.
// Otherwise a single publish carries the fragment.
func (b *Boards) publishPair(board *db.Board, name, viewerHTML, fullHTML string) error {
	if err := b.events.Publish(board.PublicID, name, viewerHTML); err != nil {
		return err
	}
	if board.CardsHidden {
		return b.events.Publish(board.PublicID, name+fullEventSuffix, fullHTML)
	}
	return nil
}

// actorVariant selects the fragment flavor for the acting client:
// full bodies for entitled viewers, the redacted flavor otherwise.
func (b *Boards) actorVariant(r *http.Request, board *db.Board, viewerHTML, fullHTML string) string {
	if b.seesFullCards(r, board) {
		return fullHTML
	}
	return viewerHTML
}

// pairedBaseName reports whether a stream event name is a
// card-carrying base that publishPair may duplicate with the full
// variant suffix. Only these bases are filtered per role; any other
// name — including one that merely ends in ":full" — passes through
// untouched.
func pairedBaseName(base string) bool {
	switch base {
	case "kudos-added", "card-updated", "vote-changed":
		return true
	}
	for _, prefix := range []string{"column-", "card-updated:", "vote-changed:"} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

// filterStreamEvent enforces blind collection on one stream message.
// Full variants are renamed to the base name for facilitators and
// dropped for everyone else; every other event passes through.
func filterStreamEvent(ev realtime.Event, facilitator bool) (realtime.Event, bool) {
	base, ok := strings.CutSuffix(ev.Name, fullEventSuffix)
	if !ok || !pairedBaseName(base) {
		return ev, true
	}
	if !facilitator {
		return realtime.Event{}, false
	}
	ev.Name = base
	return ev, true
}
