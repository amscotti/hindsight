package db

import (
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
)

func seedActions(t *testing.T, store *Store, boardID int64) (open1, open2, done *Action) {
	t.Helper()

	var err error
	open1, err = store.CreateAction(t.Context(), boardID, "follow up with team", "ana", "ana", nil)
	if err != nil {
		t.Fatalf("CreateAction open1: %v", err)
	}
	open2, err = store.CreateAction(t.Context(), boardID, "fix flaky test", "", "bo", nil)
	if err != nil {
		t.Fatalf("CreateAction open2: %v", err)
	}
	done, err = store.CreateAction(t.Context(), boardID, "already shipped", "bo", "bo", nil)
	if err != nil {
		t.Fatalf("CreateAction done: %v", err)
	}
	if err := store.SetActionDone(t.Context(), done.ID, true); err != nil {
		t.Fatalf("SetActionDone: %v", err)
	}
	return open1, open2, done
}

func TestListBoardsWithStatsEmpty(t *testing.T) {
	store := NewStore(openTestDB(t))

	got, err := store.ListBoardsWithStats(t.Context())
	if err != nil {
		t.Fatalf("ListBoardsWithStats: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("fresh DB lists %d boards, want 0", len(got))
	}
}

func TestListBoardsWithStatsCountsAndOrder(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()

	first := seedBoard(t, store)
	second, err := store.CreateBoard(ctx, "Second", "", "other-token", 3)
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}

	col := seedColumn(t, store, first.ID, "col", 0)
	if _, err := store.CreateCard(ctx, col.ID, "a card", "ana"); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if _, err := store.CreateAction(ctx, first.ID, "open item", "", "ana", nil); err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	doneAction, err := store.CreateAction(ctx, first.ID, "finished item", "", "ana", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if err := store.SetActionDone(ctx, doneAction.ID, true); err != nil {
		t.Fatalf("SetActionDone: %v", err)
	}

	got, err := store.ListBoardsWithStats(ctx)
	if err != nil {
		t.Fatalf("ListBoardsWithStats: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d boards, want 2", len(got))
	}
	// Newest first.
	if got[0].ID != second.ID || got[1].ID != first.ID {
		t.Errorf("order = [%d %d], want newest first [%d %d]",
			got[0].ID, got[1].ID, second.ID, first.ID)
	}
	if got[1].CardCount != 1 {
		t.Errorf("first board cards = %d, want 1", got[1].CardCount)
	}
	if got[1].OpenActionCount != 1 {
		t.Errorf("first board open actions = %d, want 1 (done item excluded)", got[1].OpenActionCount)
	}
	if got[0].CardCount != 0 || got[0].OpenActionCount != 0 {
		t.Errorf("second board counts = %d/%d, want 0/0",
			got[0].CardCount, got[0].OpenActionCount)
	}
	if got[0].ArchivedAt != nil || got[1].ArchivedAt != nil {
		t.Error("fresh boards must not be archived")
	}
	for _, item := range got {
		if item.FacilitatorTokenHash != "" {
			t.Errorf("list item %d leaks facilitator hash", item.ID)
		}
	}
}

func TestSetArchivedPreservesContent(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	col := seedColumn(t, store, board.ID, "col", 0)
	if _, err := store.CreateCard(ctx, col.ID, "keep me", "ana"); err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	archived, err := store.SetArchived(ctx, board.ID, true)
	if err != nil {
		t.Fatalf("SetArchived(true): %v", err)
	}
	if archived.ArchivedAt == nil {
		t.Error("archived board has nil ArchivedAt")
	}

	got, err := store.GetBoardByPublicID(ctx, board.PublicID)
	if err != nil {
		t.Fatalf("GetBoardByPublicID after archive: %v", err)
	}
	if got.ArchivedAt == nil {
		t.Error("archive flag not persisted")
	}
	cols, err := store.ListColumns(ctx, board.ID)
	if err != nil {
		t.Fatalf("ListColumns after archive: %v", err)
	}
	if len(cols) != 1 {
		t.Errorf("archived board columns = %d, want content preserved", len(cols))
	}

	restored, err := store.SetArchived(ctx, board.ID, false)
	if err != nil {
		t.Fatalf("SetArchived(false): %v", err)
	}
	if restored.ArchivedAt != nil {
		t.Error("unarchived board still has ArchivedAt set")
	}
}

func TestSetArchivedUnknownBoard(t *testing.T) {
	store := NewStore(openTestDB(t))

	if _, err := store.SetArchived(t.Context(), 9999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("archive unknown err = %v, want ErrNotFound", err)
	}
}

func TestDuplicateBoardCopiesColumnsInOrder(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	src := seedBoard(t, store)
	if _, err := store.SetPhase(ctx, src.ID, "vote"); err != nil {
		t.Fatalf("SetPhase vote: %v", err)
	}
	for i, title := range []string{"Mad", "Sad", "Glad"} {
		if _, err := store.CreateColumn(ctx, src.ID, title, "red", i); err != nil {
			t.Fatalf("CreateColumn: %v", err)
		}
	}
	// A card plus a vote must not follow the copy.
	col, err := store.CreateColumn(ctx, src.ID, "Extra", "blue", 3)
	if err != nil {
		t.Fatalf("CreateColumn: %v", err)
	}
	card, err := store.CreateCard(ctx, col.ID, "do not copy", "ana")
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	voter := seedParticipant(t, store, src.ID, "bo")
	if err := store.Vote(ctx, voter.ID, card.ID); err != nil {
		t.Fatalf("Vote: %v", err)
	}

	dup, err := store.DuplicateBoard(ctx, src.ID, "fresh-facilitator-token", false)
	if err != nil {
		t.Fatalf("DuplicateBoard: %v", err)
	}
	if dup.ID == src.ID || dup.PublicID == src.PublicID {
		t.Error("duplicate must have fresh IDs")
	}
	if dup.FacilitatorTokenHash != HashToken("fresh-facilitator-token") {
		t.Error("duplicate must store the new facilitator token hash")
	}
	if dup.ArchivedAt != nil {
		t.Error("duplicate of an active board must not be archived")
	}

	cols, err := store.ListColumns(ctx, dup.ID)
	if err != nil {
		t.Fatalf("ListColumns duplicate: %v", err)
	}
	if len(cols) != 4 {
		t.Fatalf("duplicate columns = %d, want 4", len(cols))
	}
	for i, want := range []string{"Mad", "Sad", "Glad", "Extra"} {
		if cols[i].Title != want || cols[i].Position != i {
			t.Errorf("duplicate column %d = %q@%d, want %q@%d",
				i, cols[i].Title, cols[i].Position, want, i)
		}
		if cols[i].BoardID != dup.ID {
			t.Errorf("duplicate column %d board = %d, want %d", i, cols[i].BoardID, dup.ID)
		}
	}

	stats, err := store.ListBoardsWithStats(ctx)
	if err != nil {
		t.Fatalf("ListBoardsWithStats: %v", err)
	}
	for _, s := range stats {
		if s.ID == dup.ID && s.CardCount != 0 {
			t.Errorf("duplicate cards = %d, want 0 (votes and cards never copy)", s.CardCount)
		}
	}
	if actions, err := store.ListOpenActions(ctx, dup.ID); err != nil {
		t.Fatalf("ListOpenActions duplicate: %v", err)
	} else if len(actions) != 0 {
		t.Errorf("duplicate open actions = %d, want 0 without carry", len(actions))
	}
}

func TestDuplicateBoardCarriesOnlyOpenActions(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	src := seedBoard(t, store)
	seedColumn(t, store, src.ID, "col", 0)
	open1, open2, _ := seedActions(t, store, src.ID)

	dup, err := store.DuplicateBoard(ctx, src.ID, "another-token", true)
	if err != nil {
		t.Fatalf("DuplicateBoard with carry: %v", err)
	}
	carried, err := store.ListOpenActions(ctx, dup.ID)
	if err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	}
	if len(carried) != 2 {
		t.Fatalf("carried actions = %d, want 2 (done item excluded)", len(carried))
	}
	byText := map[string]Action{}
	for _, a := range carried {
		byText[a.Text] = a
		if a.BoardID != dup.ID {
			t.Errorf("carried action board = %d, want %d", a.BoardID, dup.ID)
		}
		if a.Done {
			t.Errorf("carried action %q marked done", a.Text)
		}
		if a.CarriedFrom == nil {
			t.Errorf("carried action %q missing provenance", a.Text)
		}
	}
	if got := byText["follow up with team"]; got.CarriedFrom == nil || *got.CarriedFrom != open1.ID {
		t.Errorf("carried_from = %v, want %d", got.CarriedFrom, open1.ID)
	} else if got.Owner != "ana" {
		t.Errorf("carried owner = %q, want ana", got.Owner)
	}
	if got := byText["fix flaky test"]; got.CarriedFrom == nil || *got.CarriedFrom != open2.ID {
		t.Errorf("carried_from = %v, want %d", got.CarriedFrom, open2.ID)
	}
}

