package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Action mirrors the actions row. CarriedFrom holds the source action id
// when the item was carried over from an earlier board, nil otherwise.
// AuthorName records who created the item, so later edits can tell the
// author apart from other participants.
type Action struct {
	ID          int64
	BoardID     int64
	Text        string
	Owner       string
	AuthorName  string
	Done        bool
	CarriedFrom *int64
	CreatedAt   time.Time
}

// BoardWithStats pairs a board with its live card and open-action counts
// for dashboard lists.
type BoardWithStats struct {
	Board
	CardCount       int
	OpenActionCount int
}

// maxActionText and maxActionOwner bound action rows; longer input is
// rejected so one row cannot bloat the board. Handlers pre-check the
// same bounds; the store re-checks as a backstop.
const (
	maxActionText   = 500
	maxActionOwner  = 100
	maxActionAuthor = 100
	// maxBoardName mirrors the handler bound so DuplicateBoard's
	// derived " (copy)" name never exceeds what CreateBoard accepts.
	maxBoardName = 200
)

const actionColumns = `id, board_id, text, owner, author_name, done, carried_from, created_at`

func scanActionRow(s rowScanner) (*Action, error) {
	var a Action
	var carriedFrom sql.NullInt64
	var done int
	var createdAt string
	if err := s.Scan(
		&a.ID, &a.BoardID, &a.Text, &a.Owner, &a.AuthorName, &done, &carriedFrom, &createdAt,
	); err != nil {
		return nil, err
	}
	a.Done = done != 0
	if carriedFrom.Valid {
		id := carriedFrom.Int64
		a.CarriedFrom = &id
	}
	created, err := parseTimestamp(createdAt)
	if err != nil {
		return nil, err
	}
	a.CreatedAt = created
	return &a, nil
}

// ListBoardsWithStats returns every board, newest first, with card and
// open-action counts computed in the same query.
func (s *Store) ListBoardsWithStats(ctx context.Context) ([]BoardWithStats, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+boardColumns+`,
			(SELECT COUNT(*) FROM cards c WHERE c.board_id = b.id),
			(SELECT COUNT(*) FROM actions a WHERE a.board_id = b.id AND a.done = 0)
		 FROM boards b ORDER BY b.created_at DESC, b.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list boards: %w", err)
	}
	defer rows.Close()
	out := []BoardWithStats{}
	for rows.Next() {
		var item BoardWithStats
		var timerEndsAt, archivedAt sql.NullString
		var focusedCardID sql.NullInt64
		var createdAt string
		var votingLocked, cardsLocked, cardsHidden int
		b := &item.Board
		if err := rows.Scan(
			&b.ID, &b.PublicID, &b.Name, &b.Context, &b.FacilitatorTokenHash,
			&b.Phase, &b.VotesPerPerson, &timerEndsAt, &votingLocked, &cardsLocked,
			&cardsHidden, &focusedCardID, &archivedAt, &createdAt,
			&item.CardCount, &item.OpenActionCount,
		); err != nil {
			return nil, fmt.Errorf("scan board: %w", err)
		}
		b.VotingLocked = votingLocked != 0
		b.CardsLocked = cardsLocked != 0
		b.CardsHidden = cardsHidden != 0
		if timerEndsAt.Valid {
			t, err := parseTimestamp(timerEndsAt.String)
			if err != nil {
				return nil, err
			}
			b.TimerEndsAt = &t
		}
		if focusedCardID.Valid {
			id := focusedCardID.Int64
			b.FocusedCardID = &id
		}
		if archivedAt.Valid {
			t, err := parseTimestamp(archivedAt.String)
			if err != nil {
				return nil, err
			}
			b.ArchivedAt = &t
		}
		created, err := parseTimestamp(createdAt)
		if err != nil {
			return nil, err
		}
		b.CreatedAt = created
		// Dashboard lists never need the facilitator hash; redact it at
		// the store boundary so callers cannot render or serialize it.
		b.FacilitatorTokenHash = ""
		out = append(out, item)
	}
	return out, rows.Err()
}

// RotateFacilitatorToken swaps the facilitator token hash only when
// the stored hash still matches expectedOldHash, reporting whether
// this caller won the rotation. The hash predicate keeps concurrent
// rotations from minting two live tokens: exactly one raw value stays
// valid, and the loser learns its old token is already dead.
func (s *Store) RotateFacilitatorToken(ctx context.Context, boardID int64, expectedOldHash, newHash string) (bool, error) {
	if strings.TrimSpace(newHash) == "" {
		return false, fmt.Errorf("%w: facilitator token must not be empty", ErrInvalidInput)
	}
	var rotated bool
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE boards SET facilitator_token_hash = ?
			 WHERE id = ? AND facilitator_token_hash = ?`,
			newHash, boardID, expectedOldHash)
		if err != nil {
			return fmt.Errorf("rotate facilitator token: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("facilitator token rows affected: %w", err)
		}
		rotated = n > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return rotated, nil
}

