package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()

	sqldb, _, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open test DB: %v", err)
	}
	t.Cleanup(func() {
		if err := sqldb.Close(); err != nil {
			t.Errorf("close test DB: %v", err)
		}
	})
	return sqldb
}

func seedBoard(t *testing.T, store *Store) *Board {
	t.Helper()

	board, err := store.CreateBoard(t.Context(), "Retro", "Sprint 12", "facilitator-raw-token", 3)
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	return board
}

func seedColumn(t *testing.T, store *Store, boardID int64, title string, position int) *Column {
	t.Helper()

	col, err := store.CreateColumn(t.Context(), boardID, title, "red", position)
	if err != nil {
		t.Fatalf("CreateColumn: %v", err)
	}
	return col
}

func seedParticipant(t *testing.T, store *Store, boardID int64, name string) *Participant {
	t.Helper()

	p, err := store.CreateParticipant(t.Context(), boardID, name, "participant-raw-token-"+name)
	if err != nil {
		t.Fatalf("CreateParticipant: %v", err)
	}
	return p
}

func TestMigrationsApplyOnFreshDB(t *testing.T) {
	sqldb := openTestDB(t)

	for _, table := range []string{
		"boards", "columns", "cards", "votes", "comments",
		"kudos", "actions", "participants", "app_meta", "goose_db_version",
	} {
		var name string
		err := sqldb.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("expected table %q after migrate: %v", table, err)
		}
	}

	version, err := goose.GetDBVersion(sqldb)
	if err != nil {
		t.Fatalf("GetDBVersion: %v", err)
	}
	if version != 2 {
		t.Errorf("goose version = %d, want 2", version)
	}
}

func TestConnectionPragmas(t *testing.T) {
	sqldb := openTestDB(t)

	for range 2 {
		conn, err := sqldb.Conn(t.Context())
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		var journalMode string
		if err := conn.QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
			conn.Close()
			t.Fatalf("PRAGMA journal_mode: %v", err)
		}
		var foreignKeys, busyTimeout, synchronous int
		if err := conn.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			conn.Close()
			t.Fatalf("PRAGMA foreign_keys: %v", err)
		}
		if err := conn.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			conn.Close()
			t.Fatalf("PRAGMA busy_timeout: %v", err)
		}
		if err := conn.QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&synchronous); err != nil {
			conn.Close()
			t.Fatalf("PRAGMA synchronous: %v", err)
		}
		conn.Close()
		if journalMode != "wal" {
			t.Errorf("journal_mode = %q, want wal", journalMode)
		}
		if foreignKeys != 1 {
			t.Error("foreign_keys is off, want on")
		}
		if busyTimeout != 5000 {
			t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
		}
		if synchronous != 1 {
			t.Errorf("synchronous = %d, want 1 (NORMAL)", synchronous)
		}
	}
}

func TestDuplicateParticipantNameRejected(t *testing.T) {
	store := NewStore(openTestDB(t))
	board := seedBoard(t, store)

	seedParticipant(t, store, board.ID, "ana")
	if _, err := store.CreateParticipant(t.Context(), board.ID, "ana", "another-token"); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("duplicate name err = %v, want ErrDuplicateName", err)
	}
}

func TestMigrationsRerunIsNoOp(t *testing.T) {
	sqldb := openTestDB(t)
	store := NewStore(sqldb)

	before := seedBoard(t, store)

	if err := goose.Up(sqldb, "migrations"); err != nil {
		t.Fatalf("second goose.Up: %v", err)
	}

	version, err := goose.GetDBVersion(sqldb)
	if err != nil {
		t.Fatalf("GetDBVersion: %v", err)
	}
	if version != 2 {
		t.Errorf("goose version after re-run = %d, want 2", version)
	}

	got, err := store.GetBoardByPublicID(t.Context(), before.PublicID)
	if err != nil {
		t.Fatalf("board lost after re-run: %v", err)
	}
	if got.ID != before.ID || got.Name != before.Name {
		t.Errorf("board changed across re-run: %+v vs %+v", got, before)
	}
}