func TestDuplicateUnknownBoard(t *testing.T) {
	store := NewStore(openTestDB(t))

	if _, err := store.DuplicateBoard(t.Context(), 9999, "tok", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("duplicate unknown err = %v, want ErrNotFound", err)
	}
}

func TestCreateActionValidation(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)

	if _, err := store.CreateAction(ctx, board.ID, "   ", "", "ana", nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("blank action err = %v, want ErrInvalidInput", err)
	}
	long := make([]byte, 501)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := store.CreateAction(ctx, board.ID, string(long), "", "ana", nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-length action err = %v, want ErrInvalidInput", err)
	}
	longOwner := strings.Repeat("x", 101)
	if _, err := store.CreateAction(ctx, board.ID, "valid", longOwner, "ana", nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-length owner err = %v, want ErrInvalidInput", err)
	}
	if _, err := store.CreateAction(ctx, 9999, "valid", "", "ana", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("action on unknown board err = %v, want ErrNotFound", err)
	}
}

func TestListOpenActionsExcludesDone(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	seedActions(t, store, board.ID)

	got, err := store.ListOpenActions(ctx, board.ID)
	if err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("open actions = %d, want 2", len(got))
	}
	for _, a := range got {
		if a.Done {
			t.Errorf("done action %q listed as open", a.Text)
		}
	}
}

