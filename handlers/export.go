package handlers

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"hindsight/db"
)

// Export serves the board as a Markdown download: columns with their
// cards ordered by votes (vote counts inline), threaded remarks, the
// kudos wall, and open plus done actions. Grouped cards render under
// their leader with each member's own vote count. Unknown boards are
// a plain 404. Blind collection stays blind here too: facilitators
// always read the full board, while other viewers are admitted only
// once cards are revealed; strangers without a participant row get
// the join hint instead.
func (b *Boards) Export(w http.ResponseWriter, r *http.Request) {
	bid := r.PathValue("bid")
	board, err := b.store.GetBoardByPublicID(r.Context(), bid)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load board", http.StatusInternalServerError)
		return
	}
	_, isFacilitator := b.facilitatorToken(r, board)
	if !isFacilitator {
		if b.participant(r, board) == nil {
			writeFlash(w, http.StatusForbidden, "Join the board first.")
			return
		}
		if board.CardsHidden {
			writeFlash(w, http.StatusForbidden, "Cards are still hidden — export opens after the reveal.")
			return
		}
	}
	doc, err := b.exportDocument(r, board)
	if err != nil {
		http.Error(w, "build export", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportFilename(board.Name)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(doc))
}

// exportDocument loads every wall of a board and renders the download.
// Columns arrive in display order; cards sort by vote total inside
// each column with the id breaking ties, so equal counts stay stable
// across runs.
func (b *Boards) exportDocument(r *http.Request, board *db.Board) (string, error) {
	ctx := r.Context()
	columns, err := b.store.ListColumns(ctx, board.ID)
	if err != nil {
		return "", err
	}
	sums, err := b.store.GroupVoteSums(ctx, board.ID)
	if err != nil {
		return "", err
	}
	comments, err := b.store.ListCommentsByBoard(ctx, board.ID)
	if err != nil {
		return "", err
	}
	kudos, err := b.store.ListKudos(ctx, board.ID)
	if err != nil {
		return "", err
	}
	actions, err := b.store.ListActions(ctx, board.ID)
	if err != nil {
		return "", err
	}
	byColumn, err := b.store.ListCardsByBoard(ctx, board.ID)
	if err != nil {
		return "", err
	}

	var doc strings.Builder
	doc.WriteString("# " + oneLine(board.Name) + "\n\n")
	if strings.TrimSpace(board.Context) != "" {
		doc.WriteString(oneLine(board.Context) + "\n\n")
	}
	for _, column := range columns {
		doc.WriteString("## " + oneLine(column.Title) + "\n\n")
		doc.WriteString(renderExportColumn(byColumn[column.ID], sums, comments))
		doc.WriteString("\n")
	}
	doc.WriteString("## Kudos\n\n")
	if len(kudos) == 0 {
		doc.WriteString("No kudos yet.\n\n")
	} else {
		for _, kudo := range kudos {
			doc.WriteString("- To " + oneLine(kudo.To) + ": " + oneLine(kudo.Body) +
				" (from " + displayName(kudo.From) + ")\n")
		}
		doc.WriteString("\n")
	}
	doc.WriteString("## Action items\n\n### Open\n\n")
	writeExportActions(&doc, actions, false)
	doc.WriteString("### Done\n\n")
	writeExportActions(&doc, actions, true)
	return strings.TrimRight(doc.String(), "\n") + "\n", nil
}

