package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Comment mirrors the comments row: one remark under a card.
type Comment struct {
	ID         int64
	CardID     int64
	BoardID    int64
	Body       string
	AuthorName string
	CreatedAt  time.Time
}

// Kudo mirrors the kudos row: one entry on the board-level wall.
type Kudo struct {
	ID        int64
	BoardID   int64
	To        string
	Body      string
	From      string
	CreatedAt time.Time
}

// Input bounds for discussion entries. Handlers reject longer input
// with 422 before it reaches the store; the store enforces the same
// bounds as a backstop.
const (
	maxCommentBody   = 1000
	maxCommentAuthor = 100
	maxKudoBody      = 500
	maxKudoTo        = 200
	maxKudoFrom      = 100
)

const commentColumns = `id, card_id, board_id, body, author_name, created_at`

const kudoColumns = `id, board_id, recipient, body, sender, created_at`

func commentOK(body, author string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("%w: comment body must not be empty", ErrInvalidInput)
	}
	if len([]rune(body)) > maxCommentBody {
		return fmt.Errorf("%w: comment body exceeds %d characters", ErrInvalidInput, maxCommentBody)
	}
	if len([]rune(author)) > maxCommentAuthor {
		return fmt.Errorf("%w: comment author exceeds %d characters", ErrInvalidInput, maxCommentAuthor)
	}
	return nil
}

func kudoOK(to, body, from string) error {
	if strings.TrimSpace(to) == "" {
		return fmt.Errorf("%w: kudo recipient must not be empty", ErrInvalidInput)
	}
	if len([]rune(to)) > maxKudoTo {
		return fmt.Errorf("%w: kudo recipient exceeds %d characters", ErrInvalidInput, maxKudoTo)
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("%w: kudo body must not be empty", ErrInvalidInput)
	}
	if len([]rune(body)) > maxKudoBody {
		return fmt.Errorf("%w: kudo body exceeds %d characters", ErrInvalidInput, maxKudoBody)
	}
	if len([]rune(from)) > maxKudoFrom {
		return fmt.Errorf("%w: kudo sender exceeds %d characters", ErrInvalidInput, maxKudoFrom)
	}
	return nil
}

func scanCommentRow(s rowScanner) (*Comment, error) {
	var c Comment
	var createdAt string
	if err := s.Scan(&c.ID, &c.CardID, &c.BoardID, &c.Body, &c.AuthorName, &createdAt); err != nil {
		return nil, err
	}
	created, err := parseTimestamp(createdAt)
	if err != nil {
		return nil, err
	}
	c.CreatedAt = created
	return &c, nil
}

func scanKudoRow(s rowScanner) (*Kudo, error) {
	var k Kudo
	var createdAt string
	if err := s.Scan(&k.ID, &k.BoardID, &k.To, &k.Body, &k.From, &createdAt); err != nil {
		return nil, err
	}
	created, err := parseTimestamp(createdAt)
	if err != nil {
		return nil, err
	}
	k.CreatedAt = created
	return &k, nil
}

// CreateComment appends a remark to a card. The board is derived from
// the card so the two can never disagree; unknown cards are ErrNotFound.
func (s *Store) CreateComment(ctx context.Context, cardID int64, body, authorName string) (*Comment, error) {
	if err := commentOK(body, authorName); err != nil {
		return nil, err
	}
	var comment *Comment
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		var boardID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT board_id FROM cards WHERE id = ?`, cardID).Scan(&boardID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find card: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO comments (card_id, board_id, body, author_name)
			 VALUES (?, ?, ?, ?)`,
			cardID, boardID, body, authorName)
		if err != nil {
			return fmt.Errorf("insert comment: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("comment id: %w", err)
		}
		comment, err = scanCommentRow(tx.QueryRowContext(ctx,
			`SELECT `+commentColumns+` FROM comments WHERE id = ?`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return comment, nil
}

// ListComments returns one card's remarks in insertion order.
func (s *Store) ListComments(ctx context.Context, cardID int64) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+commentColumns+` FROM comments
		 WHERE card_id = ? ORDER BY id ASC`, cardID)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		comment, err := scanCommentRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		out = append(out, *comment)
	}
	return out, rows.Err()
}

// ListCommentsByBoard returns every remark on a board, grouped by card
// id, so card and column renders fetch comments in one query instead
// of one per card.
func (s *Store) ListCommentsByBoard(ctx context.Context, boardID int64) (map[int64][]Comment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+commentColumns+` FROM comments
		 WHERE board_id = ? ORDER BY id ASC`, boardID)
	if err != nil {
		return nil, fmt.Errorf("list board comments: %w", err)
	}
	defer rows.Close()
	grouped := map[int64][]Comment{}
	for rows.Next() {
		comment, err := scanCommentRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		grouped[comment.CardID] = append(grouped[comment.CardID], *comment)
	}
	return grouped, rows.Err()
}

