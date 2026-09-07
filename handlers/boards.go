// Package handlers is HTTP only: parse input, call the store through
// consumer-side interfaces, and render templates partials. No SQL here.
package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"hindsight/db"
	"hindsight/templates"
)

// component is the renderable fragment surface (satisfied by generated
// templ components).
type component interface {
	Render(ctx context.Context, w io.Writer) error
}

// Publisher receives board events after the DB transaction commits.
// Handlers publish through this seam, never the concrete broker, so
// tests substitute fakes without changing handler signatures.
type Publisher interface {
	Publish(boardID, name, html string) error
}

// NopPublisher discards events. It backs the nil-publisher fallback in
// NewBoards; the binary wires the live broker instead.
type NopPublisher struct{}

// Publish implements Publisher.
func (NopPublisher) Publish(_, _, _ string) error { return nil }

// boardStore is the consumer-side store contract for boards, columns,
// participants, and action items. *db.Store implements it; tests use a
// real store over temp-dir databases.
type boardStore interface {
	CreateBoard(ctx context.Context, name, boardContext, facilitatorToken string, votesPerPerson int) (*db.Board, error)
	CreateSeededBoard(ctx context.Context, seed db.SeededBoard) (*db.Board, error)
	GetBoardByPublicID(ctx context.Context, publicID string) (*db.Board, error)
	ListBoardsWithStats(ctx context.Context) ([]db.BoardWithStats, error)
	CreateColumn(ctx context.Context, boardID int64, title, color string, position int) (*db.Column, error)
	ListColumns(ctx context.Context, boardID int64) ([]db.Column, error)
	ListOpenActions(ctx context.Context, boardID int64) ([]db.Action, error)
	CreateAction(ctx context.Context, boardID int64, text, owner, author string, carriedFrom *int64) (*db.Action, error)
	GetAction(ctx context.Context, actionID int64) (*db.Action, error)
	ListActions(ctx context.Context, boardID int64) ([]db.Action, error)
	UpdateAction(ctx context.Context, actionID int64, text, owner *string, done *bool) (*db.Action, error)
	DeleteAction(ctx context.Context, actionID int64) (*db.Action, error)
	CreateComment(ctx context.Context, cardID int64, body, authorName string) (*db.Comment, error)
	ListComments(ctx context.Context, cardID int64) ([]db.Comment, error)
	ListCommentsByBoard(ctx context.Context, boardID int64) (map[int64][]db.Comment, error)
	CreateKudo(ctx context.Context, boardID int64, to, body, from string) (*db.Kudo, error)
	ListKudos(ctx context.Context, boardID int64) ([]db.Kudo, error)
	GetKudo(ctx context.Context, kudoID int64) (*db.Kudo, error)
	DeleteKudo(ctx context.Context, kudoID int64) (*db.Kudo, error)
	SetArchived(ctx context.Context, boardID int64, archived bool) (*db.Board, error)
	DuplicateBoard(ctx context.Context, srcBoardID int64, newFacilitatorToken string, carryOpenActions bool) (*db.Board, error)
	GetParticipantByToken(ctx context.Context, boardID int64, rawToken string) (*db.Participant, error)
	CreateParticipantWithSuffix(ctx context.Context, boardID int64, baseName, rawToken string) (*db.Participant, error)
	GetBoardByID(ctx context.Context, boardID int64) (*db.Board, error)
	GetColumn(ctx context.Context, columnID int64) (*db.Column, error)
	ListCards(ctx context.Context, columnID int64) ([]db.Card, error)
	ListCardsByBoard(ctx context.Context, boardID int64) (map[int64][]db.Card, error)
	CreateCard(ctx context.Context, columnID int64, body, authorName string) (*db.Card, error)
	GetCard(ctx context.Context, cardID int64) (*db.Card, error)
	UpdateCardBody(ctx context.Context, cardID int64, body string) (*db.Card, error)
	MoveCard(ctx context.Context, cardID, newColumnID int64, newPosition int) error
	MoveCardExpected(ctx context.Context, cardID, newColumnID int64, newPosition int, expectedColumnID int64, expectedPosition int) error
	DeleteCard(ctx context.Context, cardID int64) (*db.Card, []int64, error)
	Vote(ctx context.Context, participantID, cardID int64) error
	Unvote(ctx context.Context, participantID, cardID int64) error
	VotedCardIDs(ctx context.Context, participantID int64) (map[int64]bool, error)
	SetPhase(ctx context.Context, boardID int64, phase string) (*db.Board, error)
	SetCardsHidden(ctx context.Context, boardID int64, hidden bool) (*db.Board, error)
	SetVotingLocked(ctx context.Context, boardID int64, locked bool) (*db.Board, error)
	SetCardsLocked(ctx context.Context, boardID int64, locked bool) (*db.Board, error)
	SetTimerEndsAt(ctx context.Context, boardID int64, endsAt *time.Time) (*db.Board, error)
	DisarmTimer(ctx context.Context, boardID int64, endsAt time.Time) (bool, error)
	ListArmedTimers(ctx context.Context) ([]db.ArmedTimer, error)
	RotateFacilitatorToken(ctx context.Context, boardID int64, expectedOldHash, newHash string) (bool, error)
	SetFocus(ctx context.Context, boardID int64, cardID *int64) (*db.Board, error)
	SetDiscussed(ctx context.Context, cardID int64, discussed bool) (*db.Card, error)
	SetGroup(ctx context.Context, cardID int64, leaderID *int64) (*db.Card, error)
	SortColumnByVotes(ctx context.Context, columnID int64) error
	GroupVoteSums(ctx context.Context, boardID int64) (map[int64]int, error)
	StepFocus(ctx context.Context, boardID int64, forward bool) (*db.Board, []int64, error)
}

