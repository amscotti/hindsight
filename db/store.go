package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// Sentinel errors returned by Store mutations. Handlers map these to
// status codes plus the human flash fragment.
var (
	ErrNotFound         = errors.New("not found")
	ErrDoubleVote       = errors.New("already voted for this card")
	ErrVoteCapExceeded  = errors.New("vote limit reached")
	ErrNoVote           = errors.New("no vote to remove")
	ErrBoardMismatch    = errors.New("participant and card belong to different boards")
	ErrStalePosition    = errors.New("card moved since the drag started")
	ErrDuplicateName    = errors.New("name already taken on this board")
	ErrInvalidInput     = errors.New("invalid input")
	ErrCrossColumnBoard = errors.New("card and column belong to different boards")
	ErrVotingClosed     = errors.New("voting is not open")
)

// Board mirrors the boards row. TimerEndsAt, FocusedCardID and ArchivedAt
// are nil when unset.
type Board struct {
	ID                   int64
	PublicID             string
	Name                 string
	Context              string
	FacilitatorTokenHash string
	Phase                string
	VotesPerPerson       int
	TimerEndsAt          *time.Time
	VotingLocked         bool
	CardsLocked          bool
	CardsHidden          bool
	FocusedCardID        *int64
	ArchivedAt           *time.Time
	CreatedAt            time.Time
}

// Column mirrors the columns row.
type Column struct {
	ID       int64
	BoardID  int64
	Title    string
	Color    string
	Position int
}

// Card mirrors the cards row. GroupID is the leader card id, nil when
// the card is not grouped.
type Card struct {
	ID         int64
	ColumnID   int64
	BoardID    int64
	Body       string
	AuthorName string
	Votes      int
	GroupID    *int64
	Discussed  bool
	Position   int
	CreatedAt  time.Time
}

// Participant mirrors the participants row. Votes key to this row.
type Participant struct {
	ID        int64
	BoardID   int64
	Name      string
	TokenHash string
	CreatedAt time.Time
}

// DBTX is the transactional surface mutations run against. Both
// *sql.Conn (used by WithTx after BEGIN IMMEDIATE) and *sql.Tx satisfy it.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store wraps a *sql.DB. Every mutation runs inside WithTx.
type Store struct {
	db *sql.DB
}

// NewStore builds a Store over an open database handle.
func NewStore(sqldb *sql.DB) *Store {
	return &Store{db: sqldb}
}

