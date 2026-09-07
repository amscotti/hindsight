package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hindsight/db"
)

// maxTimerSeconds caps one timer arming at a day; longer runs re-arm.
const maxTimerSeconds = 86400

// phasePayload is the phase broadcast body: viewers update the phase
// badge and the facilitator panel from it.
type phasePayload struct {
	Phase string `json:"phase"`
}

// focusPayload is the focus broadcast body: viewers move the spotlight
// to the card, or clear it when the id is null.
type focusPayload struct {
	CardID *int64 `json:"card_id"`
}

// timerPayload is the timer broadcast body: viewers render the
// countdown to the instant, or idle when it is null.
type timerPayload struct {
	EndsAt *time.Time `json:"ends_at"`
}

// timerEndedPayload is the expiry broadcast body: the instant the
// server fired at.
type timerEndedPayload struct {
	At time.Time `json:"at"`
}

// marshalControl encodes a facilitation broadcast body. Encoding one
// of these shapes cannot fail; a failure still answers 500 rather
// than publishing a corrupt payload.
func marshalControl(w http.ResponseWriter, value any) (string, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode event", http.StatusInternalServerError)
		return "", false
	}
	return string(raw), true
}

// SetPhase moves the board lifecycle value. Any transition between
// known values is allowed; unknown values are rejected. The commit
// lands first, then the phase broadcast goes out for every viewer.
func (b *Boards) SetPhase(w http.ResponseWriter, r *http.Request) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	phase := strings.TrimSpace(r.Form.Get("phase"))
	if _, err := b.store.SetPhase(r.Context(), board.ID, phase); err != nil {
		if errors.Is(err, db.ErrInvalidInput) {
			writeFlash(w, http.StatusUnprocessableEntity, "Unknown board phase.")
			return
		}
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "set phase", http.StatusInternalServerError)
		return
	}
	payload, ok := marshalControl(w, phasePayload{Phase: phase})
	if !ok {
		return
	}
	b.checkTimerExpiry(r, board)
	if err := b.events.Publish(board.PublicID, "phase", payload); err != nil {
		writeSyncFailure(w)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Reveal ends blind collection: every viewer refetches full columns
// through the cards-revealed broadcast, which is what makes the
// redaction machinery lift for participants.
func (b *Boards) Reveal(w http.ResponseWriter, r *http.Request) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	if _, err := b.store.SetCardsHidden(r.Context(), board.ID, false); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "reveal cards", http.StatusInternalServerError)
		return
	}
	b.checkTimerExpiry(r, board)
	if err := b.events.Publish(board.PublicID, "cards-revealed", ""); err != nil {
		writeSyncFailure(w)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Lock flips the voting or card-adding lock. Locks change controls on
// every column, so every column refreshes for all viewers through the
// redaction-aware pair path.
func (b *Boards) Lock(w http.ResponseWriter, r *http.Request) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	target := strings.TrimSpace(r.Form.Get("target"))
	raw := strings.TrimSpace(r.Form.Get("locked"))
	var locked bool
	switch raw {
	case "1":
		locked = true
	case "0":
		locked = false
	default:
		writeFlash(w, http.StatusUnprocessableEntity, "Lock state must be 0 or 1.")
		return
	}
	var err error
	switch target {
	case "voting":
		_, err = b.store.SetVotingLocked(r.Context(), board.ID, locked)
	case "cards":
		_, err = b.store.SetCardsLocked(r.Context(), board.ID, locked)
	default:
		writeFlash(w, http.StatusUnprocessableEntity, "Lock target must be voting or cards.")
		return
	}
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "set lock", http.StatusInternalServerError)
		return
	}
	board, err = b.store.GetBoardByPublicID(r.Context(), board.PublicID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load board", http.StatusInternalServerError)
		return
	}
	// Locks render every column, so an expiry noticed here broadcasts
	// before the refreshes below; the conditional disarm keeps it to
	// one broadcast across concurrent readers.
	b.checkTimerExpiry(r, board)
	columns, err := b.store.ListColumns(r.Context(), board.ID)
	if err != nil {
		http.Error(w, "load columns", http.StatusInternalServerError)
		return
	}
	voted, err := b.votedSet(r.Context(), b.participant(r, board))
	if err != nil {
		http.Error(w, "load votes", http.StatusInternalServerError)
		return
	}
	var actorPartials []string
	failed := false
	for i := range columns {
		viewerHTML, fullHTML, err := b.renderColumnPair(r, board, &columns[i], voted)
		if err != nil {
			http.Error(w, "render column", http.StatusInternalServerError)
			return
		}
		if err := b.publishPair(board,
			"column-"+strconv.FormatInt(columns[i].ID, 10), viewerHTML, fullHTML); err != nil {
			failed = true
		}
		actorPartials = append(actorPartials, b.actorVariant(r, board, viewerHTML, fullHTML))
	}
	if failed {
		writeSyncFailure(w, actorPartials...)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Timer arms the board timer for N seconds (0 cancels it). The expiry
// instant persists server-side; clients only render the countdown.
// Arming also schedules the server-side expiry fire below.
func (b *Boards) Timer(w http.ResponseWriter, r *http.Request) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("seconds")))
	if err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Timer length must be a number of seconds.")
		return
	}
	if seconds < 0 || seconds > maxTimerSeconds {
		writeFlash(w, http.StatusUnprocessableEntity, "Timer length must be between 0 and 86400 seconds.")
		return
	}
	var endsAt *time.Time
	if seconds > 0 {
		at := time.Now().UTC().Add(time.Duration(seconds) * time.Second)
		endsAt = &at
	}
	// An already-expired instant still owns exactly one broadcast, so
	// notice it before overwriting; otherwise the re-arm would swallow
	// the prior expiry silently.
	b.checkTimerExpiry(r, board)
	// Marshal the requested payload before persisting so a marshaling
	// failure never strands a stored timer without its broadcast.
	if _, ok := marshalControl(w, timerPayload{EndsAt: endsAt}); !ok {
		return
	}
	updated, err := b.store.SetTimerEndsAt(r.Context(), board.ID, endsAt)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "set timer", http.StatusInternalServerError)
		return
	}
	// The stored row is authoritative (a clock race can nudge the
	// persisted instant), so broadcast what was persisted, not requested.
	payload, ok := marshalControl(w, timerPayload{EndsAt: updated.TimerEndsAt})
	if !ok {
		return
	}
	publishErr := b.events.Publish(board.PublicID, "timer", payload)
	// Keep the in-memory fire converged with the persisted instant even
	// when the viewer missed the event: the next board read re-renders
	// the countdown from the DB and re-arms a live timer.
	if updated.TimerEndsAt == nil {
		b.cancelTimer(board.ID)
	} else {
		b.armTimer(board.PublicID, board.ID, *updated.TimerEndsAt)
	}
	if publishErr != nil {
		writeSyncFailure(w)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// armTimer schedules the server-side expiry fire for one board,
// replacing any pending fire. A repeat arming for the same instant
// keeps the existing fire, so board reads that notice an armed timer
// (after a restart, which drops in-memory fires) restore it cheaply.
func (b *Boards) armTimer(publicID string, boardID int64, endsAt time.Time) {
	b.timerMu.Lock()
	defer b.timerMu.Unlock()
	if pending, ok := b.armedTimers[boardID]; ok {
		if pending.endsAt.Equal(endsAt) {
			return
		}
		pending.timer.Stop()
	}
	delay := time.Until(endsAt)
	if delay < 0 {
		delay = 0
	}
	b.armedTimers[boardID] = armedTimer{
		timer:    time.AfterFunc(delay, func() { b.fireTimer(boardID, publicID) }),
		endsAt:   endsAt,
		publicID: publicID,
	}
}

// cancelTimer drops a board's pending expiry fire, if any.
func (b *Boards) cancelTimer(boardID int64) {
	b.timerMu.Lock()
	defer b.timerMu.Unlock()
	if pending, ok := b.armedTimers[boardID]; ok {
		pending.timer.Stop()
		delete(b.armedTimers, boardID)
	}
}

// fireTimer runs when a board timer expires. It re-reads the board so
// a re-armed or canceled timer never fires stale, disarms exactly
// once across concurrent firers (the disarm matches the re-read
// instant, so a re-arm landing in the window survives), and broadcasts
// the expiry with no client involved.
func (b *Boards) fireTimer(boardID int64, publicID string) {
	_ = publicID
	b.cancelTimer(boardID)
	ctx := context.Background()
	board, err := b.store.GetBoardByID(ctx, boardID)
	if err != nil {
		return
	}
	if board.TimerEndsAt == nil || board.TimerEndsAt.After(time.Now()) {
		if board.TimerEndsAt != nil {
			b.armTimer(board.PublicID, board.ID, *board.TimerEndsAt)
		}
		return
	}
	endsAt := *board.TimerEndsAt
	armed, err := b.store.DisarmTimer(ctx, boardID, endsAt)
	if err != nil || !armed {
		return
	}
	payload, err := json.Marshal(timerEndedPayload{At: endsAt})
	if err != nil {
		return
	}
	_ = b.events.Publish(board.PublicID, "timer-ended", string(payload))
}

// checkTimerExpiry notices an expired timer on a board read, disarming
// exactly once and broadcasting the expiry. The disarm matches the
// read instant, so concurrent readers converge on one broadcast and a
// re-arm landing in the window survives. Boards with a live timer
// ensure a server-side fire is armed (restoring it after a restart).
func (b *Boards) checkTimerExpiry(r *http.Request, board *db.Board) {
	if board.TimerEndsAt == nil {
		return
	}
	if !board.TimerEndsAt.After(time.Now()) {
		endsAt := *board.TimerEndsAt
		if armed, err := b.store.DisarmTimer(r.Context(), board.ID, endsAt); err == nil && armed {
			board.TimerEndsAt = nil
			payload, err := json.Marshal(timerEndedPayload{At: endsAt})
			if err == nil {
				_ = b.events.Publish(board.PublicID, "timer-ended", string(payload))
			}
		}
		return
	}
	b.armTimer(board.PublicID, board.ID, *board.TimerEndsAt)
}

// RestoreArmedTimers re-arms server-side expiry fires from persisted
// timer instants. Restarts drop the in-memory fires, so startup scans
// the boards table and schedules one fire per future instant. Past
// instants are left alone: the next board read, subscribe, or
// heartbeat tick notices and broadcasts them exactly once.
func (b *Boards) RestoreArmedTimers(ctx context.Context) error {
	timers, err := b.store.ListArmedTimers(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, timer := range timers {
		if timer.EndsAt.After(now) {
			b.armTimer(timer.PublicID, timer.BoardID, timer.EndsAt)
		}
	}
	return nil
}

// Focus spotlights one card for every viewer (empty clears it).
func (b *Boards) Focus(w http.ResponseWriter, r *http.Request) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	var cardID *int64
	if raw := strings.TrimSpace(r.Form.Get("card_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeFlash(w, http.StatusUnprocessableEntity, "Focused card is invalid.")
			return
		}
		cardID = &id
	}
	updated, err := b.store.SetFocus(r.Context(), board.ID, cardID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "set focus", http.StatusInternalServerError)
		return
	}
	b.checkTimerExpiry(r, board)
	payload, ok := marshalControl(w, focusPayload{CardID: updated.FocusedCardID})
	if !ok {
		return
	}
	if err := b.events.Publish(board.PublicID, "focus", payload); err != nil {
		writeSyncFailure(w)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// stepFocus marks the current spotlight discussed and advances the
// queue, refreshing the affected columns through the redaction-aware
// pair path so discussed flags converge for every viewer.
func (b *Boards) stepFocus(w http.ResponseWriter, r *http.Request, forward bool) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	updated, affected, err := b.store.StepFocus(r.Context(), board.ID, forward)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "step focus", http.StatusInternalServerError)
		return
	}
	// Stepping refreshes discussed columns, so an expiry noticed here
	// broadcasts before the focus event; the conditional disarm keeps
	// it to one broadcast across concurrent readers.
	b.checkTimerExpiry(r, updated)
	payload, ok := marshalControl(w, focusPayload{CardID: updated.FocusedCardID})
	if !ok {
		return
	}
	voted, err := b.votedSet(r.Context(), b.participant(r, updated))
	if err != nil {
		http.Error(w, "load votes", http.StatusInternalServerError)
		return
	}
	seen := map[int64]bool{}
	type columnRefresh struct {
		columnID   int64
		viewerHTML string
		fullHTML   string
	}
	var refreshes []columnRefresh
	var actorPartials []string
	for _, id := range affected {
		card, err := b.store.GetCard(r.Context(), id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				continue
			}
			http.Error(w, "load card", http.StatusInternalServerError)
			return
		}
		if seen[card.ColumnID] {
			continue
		}
		seen[card.ColumnID] = true
		column, err := b.store.GetColumn(r.Context(), card.ColumnID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				continue
			}
			http.Error(w, "load column", http.StatusInternalServerError)
			return
		}
		viewerHTML, fullHTML, err := b.renderColumnPair(r, updated, column, voted)
		if err != nil {
			http.Error(w, "render column", http.StatusInternalServerError)
			return
		}
		refreshes = append(refreshes, columnRefresh{columnID: column.ID, viewerHTML: viewerHTML, fullHTML: fullHTML})
		actorPartials = append(actorPartials, b.actorVariant(r, updated, viewerHTML, fullHTML))
	}
	failed := false
	for _, ref := range refreshes {
		if err := b.publishPair(updated,
			"column-"+strconv.FormatInt(ref.columnID, 10), ref.viewerHTML, ref.fullHTML); err != nil {
			failed = true
		}
	}
	if failed {
		writeSyncFailure(w, actorPartials...)
		return
	}
	if err := b.events.Publish(board.PublicID, "focus", payload); err != nil {
		writeSyncFailure(w, actorPartials...)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// FocusNext marks the current spotlight discussed and advances to the
// next undiscussed card in vote-total order.
func (b *Boards) FocusNext(w http.ResponseWriter, r *http.Request) {
	b.stepFocus(w, r, true)
}

// FocusPrev marks the current spotlight discussed and steps back to
// the previous undiscussed card in vote-total order.
func (b *Boards) FocusPrev(w http.ResponseWriter, r *http.Request) {
	b.stepFocus(w, r, false)
}

// Sort reorders one column by vote totals (leaders rank by group sum)
// and broadcasts the refreshed column to every viewer. Sorting is
// refused while blind collection hides cards: persisting vote order
// would leak the ranking through card positions despite hidden counts.
func (b *Boards) Sort(w http.ResponseWriter, r *http.Request) {
	board, ok := b.facilitatorBoard(w, r)
	if !ok {
		return
	}
	if board.CardsHidden {
		writeFlash(w, http.StatusForbidden, "Reveal cards before sorting.")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Could not read the form.")
		return
	}
	columnID, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get("column_id")), 10, 64)
	if err != nil {
		writeFlash(w, http.StatusUnprocessableEntity, "Column is invalid.")
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
	if err := b.store.SortColumnByVotes(r.Context(), column.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "sort column", http.StatusInternalServerError)
		return
	}
	board, err = b.store.GetBoardByPublicID(r.Context(), board.PublicID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "load board", http.StatusInternalServerError)
		return
	}
	// The sort renders its column, so an expiry noticed here broadcasts
	// before that refresh; the conditional disarm keeps it to one
	// broadcast across concurrent readers.
	b.checkTimerExpiry(r, board)
	voted, err := b.votedSet(r.Context(), b.participant(r, board))
	if err != nil {
		http.Error(w, "load votes", http.StatusInternalServerError)
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
	w.WriteHeader(http.StatusOK)
}
