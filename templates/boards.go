package templates

import "time"

// KudoView renders one entry on the board-level kudos wall.
type KudoView struct {
	ID   int64
	To   string
	Body string
	From string
}

// ActionView renders one action item on the board wall. CarriedFrom
// holds the source action id when the item arrived via carry-over.
type ActionView struct {
	ID          int64
	Text        string
	Owner       string
	Done        bool
	CarriedFrom *int64
}

// BoardSummary is the dashboard row view model: identity plus live
// counts. Handlers map store rows into this shape at the boundary.
type BoardSummary struct {
	PublicID        string
	Name            string
	Context         string
	Archived        bool
	CardCount       int
	OpenActionCount int
}

// ColumnView renders one board column with its cards in display order.
// The lock flags converge controls for every viewer: a locked column
// swaps its add form for a notice.
type ColumnView struct {
	ID           int64
	Title        string
	Color        string
	Position     int
	VotingLocked bool
	CardsLocked  bool
	Cards        []CardView
}

// BoardView renders the board shell and the join gate. The
// facilitation fields drive the phase badge, the countdown, the
// spotlight, and the facilitator panel.
type BoardView struct {
	PublicID      string
	Name          string
	Context       string
	Archived      bool
	Phase         string
	VotingLocked  bool
	CardsLocked   bool
	CardsHidden   bool
	TimerEndsAt   *time.Time
	FocusedCardID *int64
}

// PhaseLabel renders the lifecycle value for the phase badge.
func PhaseLabel(phase string) string {
	switch phase {
	case "collect":
		return "Collect"
	case "vote":
		return "Vote"
	case "discuss":
		return "Discuss"
	case "done":
		return "Done"
	default:
		return phase
	}
}

// TimerEndsAtAttr renders the countdown instant for the timer node, or
// an empty string when no timer runs.
func TimerEndsAtAttr(endsAt *time.Time) string {
	if endsAt == nil {
		return ""
	}
	return endsAt.UTC().Format(time.RFC3339Nano)
}

// timerState reports the countdown state for the timer node.
func timerState(endsAt *time.Time) string {
	if endsAt == nil {
		return "idle"
	}
	if time.Now().After(*endsAt) {
		return "ended"
	}
	return "running"
}

// timerText renders the timer node's initial copy; the countdown scope
// takes over from there.
func timerText(endsAt *time.Time) string {
	if endsAt == nil {
		return "No timer running"
	}
	if time.Now().After(*endsAt) {
		return "Time's up!"
	}
	return "Timer running"
}

// pressed marks the active phase button for assistive tech.
func pressed(current, value string) string {
	if current == value {
		return "true"
	}
	return "false"
}

// lockFlip renders the lock value a toggle button submits: the
// opposite of the current flag.
func lockFlip(locked bool) string {
	if locked {
		return "0"
	}
	return "1"
}

// FocusAttr renders the spotlight's focused card id, empty when clear.
func FocusAttr(id *int64) string {
	if id == nil {
		return ""
	}
	return itoa64(*id)
}

// CarrySource is one board offered in the carry-over selector: only
// boards with unfinished action items appear.
type CarrySource struct {
	PublicID    string
	Name        string
	OpenActions int
}

// TemplateColumn is one seeded column of a board template.
type TemplateColumn struct {
	Title string
	Color string
}

// BoardTemplate is a classic retro format offered on the creation page.
type BoardTemplate struct {
	Key     string
	Name    string
	Blurb   string
	Columns []TemplateColumn
}

// BoardTemplates lists every creation format. Columns seed in slice
// order starting at position zero.
func BoardTemplates() []BoardTemplate {
	return []BoardTemplate{
		{
			Key:   "went-well-better-kudos",
			Name:  "Went well / Better / Kudos",
			Blurb: "What went well, what we could have done better, and kudos for the team.",
			Columns: []TemplateColumn{
				{Title: "Went well", Color: "#46a758"},
				{Title: "Could be better", Color: "#e5484d"},
				{Title: "Kudos", Color: "#f5a524"},
			},
		},
		{
			Key:   "mad-sad-glad",
			Name:  "Mad, Sad, Glad",
			Blurb: "Name emotions about the sprint: frustrations, disappointments, wins.",
			Columns: []TemplateColumn{
				{Title: "Mad", Color: "#e5484d"},
				{Title: "Sad", Color: "#3e8ef7"},
				{Title: "Glad", Color: "#46a758"},
			},
		},
		{
			Key:   "start-stop-continue",
			Name:  "Start, Stop, Continue",
			Blurb: "What should the team start, stop, and keep doing?",
			Columns: []TemplateColumn{
				{Title: "Start", Color: "#46a758"},
				{Title: "Stop", Color: "#e5484d"},
				{Title: "Continue", Color: "#3e8ef7"},
			},
		},
		{
			Key:   "four-ls",
			Name:  "The 4 Ls",
			Blurb: "Liked, Learned, Lacked, Longed For — a balanced look back.",
			Columns: []TemplateColumn{
				{Title: "Liked", Color: "#46a758"},
				{Title: "Learned", Color: "#3e8ef7"},
				{Title: "Lacked", Color: "#e5484d"},
				{Title: "Longed For", Color: "#8e4ec6"},
			},
		},
		{
			Key:   "kudos-mix",
			Name:  "Kudos Mix",
			Blurb: "Celebrate the team first, then note what to improve.",
			Columns: []TemplateColumn{
				{Title: "Kudos", Color: "#f5a524"},
				{Title: "Thank-Yous", Color: "#46a758"},
				{Title: "Shout-outs", Color: "#3e8ef7"},
			},
		},
		{
			Key:   "plus-delta",
			Name:  "Plus / Delta",
			Blurb: "What worked (plus) and what to change (delta). Short and sharp.",
			Columns: []TemplateColumn{
				{Title: "Plus", Color: "#46a758"},
				{Title: "Delta", Color: "#e5484d"},
			},
		},
	}
}

// LookupTemplate returns the format for a creation key, if it exists.
func LookupTemplate(key string) (BoardTemplate, bool) {
	for _, tmpl := range BoardTemplates() {
		if tmpl.Key == key {
			return tmpl, true
		}
	}
	return BoardTemplate{}, false
}