// WithTx runs fn inside a BEGIN IMMEDIATE transaction, committing on a
// nil return and rolling back otherwise. IMMEDIATE reserves the write
// lock up front so concurrent writers queue on busy_timeout instead of
// failing mid-transaction.
func (s *Store) WithTx(ctx context.Context, fn func(context.Context, DBTX) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if err := fn(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		// The deferred ROLLBACK still runs (committed==false); attempt an
		// eager rollback with a non-canceled context so a request-canceled
		// ctx cannot leave the connection in-transaction, and surface its
		// outcome alongside the commit error for observability.
		if _, rbErr := conn.ExecContext(context.Background(), "ROLLBACK"); rbErr != nil {
			return fmt.Errorf("commit: %w (rollback: %v)", err, rbErr)
		}
		committed = true // eager rollback succeeded; suppress deferred retry
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// GenerateToken returns a 128-bit crypto/rand token, base64url-encoded,
// for share IDs and auth tokens.
func GenerateToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// HashToken returns the SHA-256 hex digest of a raw token. Only digests
// are persisted.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CompareTokenHash compares two hex digests in constant time.
func CompareTokenHash(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

const boardColumns = `id, public_id, name, context, facilitator_token_hash,
	phase, votes_per_person, timer_ends_at, voting_locked, cards_locked,
	cards_hidden, focused_card_id, archived_at, created_at`

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so board
// scanning is shared between point lookups and list queries.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanBoard(row *sql.Row) (*Board, error) {
	return scanBoardRow(row)
}

func scanBoardRow(s rowScanner) (*Board, error) {
	var b Board
	var timerEndsAt, archivedAt sql.NullString
	var focusedCardID sql.NullInt64
	var createdAt string
	var votingLocked, cardsLocked, cardsHidden int
	err := s.Scan(
		&b.ID, &b.PublicID, &b.Name, &b.Context, &b.FacilitatorTokenHash,
		&b.Phase, &b.VotesPerPerson, &timerEndsAt, &votingLocked, &cardsLocked,
		&cardsHidden, &focusedCardID, &archivedAt, &createdAt,
	)
	if err != nil {
		return nil, err
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
	return &b, nil
}

// CreateBoard inserts a board with a fresh random public ID and stores
// the facilitator token as a SHA-256 hex digest.
func (s *Store) CreateBoard(ctx context.Context, name, boardContext, facilitatorToken string, votesPerPerson int) (*Board, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: board name must not be empty", ErrInvalidInput)
	}
	if votesPerPerson <= 0 {
		return nil, fmt.Errorf("%w: votes per person must be positive, got %d", ErrInvalidInput, votesPerPerson)
	}
	if strings.TrimSpace(facilitatorToken) == "" {
		return nil, fmt.Errorf("%w: facilitator token must not be empty", ErrInvalidInput)
	}
	publicID, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	var board *Board
	err = s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO boards (public_id, name, context, facilitator_token_hash, votes_per_person)
			 VALUES (?, ?, ?, ?, ?)`,
			publicID, name, boardContext, HashToken(facilitatorToken), votesPerPerson)
		if err != nil {
			return fmt.Errorf("insert board: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("board id: %w", err)
		}
		board, err = scanBoard(tx.QueryRowContext(ctx,
			`SELECT `+boardColumns+` FROM boards WHERE id = ?`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return board, nil
}

// ColumnSeed is one column to seed while creating a board. Position is
// the slice index, starting at zero.
type ColumnSeed struct {
	Title string
	Color string
}

// SeededBoard describes a board plus everything creation seeds with it:
// template columns, carried-over open actions from an earlier board, and
// the creator's participant row.
type SeededBoard struct {
	Name             string
	Context          string
	FacilitatorToken string
	VotesPerPerson   int
	Columns          []ColumnSeed
	CarryFromBoardID *int64
	ParticipantName  string
	ParticipantToken string
}

// CreateSeededBoard inserts a board, its template columns, carried-over
// open actions, and the creator's participant row in a single
// transaction, so a mid-seed failure leaves no half-seeded residue.
func (s *Store) CreateSeededBoard(ctx context.Context, seed SeededBoard) (*Board, error) {
	if strings.TrimSpace(seed.Name) == "" {
		return nil, fmt.Errorf("%w: board name must not be empty", ErrInvalidInput)
	}
	if seed.VotesPerPerson <= 0 {
		return nil, fmt.Errorf("%w: votes per person must be positive, got %d", ErrInvalidInput, seed.VotesPerPerson)
	}
	if strings.TrimSpace(seed.FacilitatorToken) == "" {
		return nil, fmt.Errorf("%w: facilitator token must not be empty", ErrInvalidInput)
	}
	participantName := strings.TrimSpace(seed.ParticipantName)
	if participantName == "" {
		return nil, fmt.Errorf("%w: participant name must not be empty", ErrInvalidInput)
	}
	if strings.TrimSpace(seed.ParticipantToken) == "" {
		return nil, fmt.Errorf("%w: participant token must not be empty", ErrInvalidInput)
	}
	for _, column := range seed.Columns {
		if strings.TrimSpace(column.Title) == "" {
			return nil, fmt.Errorf("%w: column title must not be empty", ErrInvalidInput)
		}
	}
	publicID, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	var board *Board
	err = s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO boards (public_id, name, context, facilitator_token_hash, votes_per_person)
			 VALUES (?, ?, ?, ?, ?)`,
			publicID, seed.Name, seed.Context, HashToken(seed.FacilitatorToken), seed.VotesPerPerson)
		if err != nil {
			return fmt.Errorf("insert board: %w", err)
		}
		newID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("board id: %w", err)
		}
		for i, column := range seed.Columns {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO columns (board_id, title, color, position) VALUES (?, ?, ?, ?)`,
				newID, column.Title, column.Color, i); err != nil {
				return fmt.Errorf("seed column: %w", err)
			}
		}
		if seed.CarryFromBoardID != nil {
			if err := boardExists(ctx, tx, *seed.CarryFromBoardID); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx,
				`SELECT id, text, owner, author_name FROM actions
				 WHERE board_id = ? AND done = 0 ORDER BY id ASC`, *seed.CarryFromBoardID)
			if err != nil {
				return fmt.Errorf("read carry-over actions: %w", err)
			}
			defer rows.Close()
			type actionSeed struct {
				id                  int64
				text, owner, author string
			}
			var seeds []actionSeed
			for rows.Next() {
				var item actionSeed
				if err := rows.Scan(&item.id, &item.text, &item.owner, &item.author); err != nil {
					return fmt.Errorf("scan carry-over action: %w", err)
				}
				seeds = append(seeds, item)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("read carry-over actions: %w", err)
			}
			rows.Close()
			for _, item := range seeds {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO actions (board_id, text, owner, author_name, carried_from)
					 VALUES (?, ?, ?, ?, ?)`,
					newID, item.text, item.owner, item.author, item.id); err != nil {
					return fmt.Errorf("carry action: %w", err)
				}
			}
		}
		candidate := participantName
		for n := 2; ; n++ {
			res, err := tx.ExecContext(ctx,
				`INSERT INTO participants (board_id, name, token_hash) VALUES (?, ?, ?)`,
				newID, candidate, HashToken(seed.ParticipantToken))
			if err == nil {
				if _, err := res.LastInsertId(); err != nil {
					return fmt.Errorf("participant id: %w", err)
				}
				break
			}
			if !isUniqueViolation(err) {
				return fmt.Errorf("insert participant: %w", err)
			}
			if n > 10000 {
				return fmt.Errorf("%w: no free display name near %q", ErrInvalidInput, participantName)
			}
			candidate = fmt.Sprintf("%s-%d", participantName, n)
		}
		board, err = scanBoardRow(tx.QueryRowContext(ctx,
			`SELECT `+boardColumns+` FROM boards WHERE id = ?`, newID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return board, nil
}

// GetBoardByPublicID fetches a board by its share ID.
func (s *Store) GetBoardByPublicID(ctx context.Context, publicID string) (*Board, error) {
	board, err := scanBoard(s.db.QueryRowContext(ctx,
		`SELECT `+boardColumns+` FROM boards WHERE public_id = ?`, publicID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return board, err
}

// CreateColumn appends a column to a board at an explicit position.
func (s *Store) CreateColumn(ctx context.Context, boardID int64, title, color string, position int) (*Column, error) {
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("%w: column title must not be empty", ErrInvalidInput)
	}
	if position < 0 {
		return nil, fmt.Errorf("%w: column position must be >= 0", ErrInvalidInput)
	}
	var col Column
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		if err := boardExists(ctx, tx, boardID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO columns (board_id, title, color, position) VALUES (?, ?, ?, ?)`,
			boardID, title, color, position)
		if err != nil {
			return fmt.Errorf("insert column: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("column id: %w", err)
		}
		return tx.QueryRowContext(ctx,
			`SELECT id, board_id, title, color, position FROM columns WHERE id = ?`, id).
			Scan(&col.ID, &col.BoardID, &col.Title, &col.Color, &col.Position)
	})
	if err != nil {
		return nil, err
	}
	return &col, nil
}

// ListColumns returns a board's columns in position order.
func (s *Store) ListColumns(ctx context.Context, boardID int64) ([]Column, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, board_id, title, color, position FROM columns
		 WHERE board_id = ? ORDER BY position ASC, id ASC`, boardID)
	if err != nil {
		return nil, fmt.Errorf("list columns: %w", err)
	}
	defer rows.Close()
	var cols []Column
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.ID, &c.BoardID, &c.Title, &c.Color, &c.Position); err != nil {
			return nil, fmt.Errorf("scan column: %w", err)
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

const cardColumns = `id, column_id, board_id, body, author_name, votes,
	group_id, discussed, position, created_at`

func scanCard(row *sql.Row) (*Card, error) {
	return scanCardRow(row)
}

// CreateCard appends a card to a column; the board is derived from the
// column so the two can never disagree. The card lands at the end of
// the column's display order.
func (s *Store) CreateCard(ctx context.Context, columnID int64, body, authorName string) (*Card, error) {
	if err := cardBodyOK(body); err != nil {
		return nil, err
	}
	var card *Card
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		var boardID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT board_id FROM columns WHERE id = ?`, columnID).Scan(&boardID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find column: %w", err)
		}
		var next int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(position) + 1, 0) FROM cards WHERE column_id = ?`,
			columnID).Scan(&next); err != nil {
			return fmt.Errorf("place card: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO cards (column_id, board_id, body, author_name, position)
			 VALUES (?, ?, ?, ?, ?)`,
			columnID, boardID, body, authorName, next)
		if err != nil {
			return fmt.Errorf("insert card: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("card id: %w", err)
		}
		card, err = scanCard(tx.QueryRowContext(ctx,
			`SELECT `+cardColumns+` FROM cards WHERE id = ?`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return card, nil
}

// GetCard fetches a card by id.
func (s *Store) GetCard(ctx context.Context, cardID int64) (*Card, error) {
	card, err := scanCard(s.db.QueryRowContext(ctx,
		`SELECT `+cardColumns+` FROM cards WHERE id = ?`, cardID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return card, err
}

// MoveCard reassigns a card to a column and position within the same
// board, shifting the neighbors so both columns stay dense. Within one
// column the moved card is lifted out and the gap closed before the
// destination slot opens; across columns the source compacts and the
// destination opens at the requested position. Positions past the end
// clamp to the last slot, so the columns stay dense no matter what the
// caller sends.
func (s *Store) MoveCard(ctx context.Context, cardID, newColumnID int64, newPosition int) error {
	if newPosition < 0 {
		return fmt.Errorf("%w: card position must be >= 0", ErrInvalidInput)
	}
	return s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		return moveCardInTx(ctx, tx, cardID, newColumnID, newPosition)
	})
}

// MoveCardExpected moves a card only when it still sits in the expected
// slot (the column and position seen when the drag started). A mismatch
// means a remote update landed mid-drag, so the drop is rejected with
// ErrStalePosition and the placement stays untouched; the caller
// refetches and the user retries against fresh positions. The check and
// the move share one transaction, so the decision is atomic.
func (s *Store) MoveCardExpected(ctx context.Context, cardID, newColumnID int64, newPosition int, expectedColumnID int64, expectedPosition int) error {
	if newPosition < 0 {
		return fmt.Errorf("%w: card position must be >= 0", ErrInvalidInput)
	}
	return s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		var columnID int64
		var position int
		if err := tx.QueryRowContext(ctx,
			`SELECT column_id, position FROM cards WHERE id = ?`,
			cardID).Scan(&columnID, &position); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find card: %w", err)
		}
		if columnID != expectedColumnID || position != expectedPosition {
			return ErrStalePosition
		}
		return moveCardInTx(ctx, tx, cardID, newColumnID, newPosition)
	})
}

// moveCardInTx reassigns a card to a column and position, shifting the
// neighbors so both columns stay dense. It runs inside the caller's
// transaction.
func moveCardInTx(ctx context.Context, tx DBTX, cardID, newColumnID int64, newPosition int) error {
	var cardBoard, oldColumnID int64
	var oldPosition int
	if err := tx.QueryRowContext(ctx,
		`SELECT board_id, column_id, position FROM cards WHERE id = ?`,
		cardID).Scan(&cardBoard, &oldColumnID, &oldPosition); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("find card: %w", err)
	}
	var columnBoard int64
	if err := tx.QueryRowContext(ctx,
		`SELECT board_id FROM columns WHERE id = ?`, newColumnID).Scan(&columnBoard); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("find column: %w", err)
	}
	if cardBoard != columnBoard {
		return ErrCrossColumnBoard
	}
	if newColumnID == oldColumnID {
		var siblings int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM cards WHERE column_id = ? AND id != ?`,
			oldColumnID, cardID).Scan(&siblings); err != nil {
			return fmt.Errorf("count siblings: %w", err)
		}
		if newPosition > siblings {
			newPosition = siblings
		}
		switch {
		case newPosition == oldPosition:
			return nil
		case newPosition < oldPosition:
			if _, err := tx.ExecContext(ctx,
				`UPDATE cards SET position = position + 1
					 WHERE column_id = ? AND position >= ? AND position < ? AND id != ?`,
				oldColumnID, newPosition, oldPosition, cardID); err != nil {
				return fmt.Errorf("open slot: %w", err)
			}
		default:
			if _, err := tx.ExecContext(ctx,
				`UPDATE cards SET position = position - 1
					 WHERE column_id = ? AND position > ? AND position <= ? AND id != ?`,
				oldColumnID, oldPosition, newPosition, cardID); err != nil {
				return fmt.Errorf("close gap: %w", err)
			}
		}
	} else {
		var room int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM cards WHERE column_id = ?`,
			newColumnID).Scan(&room); err != nil {
			return fmt.Errorf("count destination: %w", err)
		}
		if newPosition > room {
			newPosition = room
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET position = position - 1
				 WHERE column_id = ? AND position > ?`,
			oldColumnID, oldPosition); err != nil {
			return fmt.Errorf("compact source: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET position = position + 1
				 WHERE column_id = ? AND position >= ?`,
			newColumnID, newPosition); err != nil {
			return fmt.Errorf("open slot: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE cards SET column_id = ?, position = ? WHERE id = ?`,
		newColumnID, newPosition, cardID); err != nil {
		return fmt.Errorf("move card: %w", err)
	}
	return nil
}

// CreateParticipant registers a display name on a board. Names are unique
// per board; callers suffix duplicates before retrying.
func (s *Store) CreateParticipant(ctx context.Context, boardID int64, name, rawToken string) (*Participant, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: participant name must not be empty", ErrInvalidInput)
	}
	if strings.TrimSpace(rawToken) == "" {
		return nil, fmt.Errorf("%w: participant token must not be empty", ErrInvalidInput)
	}
	var p Participant
	err := s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		if err := boardExists(ctx, tx, boardID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO participants (board_id, name, token_hash) VALUES (?, ?, ?)`,
			boardID, name, HashToken(rawToken))
		if err != nil {
			if isUniqueViolation(err) {
				return ErrDuplicateName
			}
			return fmt.Errorf("insert participant: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("participant id: %w", err)
		}
		var createdAt string
		if err := tx.QueryRowContext(ctx,
			`SELECT id, board_id, name, token_hash, created_at FROM participants WHERE id = ?`, id).
			Scan(&p.ID, &p.BoardID, &p.Name, &p.TokenHash, &createdAt); err != nil {
			return fmt.Errorf("read participant: %w", err)
		}
		created, err := parseTimestamp(createdAt)
		if err != nil {
			return err
		}
		p.CreatedAt = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Vote records one participant's vote for a card. The existing-row check,
// the per-board cap check, the phase/lock gate, the insert, and the card
// counter update all happen in one transaction; the PRIMARY KEY backstop maps to
// ErrDoubleVote if two racing transactions slip past the check.
func (s *Store) Vote(ctx context.Context, participantID, cardID int64) error {
	return s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		var boardID int64
		var cap int
		var votingLocked int
		var phase string
		err := tx.QueryRowContext(ctx,
			`SELECT b.id, b.votes_per_person, b.voting_locked, b.phase FROM boards b
			 JOIN participants p ON p.board_id = b.id WHERE p.id = ?`,
			participantID).Scan(&boardID, &cap, &votingLocked, &phase)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find participant board: %w", err)
		}
		if votingLocked != 0 || phase != "vote" {
			return ErrVotingClosed
		}
		var cardBoard int64
		if err := tx.QueryRowContext(ctx,
			`SELECT board_id FROM cards WHERE id = ?`, cardID).Scan(&cardBoard); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find card: %w", err)
		}
		if cardBoard != boardID {
			return ErrBoardMismatch
		}
		var one int
		err = tx.QueryRowContext(ctx,
			`SELECT 1 FROM votes WHERE participant_id = ? AND card_id = ?`,
			participantID, cardID).Scan(&one)
		switch {
		case err == nil:
			return ErrDoubleVote
		case errors.Is(err, sql.ErrNoRows):
		default:
			return fmt.Errorf("check existing vote: %w", err)
		}
		var used int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM votes v JOIN cards c ON c.id = v.card_id
			 WHERE v.participant_id = ? AND c.board_id = ?`,
			participantID, boardID).Scan(&used); err != nil {
			return fmt.Errorf("count votes: %w", err)
		}
		if used >= cap {
			return ErrVoteCapExceeded
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO votes (participant_id, card_id) VALUES (?, ?)`,
			participantID, cardID); err != nil {
			if isUniqueViolation(err) {
				return ErrDoubleVote
			}
			return fmt.Errorf("insert vote: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET votes = votes + 1 WHERE id = ?`, cardID); err != nil {
			return fmt.Errorf("bump card votes: %w", err)
		}
		return nil
	})
}

