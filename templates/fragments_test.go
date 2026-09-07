package templates

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
)

func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var sb strings.Builder
	if err := c.Render(context.Background(), io.Writer(&sb)); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

func TestDashStatusFallback(t *testing.T) {
	for in, want := range map[string]string{"": "all", "all": "all", "active": "active", "archived": "archived", "foo": "all"} {
		if got := dashStatus(in); got != want {
			t.Errorf("dashStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDashboardUnknownStatusRendersBothSections(t *testing.T) {
	html := render(t, Dashboard(nil, nil, "", "foo"))
	if !strings.Contains(html, "Active boards") || !strings.Contains(html, "Archived boards") {
		t.Errorf("unknown status should render both sections, got:\n%s", html)
	}
}

func TestItoaSign(t *testing.T) {
	for n, want := range map[int]string{0: "0", 7: "7", 42: "42", -1: "-1", -123: "-123"} {
		if got := itoa(n); got != want {
			t.Errorf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTimerStateAndText(t *testing.T) {
	if got := timerState(nil); got != "idle" {
		t.Errorf("timerState(nil) = %q", got)
	}
	if got := timerText(nil); got != "No timer running" {
		t.Errorf("timerText(nil) = %q", got)
	}
	future := time.Now().Add(time.Hour)
	if got := timerState(&future); got != "running" {
		t.Errorf("timerState(future) = %q", got)
	}
	past := time.Now().Add(-time.Hour)
	if got := timerState(&past); got != "ended" {
		t.Errorf("timerState(past) = %q, want ended", got)
	}
	if got := timerText(&past); got != "Time's up!" {
		t.Errorf("timerText(past) = %q, want Time's up!", got)
	}
}

func TestCardEscapesBodyAndIds(t *testing.T) {
	html := render(t, Card(CardView{ID: 7, ColumnID: 3, Body: "<b>x</b>", AuthorName: "<i>a</i>", Comments: []CommentView{{ID: 1, Body: "<u>c</u>"}}}))
	if strings.Contains(html, "<b>x</b>") || !strings.Contains(html, "&lt;b&gt;") {
		t.Errorf("card body not escaped:\n%s", html)
	}
	for _, want := range []string{`id="card-7"`, `sse-swap="card-updated:7, vote-changed:7, card-removed:7"`, `hx-swap="morph:outerHTML"`} {
		if !strings.Contains(html, want) {
			t.Errorf("card missing %q:\n%s", want, html)
		}
	}
}

func TestGroupMemberIsNonInteractive(t *testing.T) {
	m := CardView{ID: 2, Body: "m", Members: []CardView{{ID: 1}}}
	html := render(t, Card(CardView{ID: 9, ColumnID: 1, Body: "lead", Members: []CardView{m}}))
	if strings.Count(html, "sse-swap") != 1 {
		t.Errorf("nested member must not carry sse-swap:\n%s", html)
	}
	memberPart := html[strings.Index(html, "group-members"):]
	if strings.Contains(memberPart, "/vote") || strings.Contains(memberPart, "comment-form") {
		t.Errorf("nested member must not render vote/comment controls:\n%s", html)
	}
	// Cyclic input must not recurse.
	cyc := CardView{ID: 1, Body: "c"}
	cyc.Members = []CardView{{ID: 1, Body: "c", Members: []CardView{{ID: 1, Body: "deep"}}}}
	cycHTML := render(t, Card(cyc))
	if strings.Count(cycHTML, "group-members") != 1 {
		t.Errorf("member nesting must be depth-capped at 1:\n%s", cycHTML)
	}
}

func TestWallsSwapSpecAndEscaping(t *testing.T) {
	html := render(t, KudosWall("b1", []KudoView{{ID: 1, To: "<t>", Body: "<b>hi</b>"}}))
	for _, want := range []string{`id="kudos-list"`, `sse-swap="kudos-added"`, `hx-swap="morph:innerHTML"`} {
		if !strings.Contains(html, want) {
			t.Errorf("kudos wall missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "<b>hi</b>") {
		t.Errorf("kudo body not escaped:\n%s", html)
	}
	ahtml := render(t, ActionsWall("b1", []ActionView{{ID: 2, Text: "<x>"}}))
	for _, want := range []string{`id="actions-list"`, `sse-swap="action-added, action-updated"`, `hx-swap="morph:innerHTML"`} {
		if !strings.Contains(ahtml, want) {
			t.Errorf("actions wall missing %q:\n%s", want, ahtml)
		}
	}
}

func TestHelpers(t *testing.T) {
	if got := PhaseLabel("bogus"); got != "bogus" {
		t.Errorf("PhaseLabel unknown = %q", got)
	}
	if got := TimerEndsAtAttr(nil); got != "" {
		t.Errorf("TimerEndsAtAttr(nil) = %q", got)
	}
	if pressed("vote", "vote") != "true" || pressed("vote", "done") != "false" {
		t.Error("pressed wrong")
	}
	if lockFlip(true) != "0" || lockFlip(false) != "1" {
		t.Error("lockFlip wrong")
	}
	var nilID *int64
	if FocusAttr(nilID) != "" {
		t.Error("FocusAttr(nil) must be empty")
	}
	if cardVotesLabel(1) != "1 vote" || commentCountLabel(1) != "1 comment" {
		t.Error("singular labels wrong")
	}
	if dashboardURL("", "all") != "/" || !strings.Contains(dashboardURL("a b", "active"), "status=active") {
		t.Error("dashboardURL wrong")
	}
}