func TestGetParticipantByToken(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	seedParticipant(t, store, board.ID, "ana")

	got, err := store.GetParticipantByToken(ctx, board.ID, "participant-raw-token-ana")
	if err != nil {
		t.Fatalf("GetParticipantByToken: %v", err)
	}
	if got.Name != "ana" || got.BoardID != board.ID {
		t.Errorf("participant mismatch: %+v", got)
	}
	if _, err := store.GetParticipantByToken(ctx, board.ID, "wrong-token"); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong token err = %v, want ErrNotFound", err)
	}
	other := seedBoard(t, store)
	if _, err := store.GetParticipantByToken(ctx, other.ID, "participant-raw-token-ana"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-board token err = %v, want ErrNotFound", err)
	}
}

func TestCreateParticipantWithSuffix(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)

	want := []string{"ana", "ana-2", "ana-3"}
	for i, name := range want {
		p, err := store.CreateParticipantWithSuffix(ctx, board.ID, "ana", "raw-token-"+string(rune('a'+i)))
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		if p.Name != name {
			t.Errorf("join %d name = %q, want %q", i, p.Name, name)
		}
	}
	if _, err := store.CreateParticipantWithSuffix(ctx, board.ID, "   ", "tok"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("blank name err = %v, want ErrInvalidInput", err)
	}
}

func TestCreateParticipantWithSuffixConcurrent(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)

	const n = 8
	names := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := store.CreateParticipantWithSuffix(ctx, board.ID, "ana", "raw-token-conc-"+string(rune('a'+i)))
			if err == nil {
				names[i] = p.Name
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
	}
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || seen[name] {
			t.Fatalf("names not distinct: %v", names)
		}
		seen[name] = true
	}
}

func TestRotateFacilitatorTokenConcurrent(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	oldHash := board.FacilitatorTokenHash

	const n = 8
	won := make([]bool, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rotated, err := store.RotateFacilitatorToken(ctx, board.ID, oldHash, HashToken("new-token"))
			if err != nil {
				t.Errorf("rotation %d: %v", i, err)
				return
			}
			won[i] = rotated
		}(i)
	}
	wg.Wait()
	winners := 0
	for _, w := range won {
		if w {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("concurrent rotation winners = %d, want exactly 1", winners)
	}
}

func TestCreateSeededBoardSeedsEverything(t *testing.T) {
	sqldb := openTestDB(t)
	store := NewStore(sqldb)
	ctx := t.Context()

	src := seedBoard(t, store)
	open, err := store.CreateAction(ctx, src.ID, "carry me", "ana", "bo", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	done, err := store.CreateAction(ctx, src.ID, "leave me", "", "bo", nil)
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}
	if err := store.SetActionDone(ctx, done.ID, true); err != nil {
		t.Fatalf("SetActionDone: %v", err)
	}

	board, err := store.CreateSeededBoard(ctx, SeededBoard{
		Name:             "Seeded",
		Context:          "Sprint 12",
		FacilitatorToken: "facilitator-raw-token",
		VotesPerPerson:   3,
		Columns:          []ColumnSeed{{Title: "Mad", Color: "red"}, {Title: "Glad", Color: "green"}},
		CarryFromBoardID: &src.ID,
		ParticipantName:  "Facilitator",
		ParticipantToken: "participant-raw-token",
	})
	if err != nil {
		t.Fatalf("CreateSeededBoard: %v", err)
	}
	if board.Name != "Seeded" || board.FacilitatorTokenHash != HashToken("facilitator-raw-token") {
		t.Errorf("seeded board mismatch: %+v", board)
	}

	cols, err := store.ListColumns(ctx, board.ID)
	if err != nil {
		t.Fatalf("ListColumns: %v", err)
	}
	if len(cols) != 2 || cols[0].Title != "Mad" || cols[0].Position != 0 || cols[1].Title != "Glad" || cols[1].Position != 1 {
		t.Errorf("seeded columns mismatch: %+v", cols)
	}

	carried, err := store.ListOpenActions(ctx, board.ID)
	if err != nil {
		t.Fatalf("ListOpenActions: %v", err)
	}
	if len(carried) != 1 || carried[0].Text != "carry me" {
		t.Fatalf("carried actions = %+v, want only the open item", carried)
	}
	if carried[0].CarriedFrom == nil || *carried[0].CarriedFrom != open.ID {
		t.Errorf("carried_from = %v, want %d", carried[0].CarriedFrom, open.ID)
	}

	participant, err := store.GetParticipantByToken(ctx, board.ID, "participant-raw-token")
	if err != nil {
		t.Fatalf("GetParticipantByToken: %v", err)
	}
	if participant.Name != "Facilitator" {
		t.Errorf("seeded participant = %q, want Facilitator", participant.Name)
	}
}