// Boards serves the dashboard and board lifecycle routes.
type Boards struct {
	store     boardStore
	secret    string
	events    Publisher
	broker    eventBroker
	heartbeat time.Duration

	// armedTimers tracks one server-side expiry goroutine per board
	// with a running timer, so re-arming or canceling replaces the
	// pending fire instead of stacking duplicates.
	timerMu     sync.Mutex
	armedTimers map[int64]armedTimer
}

// armedTimer is one pending board-timer expiry.
type armedTimer struct {
	timer    *time.Timer
	endsAt   time.Time
	publicID string
}

// NewBoards wires the board routes over a store, the HMAC server secret,
// an event publisher, and the realtime broker backing the event stream.
// A nil broker leaves the stream unavailable until one is assigned.
func NewBoards(store boardStore, serverSecret string, events Publisher, broker eventBroker) *Boards {
	if events == nil {
		events = NopPublisher{}
	}
	return &Boards{store: store, secret: serverSecret, events: events, broker: broker, heartbeat: defaultHeartbeat, armedTimers: map[int64]armedTimer{}}
}

// defaultVotesPerPerson matches the schema default for new boards.
const defaultVotesPerPerson = 3

const (
	maxBoardName    = 200
	maxBoardContext = 2000
	maxDisplayName  = 100
)

// revealLifespan bounds the single-use creation reveal cookie.
const revealLifespan = 10 * time.Minute

// Dashboard renders the board lists with counts, honoring ?q= search
// and the ?status= shelf filter (all, active, or archived; anything
// else reads as all).
func (b *Boards) Dashboard(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	status := dashboardStatus(r.URL.Query().Get("status"))
	summaries, err := b.summaries(r.Context(), query)
	if err != nil {
		http.Error(w, "load boards", http.StatusInternalServerError)
		return
	}
	var active, archived []templates.BoardSummary
	for _, summary := range summaries {
		if summary.Archived {
			archived = append(archived, summary)
		} else {
			active = append(active, summary)
		}
	}
	if active == nil {
		active = []templates.BoardSummary{}
	}
	if archived == nil {
		archived = []templates.BoardSummary{}
	}
	writePage(w, r, templates.Dashboard(active, archived, query, status))
}

// dashboardStatus normalizes the shelf filter to its canonical value.
func dashboardStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "active", "archived":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return "all"
	}
}

// New renders the board creation page with the template picker and the
// carry-over selector.
func (b *Boards) New(w http.ResponseWriter, r *http.Request) {
	stats, err := b.store.ListBoardsWithStats(r.Context())
	if err != nil {
		http.Error(w, "load boards", http.StatusInternalServerError)
		return
	}
	var sources []templates.CarrySource
	for _, item := range stats {
		if item.OpenActionCount == 0 {
			continue
		}
		sources = append(sources, templates.CarrySource{
			PublicID:    item.PublicID,
			Name:        item.Name,
			OpenActions: item.OpenActionCount,
		})
	}
	if sources == nil {
		sources = []templates.CarrySource{}
	}
	writePage(w, r, templates.BoardNew(sources, templates.BoardTemplates()))
}