func TestForeignKeyColumnsAreIndexed(t *testing.T) {
	sqldb := openTestDB(t)

	indexed := map[string]map[string]bool{}
	for _, table := range []string{
		"participants", "columns", "cards", "votes", "comments", "kudos", "actions",
	} {
		rows, err := sqldb.Query(`PRAGMA index_list(` + table + `)`)
		if err != nil {
			t.Fatalf("PRAGMA index_list(%s): %v", table, err)
		}
		cols := map[string]bool{}
		for rows.Next() {
			var seq, unique int
			var name, origin string
			var partial int
			if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
				rows.Close()
				t.Fatalf("scan index_list: %v", err)
			}
			inforows, err := sqldb.Query(`PRAGMA index_info(` + name + `)`)
			if err != nil {
				rows.Close()
				t.Fatalf("PRAGMA index_info(%s): %v", name, err)
			}
			for inforows.Next() {
				var rank, cid int
				var col string
				if err := inforows.Scan(&rank, &cid, &col); err != nil {
					inforows.Close()
					rows.Close()
					t.Fatalf("scan index_info: %v", err)
				}
				cols[col] = true
			}
			inforows.Close()
		}
		rows.Close()
		indexed[table] = cols

		fkrows, err := sqldb.Query(`PRAGMA foreign_key_list(` + table + `)`)
		if err != nil {
			t.Fatalf("PRAGMA foreign_key_list(%s): %v", table, err)
		}
		fks := map[string]bool{}
		for fkrows.Next() {
			var id, seq int
			var refTable, from, to, onUpdate, onDelete, match string
			if err := fkrows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				fkrows.Close()
				t.Fatalf("scan foreign_key_list: %v", err)
			}
			fks[from] = true
		}
		fkrows.Close()
		if len(fks) == 0 {
			t.Errorf("table %s has no foreign keys", table)
		}
		for fk := range fks {
			if !cols[fk] {
				t.Errorf("table %s: FK column %q has no index", table, fk)
			}
		}
	}
}

func TestCreateAndGetBoard(t *testing.T) {
	store := NewStore(openTestDB(t))

	board, err := store.CreateBoard(t.Context(), "Mad Sad Glad", "Sprint 12", "raw-facilitator-token", 3)
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	if board.ID == 0 {
		t.Error("board ID is zero")
	}
	if board.PublicID == "" {
		t.Error("board public ID is empty")
	}
	if board.FacilitatorTokenHash != HashToken("raw-facilitator-token") {
		t.Error("facilitator token was not stored as SHA-256 hex")
	}
	if board.Phase != "collect" {
		t.Errorf("phase = %q, want collect", board.Phase)
	}
	if board.VotesPerPerson != 3 {
		t.Errorf("votes per person = %d, want 3", board.VotesPerPerson)
	}
	if board.VotingLocked || board.CardsLocked || board.CardsHidden {
		t.Error("locks should default to false")
	}

	got, err := store.GetBoardByPublicID(t.Context(), board.PublicID)
	if err != nil {
		t.Fatalf("GetBoardByPublicID: %v", err)
	}
	if got.ID != board.ID || got.Name != "Mad Sad Glad" || got.Context != "Sprint 12" {
		t.Errorf("round trip mismatch: %+v", got)
	}

	other, err := store.CreateBoard(t.Context(), "Second", "", "other-token", 3)
	if err != nil {
		t.Fatalf("second CreateBoard: %v", err)
	}
	if other.PublicID == board.PublicID {
		t.Error("public IDs must be unique")
	}

	if _, err := store.GetBoardByPublicID(t.Context(), "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown board err = %v, want ErrNotFound", err)
	}
}

func TestCreateBoardRejectsBadCap(t *testing.T) {
	store := NewStore(openTestDB(t))

	if _, err := store.CreateBoard(t.Context(), "Bad", "", "tok", 0); err == nil {
		t.Error("votes_per_person = 0 should be rejected")
	}
	if _, err := store.CreateBoard(t.Context(), "Bad", "", "tok", -2); err == nil {
		t.Error("negative votes_per_person should be rejected")
	}
}