func TestCreateSeededBoardRollsBackOnSeedFailure(t *testing.T) {
	sqldb := openTestDB(t)
	store := NewStore(sqldb)
	ctx := t.Context()

	// The carry-over source does not exist, so seeding fails after the
	// board row and the columns are already inserted: the single
	// transaction must roll all of it back.
	missing := int64(9999)
	_, err := store.CreateSeededBoard(ctx, SeededBoard{
		Name:             "Half-seeded",
		FacilitatorToken: "facilitator-raw-token",
		VotesPerPerson:   3,
		Columns:          []ColumnSeed{{Title: "Mad", Color: "red"}, {Title: "Sad", Color: "blue"}},
		CarryFromBoardID: &missing,
		ParticipantName:  "Facilitator",
		ParticipantToken: "participant-raw-token",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateSeededBoard err = %v, want ErrNotFound", err)
	}
	for _, table := range []string{"boards", "columns", "participants", "actions"} {
		var n int
		if err := sqldb.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("table %s holds %d rows after failed seed, want 0 (no half-seeded residue)", table, n)
		}
	}
}

func TestShareIDsCarryFullEntropy(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()

	seen := map[string]bool{}
	for range 100 {
		board, err := store.CreateBoard(ctx, "Entropy", "", "fac-token", 3)
		if err != nil {
			t.Fatalf("CreateBoard: %v", err)
		}
		if seen[board.PublicID] {
			t.Fatalf("duplicate public ID %q", board.PublicID)
		}
		seen[board.PublicID] = true
		raw, err := base64.RawURLEncoding.DecodeString(board.PublicID)
		if err != nil {
			t.Fatalf("public ID %q is not base64url: %v", board.PublicID, err)
		}
		if len(raw) < 16 {
			t.Fatalf("public ID decodes to %d bytes, want >= 16 (128-bit)", len(raw))
		}
	}

	raw, err := base64.RawURLEncoding.DecodeString(mustGenerateToken(t))
	if err != nil {
		t.Fatalf("generated token is not base64url: %v", err)
	}
	if len(raw) < 16 {
		t.Errorf("generated token decodes to %d bytes, want >= 16 (128-bit)", len(raw))
	}
}

func mustGenerateToken(t *testing.T) string {
	t.Helper()

	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	return tok
}

func TestRotateFacilitatorToken(t *testing.T) {
	store := NewStore(openTestDB(t))
	ctx := t.Context()
	board := seedBoard(t, store)
	oldHash := board.FacilitatorTokenHash

	rotated, err := store.RotateFacilitatorToken(ctx, board.ID, oldHash, HashToken("brand-new-token"))
	if err != nil || !rotated {
		t.Fatalf("RotateFacilitatorToken = %v, %v; want true, nil", rotated, err)
	}
	fresh, err := store.GetBoardByID(ctx, board.ID)
	if err != nil {
		t.Fatalf("GetBoardByID: %v", err)
	}
	if fresh.FacilitatorTokenHash == oldHash {
		t.Error("rotation left the old hash in place")
	}
	if !CompareTokenHash(fresh.FacilitatorTokenHash, HashToken("brand-new-token")) {
		t.Error("rotation did not store the new hash")
	}

	// A stale old hash loses: exactly one raw value stays valid.
	rotated, err = store.RotateFacilitatorToken(ctx, board.ID, oldHash, HashToken("another-token"))
	if err != nil || rotated {
		t.Fatalf("stale RotateFacilitatorToken = %v, %v; want false, nil", rotated, err)
	}
	if _, err := store.RotateFacilitatorToken(ctx, board.ID, oldHash, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty rotation err = %v, want ErrInvalidInput", err)
	}
}