// Create seeds a board from a template, signs the creator in as
// facilitator and participant, and redirects to the new board. The raw
// facilitator token surfaces exactly once, on the first board view.
func (b *Boards) Create(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	name := strings.TrimSpace(r.Form.Get("name"))
	boardContext := strings.TrimSpace(r.Form.Get("context"))
	displayName := strings.TrimSpace(r.Form.Get("display_name"))
	templateKey := strings.TrimSpace(r.Form.Get("template"))
	carryFrom := strings.TrimSpace(r.Form.Get("carry_from"))

	if name == "" {
		writeFlash(w, http.StatusUnprocessableEntity, "Give the board a name.")
		return
	}
	if len([]rune(name)) > maxBoardName {
		writeFlash(w, http.StatusUnprocessableEntity, "Board name is too long.")
		return
	}
	if len([]rune(boardContext)) > maxBoardContext {
		writeFlash(w, http.StatusUnprocessableEntity, "Board context is too long.")
		return
	}
	if displayName == "" {
		displayName = "Facilitator"
	}
	if len([]rune(displayName)) > maxDisplayName {
		writeFlash(w, http.StatusUnprocessableEntity, "Display name is too long.")
		return
	}
	format, ok := templates.LookupTemplate(templateKey)
	if !ok {
		if templateKey == "" {
			format = templates.BoardTemplates()[0]
		} else {
			writeFlash(w, http.StatusUnprocessableEntity, "Unknown board template.")
			return
		}
	}
	var carrySource *db.Board
	if carryFrom != "" {
		src, err := b.store.GetBoardByPublicID(r.Context(), carryFrom)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeFlash(w, http.StatusUnprocessableEntity, "Unknown carry-over board.")
				return
			}
			http.Error(w, "load carry-over board", http.StatusInternalServerError)
			return
		}
		carrySource = src
	}

	facToken, err := db.GenerateToken()
	if err != nil {
		http.Error(w, "mint token", http.StatusInternalServerError)
		return
	}
	partToken, err := db.GenerateToken()
	if err != nil {
		http.Error(w, "mint token", http.StatusInternalServerError)
		return
	}
	seeds := make([]db.ColumnSeed, 0, len(format.Columns))
	for _, column := range format.Columns {
		seeds = append(seeds, db.ColumnSeed{Title: column.Title, Color: column.Color})
	}
	var carryID *int64
	if carrySource != nil {
		carryID = &carrySource.ID
	}
	// One store call seeds the board, columns, carried-over actions, and
	// the creator's participant row in a single transaction: a seed
	// failure rolls everything back instead of leaving a half-seeded board.
	board, err := b.store.CreateSeededBoard(r.Context(), db.SeededBoard{
		Name:             name,
		Context:          boardContext,
		FacilitatorToken: facToken,
		VotesPerPerson:   defaultVotesPerPerson,
		Columns:          seeds,
		CarryFromBoardID: carryID,
		ParticipantName:  displayName,
		ParticipantToken: partToken,
	})
	if err != nil {
		if errors.Is(err, db.ErrInvalidInput) {
			writeFlash(w, http.StatusUnprocessableEntity, "Could not create the board.")
			return
		}
		http.Error(w, "create board", http.StatusInternalServerError)
		return
	}
	writeMapCookie(w, r, b.secret, facCookieName, board.PublicID, facToken, cookieLifespan)
	writeMapCookie(w, r, b.secret, partsCookieName, board.PublicID, partToken, cookieLifespan)
	writeMapCookie(w, r, b.secret, revealCookieName, board.PublicID, facToken, revealLifespan)
	http.Redirect(w, r, "/b/"+board.PublicID, http.StatusSeeOther)
}