func TestColumnOrdering(t *testing.T) {
	store := NewStore(openTestDB(t))
	board := seedBoard(t, store)
	ctx := t.Context()

	for _, c := range []struct {
		title string
		pos   int
	}{{"third", 2}, {"first", 0}, {"second", 1}} {
		if _, err := store.CreateColumn(ctx, board.ID, c.title, "blue", c.pos); err != nil {
			t.Fatalf("CreateColumn: %v", err)
		}
	}

	cols, err := store.ListColumns(ctx, board.ID)
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	if len(cols) != 3 {
		t.Fatalf("got %d columns, want 3", len(cols))
	}
	for i, want := range []string{"first", "second", "third"} {
		if cols[i].Title != want {
			t.Errorf("position %d = %q, want %q", i, cols[i].Title, want)
		}
	}
}

func TestMoveCard(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	left := seedColumn(t, store, board.ID, "left", 0)
	right := seedColumn(t, store, board.ID, "right", 1)

	card, err := store.CreateCard(ctx, left.ID, "hello", "ana")
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if card.BoardID != board.ID {
		t.Errorf("card board = %d, want %d", card.BoardID, board.ID)
	}

	// Past-the-end clamps to the only slot instead of opening a gap.
	if err := store.MoveCard(ctx, card.ID, right.ID, 4); err != nil {
		t.Fatalf("MoveCard: %v", err)
	}
	got, err := store.GetCard(ctx, card.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.ColumnID != right.ID || got.Position != 0 {
		t.Errorf("after move: column = %d position = %d, want %d/0 (clamped)", got.ColumnID, got.Position, right.ID)
	}
	if got.Body != "hello" || got.AuthorName != "ana" {
		t.Errorf("move changed card content: %+v", got)
	}
}

func TestVoteRejectedAfterPhaseChange(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	col := seedColumn(t, store, board.ID, "col", 0)
	if _, err := store.SetPhase(ctx, board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	card, err := store.CreateCard(ctx, col.ID, "body", "ana")
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	voter := seedParticipant(t, store, board.ID, "bo")

	// A concurrent facilitator moves the board out of the vote phase
	// between the handler pre-check and the store commit: the in-tx
	// re-read must still reject the vote and the unvote.
	if _, err := store.SetPhase(ctx, board.ID, "discuss"); err != nil {
		t.Fatalf("SetPhase discuss: %v", err)
	}
	if err := store.Vote(ctx, voter.ID, card.ID); !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("Vote after phase change err = %v, want ErrVotingClosed", err)
	}
	if _, err := store.SetPhase(ctx, board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	if err := store.Vote(ctx, voter.ID, card.ID); err != nil {
		t.Fatalf("Vote: %v", err)
	}
	if _, err := store.SetPhase(ctx, board.ID, "done"); err != nil {
		t.Fatalf("SetPhase done: %v", err)
	}
	if err := store.Unvote(ctx, voter.ID, card.ID); !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("Unvote after phase change err = %v, want ErrVotingClosed", err)
	}
}

func TestDoubleVoteRejected(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	col := seedColumn(t, store, board.ID, "col", 0)
	if _, err := store.SetPhase(ctx, board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}

	card, err := store.CreateCard(ctx, col.ID, "body", "ana")
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	voter := seedParticipant(t, store, board.ID, "bo")

	if err := store.Vote(ctx, voter.ID, card.ID); err != nil {
		t.Fatalf("first Vote: %v", err)
	}
	if err := store.Vote(ctx, voter.ID, card.ID); !errors.Is(err, ErrDoubleVote) {
		t.Fatalf("second Vote err = %v, want ErrDoubleVote", err)
	}

	got, err := store.GetCard(ctx, card.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.Votes != 1 {
		t.Errorf("card votes = %d, want 1 after rejected double vote", got.Votes)
	}
}

func TestUnvoteDecrements(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	col := seedColumn(t, store, board.ID, "col", 0)
	if _, err := store.SetPhase(ctx, board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}

	card, err := store.CreateCard(ctx, col.ID, "body", "ana")
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	voter := seedParticipant(t, store, board.ID, "bo")

	if err := store.Vote(ctx, voter.ID, card.ID); err != nil {
		t.Fatalf("Vote: %v", err)
	}
	if err := store.Unvote(ctx, voter.ID, card.ID); err != nil {
		t.Fatalf("Unvote: %v", err)
	}
	got, err := store.GetCard(ctx, card.ID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.Votes != 0 {
		t.Errorf("card votes = %d, want 0 after unvote", got.Votes)
	}
	if n := store.countVotesForTest(t, voter.ID); n != 0 {
		t.Errorf("participant vote count = %d, want 0 after unvote", n)
	}
	if err := store.Unvote(ctx, voter.ID, card.ID); !errors.Is(err, ErrNoVote) {
		t.Errorf("second Unvote err = %v, want ErrNoVote", err)
	}
}

func TestVoteCapRace(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	if _, err := store.SetPhase(ctx, board.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	cap := board.VotesPerPerson
	const racers = 8
	col := seedColumn(t, store, board.ID, "col", 0)
	voter := seedParticipant(t, store, board.ID, "bo")

	cards := make([]*Card, racers)
	for i := range cards {
		card, err := store.CreateCard(ctx, col.ID, "body", "ana")
		if err != nil {
			t.Fatalf("CreateCard: %v", err)
		}
		cards[i] = card
	}

	var mu sync.Mutex
	succeeded, capped := 0, 0
	var wg sync.WaitGroup
	for _, card := range cards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.Vote(ctx, voter.ID, card.ID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrVoteCapExceeded):
				capped++
			default:
				t.Errorf("unexpected Vote error: %v", err)
			}
		}()
	}
	wg.Wait()

	if succeeded != cap {
		t.Errorf("succeeded = %d, want exactly the cap %d", succeeded, cap)
	}
	if capped != racers-cap {
		t.Errorf("capped = %d, want %d", capped, racers-cap)
	}
	if n := store.countVotesForTest(t, voter.ID); n != cap {
		t.Errorf("participant vote count = %d, want %d", n, cap)
	}
}

func TestConcurrentWritersSeeNoBusyErrors(t *testing.T) {
	sqldb := openTestDB(t)
	store := NewStore(sqldb)
	ctx := t.Context()
	board := seedBoard(t, store)
	col := seedColumn(t, store, board.ID, "col", 0)

	const writers = 20
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.CreateCard(ctx, col.ID, "concurrent body", "writer")
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			if strings.Contains(err.Error(), "SQLITE_BUSY") {
				t.Errorf("writer %d hit SQLITE_BUSY: %v", i, err)
			} else {
				t.Errorf("writer %d: %v", i, err)
			}
		}
	}

	var count int
	if err := sqldb.QueryRow(`SELECT COUNT(*) FROM cards`).Scan(&count); err != nil {
		t.Fatalf("count cards: %v", err)
	}
	if count != writers {
		t.Errorf("cards = %d, want %d", count, writers)
	}
}

func TestServerSecretSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.db")

	first, secret, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(secret) != 64 {
		t.Errorf("secret length = %d, want 64 hex chars", len(secret))
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if again != secret {
		t.Error("server secret changed across reopen")
	}
}

func TestTokenHelpers(t *testing.T) {
	raw := "some-raw-token"
	hashed := HashToken(raw)
	if len(hashed) != 64 {
		t.Errorf("HashToken length = %d, want 64", len(hashed))
	}
	if HashToken(raw) != hashed {
		t.Error("HashToken is not deterministic")
	}
	if !CompareTokenHash(hashed, HashToken(raw)) {
		t.Error("CompareTokenHash rejected identical hashes")
	}
	if CompareTokenHash(hashed, HashToken("different")) {
		t.Error("CompareTokenHash accepted different hashes")
	}

	seen := map[string]bool{}
	for range 4 {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if tok == "" || seen[tok] {
			t.Errorf("GenerateToken produced empty or duplicate token")
		}
		seen[tok] = true
	}
}

func (s *Store) countVotesForTest(t *testing.T, participantID int64) int {
	t.Helper()

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM votes WHERE participant_id = ?`, participantID).Scan(&n); err != nil {
		t.Fatalf("count votes: %v", err)
	}
	return n
}