// SetArchived flags (or unflags) a board. Archive never deletes content;
// columns, cards, votes, and actions stay queryable.
func (s *Store) SetArchived(ctx context.Context, boardID int64, archived bool) (*Board, error) {
	var board *Board
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		if err := boardExists(ctx, tx, boardID); err != nil {
			return err
		}
		var archivedAt any
		if archived {
			archivedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE boards SET archived_at = ? WHERE id = ?`, archivedAt, boardID); err != nil {
			return fmt.Errorf("set archived: %w", err)
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

// DuplicateBoard copies a board's columns in order, optionally carrying
// its open actions (recording provenance via carried_from). Cards, votes,
// comments, and kudos never copy. The copy starts fresh: default phase,
// no locks, no timer, no focus, no archive flag.
func (s *Store) DuplicateBoard(ctx context.Context, srcBoardID int64, newFacilitatorToken string, carryOpenActions bool) (*Board, error) {
	if strings.TrimSpace(newFacilitatorToken) == "" {
		return nil, fmt.Errorf("%w: facilitator token must not be empty", ErrInvalidInput)
	}
	var dup *Board
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		dup = nil
		publicID, genErr := GenerateToken()
		if genErr != nil {
			return nil, genErr
		}
		err = s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
			var name, boardContext string
			var votesPerPerson int
			if err := tx.QueryRowContext(ctx,
				`SELECT name, context, votes_per_person FROM boards WHERE id = ?`,
				srcBoardID).Scan(&name, &boardContext, &votesPerPerson); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return fmt.Errorf("find source board: %w", err)
			}
			copyName := name + " (copy)"
			if runes := []rune(copyName); len(runes) > maxBoardName {
				copyName = string(runes[:maxBoardName])
			}
			res, err := tx.ExecContext(ctx,
				`INSERT INTO boards (public_id, name, context, facilitator_token_hash, votes_per_person)
			 VALUES (?, ?, ?, ?, ?)`,
				publicID, copyName, boardContext,
				HashToken(newFacilitatorToken), votesPerPerson)
			if err != nil {
				return fmt.Errorf("insert duplicate board: %w", err)
			}
			newID, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("duplicate board id: %w", err)
			}
			cols, err := tx.QueryContext(ctx,
				`SELECT title, color, position FROM columns
			 WHERE board_id = ? ORDER BY position ASC, id ASC`, srcBoardID)
			if err != nil {
				return fmt.Errorf("read source columns: %w", err)
			}
			type colSeed struct{ title, color string }
			var seeds []colSeed
			var positions []int
			for cols.Next() {
				var seed colSeed
				var pos int
				if err := cols.Scan(&seed.title, &seed.color, &pos); err != nil {
					cols.Close()
					return fmt.Errorf("scan source column: %w", err)
				}
				seeds = append(seeds, seed)
				positions = append(positions, pos)
			}
			cols.Close()
			if err := cols.Err(); err != nil {
				return fmt.Errorf("read source columns: %w", err)
			}
			for i, seed := range seeds {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO columns (board_id, title, color, position)
				 VALUES (?, ?, ?, ?)`,
					newID, seed.title, seed.color, positions[i]); err != nil {
					return fmt.Errorf("copy column: %w", err)
				}
			}
			if carryOpenActions {
				actions, err := tx.QueryContext(ctx,
					`SELECT id, text, owner, author_name FROM actions
				 WHERE board_id = ? AND done = 0 ORDER BY id ASC`, srcBoardID)
				if err != nil {
					return fmt.Errorf("read source actions: %w", err)
				}
				type actionSeed struct {
					id                  int64
					text, owner, author string
				}
				var actionSeeds []actionSeed
				for actions.Next() {
					var seed actionSeed
					if err := actions.Scan(&seed.id, &seed.text, &seed.owner, &seed.author); err != nil {
						actions.Close()
						return fmt.Errorf("scan source action: %w", err)
					}
					actionSeeds = append(actionSeeds, seed)
				}
				actions.Close()
				if err := actions.Err(); err != nil {
					return fmt.Errorf("read source actions: %w", err)
				}
				for _, seed := range actionSeeds {
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO actions (board_id, text, owner, author_name, done, carried_from)
					 VALUES (?, ?, ?, ?, 0, ?)`,
						newID, seed.text, seed.owner, seed.author, seed.id); err != nil {
						return fmt.Errorf("carry action: %w", err)
					}
				}
			}
			dup, err = scanBoardRow(tx.QueryRowContext(ctx,
				`SELECT `+boardColumns+` FROM boards WHERE id = ?`, newID))
			return err
		})
		if err == nil {
			break
		}
		if !isUniqueViolation(err) || attempt == 2 {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return dup, nil
}

// CreateAction adds an action item to a board. author names the
// participant who created it; carriedFrom records the source action
// when the item arrives via carry-over, nil otherwise.
func (s *Store) CreateAction(ctx context.Context, boardID int64, text, owner, author string, carriedFrom *int64) (*Action, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: action text must not be empty", ErrInvalidInput)
	}
	if len([]rune(text)) > maxActionText {
		return nil, fmt.Errorf("%w: action text exceeds %d characters", ErrInvalidInput, maxActionText)
	}
	if len([]rune(owner)) > maxActionOwner {
		return nil, fmt.Errorf("%w: action owner exceeds %d characters", ErrInvalidInput, maxActionOwner)
	}
	if len([]rune(author)) > maxActionAuthor {
		return nil, fmt.Errorf("%w: action author exceeds %d characters", ErrInvalidInput, maxActionAuthor)
	}
	var action *Action
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		if err := boardExists(ctx, tx, boardID); err != nil {
			return err
		}
		var carried any
		if carriedFrom != nil {
			carried = *carriedFrom
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO actions (board_id, text, owner, author_name, carried_from)
			 VALUES (?, ?, ?, ?, ?)`,
			boardID, text, owner, author, carried)
		if err != nil {
			return fmt.Errorf("insert action: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("action id: %w", err)
		}
		action, err = scanActionRow(tx.QueryRowContext(ctx,
			`SELECT `+actionColumns+` FROM actions WHERE id = ?`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return action, nil
}

// SetActionDone marks an action item done or reopens it.
// Callers must verify the returned board scope: the id is global,
// so check Action.BoardID against the request board to avoid
// cross-board (IDOR) mutations.
func (s *Store) SetActionDone(ctx context.Context, actionID int64, done bool) error {
	return s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		flag := 0
		if done {
			flag = 1
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE actions SET done = ? WHERE id = ?`, flag, actionID)
		if err != nil {
			return fmt.Errorf("set action done: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("action rows affected: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ListOpenActions returns a board's unfinished action items in id order.
func (s *Store) ListOpenActions(ctx context.Context, boardID int64) ([]Action, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+actionColumns+` FROM actions
		 WHERE board_id = ? AND done = 0 ORDER BY id ASC`, boardID)
	if err != nil {
		return nil, fmt.Errorf("list open actions: %w", err)
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

// GetParticipantByToken resolves a participant cookie token to its row.
// Only the token hash is compared; raw tokens never persist. Lookup is
// hash-equality in SQLite over 128-bit tokens; timing leak accepted for v1.
func (s *Store) GetParticipantByToken(ctx context.Context, boardID int64, rawToken string) (*Participant, error) {
	var p Participant
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, board_id, name, token_hash, created_at FROM participants
		 WHERE board_id = ? AND token_hash = ?`,
		boardID, HashToken(rawToken)).Scan(
		&p.ID, &p.BoardID, &p.Name, &p.TokenHash, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find participant: %w", err)
	}
	created, err := parseTimestamp(createdAt)
	if err != nil {
		return nil, err
	}
	p.CreatedAt = created
	return &p, nil
}

// CreateParticipantWithSuffix registers a display name on a board,
// suffixing duplicates as name-2, name-3, and so on until the insert
// succeeds. The UNIQUE backstop keeps racing joins distinct.
func (s *Store) CreateParticipantWithSuffix(ctx context.Context, boardID int64, baseName, rawToken string) (*Participant, error) {
	trimmed := strings.TrimSpace(baseName)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: participant name must not be empty", ErrInvalidInput)
	}
	if strings.TrimSpace(rawToken) == "" {
		return nil, fmt.Errorf("%w: participant token must not be empty", ErrInvalidInput)
	}
	candidate := trimmed
	for n := 2; ; n++ {
		p, err := s.CreateParticipant(ctx, boardID, candidate, rawToken)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrDuplicateName) {
			return nil, err
		}
		if n > 10000 {
			return nil, fmt.Errorf("%w: no free display name near %q", ErrInvalidInput, trimmed)
		}
		candidate = fmt.Sprintf("%s-%d", trimmed, n)
	}
}