// Show renders the join gate for strangers and the board shell for
// participants and facilitators. A valid ?fac= token claims facilitator
// rights, which is what the facilitator share link carries.
func (b *Boards) Show(w http.ResponseWriter, r *http.Request) {
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
	// Server-owned timer expiry surfaces on every board read, alongside
	// the per-board expiry fire, so a slept client still learns the
	// timer ended without trusting its own clock.
	b.checkTimerExpiry(r, board)
	if raw := strings.TrimSpace(r.URL.Query().Get("fac")); raw != "" {
		if db.CompareTokenHash(db.HashToken(raw), board.FacilitatorTokenHash) {
			writeMapCookie(w, r, b.secret, facCookieName, board.PublicID, raw, cookieLifespan)
			http.Redirect(w, r, "/b/"+board.PublicID, http.StatusSeeOther)
			return
		}
	}
	participant := b.participant(r, board)
	facRaw, isFacilitator := b.facilitatorToken(r, board)
	if participant == nil && !isFacilitator {
		if strings.TrimSpace(r.URL.Query().Get("fragment")) == "columns" {
			writeFlash(w, http.StatusForbidden, "Join the board first.")
			return
		}
		writePage(w, r, templates.JoinGate(boardView(board)))
		return
	}
	columns, err := b.store.ListColumns(r.Context(), board.ID)
	if err != nil {
		http.Error(w, "load columns", http.StatusInternalServerError)
		return
	}
	voted, err := b.votedSet(r.Context(), participant)
	if err != nil {
		http.Error(w, "load votes", http.StatusInternalServerError)
		return
	}
	// The disconnected poll fallback and reconnect resyncs refetch the
	// columns through this URL; it renders the same fragment joining
	// returns, gated the same way. The countdown re-syncs its instant
	// through the timer fragment after the tab was hidden.
	if fragment := strings.TrimSpace(r.URL.Query().Get("fragment")); fragment != "" {
		switch fragment {
		case "columns":
			views, err := b.columnViews(r.Context(), board, columns, voted, !b.seesFullCards(r, board))
			if err != nil {
				http.Error(w, "load cards", http.StatusInternalServerError)
				return
			}
			kudos, actions, err := b.wallViews(r, board)
			if err != nil {
				http.Error(w, "load walls", http.StatusInternalServerError)
				return
			}
			writeFragment(w, r, templates.ColumnsFragment(board.PublicID, views, kudos, actions))
			return
		case "timer":
			writeFragment(w, r, templates.TimerFragment(boardView(board)))
			return
		default:
			http.NotFound(w, r)
			return
		}
	}
	participantName := ""
	if participant != nil {
		participantName = participant.Name
	}
	reveal := ""
	if raw, ok := lookupMapCookie(r, b.secret, revealCookieName, board.PublicID); ok {
		if _, fac := b.facilitatorTokenWith(board, raw); fac {
			reveal = raw
		}
		dropMapEntry(w, r, b.secret, revealCookieName, board.PublicID, revealLifespan)
	}
	participantLink := absoluteBoardLink(r, board.PublicID)
	facilitatorLink := ""
	if isFacilitator {
		facilitatorLink = participantLink + "?fac=" + facRaw
	}
	views, err := b.columnViews(r.Context(), board, columns, voted, !b.seesFullCards(r, board))
	if err != nil {
		http.Error(w, "load cards", http.StatusInternalServerError)
		return
	}
	kudos, actions, err := b.wallViews(r, board)
	if err != nil {
		http.Error(w, "load walls", http.StatusInternalServerError)
		return
	}
	writePage(w, r, templates.BoardPage(
		boardView(board), views, kudos, actions, isFacilitator,
		participantName, reveal, participantLink, facilitatorLink,
	))
}

// Join upserts the visitor's participant row (suffixing duplicate names),
// sets the participant cookie, and returns the board columns HTML.
func (b *Boards) Join(w http.ResponseWriter, r *http.Request) {
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
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	if existing := b.participant(r, board); existing != nil {
		b.writeColumns(w, r, board, existing)
		return
	}
	name := strings.TrimSpace(r.Form.Get("name"))
	if name == "" {
		writeFlash(w, http.StatusUnprocessableEntity, "Pick a display name to join.")
		return
	}
	if len([]rune(name)) > maxDisplayName {
		writeFlash(w, http.StatusUnprocessableEntity, "Display name is too long.")
		return
	}
	rawToken, err := db.GenerateToken()
	if err != nil {
		http.Error(w, "mint token", http.StatusInternalServerError)
		return
	}
	if _, err := b.store.CreateParticipantWithSuffix(r.Context(), board.ID, name, rawToken); err != nil {
		if errors.Is(err, db.ErrInvalidInput) {
			writeFlash(w, http.StatusUnprocessableEntity, "Pick a display name to join.")
			return
		}
		http.Error(w, "join board", http.StatusInternalServerError)
		return
	}
	writeMapCookie(w, r, b.secret, partsCookieName, board.PublicID, rawToken, cookieLifespan)
	joined, err := b.store.GetParticipantByToken(r.Context(), board.ID, rawToken)
	if err != nil {
		http.Error(w, "join board", http.StatusInternalServerError)
		return
	}
	b.writeColumns(w, r, board, joined)
}