// VotedCardIDs returns the set of card ids a participant has voted for,
// so readers can render each card's vote control in its honest state
// without a query per card.
func (s *Store) VotedCardIDs(ctx context.Context, participantID int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT card_id FROM votes WHERE participant_id = ?`, participantID)
	if err != nil {
		return nil, fmt.Errorf("list voted cards: %w", err)
	}
	defer rows.Close()
	voted := map[int64]bool{}
	for rows.Next() {
		var cardID int64
		if err := rows.Scan(&cardID); err != nil {
			return nil, fmt.Errorf("scan voted card: %w", err)
		}
		voted[cardID] = true
	}
	return voted, rows.Err()
}

// Unvote removes a participant's vote and decrements the card counter in
// the same transaction. Removing a vote that does not exist is an error.
// The voting window is re-checked inside the transaction so a concurrent
// lock or phase change rejects the removal with ErrVotingClosed.
func (s *Store) Unvote(ctx context.Context, participantID, cardID int64) error {
	return s.WithTx(ctx, func(ctx context.Context, tx DBTX) error {
		var boardID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT board_id FROM participants WHERE id = ?`,
			participantID).Scan(&boardID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find participant board: %w", err)
		}
		var votingLocked int
		var phase string
		if err := tx.QueryRowContext(ctx,
			`SELECT voting_locked, phase FROM boards WHERE id = ?`,
			boardID).Scan(&votingLocked, &phase); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find board: %w", err)
		}
		if votingLocked != 0 || phase != "vote" {
			return ErrVotingClosed
		}
		var cardBoard int64
		if err := tx.QueryRowContext(ctx,
			`SELECT board_id FROM cards WHERE id = ?`, cardID).Scan(&cardBoard); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("find card: %w", err)
		}
		if cardBoard != boardID {
			return ErrBoardMismatch
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM votes WHERE participant_id = ? AND card_id = ?`,
			participantID, cardID)
		if err != nil {
			return fmt.Errorf("delete vote: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("vote rows affected: %w", err)
		}
		if n == 0 {
			return ErrNoVote
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cards SET votes = votes - 1 WHERE id = ? AND votes > 0`, cardID); err != nil {
			return fmt.Errorf("drop card votes: %w", err)
		}
		return nil
	})
}

func boardExists(ctx context.Context, tx DBTX, boardID int64) error {
	var one int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM boards WHERE id = ?`, boardID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("find board: %w", err)
	}
	return nil
}

func parseTimestamp(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", value); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("parse timestamp %q", value)
}

// isUniqueViolation reports PRIMARY KEY / UNIQUE constraint failures,
// the backstop for races that slip past in-transaction existence checks.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		// 19 = SQLITE_CONSTRAINT, 2067 = SQLITE_CONSTRAINT_UNIQUE.
		if code := sqliteErr.Code(); code == 19 || code == 2067 {
			return true
		}
		// Extended result codes embed the primary code in the low byte
		// (e.g. 2067 == 19 | (8<<8)); accept any extended constraint code.
		type extendedCoder interface{ ExtendedCode() int }
		var ec extendedCoder
		if errors.As(err, &ec) {
			if ex := ec.ExtendedCode(); ex&0xff == 19 {
				return true
			}
		}
	}
	// Last-resort fallback for wrapped driver errors without a typed code.
	return strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "PRIMARY KEY constraint failed")
}
