package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// maxCardBody caps card text so one row cannot bloat the board. The
// handlers reject longer input with 422 before it reaches the store;
// the store enforces the same bound as a backstop.
const maxCardBody = 2000

func cardBodyOK(body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("%w: card body must not be empty", ErrInvalidInput)
	}
	if len([]rune(body)) > maxCardBody {
		return fmt.Errorf("%w: card body exceeds %d characters", ErrInvalidInput, maxCardBody)
	}
	return nil
}

func scanCardRow(s rowScanner) (*Card, error) {
	var c Card
	var groupID sql.NullInt64
	var discussed int
	var createdAt string
	err := s.Scan(
		&c.ID, &c.ColumnID, &c.BoardID, &c.Body, &c.AuthorName, &c.Votes,
		&groupID, &discussed, &c.Position, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	if groupID.Valid {
		id := groupID.Int64
		c.GroupID = &id
	}
	c.Discussed = discussed != 0
	created, err := parseTimestamp(createdAt)
	if err != nil {
		return nil, err
	}
	c.CreatedAt = created
	return &c, nil
}

// GetBoardByID fetches a board by its numeric id. Card routes carry no
// share id in the URL, so they resolve the board through the card.
func (s *Store) GetBoardByID(ctx context.Context, boardID int64) (*Board, error) {
	board, err := scanBoard(s.db.QueryRowContext(ctx,
		`SELECT `+boardColumns+` FROM boards WHERE id = ?`, boardID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return board, err
}

// GetColumn fetches one column by id.
func (s *Store) GetColumn(ctx context.Context, columnID int64) (*Column, error) {
	var col Column
	err := s.db.QueryRowContext(ctx,
		`SELECT id, board_id, title, color, position FROM columns WHERE id = ?`,
		columnID).Scan(&col.ID, &col.BoardID, &col.Title, &col.Color, &col.Position)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find column: %w", err)
	}
	return &col, nil
}

// ListCards returns one column's cards in display order: position first,
// id breaking ties so equal positions stay stable.
func (s *Store) ListCards(ctx context.Context, columnID int64) ([]Card, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cardColumns+` FROM cards
		 WHERE column_id = ? ORDER BY position ASC, id ASC`, columnID)
	if err != nil {
		return nil, fmt.Errorf("list cards: %w", err)
	}
	defer rows.Close()
	var out []Card
	for rows.Next() {
		card, err := scanCardRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan card: %w", err)
		}
		out = append(out, *card)
	}
	return out, rows.Err()
}

// ListCardsByBoard returns every card on a board in display order,
// grouped by the caller per column. One round-trip backs the board
// shell, join, and columns fragment instead of one query per column.
func (s *Store) ListCardsByBoard(ctx context.Context, boardID int64) (map[int64][]Card, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cardColumns+` FROM cards
		 WHERE board_id = ? ORDER BY column_id ASC, position ASC, id ASC`, boardID)
	if err != nil {
		return nil, fmt.Errorf("list cards by board: %w", err)
	}
	defer rows.Close()
	out := map[int64][]Card{}
	for rows.Next() {
		card, err := scanCardRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan card: %w", err)
		}
		out[card.ColumnID] = append(out[card.ColumnID], *card)
	}
	return out, rows.Err()
}

// UpdateCardBody replaces a card's text. Votes, placement, grouping,
// and authorship stay untouched.
func (s *Store) UpdateCardBody(ctx context.Context, cardID int64, body string) (*Card, error) {
	if err := cardBodyOK(body); err != nil {
		return nil, err
	}
	var card *Card
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		var one int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM cards WHERE id = ?`, cardID).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find card: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET body = ? WHERE id = ?`, body, cardID); err != nil {
			return fmt.Errorf("update card: %w", err)
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

// DeleteCard removes a card and compacts the column behind it so the
// remaining positions stay dense. Votes and comments on the card vanish
// through the foreign-key cascades. The returned snapshot lets callers
// broadcast the removal without a second lookup, and the affected
// columns tell callers which columns to refresh: a grouped delete
// renests promoted members and retotals leaders, so the card's own
// column plus its leader's and members' columns all change. A lone
// ungrouped card reports no columns; its out-of-band removal is
// enough. The ids sort ascending for deterministic broadcasts.
func (s *Store) DeleteCard(ctx context.Context, cardID int64) (*Card, []int64, error) {
	var card *Card
	var affected []int64
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		snapshot, err := scanCardRow(tx.QueryRowContext(ctx,
			`SELECT `+cardColumns+` FROM cards WHERE id = ?`, cardID))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find card: %w", err)
		}
		seen := map[int64]bool{}
		add := func(id int64) {
			if !seen[id] {
				seen[id] = true
				affected = append(affected, id)
			}
		}
		grouped := false
		if snapshot.GroupID != nil {
			grouped = true
			var leaderColumn int64
			if err := tx.QueryRowContext(ctx,
				`SELECT column_id FROM cards WHERE id = ?`, *snapshot.GroupID).Scan(&leaderColumn); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("find group leader: %w", err)
				}
			} else {
				add(leaderColumn)
			}
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT DISTINCT column_id FROM cards WHERE group_id = ? ORDER BY column_id ASC`, cardID)
		if err != nil {
			return fmt.Errorf("find group members: %w", err)
		}
		for rows.Next() {
			var memberColumn int64
			if err := rows.Scan(&memberColumn); err != nil {
				rows.Close()
				return fmt.Errorf("scan member column: %w", err)
			}
			grouped = true
			add(memberColumn)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("find group members: %w", err)
		}
		if grouped {
			add(snapshot.ColumnID)
		}
		sort.Slice(affected, func(i, j int) bool { return affected[i] < affected[j] })
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM cards WHERE id = ?`, cardID); err != nil {
			return fmt.Errorf("delete card: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET position = position - 1
			 WHERE column_id = ? AND position > ?`,
			snapshot.ColumnID, snapshot.Position); err != nil {
			return fmt.Errorf("compact column: %w", err)
		}
		card = snapshot
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return card, affected, nil
}