// Archive flags a board; the row partial lets the dashboard move it to
// the archived list without losing content.
func (b *Boards) Archive(w http.ResponseWriter, r *http.Request) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	updated, err := b.store.SetArchived(r.Context(), board.ID, true)
	if err != nil {
		http.Error(w, "archive board", http.StatusInternalServerError)
		return
	}
	b.writeRow(w, r, updated)
}

// Unarchive clears the archive flag; the row partial moves the board
// back to the active list.
func (b *Boards) Unarchive(w http.ResponseWriter, r *http.Request) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	updated, err := b.store.SetArchived(r.Context(), board.ID, false)
	if err != nil {
		http.Error(w, "unarchive board", http.StatusInternalServerError)
		return
	}
	b.writeRow(w, r, updated)
}

// Duplicate copies a board's columns (plus open actions on request) and
// redirects to the copy. The duplicator becomes its facilitator.
func (b *Boards) Duplicate(w http.ResponseWriter, r *http.Request) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	carry := isTruthy(r.Form.Get("carry_actions"))
	rawToken, err := db.GenerateToken()
	if err != nil {
		http.Error(w, "mint token", http.StatusInternalServerError)
		return
	}
	dup, err := b.store.DuplicateBoard(r.Context(), board.ID, rawToken, carry)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "duplicate board", http.StatusInternalServerError)
		return
	}
	writeMapCookie(w, r, b.secret, facCookieName, dup.PublicID, rawToken, cookieLifespan)
	writeMapCookie(w, r, b.secret, revealCookieName, dup.PublicID, rawToken, revealLifespan)
	http.Redirect(w, r, "/b/"+dup.PublicID, http.StatusSeeOther)
}

// facilitatorBoard loads the board from the URL and enforces facilitator
// rights: unknown boards get a plain 404, strangers get 403 plus the
// out-of-band flash fragment.
func (b *Boards) facilitatorBoard(w http.ResponseWriter, r *http.Request) (*db.Board, bool) {
	board, err := b.store.GetBoardByPublicID(r.Context(), r.PathValue("bid"))
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return nil, false
		}
		http.Error(w, "load board", http.StatusInternalServerError)
		return nil, false
	}
	if _, ok := b.facilitatorToken(r, board); !ok {
		writeFlash(w, http.StatusForbidden, "Only the facilitator can do that.")
		return nil, false
	}
	return board, true
}

// facilitatorToken returns the raw facilitator token when the request
// cookie verifies against the stored hash via constant-time comparison.
func (b *Boards) facilitatorToken(r *http.Request, board *db.Board) (string, bool) {
	raw, ok := lookupMapCookie(r, b.secret, facCookieName, board.PublicID)
	if !ok {
		return "", false
	}
	return b.facilitatorTokenWith(board, raw)
}

func (b *Boards) facilitatorTokenWith(board *db.Board, raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	if !db.CompareTokenHash(db.HashToken(raw), board.FacilitatorTokenHash) {
		return "", false
	}
	return raw, true
}

// participant resolves the participant cookie to its row, if the token
// still maps to a participant on this board.
func (b *Boards) participant(r *http.Request, board *db.Board) *db.Participant {
	raw, ok := lookupMapCookie(r, b.secret, partsCookieName, board.PublicID)
	if !ok {
		return nil
	}
	found, err := b.store.GetParticipantByToken(r.Context(), board.ID, raw)
	if err != nil {
		return nil
	}
	return found
}