// CreateKudo pins an entry to a board's kudos wall. Unknown boards are
// ErrNotFound.
func (s *Store) CreateKudo(ctx context.Context, boardID int64, to, body, from string) (*Kudo, error) {
	if err := kudoOK(to, body, from); err != nil {
		return nil, err
	}
	var kudo *Kudo
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		if err := boardExists(ctx, tx, boardID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO kudos (board_id, recipient, body, sender)
			 VALUES (?, ?, ?, ?)`,
			boardID, to, body, from)
		if err != nil {
			return fmt.Errorf("insert kudo: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("kudo id: %w", err)
		}
		kudo, err = scanKudoRow(tx.QueryRowContext(ctx,
			`SELECT `+kudoColumns+` FROM kudos WHERE id = ?`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return kudo, nil
}

// ListKudos returns a board's wall entries in insertion order.
func (s *Store) ListKudos(ctx context.Context, boardID int64) ([]Kudo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+kudoColumns+` FROM kudos
		 WHERE board_id = ? ORDER BY id ASC`, boardID)
	if err != nil {
		return nil, fmt.Errorf("list kudos: %w", err)
	}
	defer rows.Close()
	var out []Kudo
	for rows.Next() {
		kudo, err := scanKudoRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan kudo: %w", err)
		}
		out = append(out, *kudo)
	}
	return out, rows.Err()
}

// GetKudo fetches one wall entry by id. Callers must check Kudo.BoardID
// against the request board; ids are global and the store does no scoping.
func (s *Store) GetKudo(ctx context.Context, kudoID int64) (*Kudo, error) {
	kudo, err := scanKudoRow(s.db.QueryRowContext(ctx,
		`SELECT `+kudoColumns+` FROM kudos WHERE id = ?`, kudoID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return kudo, err
}

// DeleteKudo removes one wall entry. The snapshot lets callers check
// authorship and broadcast the removal without a second lookup.
// Callers must verify snapshot.BoardID against the request board.
func (s *Store) DeleteKudo(ctx context.Context, kudoID int64) (*Kudo, error) {
	var kudo *Kudo
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		snapshot, err := scanKudoRow(tx.QueryRowContext(ctx,
			`SELECT `+kudoColumns+` FROM kudos WHERE id = ?`, kudoID))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find kudo: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM kudos WHERE id = ?`, kudoID); err != nil {
			return fmt.Errorf("delete kudo: %w", err)
		}
		kudo = snapshot
		return nil
	})
	if err != nil {
		return nil, err
	}
	return kudo, nil
}

// GetAction fetches one action item by id. Card-less routes resolve
// the board through the item; callers must check Action.BoardID
// against the request board before mutating.
func (s *Store) GetAction(ctx context.Context, actionID int64) (*Action, error) {
	action, err := scanActionRow(s.db.QueryRowContext(ctx,
		`SELECT `+actionColumns+` FROM actions WHERE id = ?`, actionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return action, err
}

// ListActions returns a board's action items, open and done, in id
// order for the wall.
func (s *Store) ListActions(ctx context.Context, boardID int64) ([]Action, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionColumns+` FROM actions
		 WHERE board_id = ? ORDER BY id ASC`, boardID)
	if err != nil {
		return nil, fmt.Errorf("list actions: %w", err)
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		action, err := scanActionRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		out = append(out, *action)
	}
	return out, rows.Err()
}

// UpdateAction applies the present fields to an action item: a nil
// text, owner, or done leaves that column alone. Unknown ids are
// ErrNotFound. Callers must verify the returned Action.BoardID.
func (s *Store) UpdateAction(ctx context.Context, actionID int64, text, owner *string, done *bool) (*Action, error) {
	if text != nil {
		if strings.TrimSpace(*text) == "" {
			return nil, fmt.Errorf("%w: action text must not be empty", ErrInvalidInput)
		}
		if len([]rune(*text)) > maxActionText {
			return nil, fmt.Errorf("%w: action text exceeds %d characters", ErrInvalidInput, maxActionText)
		}
	}
	if owner != nil && len([]rune(*owner)) > maxActionOwner {
		return nil, fmt.Errorf("%w: action owner exceeds %d characters", ErrInvalidInput, maxActionOwner)
	}
	var action *Action
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		current, err := scanActionRow(tx.QueryRowContext(ctx,
			`SELECT `+actionColumns+` FROM actions WHERE id = ?`, actionID))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find action: %w", err)
		}
		next := *current
		if text != nil {
			next.Text = *text
		}
		if owner != nil {
			next.Owner = *owner
		}
		if done != nil {
			next.Done = *done
		}
		flag := 0
		if next.Done {
			flag = 1
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE actions SET text = ?, owner = ?, done = ? WHERE id = ?`,
			next.Text, next.Owner, flag, actionID); err != nil {
			return fmt.Errorf("update action: %w", err)
		}
		action, err = scanActionRow(tx.QueryRowContext(ctx,
			`SELECT `+actionColumns+` FROM actions WHERE id = ?`, actionID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return action, nil
}

// DeleteAction removes one action item. The snapshot lets callers
// broadcast the removal without a second lookup. Callers must verify
// snapshot.BoardID against the request board.
func (s *Store) DeleteAction(ctx context.Context, actionID int64) (*Action, error) {
	var action *Action
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		snapshot, err := scanActionRow(tx.QueryRowContext(ctx,
			`SELECT `+actionColumns+` FROM actions WHERE id = ?`, actionID))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find action: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM actions WHERE id = ?`, actionID); err != nil {
			return fmt.Errorf("delete action: %w", err)
		}
		action = snapshot
		return nil
	})
	if err != nil {
		return nil, err
	}
	return action, nil
}