// renderExportColumn renders one column's cards by vote total,
// descending, with the id breaking ties. Leaders carry their members
// (own votes each) plus the summed total; members whose leader sits
// elsewhere fall back to top level so no card ever vanishes. Remarks
// follow their card one indent deeper.
func renderExportColumn(cards []db.Card, sums map[int64]int, comments map[int64][]db.Comment) string {
	byID := make(map[int64]db.Card, len(cards))
	for _, card := range cards {
		byID[card.ID] = card
	}
	membersOf := map[int64][]db.Card{}
	var tops []db.Card
	for _, card := range cards {
		if card.GroupID != nil {
			if _, ok := byID[*card.GroupID]; ok {
				membersOf[*card.GroupID] = append(membersOf[*card.GroupID], card)
				continue
			}
		}
		tops = append(tops, card)
	}
	sort.Slice(tops, func(i, j int) bool {
		if exportTotal(tops[i], sums) != exportTotal(tops[j], sums) {
			return exportTotal(tops[i], sums) > exportTotal(tops[j], sums)
		}
		return tops[i].ID < tops[j].ID
	})
	if len(tops) == 0 {
		return "No cards yet.\n"
	}
	var out strings.Builder
	for _, top := range tops {
		members := membersOf[top.ID]
		sort.Slice(members, func(i, j int) bool {
			if members[i].Votes != members[j].Votes {
				return members[i].Votes > members[j].Votes
			}
			return members[i].ID < members[j].ID
		})
		if len(members) == 0 {
			out.WriteString("- " + oneLine(top.Body) + " (" + voteLabel(top.Votes) +
				", by " + displayName(top.AuthorName) + discussedSuffix(top.Discussed) + ")\n")
		} else {
			out.WriteString("- " + oneLine(top.Body) + " (" + totalLabel(exportTotal(top, sums)) +
				", by " + displayName(top.AuthorName) + discussedSuffix(top.Discussed) + ")\n")
			for _, member := range members {
				out.WriteString("  - " + oneLine(member.Body) + " (" + voteLabel(member.Votes) +
					", by " + displayName(member.AuthorName) + discussedSuffix(member.Discussed) + ")\n")
				writeExportComments(&out, comments[member.ID], "    - ")
			}
		}
		writeExportComments(&out, comments[top.ID], "  - ")
	}
	return out.String()
}

// exportTotal ranks a card by its group sum when it leads members,
// else by its own votes.
func exportTotal(card db.Card, sums map[int64]int) int {
	if sum, ok := sums[card.ID]; ok {
		return sum
	}
	return card.Votes
}

// writeExportComments renders one card's thread in insertion order.
func writeExportComments(out *strings.Builder, comments []db.Comment, prefix string) {
	for _, comment := range comments {
		out.WriteString(prefix + oneLine(comment.Body) + " (by " + displayName(comment.AuthorName) + ")\n")
	}
}

// writeExportActions renders one action-items section: open items as
// unchecked boxes, done items as checked ones, each in id order with
// owners and carry-over provenance where present.
func writeExportActions(doc *strings.Builder, actions []db.Action, done bool) {
	empty := true
	for _, action := range actions {
		if action.Done != done {
			continue
		}
		empty = false
		box := "- [ ] "
		if done {
			box = "- [x] "
		}
		line := box + oneLine(action.Text)
		if strings.TrimSpace(action.Owner) != "" {
			line += " (owner: " + oneLine(action.Owner) + ")"
		}
		if action.CarriedFrom != nil {
			line += " (carried over)"
		}
		doc.WriteString(line + "\n")
	}
	if empty {
		if done {
			doc.WriteString("No completed actions.\n")
		} else {
			doc.WriteString("No open actions.\n")
		}
	}
	doc.WriteString("\n")
}

// voteLabel counts one card's own votes; totalLabel counts a leader's
// summed group votes.
func voteLabel(n int) string {
	switch n {
	case 0:
		return "no votes"
	case 1:
		return "1 vote"
	default:
		return itoaVote(n) + " votes"
	}
}

func totalLabel(n int) string {
	switch n {
	case 0:
		return "no votes total"
	case 1:
		return "1 vote total"
	default:
		return itoaVote(n) + " votes total"
	}
}

func discussedSuffix(discussed bool) string {
	if discussed {
		return ", discussed"
	}
	return ""
}

// displayName falls back to Anonymous for blank authors, matching the
// board's own rendering.
func displayName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "Anonymous"
	}
	return oneLine(name)
}

// oneLine collapses embedded line breaks so one card, remark, or wall
// entry always renders as one Markdown list item.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\n", " ")
}

// exportFilename slugs a board name into a safe download filename,
// falling back to a fixed name when nothing slug-safe remains.
func exportFilename(name string) string {
	lowered := strings.ToLower(name)
	var slug strings.Builder
	hyphen := true
	for _, r := range lowered {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			slug.WriteRune(r)
			hyphen = false
		} else if !hyphen {
			slug.WriteByte('-')
			hyphen = true
		}
	}
	out := strings.Trim(slug.String(), "-")
	if len(out) > 50 {
		out = strings.TrimRight(out[:50], "-")
	}
	if out == "" {
		out = "board"
	}
	return out + ".md"
}

// itoaVote renders a non-negative vote count without importing number
// formatting into the handler layer.
func itoaVote(n int) string {
	if n <= 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