func (b *Boards) writeColumns(w http.ResponseWriter, r *http.Request, board *db.Board, participant *db.Participant) {
	columns, err := b.store.ListColumns(r.Context(), board.ID)
	if err != nil {
		http.Error(w, "load columns", http.StatusInternalServerError)
		return
	}
	voted, err := b.votedSet(r.Context(), participant)
	if err != nil {
		http.Error(w, "load votes", http.StatusInternalServerError)
		return
	}
	views, err := b.columnViews(r.Context(), board, columns, voted, !b.seesFullCards(r, board))
	if err != nil {
		http.Error(w, "load cards", http.StatusInternalServerError)
		return
	}
	kudos, actions, err := b.wallViews(r, board)
	if err != nil {
		http.Error(w, "load walls", http.StatusInternalServerError)
		return
	}
	writeFragment(w, r, templates.ColumnsFragment(board.PublicID, views, kudos, actions))
}

// wallViews loads the kudos and action walls for a viewer. Kudos
// messages redact for non-facilitators while blind collection hides
// cards; action texts always stay visible.
func (b *Boards) wallViews(r *http.Request, board *db.Board) ([]templates.KudoView, []templates.ActionView, error) {
	entries, err := b.store.ListKudos(r.Context(), board.ID)
	if err != nil {
		return nil, nil, err
	}
	items, err := b.store.ListActions(r.Context(), board.ID)
	if err != nil {
		return nil, nil, err
	}
	return kudoViews(entries, !b.seesFullCards(r, board)), actionViews(items), nil
}

func (b *Boards) writeRow(w http.ResponseWriter, r *http.Request, board *db.Board) {
	summary, err := b.summaryFor(r.Context(), board.ID)
	if err != nil {
		http.Error(w, "load board", http.StatusInternalServerError)
		return
	}
	writeFragment(w, r, templates.BoardRow(summary))
}

func (b *Boards) summaries(ctx context.Context, query string) ([]templates.BoardSummary, error) {
	stats, err := b.store.ListBoardsWithStats(ctx)
	if err != nil {
		return nil, err
	}
	var out []templates.BoardSummary
	for _, item := range stats {
		if query != "" &&
			!strings.Contains(strings.ToLower(item.Name+" "+item.Context), strings.ToLower(query)) {
			continue
		}
		out = append(out, boardSummary(item))
	}
	if out == nil {
		out = []templates.BoardSummary{}
	}
	return out, nil
}

func (b *Boards) summaryFor(ctx context.Context, boardID int64) (templates.BoardSummary, error) {
	stats, err := b.store.ListBoardsWithStats(ctx)
	if err != nil {
		return templates.BoardSummary{}, err
	}
	for _, item := range stats {
		if item.ID == boardID {
			return boardSummary(item), nil
		}
	}
	return templates.BoardSummary{}, db.ErrNotFound
}

func boardSummary(item db.BoardWithStats) templates.BoardSummary {
	return templates.BoardSummary{
		PublicID:        item.PublicID,
		Name:            item.Name,
		Context:         item.Context,
		Archived:        item.ArchivedAt != nil,
		CardCount:       item.CardCount,
		OpenActionCount: item.OpenActionCount,
	}
}

func boardView(board *db.Board) templates.BoardView {
	return templates.BoardView{
		PublicID:      board.PublicID,
		Name:          board.Name,
		Context:       board.Context,
		Archived:      board.ArchivedAt != nil,
		Phase:         board.Phase,
		VotingLocked:  board.VotingLocked,
		CardsLocked:   board.CardsLocked,
		CardsHidden:   board.CardsHidden,
		TimerEndsAt:   board.TimerEndsAt,
		FocusedCardID: board.FocusedCardID,
	}
}

// columnViews maps store columns to view models with each column's
// cards in display order, ready for the shell and the join fragment.
// The voted set marks each card's control for its viewer; a nil set
// leaves every control in the unvoted state. While blind collection
// hides cards, redact replaces bodies, authors, and vote counts with
// placeholders for non-facilitator viewers. Grouped cards nest under
// their leader with the summed vote count; the sums come from the
// read query, never from stored counters.
func (b *Boards) columnViews(ctx context.Context, board *db.Board, columns []db.Column, voted map[int64]bool, redact bool) ([]templates.ColumnView, error) {
	sums, err := b.store.GroupVoteSums(ctx, board.ID)
	if err != nil {
		return nil, err
	}
	comments, err := b.store.ListCommentsByBoard(ctx, board.ID)
	if err != nil {
		return nil, err
	}
	byColumn, err := b.store.ListCardsByBoard(ctx, board.ID)
	if err != nil {
		return nil, err
	}
	views := make([]templates.ColumnView, 0, len(columns))
	for _, column := range columns {
		cards := byColumn[column.ID]
		views = append(views, templates.ColumnView{
			ID:           column.ID,
			Title:        column.Title,
			Color:        column.Color,
			Position:     column.Position,
			VotingLocked: board.VotingLocked,
			CardsLocked:  board.CardsLocked,
			Cards:        cardForest(cards, voted, sums, comments, redact),
		})
	}
	return views, nil
}

