package templates

import "regexp"

// columnColorAllow matches exactly the server-seeded palette shape:
// #rrggbb or #rgb. Anything else falls back to transparent so a
// future user-editable color can never become a style sink.
var columnColorAllow = regexp.MustCompile(`^#[0-9a-fA-F]{3}([0-9a-fA-F]{3})?$`)

// columnColor sanitizes a column color for the inline border style.
func columnColor(color string) string {
	if columnColorAllow.MatchString(color) {
		return color
	}
	return "transparent"
}

// cardCountLabel renders the per-column card count for the header.
func cardCountLabel(n int) string {
	if n == 1 {
		return "1 card"
	}
	return itoa(n) + " cards"
}

// CardView renders one card. Handlers map store rows into this shape at
// the boundary; templates never query the store. Voted tells the card
// whether its viewer already voted for it, so the fragment offers the
// matching control (vote or unvote). Leaders carry their nested
// Members with the summed vote count in Votes; Grouped marks a member
// so it offers an ungroup control; Discussed dims the card once the
// discuss queue moves past it. Comments render inside the card, so a
// per-card broadcast carries the whole thread.
type CardView struct {
	ID         int64
	ColumnID   int64
	Body       string
	AuthorName string
	Votes      int
	Position   int
	Voted      bool
	Discussed  bool
	Grouped    bool
	// Redacted marks a view whose body, author, and vote count were
	// replaced with placeholders for blind collection, so readers
	// never have to compare body text against the placeholder.
	Redacted bool
	Members  []CardView
	Comments []CommentView
}

// CommentView renders one remark under a card.
type CommentView struct {
	ID         int64
	Body       string
	AuthorName string
}
