package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// validPhases is the board lifecycle. Any transition is allowed; the
// nudge to move forward lives in the facilitation UI, not here.
var validPhases = map[string]bool{
	"collect": true,
	"vote":    true,
	"discuss": true,
	"done":    true,
}

// SetPhase moves a board to a new lifecycle value. Unknown values are
// rejected; any transition between known values is allowed.
func (s *Store) SetPhase(ctx context.Context, boardID int64, phase string) (*Board, error) {
	if !validPhases[phase] {
		return nil, fmt.Errorf("%w: unknown phase %q", ErrInvalidInput, phase)
	}
	var board *Board
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		if err := boardExists(ctx, tx, boardID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE boards SET phase = ? WHERE id = ?`, phase, boardID); err != nil {
			return fmt.Errorf("set phase: %w", err)
		}
		var err error
		board, err = scanBoardRow(tx.QueryRowContext(ctx,
			`SELECT `+boardColumns+` FROM boards WHERE id = ?`, boardID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return board, nil
}

var validBoardFlagColumns = map[string]bool{
	"voting_locked": true,
	"cards_locked":  true,
	"cards_hidden":  true,
}

// setBoardFlag flips one INTEGER flag column on a board and returns the
// refreshed row.
func (s *Store) setBoardFlag(ctx context.Context, boardID int64, column string, value bool) (*Board, error) {
	if !validBoardFlagColumns[column] {
		return nil, fmt.Errorf("%w: unknown board flag %q", ErrInvalidInput, column)
	}
	flag := 0
	if value {
		flag = 1
	}
	var board *Board
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		if err := boardExists(ctx, tx, boardID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE boards SET `+column+` = ? WHERE id = ?`, flag, boardID); err != nil {
			return fmt.Errorf("set %s: %w", column, err)
		}
		var err error
		board, err = scanBoardRow(tx.QueryRowContext(ctx,
			`SELECT `+boardColumns+` FROM boards WHERE id = ?`, boardID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return board, nil
}

// SetVotingLocked opens or closes the voting window. Voting also
// requires the voting lifecycle value; the two gates compose.
func (s *Store) SetVotingLocked(ctx context.Context, boardID int64, locked bool) (*Board, error) {
	return s.setBoardFlag(ctx, boardID, "voting_locked", locked)
}

// SetCardsLocked opens or closes adding cards.
func (s *Store) SetCardsLocked(ctx context.Context, boardID int64, locked bool) (*Board, error) {
	return s.setBoardFlag(ctx, boardID, "cards_locked", locked)
}

// SetCardsHidden hides card bodies from non-facilitators (blind
// collection) or reveals them again. Only the reveal direction has a
// route; hiding is seeded by tests and tooling.
func (s *Store) SetCardsHidden(ctx context.Context, boardID int64, hidden bool) (*Board, error) {
	return s.setBoardFlag(ctx, boardID, "cards_hidden", hidden)
}

// SetTimerEndsAt arms the board timer (nil disarms it). The stored
// instant is the expiry; the server owns it and clients only render it.
func (s *Store) SetTimerEndsAt(ctx context.Context, boardID int64, endsAt *time.Time) (*Board, error) {
	var board *Board
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		if err := boardExists(ctx, tx, boardID); err != nil {
			return err
		}
		var value any
		if endsAt != nil {
			value = endsAt.UTC().Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE boards SET timer_ends_at = ? WHERE id = ?`, value, boardID); err != nil {
			return fmt.Errorf("set timer: %w", err)
		}
		var err error
		board, err = scanBoardRow(tx.QueryRowContext(ctx,
			`SELECT `+boardColumns+` FROM boards WHERE id = ?`, boardID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return board, nil
}

// ArmedTimer is one board with a persisted timer instant, used to
// restore in-memory expiry fires after a restart.
type ArmedTimer struct {
	BoardID  int64
	PublicID string
	EndsAt   time.Time
}

// ListArmedTimers returns every board holding a timer instant, oldest
// expiry first. Callers compare against now themselves: future
// instants re-arm, past ones surface on the next board read.
func (s *Store) ListArmedTimers(ctx context.Context) ([]ArmedTimer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, public_id, timer_ends_at FROM boards
		 WHERE timer_ends_at IS NOT NULL ORDER BY timer_ends_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list armed timers: %w", err)
	}
	defer rows.Close()
	var out []ArmedTimer
	for rows.Next() {
		var timer ArmedTimer
		var endsAt string
		if err := rows.Scan(&timer.BoardID, &timer.PublicID, &endsAt); err != nil {
			return nil, fmt.Errorf("scan armed timer: %w", err)
		}
		instant, err := parseTimestamp(endsAt)
		if err != nil {
			return nil, err
		}
		timer.EndsAt = instant
		out = append(out, timer)
	}
	return out, rows.Err()
}

// DisarmTimer clears the armed timer only when it still holds the
// expected instant, reporting whether this caller won the clear. The
// instant predicate makes concurrent firers (ticker plus board read)
// converge on exactly one expiry broadcast, and a re-arm landing
// between the read and the clear keeps its own instant instead of
// being disarmed by the stale fire.
func (s *Store) DisarmTimer(ctx context.Context, boardID int64, endsAt time.Time) (bool, error) {
	var armed bool
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		var stored sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT timer_ends_at FROM boards WHERE id = ?`, boardID).Scan(&stored); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read timer: %w", err)
		}
		if !stored.Valid {
			return nil
		}
		have, err := parseTimestamp(stored.String)
		if err != nil {
			return err
		}
		if !have.Equal(endsAt) {
			return nil
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE boards SET timer_ends_at = NULL
			 WHERE id = ? AND timer_ends_at = ?`,
			boardID, stored.String)
		if err != nil {
			return fmt.Errorf("disarm timer: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("timer rows affected: %w", err)
		}
		armed = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return armed, nil
}

// SetFocus spotlights a card for every viewer (nil clears the
// spotlight). The card must live on the same board.
func (s *Store) SetFocus(ctx context.Context, boardID int64, cardID *int64) (*Board, error) {
	var board *Board
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		if err := boardExists(ctx, tx, boardID); err != nil {
			return err
		}
		var value any
		if cardID != nil {
			var cardBoard int64
			if err := tx.QueryRowContext(ctx,
				`SELECT board_id FROM cards WHERE id = ?`, *cardID).Scan(&cardBoard); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return fmt.Errorf("find focus card: %w", err)
			}
			if cardBoard != boardID {
				return ErrNotFound
			}
			value = *cardID
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE boards SET focused_card_id = ? WHERE id = ?`, value, boardID); err != nil {
			return fmt.Errorf("set focus: %w", err)
		}
		var err error
		board, err = scanBoardRow(tx.QueryRowContext(ctx,
			`SELECT `+boardColumns+` FROM boards WHERE id = ?`, boardID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return board, nil
}

// SetDiscussed marks a card discussed (or reopens it). The discuss
// queue skips discussed cards.
func (s *Store) SetDiscussed(ctx context.Context, cardID int64, discussed bool) (*Card, error) {
	flag := 0
	if discussed {
		flag = 1
	}
	var card *Card
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE cards SET discussed = ? WHERE id = ?`, flag, cardID)
		if err != nil {
			return fmt.Errorf("set discussed: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("discussed rows affected: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		scanned, scanErr := scanCardRow(tx.QueryRowContext(ctx,
			`SELECT `+cardColumns+` FROM cards WHERE id = ?`, cardID))
		if scanErr != nil {
			return scanErr
		}
		card = scanned
		return nil
	})
	if err != nil {
		return nil, err
	}
	return card, nil
}

// SetGroup nests a card under a leader (nil ungroups it). Grouping
// under a member resolves to that member's leader, and the moving
// card's own members merge into that resolved leader in the same
// transaction, so chains flatten and cycles cannot form: resolving
// back to the card itself is rejected. The leader must be a real
// card on the same board.
func (s *Store) SetGroup(ctx context.Context, cardID int64, leaderID *int64) (*Card, error) {
	var card *Card
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		var boardID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT board_id FROM cards WHERE id = ?`, cardID).Scan(&boardID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find card: %w", err)
		}
		var value any
		if leaderID != nil {
			resolved, err := resolveLeader(ctx, tx, boardID, cardID, *leaderID)
			if err != nil {
				return err
			}
			value = resolved
			// Merge: re-home the moving card's members to the
			// resolved leader, keeping every group link one level
			// deep. The resolver guarantees resolved is neither
			// the card itself nor one of its members.
			if _, err := tx.ExecContext(ctx,
				`UPDATE cards SET group_id = ? WHERE group_id = ?`, resolved, cardID); err != nil {
				return fmt.Errorf("merge group members: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET group_id = ? WHERE id = ?`, value, cardID); err != nil {
			return fmt.Errorf("set group: %w", err)
		}
		var err error
		card, err = scanCardRow(tx.QueryRowContext(ctx,
			`SELECT `+cardColumns+` FROM cards WHERE id = ?`, cardID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return card, nil
}

// resolveLeader validates a grouping target and flattens chains: a
// member target resolves to its leader. It runs inside the caller's
// transaction.
func resolveLeader(ctx context.Context, tx DBTX, boardID, cardID, leaderID int64) (int64, error) {
	if leaderID == cardID {
		return 0, fmt.Errorf("%w: a card cannot group under itself", ErrInvalidInput)
	}
	current := leaderID
	visited := map[int64]bool{cardID: true}
	for {
		if visited[current] {
			return 0, fmt.Errorf("%w: grouping would cycle", ErrInvalidInput)
		}
		visited[current] = true
		var leaderBoard int64
		var parent sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT board_id, group_id FROM cards WHERE id = ?`, current).
			Scan(&leaderBoard, &parent); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, ErrNotFound
			}
			return 0, fmt.Errorf("find group leader: %w", err)
		}
		if leaderBoard != boardID {
			return 0, ErrNotFound
		}
		if !parent.Valid {
			return current, nil
		}
		if parent.Int64 == cardID {
			return 0, fmt.Errorf("%w: grouping would cycle", ErrInvalidInput)
		}
		current = parent.Int64
	}
}

// GroupVoteSums totals votes per group leader: each leader's own votes
// plus every member's, computed here on read and never stored. Only
// leaders with at least one member appear as keys.
func (s *Store) GroupVoteSums(ctx context.Context, boardID int64) (map[int64]int, error) {
	sums := map[int64]int{}
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		if err := boardExists(ctx, tx, boardID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT id, votes, group_id FROM cards WHERE board_id = ?`, boardID)
		if err != nil {
			return fmt.Errorf("read group votes: %w", err)
		}
		defer rows.Close()
		own := map[int64]int{}
		leaderOf := map[int64]int64{}
		for rows.Next() {
			var id int64
			var votes int
			var groupID sql.NullInt64
			if err := rows.Scan(&id, &votes, &groupID); err != nil {
				return fmt.Errorf("scan group votes: %w", err)
			}
			own[id] = votes
			if groupID.Valid {
				leaderOf[id] = groupID.Int64
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read group votes: %w", err)
		}
		for member, leader := range leaderOf {
			if _, ok := own[leader]; !ok {
				continue
			}
			sums[leader] += own[member]
		}
		for leader := range sums {
			sums[leader] += own[leader]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sums, nil
}

// SortColumnByVotes reorders a column by vote totals (leaders rank by
// their group sum), ties breaking by id, repacking positions densely
// from zero. Grouped members pin directly after their leader so groups
// never split apart.
func (s *Store) SortColumnByVotes(ctx context.Context, columnID int64) error {
	return s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		var boardID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT board_id FROM columns WHERE id = ?`, columnID).Scan(&boardID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find column: %w", err)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT c.id, c.group_id, c.votes, COALESCE(SUM(m.votes), 0)
			 FROM cards c LEFT JOIN cards m ON m.group_id = c.id
			 WHERE c.column_id = ?
			 GROUP BY c.id`, columnID)
		if err != nil {
			return fmt.Errorf("order column: %w", err)
		}
		type entry struct {
			id      int64
			groupID sql.NullInt64
			total   int
		}
		var entries []entry
		own := map[int64]int{}
		leaderOf := map[int64]int64{}
		for rows.Next() {
			var e entry
			var votes, memberVotes int
			if err := rows.Scan(&e.id, &e.groupID, &votes, &memberVotes); err != nil {
				rows.Close()
				return fmt.Errorf("scan column order: %w", err)
			}
			e.total = votes + memberVotes
			entries = append(entries, e)
			own[e.id] = votes
			if e.groupID.Valid {
				leaderOf[e.id] = e.groupID.Int64
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("order column: %w", err)
		}
		// Members rank with their leader's total and sort immediately
		// after their leader (by member id), so groups stay together.
		leaderTotal := map[int64]int{}
		for _, e := range entries {
			if !e.groupID.Valid {
				leaderTotal[e.id] = e.total
			}
		}
		rankOf := func(e entry) (int, int64, int64) {
			if e.groupID.Valid {
				if t, ok := leaderTotal[e.groupID.Int64]; ok {
					return t, e.groupID.Int64, e.id
				}
				return own[e.id], e.id, e.id
			}
			return e.total, e.id, -1
		}
		sort.Slice(entries, func(i, j int) bool {
			ti, li, mi := rankOf(entries[i])
			tj, lj, mj := rankOf(entries[j])
			if ti != tj {
				return ti > tj
			}
			if li != lj {
				return li < lj
			}
			return mi < mj
		})
		var ids []int64
		for _, e := range entries {
			ids = append(ids, e.id)
		}
		for position, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`UPDATE cards SET position = ? WHERE id = ?`, position, id); err != nil {
				return fmt.Errorf("repack column: %w", err)
			}
		}
		return nil
	})
}

// StepFocus advances the discuss queue: it marks the currently focused
// card discussed, then moves focus to the next (forward) or previous
// (!forward) undiscussed top-level card in vote-total order. Members
// never take focus; their leader carries the group. Draining the queue
// clears focus. It returns the refreshed board plus the ids whose
// rendering changed (marked and newly focused cards), so callers can
// refresh exactly the affected columns.
func (s *Store) StepFocus(ctx context.Context, boardID int64, forward bool) (*Board, []int64, error) {
	var board *Board
	var affected []int64
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		var focused sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT focused_card_id FROM boards WHERE id = ?`, boardID).Scan(&focused); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find board: %w", err)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT c.id, c.votes + COALESCE(SUM(m.votes), 0) AS total
			 FROM cards c LEFT JOIN cards m ON m.group_id = c.id
			 WHERE c.board_id = ? AND c.group_id IS NULL AND c.discussed = 0
			 GROUP BY c.id
			 ORDER BY total DESC, c.id ASC`, boardID)
		if err != nil {
			return fmt.Errorf("order discuss queue: %w", err)
		}
		var queue []int64
		for rows.Next() {
			var id int64
			var total int
			if err := rows.Scan(&id, &total); err != nil {
				rows.Close()
				return fmt.Errorf("scan discuss queue: %w", err)
			}
			queue = append(queue, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("order discuss queue: %w", err)
		}
		current := int64(0)
		if focused.Valid {
			current = focused.Int64
		}
		at, found := 0, false
		for i, id := range queue {
			if id == current {
				at, found = i, true
				break
			}
		}
		var next *int64
		if !found {
			if len(queue) > 0 {
				if forward {
					next = &queue[0]
				} else {
					last := queue[len(queue)-1]
					next = &last
				}
			}
		} else if forward {
			if at+1 < len(queue) {
				following := queue[at+1]
				next = &following
			}
		} else {
			if at > 0 {
				previous := queue[at-1]
				next = &previous
			}
		}
		if found {
			if _, err := tx.ExecContext(ctx,
				`UPDATE cards SET discussed = 1 WHERE id = ? AND board_id = ?`,
				current, boardID); err != nil {
				return fmt.Errorf("mark discussed: %w", err)
			}
			affected = append(affected, current)
		} else if focused.Valid {
			// Stale focus (member, discussed, or deleted card): mark it
			// discussed when it still exists on this board so it never
			// lingers undiscussed while focus jumps elsewhere.
			res, err := tx.ExecContext(ctx,
				`UPDATE cards SET discussed = 1 WHERE id = ? AND board_id = ? AND discussed = 0`,
				focused.Int64, boardID)
			if err != nil {
				return fmt.Errorf("mark stale focus discussed: %w", err)
			}
			if n, err := res.RowsAffected(); err != nil {
				return fmt.Errorf("stale focus rows affected: %w", err)
			} else if n > 0 {
				affected = append(affected, focused.Int64)
			}
		}
		var value any
		if next != nil {
			value = *next
			affected = append(affected, *next)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE boards SET focused_card_id = ? WHERE id = ?`, value, boardID); err != nil {
			return fmt.Errorf("advance focus: %w", err)
		}
		refreshed, scanErr := scanBoardRow(tx.QueryRowContext(ctx,
			`SELECT `+boardColumns+` FROM boards WHERE id = ?`, boardID))
		if scanErr != nil {
			return scanErr
		}
		board = refreshed
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return board, affected, nil
}