// cardForest builds the render forest for one column's cards: leaders
// carry their nested members and the summed vote count, ungrouped
// cards stand alone, and members whose leader sits in another column
// fall back to top-level so no card ever vanishes. Redaction applies
// to every node exactly like the column and card pair paths.
func cardForest(cards []db.Card, voted map[int64]bool, sums map[int64]int, comments map[int64][]db.Comment, redact bool) []templates.CardView {
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
	out := make([]templates.CardView, 0, len(tops))
	for _, top := range tops {
		view := cardView(top, voted[top.ID], comments[top.ID])
		// Group sums are the single source of truth for leaders:
		// absent means zero votes, never the stored counter.
		// Ungrouped cards keep their own count.
		if len(membersOf[top.ID]) > 0 {
			view.Votes = sums[top.ID]
		}
		for _, member := range membersOf[top.ID] {
			nested := cardView(member, voted[member.ID], comments[member.ID])
			if redact {
				nested = redactCardView(nested)
			}
			view.Members = append(view.Members, nested)
		}
		if redact {
			view = redactCardView(view)
		}
		out = append(out, view)
	}
	return out
}

// votedSet loads the card ids a participant voted for. Strangers (a nil
// participant) get a nil set, which renders every control unvoted.
func (b *Boards) votedSet(ctx context.Context, participant *db.Participant) (map[int64]bool, error) {
	if participant == nil {
		return nil, nil
	}
	return b.store.VotedCardIDs(ctx, participant.ID)
}

// cardView maps one store row to its render view model, attaching the
// card's thread. Redaction (when blind collection hides cards) applies
// later through redactCardView, so comments inherit the card treatment
// automatically.
func cardView(card db.Card, voted bool, comments []db.Comment) templates.CardView {
	return templates.CardView{
		ID:         card.ID,
		ColumnID:   card.ColumnID,
		Body:       card.Body,
		AuthorName: card.AuthorName,
		Votes:      card.Votes,
		Position:   card.Position,
		Voted:      voted,
		Discussed:  card.Discussed,
		Grouped:    card.GroupID != nil,
		Comments:   commentViews(comments),
	}
}

// commentViews maps store remarks to render view models in order.
func commentViews(comments []db.Comment) []templates.CommentView {
	views := make([]templates.CommentView, 0, len(comments))
	for _, comment := range comments {
		views = append(views, templates.CommentView{
			ID:         comment.ID,
			Body:       comment.Body,
			AuthorName: comment.AuthorName,
		})
	}
	return views
}

// absoluteBoardLink builds the shareable board URL, honoring forwarded
// TLS so links pasted from behind a tunnel keep their scheme. The Host
// header is attacker-controlled, so it is strictly validated: reject
// CR/LF, spaces, slashes, "@", and backslashes, require a sane charset,
// and fall back to localhost on anything unexpected.
func absoluteBoardLink(r *http.Request, publicID string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := sanitizeHost(r.Host)
	return scheme + "://" + host + "/b/" + publicID
}

// sanitizeHost allows host[:port] with alphanumerics, dots, hyphens,
// colons (port / IPv6 brackets), and falls back to localhost otherwise.
func sanitizeHost(host string) string {
	if host == "" {
		return "localhost"
	}
	for _, c := range host {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == ':', c == '[', c == ']':
		default:
			return "localhost"
		}
	}
	if strings.Contains(host, "..") {
		return "localhost"
	}
	return host
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

// writePage and writeFragment buffer the render first: a failed Render
// must not follow a partially written body with a second status line,
// so headers and body go out only on success.
func writePage(w http.ResponseWriter, r *http.Request, component component) {
	var buf bytes.Buffer
	if err := component.Render(r.Context(), &buf); err != nil {
		http.Error(w, "render", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func writeFragment(w http.ResponseWriter, r *http.Request, component component) {
	var buf bytes.Buffer
	if err := component.Render(r.Context(), &buf); err != nil {
		http.Error(w, "render", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}
